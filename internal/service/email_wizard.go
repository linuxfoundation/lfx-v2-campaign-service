// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/utm"
)

// The email-creation wizard: plan → generate two content variants → clone into a HubSpot
// draft → set the send list, with a chat turn available throughout.
//
// Every turn is a SEPARATE request against a persisted session row, rather than one long
// call holding state in memory. That is what makes the flow survive a pod replacement
// between two clicks, and it is why the session repository is a hard dependency of these
// methods while progress streaming is not: the frames are a convenience, the row is the
// truth. A client that never opens the SSE stream still gets every result, because each
// turn also RETURNS what it published.
//
// The wizard is an OPTIONAL capability. A deployment with no session repository wired
// answers these eight routes with the typed 503 and serves every other brief route
// normally, so a missing wizard never degrades the rest of the service.

// HubSpotWizardClient is the slice of the HubSpot client the wizard uses.
//
// Declared here, in the consumer, rather than taking *hubspot.Client: the wizard's tests
// need a fake, and this service must not import internal/dispatch (dispatch's own tests
// import this package, so the edge would close a cycle). *hubspot.Client satisfies it
// as-is, so the container passes the real client with no adapter of its own.
type HubSpotWizardClient interface {
	SearchEmails(ctx context.Context, query string) ([]hubspot.Email, error)
	GetEmail(ctx context.Context, id string) (*hubspot.Email, error)
	CloneEmail(ctx context.Context, sourceID, cloneName string) (*hubspot.Email, error)
	PatchEmailSettings(ctx context.Context, id string, settings hubspot.EmailSettings) (*hubspot.Email, error)
	SetSendList(ctx context.Context, id, ilsListID string, suppressionListIDs []string) (*hubspot.Email, error)
	GetEmailHTMLWidgets(ctx context.Context, id string) ([]hubspot.EmailHTMLBlock, error)
	SetEmailHTMLWidgets(ctx context.Context, id string, widgets map[string]string) (*hubspot.Email, error)
	AuthenticatedPortalID(ctx context.Context) (string, error)
}

// HubSpotClientResolver hands the wizard a HubSpot client for one project.
//
// Resolution is deliberately PER REQUEST rather than a client held at wiring time, because
// that is what the rest of this service does (see internal/dispatch/creds.go): a project's
// connection can be added, rotated or revoked between two wizard turns, and a cached client
// would keep writing with a credential the project has since withdrawn.
type HubSpotClientResolver interface {
	// fromSystem reports that the client came from the LF-wide fallback rather than a row this
	// project owns. It is part of the contract rather than an optional extra because the search
	// below MUST refuse on it: HubSpot's email namespace is portal-wide and `projectID` only
	// chose the CONNECTION, so on the shared row a match can be another project's sent email.
	ResolveHubSpotClient(ctx context.Context, projectID string) (client HubSpotWizardClient, fromSystem bool, err error)
}

// SetWizardBackend late-binds the wizard's collaborators, alongside SetBackend on both
// startup paths. Nil arguments leave the corresponding capability unwired, which the
// handlers report as 503 rather than nil-panicking.
func (s *BriefService) SetWizardBackend(sessions domain.WizardSessionRepository, resolver HubSpotClientResolver, audiences domain.AudienceRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wizardSessions = sessions
	s.wizardHubSpot = resolver
	s.wizardAudiences = audiences
	if s.wizardProgress == nil {
		s.wizardProgress = NewWizardProgressHub()
	}
}

// WizardSessionRepoIsSet reports whether the wizard's session repository was bound.
//
// Exported for the container test that pins the shared live-wiring helper, exactly as
// CreativeAssetRepoIsSet is: the interface contract forces SetWizardBackend to EXIST, and
// only an observation like this can prove either startup path actually calls it.
func (s *BriefService) WizardSessionRepoIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wizardSessions != nil
}

// wizardDeps snapshots the wizard collaborators under the read lock.
func (s *BriefService) wizardDeps() (domain.WizardSessionRepository, HubSpotClientResolver, domain.AudienceRepository) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wizardSessions, s.wizardHubSpot, s.wizardAudiences
}

// WizardProgress returns the session's progress hub, creating it on first use.
//
// Non-nil always, including in the no-database mode: the SSE route is mounted at boot and
// a nil hub there would turn a stream request into a panic instead of an empty stream.
func (s *BriefService) WizardProgress() *WizardProgressHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wizardProgress == nil {
		s.wizardProgress = NewWizardProgressHub()
	}
	return s.wizardProgress
}

// wizardReady snapshots what every wizard turn needs — the brief repository and the session
// repository — and returns the typed 503 when either is missing.
func (s *BriefService) wizardReady() (domain.BriefRepository, domain.WizardSessionRepository, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, nil, err
	}
	sessions, _, _ := s.wizardDeps()
	if sessions == nil {
		// Availability-neutral, like ready(): in cold-start mode the wizard IS configured and
		// simply has not bound yet, so "not configured" would send an operator to change
		// config during a startup window that clears on its own.
		return nil, nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the email wizard is unavailable"}
	}
	return briefRepo, sessions, nil
}

// wizardHubSpotClient resolves this project's HubSpot client, or returns the typed 503.
func (s *BriefService) wizardHubSpotClient(ctx context.Context, projectID string) (HubSpotWizardClient, bool, error) {
	_, resolver, _ := s.wizardDeps()
	if resolver == nil {
		return nil, false, &briefs.ConnServiceUnavailableError{Code: "503", Message: "HubSpot is not configured for this deployment"}
	}
	// `fromSystem` is RETURNED, not discarded. A caller that only writes this project's own
	// content to the shared portal may ignore it -- that is the legitimate half of the fallback
	// -- but a caller that reads or clones by an id captured in an EARLIER turn must not, because
	// that id was recorded against whatever portal resolved back then.
	client, fromSystem, err := resolver.ResolveHubSpotClient(ctx, projectID)
	if err != nil {
		// 503, not 400: an unresolvable or unusable connection is a configuration state of
		// the project, not a defect in the request the caller just made.
		slog.WarnContext(ctx, "wizard could not resolve a hubspot client",
			"project_id", projectID, "error", safeErrSummary(err))
		return nil, false, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "no usable HubSpot connection is available for this project",
		}
	}
	if client == nil {
		return nil, false, &briefs.ConnServiceUnavailableError{Code: "503", Message: "no usable HubSpot connection is available for this project"}
	}
	return client, fromSystem, nil
}

// mapWizardErr maps domain errors for the wizard routes. It defers to mapBriefErr for
// everything the brief routes already classify, and adds the one sentinel only the wizard
// produces.
func mapWizardErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrStaleWizardSession) {
		// 409, not 412: there is no ETag on these routes for a caller to have supplied, so
		// "precondition failed" would point at a header they never sent. The session moved
		// under them — re-read it and retry the turn.
		return &briefs.ConflictError{
			Code:    "409",
			Message: "this wizard session changed since it was read; re-read the session and retry the turn",
		}
	}
	return mapBriefErr(err)
}

// loadWizardSession reads a session scoped to its project and brief.
// The parent brief is verified ACTIVE here, not at the call sites. It was checked in four of
// the seven wizard handlers and forgotten in three (send-list, update-sections, get-session),
// which is the shape a per-handler rule always ends in: every new endpoint is another chance
// to forget, and the omission is invisible because the session still loads perfectly.
//
// It matters because ArchiveBrief is a SOFT delete. The session rows survive it, so without
// this a caller holding a session id could go on driving a wizard for a brief the operator
// deleted -- and SetWizardSendList would mutate the HubSpot draft's recipients, an effect
// outside this database entirely. GetBrief already filters `status <> 'archived'`, so asking
// it is the whole check; the answer is a 404, which is what the brief being gone means.
func loadWizardSession(ctx context.Context, briefRepo domain.BriefRepository, sessions domain.WizardSessionRepository, projectID, briefID, sessionID string) (*model.WizardSession, *model.CampaignBrief, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil, &briefs.BadRequestError{Code: "400", Message: "session_id is required"}
	}
	// ORDER MATTERS: the session is read FIRST, the brief second.
	//
	// The reverse order leaves a window. A delete that archives and scrubs between the two
	// reads would pass the brief check (it ran before the archive) and then hand back the
	// POST-scrub session -- whose version a later generate or chat save still matches, so the
	// save succeeds and repopulates exactly the content the deletion just cleared.
	//
	// Reading the session first inverts that: any scrub landing afterwards bumps `version`
	// past the value this caller holds, and `saveWizardSession` -- which writes back at the
	// version it read -- fails stale. The brief check then runs against the newer state and
	// rejects the turn anyway. The two guards cover each other only in this order.
	sess, err := sessions.GetSession(ctx, projectID, briefID, sessionID)
	if err != nil {
		return nil, nil, mapWizardErr(err)
	}
	// RETURNED, not discarded. Four handlers re-fetched this same row immediately afterwards,
	// putting two synchronous Postgres round trips for one brief on the critical path of every
	// wizard turn -- and inviting the two reads to drift to different consistency points.
	brief, gerr := briefRepo.GetBrief(ctx, projectID, briefID)
	if gerr != nil {
		return nil, nil, mapBriefErr(gerr)
	}
	return sess, brief, nil
}

// saveWizardSession writes the session back at the version it was read at.
func saveWizardSession(ctx context.Context, sessions domain.WizardSessionRepository, sess *model.WizardSession, actor *model.Actor) (*model.WizardSession, error) {
	sess.UpdatedBy = actor
	saved, err := sessions.UpdateSession(ctx, sess, sess.Version)
	if err != nil {
		return nil, mapWizardErr(err)
	}
	return saved, nil
}

// publishWizardProgress sends one frame on the session's token, if the caller asked for
// progress at all.
//
// Fire-and-forget on purpose: the hub drops frames for a token nobody is listening on, and
// a turn must not fail or block because a browser closed its stream. Every frame published
// here is also part of the turn's own response.
// The token is the SESSION's, always. A caller-supplied override used to win, which meant any
// plan or generate request could publish plan_done, content or error frames onto any token it
// knew -- and the SSE handler, which verifies a token against the session that OWNS it, would
// hand those frames to that session's subscriber. Binding the stream at mint time is pointless
// if a later turn can address someone else's.
//
// The override parameter is gone rather than ignored: a parameter that is accepted and
// discarded reads at the call site as though it still does something.
func (s *BriefService) publishWizardProgress(sess *model.WizardSession, frame WizardProgressFrame) {
	token := strings.TrimSpace(sess.ProgressToken)
	if token == "" {
		return
	}
	s.WizardProgress().Publish(token, frame)
}

// -----------------------------------------------------------------------------
// Event facts
// -----------------------------------------------------------------------------

// wizardEventDetails is the wizard's view of a brief's opaque event_details blob.
//
// A separate decode from emailCopyEventDetails rather than an extension of it: the copy
// endpoint's struct is frozen against a byte-identity requirement (LFXV2-1940), and the
// wizard needs three fields it does not carry — speakers, sponsors and the banner image.
// Decoding twice from the same blob costs nothing and keeps that guarantee out of reach.
type wizardEventDetails struct {
	EventName       string `json:"eventName"`
	Description     string `json:"description"`
	Location        string `json:"location"`
	StartDate       string `json:"startDate"`
	EndDate         string `json:"endDate"`
	Dates           string `json:"dates"`
	Image           string `json:"image"`
	RegistrationURL string `json:"registrationUrl"`
	URL             string `json:"url"`
	Speakers        []string
	Sponsors        []wizardEventSponsor `json:"sponsors"`
}

type wizardEventSponsor struct {
	Name string `json:"name"`
	Logo string `json:"logoUrl"`
	URL  string `json:"url"`
	Tier string `json:"tier"`
}

// maxWizardFactListEntries bounds how many speakers or sponsors are carried into a prompt or
// a response. The parser already bounds what it extracts, but event_details is declared Any
// in the design and is therefore writable by a caller directly.
const maxWizardFactListEntries = 40

// decodeWizardEventDetails pulls the wizard's facts out of the brief's blob.
//
// An absent or unparseable blob is NOT an error here, unlike in GenerateEmailCopy. The
// wizard's first turn is planning, and a brief whose scrape failed is exactly the case an
// operator opens the wizard to fix by hand: refusing at the door would leave them with no
// way in. The endpoints that cannot work without an event name say so individually.
func decodeWizardEventDetails(blob json.RawMessage) wizardEventDetails {
	var d wizardEventDetails
	if len(blob) == 0 {
		return d
	}
	if err := json.Unmarshal(blob, &d); err != nil {
		return wizardEventDetails{}
	}
	if len(d.Speakers) > maxWizardFactListEntries {
		d.Speakers = d.Speakers[:maxWizardFactListEntries]
	}
	if len(d.Sponsors) > maxWizardFactListEntries {
		d.Sponsors = d.Sponsors[:maxWizardFactListEntries]
	}
	return d
}

// wizardFacts assembles the prompt facts from a brief and its details.
func wizardFacts(brief *model.CampaignBrief, d wizardEventDetails, extraContext, emailType, changeRequest string) wizardPromptFacts {
	f := wizardPromptFacts{
		eventName: strings.TrimSpace(d.EventName),
		location:  strings.TrimSpace(d.Location),
		dates:     wizardDates(d),
		// The brief's own url column is the primary, with the blob's registrationUrl as the
		// fallback — the same precedence resolveRegistrationURL documents.
		registrationURL: firstNonEmpty(httpURL(brief.URL), httpURL(d.RegistrationURL)),
		stage:           strings.TrimSpace(brief.Stage),
		extraContext:    strings.TrimSpace(extraContext),
		emailType:       strings.TrimSpace(emailType),
		changeRequest:   strings.TrimSpace(changeRequest),
	}
	for _, sp := range d.Speakers {
		if v := strings.TrimSpace(sp); v != "" {
			f.speakers = append(f.speakers, v)
		}
	}
	for _, sp := range d.Sponsors {
		if v := strings.TrimSpace(sp.Name); v != "" {
			f.sponsors = append(f.sponsors, v)
		}
	}
	return f
}

// wizardDates formats the event's dates, preferring the scraper's combined `dates` string.
func wizardDates(d wizardEventDetails) string {
	if v := strings.TrimSpace(d.Dates); v != "" {
		return v
	}
	start, end := strings.TrimSpace(d.StartDate), strings.TrimSpace(d.EndDate)
	switch {
	case start != "" && end != "" && start != end:
		return start + " - " + end
	case start != "":
		return start
	case end != "":
		return end
	}
	// No "Date TBD" default, unlike the copy endpoint: the wizard OMITS facts it does not
	// have (see factBlock), and a literal "Date TBD" in the prompt is a fact the model will
	// happily print into a marketing email.
	return ""
}

// -----------------------------------------------------------------------------
// plan-start
// -----------------------------------------------------------------------------

// StartEmailWizardPlan opens a session and returns its progress token, doing no other work.
//
// The split from plan-email-wizard is the whole point of this endpoint: the client needs the
// token BEFORE the turn that emits frames, or it subscribes after planning has already
// finished and shows an empty progress log for work that completed.
func (s *BriefService) StartEmailWizardPlan(ctx context.Context, p *briefs.StartEmailWizardPlanPayload) (*briefs.WizardPlanStart, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	// Read the brief first: a session for a brief that does not exist (or belongs to another
	// project) must be a 404, not a foreign-key error from the insert below.
	if _, gerr := briefRepo.GetBrief(ctx, p.ProjectID, p.BriefID); gerr != nil {
		return nil, mapBriefErr(gerr)
	}

	// ALWAYS server-minted. A caller-supplied token was accepted here, and the column is
	// deliberately not UNIQUE, so two sessions in DIFFERENT projects could hold the same one.
	// `WizardProgressHub.Publish` fans a frame to every subscriber of a token, and
	// `GetSessionByToken` resolves the NEWEST holder -- so an attacker who chose a token
	// colliding with a victim's passed the handler's project check against their OWN row and
	// then received the victim's frames.
	//
	// A UUID the caller cannot influence removes the collision rather than trying to detect it.
	// The caller's value is ignored rather than rejected: it is echoed back in the response, so
	// a client that sends one still gets a usable token, just not one it chose.
	token := uuid.NewString()

	sess := &model.WizardSession{
		ProjectID:     p.ProjectID,
		BriefID:       p.BriefID,
		Phase:         model.WizardPhasePlanning,
		ProgressToken: token,
		CreatedBy:     attributedActor(ctx, "start-email-wizard-plan"),
	}
	// The guidance supplied HERE is recorded immediately, because plan-start and plan are
	// separate requests and a client that sends it to only one of them must not lose it.
	// The plan turn overwrites this blob with its own, carrying the guidance forward.
	if g := (wizardPlan{
		Mode:            "planning",
		ExtraContext:    strings.TrimSpace(strVal(p.ExtraContext)),
		EmailType:       strings.TrimSpace(strVal(p.EmailType)),
		IsTransactional: p.IsTransactional != nil && *p.IsTransactional,
		URL:             strings.TrimSpace(strVal(p.URL)),
	}); g.ExtraContext != "" || g.EmailType != "" || g.IsTransactional || g.URL != "" {
		if blob, merr := json.Marshal(g); merr == nil {
			sess.PlanResult = blob
		}
	}
	saved, cerr := sessions.CreateSession(ctx, sess)
	if cerr != nil {
		slog.WarnContext(ctx, "could not open an email wizard session",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", safeErrSummary(cerr))
		return nil, mapWizardErr(cerr)
	}
	return &briefs.WizardPlanStart{SessionID: saved.ID, Token: saved.ProgressToken}, nil
}

// -----------------------------------------------------------------------------
// plan
// -----------------------------------------------------------------------------

// wizardPlan is what the planning turn records on the session and replays afterwards. It is
// stored as JSON and handed back as `Any`, so its keys are the wire contract with the UI.
type wizardPlan struct {
	Mode        string         `json:"mode"`
	Message     string         `json:"message"`
	SourceEmail *wizardSource  `json:"source_email,omitempty"`
	Stage       map[string]any `json:"stage,omitempty"`
	UTM         map[string]any `json:"utm,omitempty"`
	// The operator's guidance, carried here because it is supplied on plan-start and
	// plan — which are separate requests from generate-content, whose payload has no field
	// for it. Storing it on the session is what makes "write it for sponsors, not
	// attendees" survive the turn boundary instead of being silently dropped.
	ExtraContext    string `json:"extra_context,omitempty"`
	EmailType       string `json:"email_type,omitempty"`
	IsTransactional bool   `json:"is_transactional,omitempty"`
	// URL is the event page the caller asked the wizard to plan from, when they supplied it at
	// plan-start rather than plan. It was the ONE optional plan-start field that was accepted and
	// then never read: a caller who supplied it as the contract documents got a 200 and planning
	// from the brief's own scraped details instead, with nothing saying so.
	URL string `json:"url,omitempty"`
}

type wizardSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PlanEmailWizard runs the planning turn: resolve the event facts, resolve the stage, and
// choose a past-campaign email to clone.
//
// Synchronous, and it publishes the same frames it returns. Planning is fast (one optional
// page fetch and one optional HubSpot search) and the client cannot proceed without the
// result, so a job row and a poll loop would add a failure mode without buying anything.
func (s *BriefService) PlanEmailWizard(ctx context.Context, p *briefs.PlanEmailWizardPayload) (*briefs.WizardPlanResult, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	sess, brief, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	s.publishWizardProgress(sess, WizardProgressFrame{Type: "brief", Text: "Resolving the event details"})
	// Read BEFORE resolving the details, because the url recorded at plan-start is a fallback for
	// this turn's own. A caller may send it to either request -- the contract documents the field
	// on both -- and plan-start's copy was previously accepted and never read, so pointing the
	// wizard at a different page there silently planned from the brief's own scraped details.
	prior := wizardStoredPlan(sess)
	requestedURL := strVal(p.URL)
	if strings.TrimSpace(requestedURL) == "" {
		requestedURL = prior.URL
	}
	details := s.resolveWizardDetails(ctx, brief, requestedURL)

	tpl := emailstage.Resolve(brief.Stage)
	plan := wizardPlan{
		Mode: "stage",
		// This turn's guidance wins; anything it omits keeps what plan-start recorded.
		ExtraContext:    firstNonEmpty(strings.TrimSpace(strVal(p.ExtraContext)), prior.ExtraContext),
		EmailType:       firstNonEmpty(strings.TrimSpace(strVal(p.EmailType)), prior.EmailType),
		IsTransactional: (p.IsTransactional != nil && *p.IsTransactional) || prior.IsTransactional,
		// Carried forward like the rest. `requestedURL` already resolved this turn's value
		// against plan-start's, so rebuilding the blob without it would DROP a url the caller
		// supplied at plan-start -- the same silent loss this field was added to close.
		URL: strings.TrimSpace(requestedURL),
		Stage: map[string]any{
			"stage_name":            tpl.StageName,
			"purpose":               tpl.Purpose,
			"timing":                tpl.Timing,
			"tone":                  tpl.Tone,
			"urgency_level":         tpl.UrgencyLevel,
			"cta_strategy":          tpl.CTAStrategy,
			"primary_cta":           tpl.PrimaryCTA,
			"links_to_registration": tpl.LinksToRegistration,
		},
	}

	// A past-campaign email is a NICE-TO-HAVE for planning and a REQUIREMENT for cloning, so
	// a search failure is logged and carried rather than failing the turn: the operator can
	// still review the plan, edit the brief and re-plan.
	s.publishWizardProgress(sess, WizardProgressFrame{Type: "brief", Text: "Looking for a past campaign email to clone"})
	if src, found := s.findWizardSourceEmail(ctx, p.ProjectID, details, tpl); found {
		plan.Mode = "reference"
		plan.SourceEmail = src
	}

	res := utm.Resolve("", wizardDraftName(details, tpl, s.now()))
	plan.UTM = map[string]any{
		"source":   res.Params.Source,
		"medium":   res.Params.Medium,
		"campaign": res.Params.Campaign,
		"term":     res.Params.Term,
		"resolved": res.Source,
	}
	plan.Message = wizardPlanMessage(details, tpl, plan)

	if blob, merr := json.Marshal(plan); merr == nil {
		sess.PlanResult = blob
	} else {
		// The turn still succeeds: the plan is returned to the caller either way, and a
		// session that cannot replay its plan is degraded, not broken.
		slog.WarnContext(ctx, "could not record the wizard plan on the session",
			"session_id", sess.ID, "error", safeErrSummary(merr))
	}
	saved, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "plan-email-wizard"))
	if uerr != nil {
		return nil, uerr
	}

	out := &briefs.WizardPlanResult{
		SessionID: saved.ID,
		Message:   plan.Message,
		Phase:     string(saved.PhaseOrDefault()),
		Mode:      plan.Mode,
		Stage:     plan.Stage,
		Utm:       plan.UTM,
	}
	if plan.SourceEmail != nil {
		out.SourceEmail = &briefs.WizardSourceEmail{ID: plan.SourceEmail.ID, Name: plan.SourceEmail.Name}
	}
	s.publishWizardProgress(saved, WizardProgressFrame{
		Type: "plan_done", Text: plan.Message, Result: out, Done: true,
	})
	return out, nil
}

// resolveWizardDetails prefers the brief's CACHED scrape and fetches the page only when the
// cache has no event name and a URL is available.
//
// Cache-first rather than always-fetch: the brief's details are what the operator reviewed
// and possibly corrected by hand when the brief was created, and re-scraping would silently
// replace their corrections with whatever the page says today.
func (s *BriefService) resolveWizardDetails(ctx context.Context, brief *model.CampaignBrief, requestedURL string) wizardEventDetails {
	details := decodeWizardEventDetails(brief.EventDetails)
	if strings.TrimSpace(details.EventName) != "" {
		return details
	}
	pageURL := firstNonEmpty(httpURL(requestedURL), httpURL(brief.URL), httpURL(details.URL))
	fetcher, parser := s.eventURLDeps()
	if pageURL == "" || fetcher == nil || parser == nil {
		return details
	}
	body, ferr := fetcher.Fetch(ctx, pageURL)
	if ferr != nil {
		slog.WarnContext(ctx, "wizard could not fetch the event page; planning from the brief alone",
			"brief_id", brief.ID, "error", safeErrSummary(ferr))
		return details
	}
	parsed := parser.Parse(body)
	details.EventName = firstNonEmpty(details.EventName, parsed.Name)
	details.Description = firstNonEmpty(details.Description, parsed.Description)
	details.Location = firstNonEmpty(details.Location, parsed.Location)
	details.StartDate = firstNonEmpty(details.StartDate, parsed.StartDate)
	details.EndDate = firstNonEmpty(details.EndDate, parsed.EndDate)
	details.Image = firstNonEmpty(details.Image, parsed.Image)
	details.URL = firstNonEmpty(details.URL, parsed.URL)
	details.RegistrationURL = firstNonEmpty(details.RegistrationURL, parsed.RegistrationURL)
	if len(details.Speakers) == 0 {
		details.Speakers = parsed.Speakers
	}
	if len(details.Sponsors) == 0 {
		for _, sp := range parsed.Sponsors {
			details.Sponsors = append(details.Sponsors, wizardEventSponsor{
				Name: sp.Name, Logo: sp.Logo, URL: sp.URL, Tier: sp.Tier,
			})
		}
	}
	return decodeWizardEventDetailsBound(details)
}

// decodeWizardEventDetailsBound re-applies the list bounds after a merge.
func decodeWizardEventDetailsBound(d wizardEventDetails) wizardEventDetails {
	if len(d.Speakers) > maxWizardFactListEntries {
		d.Speakers = d.Speakers[:maxWizardFactListEntries]
	}
	if len(d.Sponsors) > maxWizardFactListEntries {
		d.Sponsors = d.Sponsors[:maxWizardFactListEntries]
	}
	return d
}

// findWizardSourceEmail searches the project's HubSpot portal for a past email to clone.
//
// Returns found=false for every failure, including an unconfigured connection: this is a
// planning HINT, and a portal outage must not stop an operator planning an email they will
// clone ten minutes later.
func (s *BriefService) findWizardSourceEmail(ctx context.Context, projectID string, d wizardEventDetails, tpl emailstage.Template) (*wizardSource, bool) {
	_, resolver, _ := s.wizardDeps()
	if resolver == nil {
		return nil, false
	}
	query := strings.TrimSpace(d.EventName)
	if query == "" {
		return nil, false
	}
	client, fromSystem, err := resolver.ResolveHubSpotClient(ctx, projectID)
	if err != nil || client == nil {
		slog.InfoContext(ctx, "wizard planning found no hubspot connection; planning without a clone source",
			"project_id", projectID)
		return nil, false
	}
	// REFUSED on the shared portal, for the same reason `EmailReferenceSource.Get` refuses:
	// SearchEmails is portal-WIDE and `projectID` only chose the connection -- it never filters
	// the results. On the system fallback a hit can be another project's past sent email, and
	// cloning it would seed this project's draft with that one's subject, body, sponsor names and
	// pricing. A cross-tenant read with no consent surface, and the COMMON case rather than an
	// edge one, because every project without its own connection resolves here.
	//
	// Unlike the dispatch-side fallback, which only ever WRITES the requesting project's own
	// content to the shared portal, this one reads everyone's.
	//
	// Degrades to planning from the stage guide -- exactly what a project with no HubSpot history
	// gets anyway. Re-enabling needs a per-project ownership signal on the emails themselves;
	// there is none today.
	if fromSystem {
		slog.InfoContext(ctx, "wizard clone-source search skipped: only the shared LF connection is available, and a portal-wide search would cross project boundaries",
			"project_id", projectID)
		return nil, false
	}
	// Bound the WHOLE search, not each request inside it. SearchEmails paginates -- up to 200
	// pages -- and only the individual calls carry a deadline, so a slow portal could keep this
	// planning handler running long past the server's write deadline while every single request
	// looked healthy. Same budget and same reason as the orchestrator's account listing.
	searchCtx, cancelSearch := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancelSearch()
	hits, serr := client.SearchEmails(searchCtx, query)
	if serr != nil {
		slog.WarnContext(ctx, "wizard could not search past hubspot emails; planning without a clone source",
			"project_id", projectID, "error", safeErrSummary(serr))
		return nil, false
	}
	if len(hits) == 0 {
		return nil, false
	}
	// SearchEmails returns most-recently-updated first. Prefer a hit whose name also mentions
	// this stage — an event's series has one email per stage, and cloning last year's
	// registration push to build a thank-you would start the operator from the wrong shape.
	if stage := strings.TrimSpace(tpl.StageName); stage != "" {
		for _, h := range hits {
			if strings.Contains(strings.ToLower(h.Name), strings.ToLower(stage)) {
				return &wizardSource{ID: h.ID, Name: h.Name}, true
			}
		}
	}
	return &wizardSource{ID: hits[0].ID, Name: hits[0].Name}, true
}

// wizardPlanMessage writes the transcript line the wizard shows for the plan.
func wizardPlanMessage(d wizardEventDetails, tpl emailstage.Template, plan wizardPlan) string {
	var b strings.Builder
	name := strings.TrimSpace(d.EventName)
	if name == "" {
		// Said plainly rather than papered over: without an event name the generated copy has
		// almost nothing to work from, and the operator can fix it in the brief right now.
		b.WriteString("This brief has no event name yet, so the generated copy will be thin — add one to the brief for a better result. ")
	} else {
		fmt.Fprintf(&b, "Planning a %s email for %s. ", strings.ToLower(tpl.StageName), name)
	}
	if plan.SourceEmail != nil {
		fmt.Fprintf(&b, "I'll clone the past email %q and match its voice. ", plan.SourceEmail.Name)
	} else {
		b.WriteString("I found no past email to clone, so I'll write from the event details and the stage guide. ")
	}
	if dates := wizardDates(d); dates != "" {
		fmt.Fprintf(&b, "Dates: %s. ", dates)
	}
	return strings.TrimSpace(b.String())
}

// wizardDraftName is the name a cloned draft is given, and the string UTM resolution
// slugifies when the brief carries no campaign of its own.
func wizardDraftName(d wizardEventDetails, tpl emailstage.Template, now time.Time) string {
	name := strings.TrimSpace(d.EventName)
	if name == "" {
		name = "LFX Campaign"
	}
	return fmt.Sprintf("%s - %s - %s", truncateString(name, 120), tpl.StageName, now.Format("2006-01-02"))
}

// -----------------------------------------------------------------------------
// generate-content
// -----------------------------------------------------------------------------

// wizardVariant is one generated variant as persisted on the session and returned to the UI.
type wizardVariant struct {
	Subject     string `json:"subject"`
	PreviewText string `json:"preview_text"`
	HTML        string `json:"html"`
	BodyHTML    string `json:"body_html"`
	Sections    []any  `json:"sections"`
	BannerURL   string `json:"banner_url,omitempty"`
	TemplateKey string `json:"template_key,omitempty"`
	StageName   string `json:"stage_name,omitempty"`
	// Mode is "ai-generated" or "failed". A failed variant is STORED rather than dropped, so
	// the UI can show which one failed and why instead of silently offering one option where
	// the operator expects two.
	Mode  string `json:"mode"`
	Error string `json:"error,omitempty"`
}

// GenerateWizardContent generates both content variants for a planned session.
//
// The two are independent model calls, and their failure handling is deliberately
// asymmetric. The REFERENCE variant is the wizard's primary output, so its failure is the
// request's failure (503). The STAGE variant is an alternative to compare against, so its
// failure is reported inside a 200 as mode "failed" — failing the whole request would throw
// away a perfectly good primary variant that cost a model call to produce.
func (s *BriefService) GenerateWizardContent(ctx context.Context, p *briefs.GenerateWizardContentPayload) (*briefs.WizardContent, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	llmClient := s.snapshotLLMClient()
	if llmClient == nil {
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "AI model is not configured; wizard content generation is unavailable",
		}
	}
	sess, brief, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	details := decodeWizardEventDetails(brief.EventDetails)
	// The guidance comes off the SESSION, not this payload: generate-content has no field
	// for it (it is supplied on plan-start/plan), and dropping it here would generate copy
	// that ignores instructions the operator has already given.
	plan := wizardStoredPlan(sess)
	facts := wizardFacts(brief, details, plan.ExtraContext, plan.EmailType, strVal(p.ChangeRequest))
	if facts.size() > maxWizardFactsSize {
		// 400 and the caller's to fix: these values come from the brief and the request, and
		// the alternative — truncating an operator's guidance — would generate copy against
		// instructions they cannot see were cut.
		slog.WarnContext(ctx, "wizard content generation blocked: the event facts exceed the prompt size limit",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "input_size", facts.size(), "limit", maxWizardFactsSize)
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "the brief's event details and guidance are too large; shorten them before generating content",
		}
	}
	tpl := emailstage.Resolve(brief.Stage)

	// Reference variant.
	s.publishWizardProgress(sess, WizardProgressFrame{Type: "brief", Text: "Writing the email in your team's voice"})
	refs := s.wizardReferenceEmails(ctx, p.ProjectID, sess)
	refSystem, refUser := composeWizardReferencePrompt(facts, refs)
	reference, rerr := generateWizardVariant(ctx, llmClient, refSystem, refUser, details, tpl)
	if rerr != nil {
		slog.WarnContext(ctx, "wizard content generation failed on the primary variant",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", safeErrSummary(rerr))
		s.publishWizardProgress(sess, WizardProgressFrame{
			Type: "error", Error: "the email content could not be generated", Done: true,
		})
		return nil, wizardGenerationUnavailable(rerr)
	}

	// Stage variant. Its error is CARRIED, not returned.
	s.publishWizardProgress(sess, WizardProgressFrame{Type: "brief", Text: "Writing a second option from the stage guide"})
	stageSystem, stageUser := composeWizardStagePrompt(facts)
	stageVariant, verr := generateWizardVariant(ctx, llmClient, stageSystem, stageUser, details, tpl)
	if verr != nil {
		slog.WarnContext(ctx, "wizard stage variant failed; returning the reference variant alone",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", safeErrSummary(verr))
		stageVariant = &wizardVariant{
			Mode: "failed",
			// A SUMMARY, never the upstream error: this string is displayed in the UI, and an
			// upstream body can carry request ids and prompt fragments.
			Error:       "this option could not be generated; the other one is ready to use",
			TemplateKey: tpl.StageName,
			StageName:   tpl.StageName,
			Sections:    []any{},
		}
	}

	sess.ReferenceVariant = marshalAny(reference)
	sess.StageVariant = marshalAny(stageVariant)
	sess.Sections = marshalAny(reference.Sections)
	// Never BACKWARDS. `cloned` is documented as the first phase with an effect outside this
	// service and therefore the first that cannot be undone, so regressing a cloned or complete
	// session to `content` described a session whose draft and recipients still exist as one
	// that has no draft — and the freshly generated copy is not in that draft either, so the
	// row contradicted both HubSpot and itself.
	//
	// Regeneration after a clone is still ALLOWED: the new variants are saved and the operator
	// can push them into the existing draft. What is refused is the phase lie. Whether a
	// re-clone should instead be required is a product question, and answering it here would
	// make the answer a contract.
	if sess.PhaseOrDefault() == model.WizardPhasePlanning {
		sess.Phase = model.WizardPhaseContent
	}
	saved, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "generate-wizard-content"))
	if uerr != nil {
		return nil, uerr
	}

	out := &briefs.WizardContent{
		SessionID:           saved.ID,
		Subject:             reference.Subject,
		PreviewText:         reference.PreviewText,
		HTML:                reference.HTML,
		BodyHTML:            reference.BodyHTML,
		Sections:            reference.Sections,
		Sponsors:            wizardSponsorsOut(details),
		VariantASubject:     stageVariant.Subject,
		VariantAPreviewText: stageVariant.PreviewText,
		VariantAHTML:        stageVariant.HTML,
		VariantABodyHTML:    stageVariant.BodyHTML,
		VariantASections:    stageVariant.Sections,
		VariantAMode:        stageVariant.Mode,
	}
	if reference.BannerURL != "" {
		out.BannerURL = &reference.BannerURL
	}
	if stageVariant.BannerURL != "" {
		out.VariantABannerURL = &stageVariant.BannerURL
	}
	if stageVariant.TemplateKey != "" {
		out.VariantATemplateKey = &stageVariant.TemplateKey
	}
	s.publishWizardProgress(saved, WizardProgressFrame{
		Type: "plan_done", Text: "Both email options are ready to review", Result: out, Done: true,
	})
	return out, nil
}

// generateWizardVariant runs one model call and renders its output.
func generateWizardVariant(ctx context.Context, client *llm.Client, systemPrompt, userPrompt string, d wizardEventDetails, tpl emailstage.Template) (*wizardVariant, error) {
	if size := utf8.RuneCountInString(systemPrompt) + utf8.RuneCountInString(userPrompt); size > maxWizardComposedPromptSize {
		// A service-owned budget, not the caller's input: the facts were already bounded
		// above, so what overflowed is a compiled-in template or a reference excerpt.
		return nil, fmt.Errorf("composed prompt is %d runes, over the %d-rune budget", size, maxWizardComposedPromptSize)
	}
	raw, cerr := client.Complete(ctx, systemPrompt, userPrompt)
	if cerr != nil {
		return nil, cerr
	}
	parsed, perr := parseWizardContentResponse(raw)
	if perr != nil {
		return nil, perr
	}
	body := renderWizardSections(parsed.Sections)
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("the model's sections rendered to an empty email body")
	}
	return &wizardVariant{
		Subject:     parsed.Subject,
		PreviewText: parsed.PreviewText,
		BodyHTML:    body,
		HTML:        wizardPreviewHTML(parsed.Subject, parsed.PreviewText, body),
		Sections:    parsed.RawSections,
		BannerURL:   httpURL(d.Image),
		TemplateKey: tpl.StageName,
		StageName:   tpl.StageName,
		Mode:        "ai-generated",
	}, nil
}

// wizardGenerationUnavailable maps a variant failure onto the endpoint's 503.
//
// 503 rather than 500 or 400 for the same reason GenerateEmailCopy does it: an unreadable
// model response is this service's dependency failing, the caller's request was fine, and a
// retry may well succeed.
func wizardGenerationUnavailable(err error) error {
	if errors.Is(err, llm.ErrNotConfigured) {
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "AI model is not configured"}
	}
	return &briefs.ConnServiceUnavailableError{
		Code:    "503",
		Message: "the email content could not be generated from the AI platform",
	}
}

// wizardReferenceEmails collects style references for the reference variant.
//
// Returns AT MOST ONE reference, and structurally cannot return more: a plan carries a single
// `SourceEmail`, so there is exactly one email to read. Its excerpt is bounded by
// maxReferenceExcerpt. Every failure yields NO references rather than an error: a facts-only
// prompt writes a usable email, so a HubSpot outage degrades the voice-matching rather than
// the endpoint.
func (s *BriefService) wizardReferenceEmails(ctx context.Context, projectID string, sess *model.WizardSession) []wizardReferenceEmail {
	src := wizardPlanSource(sess)
	if src == nil {
		return nil
	}
	_, resolver, _ := s.wizardDeps()
	if resolver == nil {
		return nil
	}
	// REFUSED when the freshly-resolved client is the shared LF row, and this is a READ despite
	// what an earlier revision of this comment claimed: `GetEmail` and `GetEmailHTMLWidgets` are
	// both documented read-only.
	//
	// `src.ID` was captured in an EARLIER turn, and `findWizardSourceEmail` only records it when
	// that turn's connection was the project's own. But a connection can be revoked or rotated
	// between two wizard turns -- the reason this resolver is deliberately per-call rather than
	// cached -- so by now the same projectID can resolve to the shared portal. Reading a numeric
	// id captured against a DIFFERENT portal can land on another tenant's sent email, whose
	// subject and body would then feed this project's prompt as a voice reference.
	//
	// Degrades to generating without a reference, which is what a project with no clone source
	// gets anyway.
	client, fromSystem, err := resolver.ResolveHubSpotClient(ctx, projectID)
	if err != nil || client == nil {
		return nil
	}
	if fromSystem {
		slog.InfoContext(ctx, "wizard voice reference skipped: the clone source was found under a connection this project no longer resolves to",
			"project_id", projectID)
		return nil
	}
	email, gerr := client.GetEmail(ctx, src.ID)
	if gerr != nil {
		slog.WarnContext(ctx, "wizard could not read the clone source email; generating without a voice reference",
			"project_id", projectID, "email_id", src.ID, "error", safeErrSummary(gerr))
		return nil
	}
	ref := wizardReferenceEmail{Name: email.Name, Subject: email.Subject}
	if blocks, berr := client.GetEmailHTMLWidgets(ctx, src.ID); berr == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if strings.TrimSpace(blk.HTML) == "" {
				continue
			}
			b.WriteString(blk.HTML)
			b.WriteString("\n")
			if utf8.RuneCountInString(b.String()) >= maxReferenceExcerpt {
				break
			}
		}
		ref.Excerpt = truncateString(b.String(), maxReferenceExcerpt)
	}
	return []wizardReferenceEmail{ref}
}

// wizardStoredPlan decodes the session's recorded plan, or the zero plan.
//
// A corrupt blob reads as ABSENT rather than failing the turn: the plan is a record of a
// completed step, and a session that cannot replay it can still generate, clone and send —
// each of those turns checks for the specific piece it needs.
func wizardStoredPlan(sess *model.WizardSession) wizardPlan {
	var plan wizardPlan
	if len(sess.PlanResult) == 0 {
		return plan
	}
	if err := json.Unmarshal(sess.PlanResult, &plan); err != nil {
		return wizardPlan{}
	}
	return plan
}

// wizardPlanSource reads the clone source recorded by the planning turn.
func wizardPlanSource(sess *model.WizardSession) *wizardSource {
	plan := wizardStoredPlan(sess)
	if plan.SourceEmail == nil || strings.TrimSpace(plan.SourceEmail.ID) == "" {
		return nil
	}
	return plan.SourceEmail
}

// wizardSponsorsOut maps the scraped sponsors onto the wire type, dropping any without both
// a name and a usable logo URL — the two fields the design marks required.
func wizardSponsorsOut(d wizardEventDetails) []*briefs.WizardSponsor {
	var out []*briefs.WizardSponsor
	for _, sp := range d.Sponsors {
		name := strings.TrimSpace(sp.Name)
		logo := httpURL(sp.Logo)
		if name == "" || logo == "" {
			continue
		}
		s := &briefs.WizardSponsor{Name: name, LogoURL: logo}
		if u := httpURL(sp.URL); u != "" {
			s.URL = &u
		}
		if t := strings.TrimSpace(sp.Tier); t != "" {
			s.Tier = &t
		}
		out = append(out, s)
	}
	return out
}

// -----------------------------------------------------------------------------
// update-sections
// -----------------------------------------------------------------------------

// UpdateWizardSections re-renders the email from the operator's edited blocks.
//
// NO model call. This is the endpoint an operator hits after every drag, edit and delete, so
// it must be deterministic and fast — and re-generating here would rewrite prose they had
// just accepted. It is also the reason the editing half of the wizard keeps working when the
// AI proxy is unconfigured.
func (s *BriefService) UpdateWizardSections(ctx context.Context, p *briefs.UpdateWizardSectionsPayload) (*briefs.WizardSections, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	sess, _, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	if len(p.Sections) == 0 {
		// An empty list is refused rather than stored: it would render an empty email, and the
		// most likely cause is a client bug, not an operator who wants a blank send.
		return nil, &briefs.BadRequestError{Code: "400", Message: "sections must contain at least one content block"}
	}
	if len(p.Sections) > maxWizardSections {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: fmt.Sprintf("an email may carry at most %d content blocks", maxWizardSections),
		}
	}
	decoded, dropped := decodeWizardSections(p.Sections)
	if len(decoded) == 0 {
		return nil, &briefs.BadRequestError{Code: "400", Message: "none of the submitted sections is a usable content block"}
	}
	if dropped > 0 {
		slog.InfoContext(ctx, "wizard dropped unusable content blocks while rendering",
			"session_id", sess.ID, "dropped", dropped, "kept", len(decoded))
	}
	body := renderWizardSections(decoded)
	if strings.TrimSpace(body) == "" {
		// The blocks decoded but rendered to nothing — every one of them carried a type this
		// renderer does not know, or content the renderer's own bounds rejected. Storing that
		// would leave the session holding sections whose preview and draft are both blank, so
		// it is refused for the same reason an empty list is.
		return nil, &briefs.BadRequestError{Code: "400", Message: "none of the submitted sections is a usable content block"}
	}
	subject, preview := wizardSubjectAndPreview(sess)
	full := wizardPreviewHTML(subject, preview, body)

	// The SUBMITTED list is persisted, not the decoded one: the UI round-trips block fields
	// this service does not model, and storing the re-marshalled form would quietly strip them
	// on the first preview. `sanitizeSectionHTML` copies every key and rewrites only `html`, so
	// those unmodelled fields survive.
	//
	// Sanitized BEFORE the write, matching GenerateWizardContent. Not exploitable today -- every
	// current reader re-sanitizes -- but that makes the stored invariant "sections are sanitized"
	// depend on all future readers remembering to. A new export, admin tool or raw dump reading
	// `sess.Sections` directly would reintroduce the third-sink bug this PR closed for the
	// generation path, for the edit path instead.
	sanitized := make([]any, 0, len(p.Sections))
	for _, sec := range p.Sections {
		clean, keep := sanitizeSectionHTML(sec)
		if !keep {
			continue
		}
		sanitized = append(sanitized, clean)
	}
	sess.Sections = marshalAny(sanitized)
	saved, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "update-wizard-sections"))
	if uerr != nil {
		return nil, uerr
	}
	return &briefs.WizardSections{SessionID: saved.ID, BodyHTML: body, GeneratedHTML: full}, nil
}

// wizardSubjectAndPreview reads the subject and preview text the session last generated.
func wizardSubjectAndPreview(sess *model.WizardSession) (subject, preview string) {
	if v := wizardVariantOf(sess, "reference"); v != nil {
		return v.Subject, v.PreviewText
	}
	if v := wizardVariantOf(sess, "stage"); v != nil {
		return v.Subject, v.PreviewText
	}
	return "", ""
}

// wizardVariantOf decodes one stored variant. Returns nil when it is absent or failed —
// a failed variant has no content to clone or preview.
func wizardVariantOf(sess *model.WizardSession, which string) *wizardVariant {
	var blob json.RawMessage
	switch which {
	case "stage", "variant_a":
		blob = sess.StageVariant
	default:
		blob = sess.ReferenceVariant
	}
	if len(blob) == 0 {
		return nil
	}
	var v wizardVariant
	if err := json.Unmarshal(blob, &v); err != nil {
		return nil
	}
	if v.Mode == "failed" {
		return nil
	}
	return &v
}

// -----------------------------------------------------------------------------
// clone
// -----------------------------------------------------------------------------

// CloneWizardEmail clones the planned source email in HubSpot and writes the generated
// content into the new draft.
//
// This is the first turn with an effect OUTSIDE this service, which is why `approved` must
// be true: every earlier turn is reversible by abandoning the session, and this one leaves a
// draft in the operator's portal that only a human can remove.
func (s *BriefService) CloneWizardEmail(ctx context.Context, p *briefs.CloneWizardEmailPayload) (*briefs.WizardClone, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	if !p.Approved {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "approved must be true to create a HubSpot draft",
		}
	}
	sess, brief, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	variant := wizardVariantOf(sess, strVal(p.Variant))
	if variant == nil {
		// 409: the request is well-formed and the state is what is missing. Generate content
		// for this session (or pick the other variant) and retry.
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "this session has no generated content to clone; generate content first",
		}
	}
	src := wizardPlanSource(sess)
	if src == nil {
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "this session has no past email to clone from; re-run the plan once a source email exists in HubSpot",
		}
	}
	// REFUSED when the connection changed since planning. `src.ID` was captured in an earlier
	// turn against whatever portal resolved THEN, and `CloneEmail` reads that id's content from
	// whatever portal resolves NOW -- so after a revoke or rotation this would clone another
	// tenant's sent email into a draft attributed to this project, persisted on the session and
	// handed back as `DraftURL`. Worse than the read in wizardReferenceEmails, which only feeds a
	// prompt: this one materialises the leak as a durable, human-reviewable artifact.
	//
	// A hard 409 rather than a silent skip: the operator asked for a specific clone, and the
	// honest answer is that the source is no longer reachable through this project's connection.
	client, fromSystem, cerr := s.wizardHubSpotClient(ctx, p.ProjectID)
	if cerr == nil && fromSystem {
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "the source email is no longer reachable through this project's HubSpot connection; re-run planning",
		}
	}
	if cerr != nil {
		return nil, cerr
	}

	details := decodeWizardEventDetails(brief.EventDetails)
	tpl := emailstage.Resolve(brief.Stage)
	cloneName := wizardDraftName(details, tpl, s.now())
	email, clerr := client.CloneEmail(ctx, src.ID, cloneName)
	if clerr != nil {
		slog.WarnContext(ctx, "wizard could not clone the source email",
			"project_id", p.ProjectID, "source_email_id", src.ID, "error", safeErrSummary(clerr))
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the HubSpot draft could not be created; no draft was made",
		}
	}

	subject := firstNonEmpty(strings.TrimSpace(strVal(p.Subject)), variant.Subject)
	body := s.wizardBodyForClone(ctx, sess, variant)

	// Tag the links BEFORE writing, so a tagging failure never leaves a draft whose links are
	// half-tagged: an error here keeps the untagged body, which is a complete email.
	res := utm.Resolve("", email.Name)
	if tagged, terr := utm.TagHTMLLinks(body, res.Params, ""); terr == nil {
		body = tagged
	} else {
		slog.WarnContext(ctx, "wizard could not tag the draft's links with utm parameters; writing the body untagged",
			"email_id", email.ID, "error", safeErrSummary(terr))
	}
	// Shared with the dispatcher rather than reimplemented: the rules about which widget may
	// receive the body (see hubspot.ApplyEmailContent) are subtle and must not diverge.
	hubspot.ApplyEmailContent(ctx, client, email.ID, subject, body)

	issues := wizardValidateDraft(ctx, client, email.ID, subject)

	sess.EmailID = email.ID
	sess.DraftURL = email.AppURL
	sess.Phase = model.WizardPhaseCloned
	saved, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "clone-wizard-email"))
	if uerr != nil {
		// The DRAFT EXISTS. Reporting a bare failure here would have an operator retry and
		// create a second draft, so the error names what happened upstream.
		slog.ErrorContext(ctx, "wizard created a hubspot draft but could not record it on the session",
			"project_id", p.ProjectID, "session_id", sess.ID, "email_id", email.ID)
		return nil, &briefs.ConflictError{
			Code: "409",
			Message: fmt.Sprintf("the HubSpot draft %s was created but this session could not be updated; "+
				"re-read the session before retrying so a second draft is not created", email.ID),
		}
	}

	message := fmt.Sprintf("Created the HubSpot draft %q. Review it in HubSpot before sending.", email.Name)
	out := &briefs.WizardClone{
		SessionID:        saved.ID,
		Message:          message,
		Phase:            string(saved.PhaseOrDefault()),
		EmailID:          &email.ID,
		ValidationPassed: len(issues) == 0,
		ValidationIssues: issues,
	}
	if email.AppURL != "" {
		out.DraftURL = &email.AppURL
	}
	// The draft carries whichever variant was cloned, and the design exposes the two variant
	// slots separately so the UI can link each one. Only the cloned variant has a draft.
	if strVal(p.Variant) == "stage" || strVal(p.Variant) == "variant_a" {
		out.VariantAEmailID, out.VariantADraftURL = &email.ID, out.DraftURL
	} else {
		out.VariantBEmailID, out.VariantBDraftURL = &email.ID, out.DraftURL
	}

	if listID := strings.TrimSpace(strVal(p.SendListID)); listID != "" {
		// Same request, so the operator is not left with a draft that has no recipients. A
		// failure here does NOT fail the clone: the draft exists and the list can be set again.
		if _, lerr := client.SetSendList(ctx, email.ID, listID, nil); lerr != nil {
			slog.WarnContext(ctx, "wizard created the draft but could not apply the requested send list",
				"email_id", email.ID, "send_list_id", listID, "error", safeErrSummary(lerr))
			out.ValidationPassed = false
			// An UNCONFIRMED outcome is not a failure, and "set it again" is the wrong advice
			// for it: HubSpot may have applied the list already, so a blind retry can act on a
			// draft that is not in the state the operator thinks it is. Same distinction
			// hubspot.IsUnconfirmed carries everywhere else in this service.
			issue := "the draft was created but the requested send list could not be applied; set it again"
			if hubspot.IsUnconfirmed(lerr) {
				issue = "the draft was created but the outcome of applying the requested send list is unknown; check the draft in HubSpot before setting it again"
			}
			out.ValidationIssues = append(out.ValidationIssues, issue)
		} else {
			out.Message += fmt.Sprintf(" Recipient list %s applied.", listID)
		}
	}
	return out, nil
}

// wizardBodyForClone picks the HTML to write into the draft.
//
// The operator's EDITED sections win over the variant's original body: update-sections is
// what they pressed last, and cloning the pre-edit body would silently discard their work.
func (s *BriefService) wizardBodyForClone(ctx context.Context, sess *model.WizardSession, variant *wizardVariant) string {
	if len(sess.Sections) == 0 {
		return variant.BodyHTML
	}
	var raw []any
	if err := json.Unmarshal(sess.Sections, &raw); err != nil || len(raw) == 0 {
		return variant.BodyHTML
	}
	decoded, _ := decodeWizardSections(raw)
	body := renderWizardSections(decoded)
	if strings.TrimSpace(body) == "" {
		slog.WarnContext(ctx, "wizard session's edited sections rendered empty; cloning the generated body instead",
			"session_id", sess.ID)
		return variant.BodyHTML
	}
	return body
}

// wizardValidateDraft re-reads the created draft and reports ADVISORY problems.
//
// Advisory, never fatal: the draft exists by the time this runs, so a failed check must
// produce a note for the operator rather than an error that suggests nothing was created.
func wizardValidateDraft(ctx context.Context, client HubSpotWizardClient, emailID, expectedSubject string) []string {
	var issues []string
	email, err := client.GetEmail(ctx, emailID)
	if err != nil {
		return []string{"the draft was created but could not be re-read to verify it; check it in HubSpot"}
	}
	if strings.TrimSpace(expectedSubject) != "" && strings.TrimSpace(email.Subject) != strings.TrimSpace(expectedSubject) {
		issues = append(issues, "the draft's subject line does not match the generated one; set it in HubSpot")
	}
	blocks, berr := client.GetEmailHTMLWidgets(ctx, emailID)
	switch {
	case berr != nil:
		issues = append(issues, "the draft's content blocks could not be read back; check the email body in HubSpot")
	case len(blocks) == 0:
		issues = append(issues, "the draft has no editable content block, so the generated body was not written; paste it in HubSpot")
	case !blocks[0].Placed:
		// Not a defect — a classic template has no placed blocks — but the operator needs to
		// know the body may not have landed where the email reads from.
		issues = append(issues, "the draft uses a classic template, so the generated body may not be in the first block; check its order in HubSpot")
	}
	_ = ctx
	return issues
}

// -----------------------------------------------------------------------------
// set-send-list
// -----------------------------------------------------------------------------

// SetWizardSendList points the draft at its recipients.
//
// Two ways in, and the difference matters. Explicit list ids are the operator's own choice
// and are passed straight through. With none supplied, the recipients come from the brief's
// BUILT audience — this repo's existing CampaignAudience row — so the wizard sends to the
// same audience the rest of the service built and recorded, rather than to a second,
// parallel notion of who the recipients are.
func (s *BriefService) SetWizardSendList(ctx context.Context, p *briefs.SetWizardSendListPayload) (*briefs.WizardSendList, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	sess, _, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	// The SESSION's draft, never the caller's. `firstNonEmpty(p.EmailID, sess.EmailID)` let a
	// supplied id WIN over the recorded one with no ownership check — and because HubSpot
	// credentials resolve through the shared LF portal, a campaign manager authorised for this
	// project could retarget the recipients of any draft in the portal, including another
	// foundation's. Authorization here is scoped to the brief, so the draft that brief created
	// is the only one this endpoint may touch.
	//
	// A supplied id that MATCHES the session's is accepted, so a client that echoes back what
	// it was given still works; anything else is refused rather than silently ignored, because
	// quietly acting on a different draft than the caller named is its own defect.
	emailID := strings.TrimSpace(sess.EmailID)
	if supplied := strings.TrimSpace(strVal(p.EmailID)); supplied != "" && supplied != emailID {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "email_id does not match this session's draft; the send list can only be set on the draft this session created",
		}
	}
	if emailID == "" {
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "this session has no HubSpot draft yet; clone the email before setting its send list",
		}
	}
	// REFUSED on the shared row, for the same reason the clone turn refuses. An earlier revision
	// of this comment claimed the opposite -- that acting on "the draft THIS session created" was
	// safe -- and that was wrong: `sess.EmailID` is recorded by the CLONE turn, so it is a
	// cross-turn id exactly like `src.ID`. After a revoke or rotation the same numeric id names a
	// different portal's email, and this call MUTATES it: it would change the recipients of
	// another tenant's draft.
	//
	// Every wizard call that carries an id across a turn boundary now checks provenance. The ones
	// that do not -- plan-start, update-sections, chat -- touch only the session row.
	client, fromSystem, cerr := s.wizardHubSpotClient(ctx, p.ProjectID)
	if cerr != nil {
		return nil, cerr
	}
	if fromSystem {
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "the draft is no longer reachable through this project's HubSpot connection; re-run the wizard",
		}
	}

	primary, suppression, listType, rerr := s.resolveWizardSendList(ctx, client, p)
	if rerr != nil {
		return nil, rerr
	}
	if _, lerr := client.SetSendList(ctx, emailID, primary, suppression); lerr != nil {
		slog.WarnContext(ctx, "wizard could not apply the send list to the draft",
			"email_id", emailID, "send_list_id", primary, "error", safeErrSummary(lerr))
		// "the draft is unchanged" is a DEFINITE claim, and it is false for an unconfirmed
		// outcome: the write may have landed. Telling an operator the draft is untouched when
		// it may already carry the list is worse than saying nothing -- they act on it.
		if hubspot.IsUnconfirmed(lerr) {
			return nil, &briefs.ConnServiceUnavailableError{
				Code:    "503",
				Message: "the outcome of applying the recipient list is unknown; check the draft in HubSpot before retrying",
			}
		}
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the recipient list could not be applied to the draft; the draft is unchanged",
		}
	}

	// Complete means "the wizard has nothing left to do", NOT "sent": a human still presses
	// send in HubSpot. See model.WizardPhaseComplete.
	sess.EmailID = emailID
	sess.Phase = model.WizardPhaseComplete
	if _, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "set-wizard-send-list")); uerr != nil {
		// The LIST IS SET upstream. Say so, so the operator does not go looking for a draft
		// with no recipients.
		slog.ErrorContext(ctx, "wizard applied the send list but could not record it on the session",
			"session_id", sess.ID, "email_id", emailID)
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "the recipient list was applied to the draft but this session could not be updated; re-read the session",
		}
	}
	out := &briefs.WizardSendList{
		Success:    true,
		EmailID:    emailID,
		SendListID: primary,
		ListType:   &listType,
		To: map[string]any{
			"ils_list_id":          primary,
			"suppression_list_ids": suppression,
		},
	}
	return out, nil
}

// resolveWizardSendList decides which lists the draft sends to.
func (s *BriefService) resolveWizardSendList(ctx context.Context, client HubSpotWizardClient, p *briefs.SetWizardSendListPayload) (primary string, suppression []string, listType string, err error) {
	suppression = trimmedNonEmpty(p.SuppressionListIds)
	// send_list_ids takes priority over send_list_id, per the design's own wording.
	if ids := trimmedNonEmpty(p.SendListIds); len(ids) > 0 {
		if len(ids) > 1 {
			// HubSpot takes ONE ILS list per send. Refusing is the honest answer: silently
			// using the first would send to a subset of the audience the operator named.
			return "", nil, "", &briefs.BadRequestError{
				Code:    "400",
				Message: "a HubSpot send takes a single recipient list; supply one send list id",
			}
		}
		return ids[0], suppression, "explicit", nil
	}
	if id := strings.TrimSpace(strVal(p.SendListID)); id != "" {
		return id, suppression, "explicit", nil
	}

	// No explicit list: fall back to the brief's built audience.
	_, _, audiences := s.wizardDeps()
	if audiences == nil {
		return "", nil, "", &briefs.BadRequestError{
			Code:    "400",
			Message: "send_list_id is required; this deployment cannot resolve the brief's audience",
		}
	}
	listID, audSuppression, portal, aerr := resolveWizardAudience(ctx, audiences, p.ProjectID, p.BriefID)
	if aerr != nil {
		return "", nil, "", aerr
	}
	// The audience's list ids are meaningless outside the portal they were built in, and
	// dispatch can resolve a DIFFERENT credential than the build used. Proven, not assumed:
	// see assertAudiencePortal in internal/dispatch for the same guard on the send path.
	if err := assertWizardPortal(ctx, client, portal); err != nil {
		return "", nil, "", err
	}
	if len(suppression) == 0 {
		suppression = audSuppression
	}
	return listID, suppression, "audience", nil
}

// resolveWizardAudience finds the brief's newest BUILT HubSpot audience.
func resolveWizardAudience(ctx context.Context, repo domain.AudienceRepository, projectID, briefID string) (listID string, suppression []string, portalID string, err error) {
	auds, lerr := repo.ListAudiences(ctx, projectID, briefID)
	if lerr != nil {
		return "", nil, "", mapWizardErr(lerr)
	}
	// Newest-first. The newest HubSpot audience decides; an older one must NOT be
	// substituted, because it describes a different set of recipients.
	for _, a := range auds {
		if a.Platform != model.ProviderHubSpot {
			continue
		}
		if a.Status != model.AudienceBuilt {
			return "", nil, "", &briefs.ConflictError{
				Code:    "409",
				Message: fmt.Sprintf("the brief's audience is %s, not built; build it or supply send_list_id", a.Status),
			}
		}
		id := strings.TrimSpace(a.PlatformMasterListID)
		if id == "" {
			return "", nil, "", &briefs.ConflictError{
				Code:    "409",
				Message: "the brief's built audience has no recipient list id; rebuild it or supply send_list_id",
			}
		}
		var ids []string
		if len(a.SuppressionListIDs) > 0 && string(a.SuppressionListIDs) != "null" {
			if uerr := json.Unmarshal(a.SuppressionListIDs, &ids); uerr != nil {
				return "", nil, "", &briefs.ConflictError{
					Code:    "409",
					Message: "the brief's audience has unreadable suppression list ids; rebuild it or supply send_list_id",
				}
			}
		}
		return id, trimmedNonEmpty(ids), strings.TrimSpace(a.BuiltInPortalID), nil
	}
	return "", nil, "", &briefs.BadRequestError{
		Code:    "400",
		Message: "this brief has no built audience; build one or supply send_list_id",
	}
}

// assertWizardPortal refuses when the audience's portal cannot be proven to be the one this
// request authenticates against. FAILS CLOSED: an unreadable identity is an unknown, not a
// match, and guessing would hand a draft list ids from a portal it cannot see.
func assertWizardPortal(ctx context.Context, client HubSpotWizardClient, audiencePortal string) error {
	if strings.TrimSpace(audiencePortal) == "" {
		return &briefs.ConflictError{
			Code: "409",
			Message: "the brief's audience does not record which HubSpot portal its lists were built in, " +
				"so they cannot be used here; rebuild the audience or supply send_list_id",
		}
	}
	current, err := client.AuthenticatedPortalID(ctx)
	if err != nil {
		return &briefs.ConnServiceUnavailableError{
			Code: "503",
			Message: "could not confirm which HubSpot portal this request authenticates against, " +
				"so the audience's recipient list cannot be proven to exist there; retry shortly",
		}
	}
	if strings.TrimSpace(current) != strings.TrimSpace(audiencePortal) {
		return &briefs.ConflictError{
			Code: "409",
			Message: fmt.Sprintf("the brief's audience was built in HubSpot portal %s but this request authenticates "+
				"against portal %s, so its recipient list does not exist there; rebuild the audience", audiencePortal, current),
		}
	}
	return nil
}

func trimmedNonEmpty(in []string) []string {
	var out []string
	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// chat
// -----------------------------------------------------------------------------

// maxWizardStoredTurns bounds the PERSISTED conversation. Older turns are dropped from the
// row, oldest first, so a long-running session cannot grow a jsonb column without limit.
const maxWizardStoredTurns = 60

// ChatWizardTurn answers one conversational turn about the email being built.
//
// The turn ANSWERS; it never acts. There is no tool-calling loop here, so the model is told
// which wizard step applies a change rather than being allowed to imply it made one — see
// composeWizardChatPrompt.
func (s *BriefService) ChatWizardTurn(ctx context.Context, p *briefs.ChatWizardTurnPayload) (*briefs.WizardChat, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	message := strings.TrimSpace(p.Message)
	if message == "" {
		return nil, &briefs.BadRequestError{Code: "400", Message: "message is required"}
	}
	if utf8.RuneCountInString(message) > maxWizardChatMessage {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: fmt.Sprintf("message is too long; keep it under %d characters", maxWizardChatMessage),
		}
	}
	llmClient := s.snapshotLLMClient()
	if llmClient == nil {
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "AI model is not configured; the wizard chat is unavailable",
		}
	}
	sess, brief, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}

	turns, terr := sess.Turns()
	if terr != nil {
		// Corruption, not an empty history: answering against silently-dropped context would
		// produce a plausible answer to the wrong question. See model.WizardSession.Turns.
		slog.ErrorContext(ctx, "wizard session's chat history is unreadable",
			"session_id", sess.ID, "error", safeErrSummary(terr))
		return nil, &briefs.ConflictError{
			Code:    "409",
			Message: "this session's conversation history is unreadable; start a new wizard session",
		}
	}
	plan := wizardStoredPlan(sess)
	facts := wizardFacts(brief, decodeWizardEventDetails(brief.EventDetails), plan.ExtraContext, plan.EmailType, "")
	// The draft the operator is looking at. Chat previously received only event facts and the
	// transcript, so a request about the email itself had nothing to act on.
	subject, preview := wizardSubjectAndPreview(sess)
	draft := wizardChatDraft{Subject: subject, PreviewText: preview}
	systemPrompt, userPrompt := composeWizardChatPrompt(facts, draft, wizardHistoryText(turns), message)
	reply, cerr := llmClient.Complete(ctx, systemPrompt, userPrompt)
	if cerr != nil {
		slog.WarnContext(ctx, "wizard chat turn failed on the AI platform",
			"session_id", sess.ID, "error", safeErrSummary(cerr))
		return nil, wizardGenerationUnavailable(cerr)
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the AI platform returned an empty reply",
		}
	}

	now := s.now()
	turns = append(turns,
		model.WizardChatTurn{Role: "user", Content: message, At: now},
		// The reply is bounded before storage, like the user's message above. The system
		// prompt asks for under 200 words, but that is an instruction to a model, not a
		// guarantee: the client accepts up to maxResponseBody (8 MiB), and 60 stored turns of
		// that is a session row measured in hundreds of megabytes. maxWizardStoredTurns bounds
		// the COUNT; nothing bounded the size until here.
		model.WizardChatTurn{Role: "assistant", Content: truncateString(reply, maxWizardChatMessage), At: now},
	)
	if len(turns) > maxWizardStoredTurns {
		turns = turns[len(turns)-maxWizardStoredTurns:]
	}
	if herr := sess.SetTurns(turns); herr != nil {
		// The REPLY still goes back: it was produced, and losing it because the transcript
		// could not be encoded would be a worse outcome than a gap in the history.
		slog.WarnContext(ctx, "could not record the wizard chat turn", "session_id", sess.ID, "error", safeErrSummary(herr))
	}
	saved, uerr := saveWizardSession(ctx, sessions, sess, attributedActor(ctx, "chat-wizard-turn"))
	if uerr != nil {
		return nil, uerr
	}
	out := &briefs.WizardChat{
		SessionID: saved.ID,
		Message:   reply,
		Phase:     string(saved.PhaseOrDefault()),
	}
	if saved.DraftURL != "" {
		out.DraftURL = &saved.DraftURL
	}
	return out, nil
}

// wizardHistoryText reduces the persisted turns to what a prompt carries, newest kept.
func wizardHistoryText(turns []model.WizardChatTurn) []wizardTurnText {
	if len(turns) > maxWizardChatHistory {
		turns = turns[len(turns)-maxWizardChatHistory:]
	}
	out := make([]wizardTurnText, 0, len(turns))
	for _, t := range turns {
		role := strings.TrimSpace(t.Role)
		if role == "" {
			role = "user"
		}
		out = append(out, wizardTurnText{Role: role, Content: truncateString(t.Content, maxWizardChatMessage)})
	}
	return out
}

// -----------------------------------------------------------------------------
// session
// -----------------------------------------------------------------------------

// GetWizardSession returns where a session has got to.
//
// The endpoint a reconnecting client calls: the SSE stream carries live frames only, so a
// browser that reloaded mid-run recovers its state here rather than re-running a turn.
func (s *BriefService) GetWizardSession(ctx context.Context, p *briefs.GetWizardSessionPayload) (*briefs.WizardSession, error) {
	briefRepo, sessions, err := s.wizardReady()
	if err != nil {
		return nil, err
	}
	sess, _, serr := loadWizardSession(ctx, briefRepo, sessions, p.ProjectID, p.BriefID, p.SessionID)
	if serr != nil {
		return nil, serr
	}
	out := &briefs.WizardSession{
		SessionID: sess.ID,
		Phase:     string(sess.PhaseOrDefault()),
	}
	if len(sess.PlanResult) > 0 {
		var plan any
		if uerr := json.Unmarshal(sess.PlanResult, &plan); uerr == nil {
			out.Plan = plan
		} else {
			// Returned as absent rather than as an error: the phase and draft links are still
			// worth serving, and a corrupt plan blob does not invalidate them.
			slog.WarnContext(ctx, "wizard session's stored plan is unreadable",
				"session_id", sess.ID, "error", safeErrSummary(uerr))
		}
	}
	if sess.EmailID != "" {
		out.EmailID = &sess.EmailID
	}
	if sess.DraftURL != "" {
		out.DraftURL = &sess.DraftURL
	}
	return out, nil
}
