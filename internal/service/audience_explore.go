// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"

	"goa.design/goa/v3/security"
)

// ---------------------------------------------------------------------------
// Audience builder handlers (LFXV2-2770)
//
// The nine exploration endpoints. This layer does three things and nothing else:
// it refuses unauthenticated callers, it maps the orchestration's neutral results
// onto the generated types, and it turns failures into the statuses the contract
// declares. Every decision about the audience itself lives in internal/audience;
// every HubSpot call in internal/dispatch.
//
// The split matters most for error mapping, which is where a handler can do real
// damage: a 404 on a list an operator typed is an answer they act on, while the same
// failure reported as a 500 sends them to look for an outage that does not exist.
// ---------------------------------------------------------------------------

// AudienceExplorer is the orchestration this service delegates to. An interface here
// rather than the concrete dispatch type so the handlers are testable without a live
// HubSpot portal, and so a deployment with no connection store degrades to the
// contract's typed 503 instead of a nil dereference.
type AudienceExplorer interface {
	// Capabilities reports whether this project can do audience work. It returns no
	// error on purpose — an unusable connection is this endpoint's ANSWER, and it is
	// what the UI renders its degraded state from.
	Capabilities(ctx context.Context, projectID string) audience.ExploreCapabilities
	Discover(ctx context.Context, projectID, eventURL string) (*audience.DiscoveryOutcome, error)
	SearchLists(ctx context.Context, projectID, query string) ([]audience.ListRow, error)
	SuppressionLists(ctx context.Context, projectID, brandShort, eventName string) ([]audience.SuppressionRow, error)
	LastSent(ctx context.Context, projectID, eventName, brandShort string, limit int) ([]audience.LastSentEmail, error)
	ExistingMasterLists(ctx context.Context, projectID, eventName, brandShort string) ([]audience.ListRow, error)
	PreviewCount(ctx context.Context, projectID string, listIDs []string) (audience.PreviewCount, error)
	// ComposeMaster CREATES lists in the project's portal and is not idempotent. A
	// failure after the suppression list was created returns a
	// *audience.ComposePartialError, which this layer must surface rather than
	// flatten — see composeErr.
	ComposeMaster(ctx context.Context, projectID string, in audience.ComposeInput) (*audience.ComposeOutcome, error)
	// AttachExisting reads existing lists back from the portal and returns them as an
	// outcome to record. It creates nothing in HubSpot. includeIDs are the lists the send
	// goes to; the first is reported as the outcome's Master, and every one is verified.
	AttachExisting(ctx context.Context, projectID string, includeIDs, suppressionIDs []string) (*audience.ComposeOutcome, error)
	RunQA(ctx context.Context, projectID, listRef, eventName string, targetsEU, targetsCA bool) (*audience.QaOutcome, error)
}

// How hard a recording compose tries to persist the audience row once the HubSpot lists
// already exist. Two attempts, briefly spaced: enough to ride out a connection blip, short
// enough that the caller gets a partial naming the real lists rather than a timeout naming
// nothing.
const (
	composeRecordAttempts   = 2
	composeRecordRetryDelay = 150 * time.Millisecond
)

// AudienceExploreService implements the generated audience-builder service.
type AudienceExploreService struct {
	authGuard

	mu       sync.RWMutex
	explorer AudienceExplorer
	// audiences and briefs are needed only by the RECORDING half of
	// ComposeAudienceMaster (a compose carrying a brief_id). They are late-bound like
	// explorer, and that path returns a typed 503 when either is absent — so a
	// deployment without a database still serves every exploratory endpoint.
	audiences domain.AudienceRepository
	briefs    domain.BriefRepository
}

var (
	_ explore.Service = (*AudienceExploreService)(nil)
	_ explore.Auther  = (*AudienceExploreService)(nil)
)

// NewAudienceExploreService constructs the service. A nil explorer mounts the routes
// in the typed-503 mode, matching the other services: the routes stay mounted and
// answer the contract's 503 rather than disappearing, so a client can tell "not
// configured here" from "wrong URL".
func NewAudienceExploreService(explorer AudienceExplorer) *AudienceExploreService {
	return &AudienceExploreService{explorer: explorer}
}

// SetExplorer late-binds the orchestration after a cold-start retry, mirroring the
// other services' late binding.
func (s *AudienceExploreService) SetExplorer(explorer AudienceExplorer) {
	if explorer == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.explorer = explorer
}

// SetAudienceRepo late-binds the audience repository a recording compose writes to.
// Separate from SetExplorer so the existing single-arg call sites are unaffected,
// mirroring AudienceService.SetBriefRepo.
func (s *AudienceExploreService) SetAudienceRepo(repo domain.AudienceRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audiences = repo
}

// SetBriefRepo late-binds the brief repository a recording compose checks against
// before it creates anything.
func (s *AudienceExploreService) SetBriefRepo(b domain.BriefRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.briefs = b
}

// recordDeps snapshots the two repositories a recording compose needs, under the same
// RWMutex the handlers take for explorer, so a cold-start injection cannot race a
// request that is already deciding whether it can record.
func (s *AudienceExploreService) recordDeps() (domain.AudienceRepository, domain.BriefRepository) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.audiences, s.briefs
}

// ExplorerIsSet reports whether the orchestration was injected. Exported only so the
// container's wiring tests can assert injection directly — every handler returns the
// same typed 503 when it is absent, so an error-based assertion cannot tell a wired
// service from an unwired one.
func (s *AudienceExploreService) ExplorerIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.explorer != nil
}

// ready snapshots the explorer, or returns the typed 503.
func (s *AudienceExploreService) ready() (AudienceExplorer, error) {
	s.mu.RLock()
	explorer := s.explorer
	s.mu.RUnlock()
	if explorer == nil {
		return nil, &explore.ConnServiceUnavailableError{
			Code:    "503",
			Message: "audience building is not configured (requires the HubSpot connection store)",
		}
	}
	return explorer, nil
}

// JWTAuth verifies the bearer token, mirroring the other services: 503 when the check
// could not be PERFORMED, 401 when the credential itself is bad.
//
// The distinction is not cosmetic. A missing verifier or an unreachable JWKS
// establishes nothing about the caller's token, and answering 401 there sends them to
// refresh a credential that was never the problem, for an outage that clears itself.
func (s *AudienceExploreService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	ctx, msg, unavailable := s.authenticate(ctx, token)
	switch {
	case unavailable:
		return ctx, &explore.ConnServiceUnavailableError{Code: "503", Message: msg}
	case msg != "":
		return ctx, &explore.UnauthorizedError{Code: "401", Message: msg, WwwAuthenticate: bearerChallenge}
	}
	return ctx, nil
}

// ─── Handlers ───

// GetAudienceBuilderCapabilities reports whether this project's portal is usable.
//
// The only handler that cannot fail for a connection reason, because an unusable
// connection is exactly what it reports. Failing it would leave the UI unable to
// render even the explanation for why everything else is disabled.
func (s *AudienceExploreService) GetAudienceBuilderCapabilities(ctx context.Context, p *explore.GetAudienceBuilderCapabilitiesPayload) (*explore.AudienceBuilderCapabilities, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	caps := explorer.Capabilities(ctx, p.ProjectID)
	out := &explore.AudienceBuilderCapabilities{HubspotConfigured: caps.HubSpotConfigured}
	if detail := strings.TrimSpace(caps.Detail); detail != "" {
		out.Detail = &detail
	}
	return out, nil
}

func (s *AudienceExploreService) DiscoverAudienceLists(ctx context.Context, p *explore.DiscoverAudienceListsPayload) (*explore.AudienceDiscoveryResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	outcome, derr := explorer.Discover(ctx, p.ProjectID, p.EventURL)
	if derr != nil {
		return nil, audienceExploreErr(ctx, "discover audience lists", p.ProjectID, derr)
	}
	inspected := int64(outcome.Inspected)
	res := &explore.AudienceDiscoveryResult{
		Event:          eventIdentityResult(outcome.Event),
		Lists:          make([]*explore.AudienceDiscoveredList, 0, len(outcome.Lists)),
		MissingSignals: signalStrings(outcome.MissingSignals),
		Inspected:      &inspected,
	}
	for _, l := range outcome.Lists {
		row := &explore.AudienceDiscoveredList{
			ListID:     l.ListID,
			Name:       l.Name,
			Signal:     string(l.Signal),
			Size:       l.Size,
			Reason:     l.Reason,
			ListType:   l.ListType,
			HubspotURL: l.HubSpotURL,
		}
		// Scope is emitted only where it distinguishes anything. "current", "past" and
		// "current + past" are genuinely different audiences for a speakers list; on
		// any other signal the field would be a value the classifier never decided.
		if l.Scope != "" {
			scope := string(l.Scope)
			row.Scope = &scope
		}
		res.Lists = append(res.Lists, row)
	}
	return res, nil
}

func (s *AudienceExploreService) SearchAudienceLists(ctx context.Context, p *explore.SearchAudienceListsPayload) (*explore.SearchAudienceListsResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	rows, serr := explorer.SearchLists(ctx, p.ProjectID, p.Q)
	if serr != nil {
		return nil, audienceExploreErr(ctx, "search audience lists", p.ProjectID, serr)
	}
	res := &explore.SearchAudienceListsResult{Lists: make([]*explore.AudienceListSearchResult, 0, len(rows))}
	for _, r := range rows {
		res.Lists = append(res.Lists, &explore.AudienceListSearchResult{
			ListID: r.ListID, Name: r.Name, Size: r.Size, HubspotURL: r.HubSpotURL,
		})
	}
	return res, nil
}

func (s *AudienceExploreService) GetAudienceSuppressionLists(ctx context.Context, p *explore.GetAudienceSuppressionListsPayload) (*explore.GetAudienceSuppressionListsResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	rows, serr := explorer.SuppressionLists(ctx, p.ProjectID, derefStr(p.BrandShort), derefStr(p.EventName))
	if serr != nil {
		return nil, audienceExploreErr(ctx, "resolve suppression lists", p.ProjectID, serr)
	}
	res := &explore.GetAudienceSuppressionListsResult{Lists: make([]*explore.AudienceSuppressionList, 0, len(rows))}
	for _, r := range rows {
		res.Lists = append(res.Lists, &explore.AudienceSuppressionList{
			Key:        r.Key,
			Label:      r.Label,
			ListID:     r.ListID,
			Name:       r.Name,
			Size:       r.Size,
			Category:   r.Category,
			HubspotURL: r.HubSpotURL,
		})
	}
	return res, nil
}

func (s *AudienceExploreService) GetAudienceLastSent(ctx context.Context, p *explore.GetAudienceLastSentPayload) (*explore.GetAudienceLastSentResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	emails, lerr := explorer.LastSent(ctx, p.ProjectID, p.EventName, derefStr(p.BrandShort), p.Limit)
	if lerr != nil {
		return nil, audienceExploreErr(ctx, "read last-sent emails", p.ProjectID, lerr)
	}
	res := &explore.GetAudienceLastSentResult{Emails: make([]*explore.AudienceLastSentEmail, 0, len(emails))}
	for _, e := range emails {
		row := &explore.AudienceLastSentEmail{
			EmailID:          e.EmailID,
			EmailName:        e.EmailName,
			HubspotURL:       e.HubSpotURL,
			IncludedLists:    listBriefResults(e.IncludedLists),
			SuppressionLists: listBriefResults(e.SuppressionLists),
		}
		// Sent only when TRUE, matching `sent_at`'s treatment below: an absent flag reads as
		// "the lists are what they say", which is the common case and the safe default.
		if e.ListsUnavailable {
			unavailable := true
			row.ListsUnavailable = &unavailable
		}
		// Left absent rather than sent as "": a published-at the portal did not report
		// is not a send that happened at the zero time.
		if sentAt := strings.TrimSpace(e.SentAt); sentAt != "" {
			row.SentAt = &sentAt
		}
		res.Emails = append(res.Emails, row)
	}
	return res, nil
}

func (s *AudienceExploreService) GetExistingAudienceMasterLists(ctx context.Context, p *explore.GetExistingAudienceMasterListsPayload) (*explore.GetExistingAudienceMasterListsResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	rows, merr := explorer.ExistingMasterLists(ctx, p.ProjectID, p.EventName, derefStr(p.BrandShort))
	if merr != nil {
		return nil, audienceExploreErr(ctx, "read existing master lists", p.ProjectID, merr)
	}
	res := &explore.GetExistingAudienceMasterListsResult{Lists: make([]*explore.AudienceMasterListBrief, 0, len(rows))}
	for _, r := range rows {
		res.Lists = append(res.Lists, &explore.AudienceMasterListBrief{
			ListID: r.ListID, Name: r.Name, Size: r.Size, HubspotURL: r.HubSpotURL,
		})
	}
	return res, nil
}

func (s *AudienceExploreService) PreviewAudienceCount(ctx context.Context, p *explore.PreviewAudienceCountPayload) (*explore.AudiencePreviewCount, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	count, cerr := explorer.PreviewCount(ctx, p.ProjectID, p.ListIds)
	if cerr != nil {
		return nil, audienceExploreErr(ctx, "preview audience count", p.ProjectID, cerr)
	}
	// Count and Estimate are BOTH carried, along with the reason. A response that
	// collapsed them into one number would make an over-count indistinguishable from
	// a real total — and `exact` is the field a caller must read before it renders a
	// figure an operator will treat as the size of the send.
	return &explore.AudiencePreviewCount{
		Exact:    count.Exact,
		Count:    int64(count.Count),
		Estimate: int64(count.Estimate),
		Reason:   count.Reason,
	}, nil
}

func (s *AudienceExploreService) ComposeAudienceMaster(ctx context.Context, p *explore.ComposeAudienceMasterPayload) (*explore.AudienceComposeMasterResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	if p.Compose == nil {
		return nil, &explore.BadRequestError{Code: "400", Message: "a compose body is required"}
	}
	briefID := strings.TrimSpace(derefStr(p.Compose.BriefID))

	// Everything that can refuse a RECORDING compose is checked here, before the
	// orchestration is entered and therefore before any HubSpot list exists. A compose
	// that is going to be refused must be refused while the refusal is still free: a
	// missing repository or an unknown brief discovered after two real list creates is
	// the worst outcome available, because the lists cannot be rolled back and the
	// caller must not retry to get an attachment it never received.
	var repo domain.AudienceRepository
	if briefID != "" {
		var briefs domain.BriefRepository
		repo, briefs = s.recordDeps()
		if repo == nil || briefs == nil {
			// 503 rather than composing-without-recording. Silently downgrading to an
			// unattached compose would hand back a master the operator believes is wired
			// to the send and is not — the same "quietly building something different"
			// failure ErrBlankExclusionID exists to prevent, with a worse blast radius.
			return nil, &explore.ConnServiceUnavailableError{
				Code:    "503",
				Message: "this deployment cannot record an audience, so a composed master cannot be attached to a brief",
			}
		}
		if _, berr := briefs.GetBrief(ctx, p.ProjectID, briefID); berr != nil {
			return nil, composeBriefErr(ctx, p.ProjectID, briefID, berr)
		}
	}

	in := audience.ComposeInput{
		ListIDs:            p.Compose.ListIds,
		ExcludeListIDs:     p.Compose.ExcludeListIds,
		Name:               derefStr(p.Compose.Name),
		BrandShort:         derefStr(p.Compose.BrandShort),
		EventName:          derefStr(p.Compose.EventName),
		EventDates:         p.Compose.EventDates,
		RecordUnderBriefID: briefID,
	}
	outcome, cerr := explorer.ComposeMaster(ctx, p.ProjectID, in)
	if cerr != nil {
		return nil, composeErr(ctx, p.ProjectID, cerr)
	}
	res := &explore.AudienceComposeMasterResult{
		Master:        composedListResult(&outcome.Master),
		SourceListIds: outcome.SourceListIDs,
	}
	if outcome.Suppression != nil {
		res.Suppression = composedListResult(outcome.Suppression)
	}
	if briefID == "" {
		return res, nil
	}

	recorded, rerr := s.recordComposedAudience(ctx, repo, p.ProjectID, briefID, outcome,
		strings.TrimSpace(derefStr(p.Compose.InclusionSummary)), nil)
	if rerr != nil {
		// The lists exist and are usable; only the attachment failed. That is a partial,
		// not a 500: a 500 invites the retry that would create a second master, and the
		// UI branches on the partial type to suppress exactly that affordance.
		return nil, composeErr(ctx, p.ProjectID, &audience.ComposePartialError{
			Master:      &outcome.Master,
			Suppression: suppressionOrZero(outcome.Suppression),
			Err:         rerr,
		})
	}
	res.Recorded = true
	res.Audience = &explore.AudienceComposeRecordedAudience{
		ID:                   recorded.ID,
		Status:               string(recorded.Status),
		Version:              recorded.Version,
		PlatformMasterListID: recorded.PlatformMasterListID,
	}
	return res, nil
}

// AttachExistingAudience records lists that already exist as the brief's built audience.
//
// Nothing is created in HubSpot, so unlike compose there is no partial state: every failure
// here, including the record itself, is safe to retry.
func (s *AudienceExploreService) AttachExistingAudience(ctx context.Context, p *explore.AttachExistingAudiencePayload) (*explore.AudienceAttachExistingResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	if p.Attach == nil {
		return nil, &explore.BadRequestError{Code: "400", Message: "an attach body is required"}
	}
	briefID := strings.TrimSpace(p.Attach.BriefID)
	repo, briefs := s.recordDeps()
	if repo == nil || briefs == nil {
		return nil, &explore.ConnServiceUnavailableError{
			Code:    "503",
			Message: "this deployment cannot record an audience, so existing lists cannot be attached to a brief",
		}
	}
	includeIDs, multi, verr := attachIncludeIDs(p.Attach)
	if verr != nil {
		return nil, verr
	}
	if _, berr := briefs.GetBrief(ctx, p.ProjectID, briefID); berr != nil {
		return nil, composeBriefErr(ctx, p.ProjectID, briefID, berr)
	}

	outcome, aerr := explorer.AttachExisting(ctx, p.ProjectID, includeIDs, p.Attach.SuppressionListIds)
	if aerr != nil {
		return nil, composeErr(ctx, p.ProjectID, aerr)
	}
	// Only the include_list_ids form records the include set. The single-master form keeps
	// writing the row it always has, so every existing reader and send path is untouched.
	var recordedIncludes []string
	if multi {
		recordedIncludes = includeIDs
	}
	recorded, rerr := s.recordComposedAudience(ctx, repo, p.ProjectID, briefID, outcome,
		strings.TrimSpace(derefStr(p.Attach.InclusionSummary)), recordedIncludes)
	if rerr != nil {
		slog.ErrorContext(ctx, "attach existing audience: record failed",
			"project_id", p.ProjectID, "brief_id", briefID, "error", rerr)
		return nil, &explore.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the lists were verified but the audience could not be saved; nothing was created in HubSpot, so it is safe to retry",
		}
	}
	recordedIDs := unmarshalStrings(recorded.IncludeListIDs)
	return &explore.AudienceAttachExistingResult{
		Master:             composedListResult(&outcome.Master),
		SuppressionListIds: unmarshalStrings(recorded.SuppressionListIDs),
		IncludeListIds:     recordedIDs,
		Audience: &explore.AudienceComposeRecordedAudience{
			ID:                   recorded.ID,
			Status:               string(recorded.Status),
			Version:              recorded.Version,
			PlatformMasterListID: recorded.PlatformMasterListID,
			IncludeListIds:       recordedIDs,
		},
	}, nil
}

// attachIncludeIDs resolves which lists an attach sends to, enforcing that the request names
// them exactly one way: master_list_id (one list, the long-standing form) or include_list_ids
// (several existing lists, sent to directly with no composed master). multi reports which form
// was used, because only the second records include_list_ids on the audience.
//
// The include ids are trimmed and de-duplicated in order. Unlike the single-master form, where
// a suppression id that is also the master is silently dropped (audience.ExclusionIDs), an
// overlap here is refused: HubSpot applies exclusions after inclusions, so an id on both sides
// would remove that whole list from the send, and with several includes the operator cannot
// tell from the response which group quietly went missing.
func attachIncludeIDs(in *explore.AudienceAttachExistingInput) (ids []string, multi bool, err error) {
	master := strings.TrimSpace(derefStr(in.MasterListID))
	hasInclude := len(in.IncludeListIds) > 0
	switch {
	case master != "" && hasInclude:
		return nil, false, &explore.BadRequestError{Code: "400", Message: "send either master_list_id or include_list_ids, not both"}
	case master == "" && !hasInclude:
		return nil, false, &explore.BadRequestError{Code: "400", Message: "master_list_id or a non-empty include_list_ids is required"}
	case master != "":
		return []string{master}, false, nil
	}
	for _, id := range in.IncludeListIds {
		if strings.TrimSpace(id) == "" {
			return nil, false, &explore.BadRequestError{Code: "400", Message: "an include_list_ids entry cannot be blank"}
		}
	}
	ids = audience.UniqueIDs(in.IncludeListIds)
	suppressed := make(map[string]struct{}, len(in.SuppressionListIds))
	for _, id := range in.SuppressionListIds {
		suppressed[strings.TrimSpace(id)] = struct{}{}
	}
	for _, id := range ids {
		if _, both := suppressed[id]; both {
			return nil, false, &explore.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("list %s is in both include_list_ids and suppression_list_ids; a list cannot be both sent to and suppressed", id),
			}
		}
	}
	return ids, true, nil
}

// composeBriefErr maps the pre-flight brief read of a RECORDING compose.
//
// It cannot go through audienceExploreErr, and the reason is the whole point of the
// function: there, domain.ErrNotFound means "this project has no usable HubSpot
// connection" — the arm says so outright — because every other caller of that helper is
// asking HubSpot about a list. Here ErrNotFound means the BRIEF does not exist, and
// reporting a mistyped brief id as a connection outage sends the operator to reconnect
// HubSpot over something no reconnection can fix.
//
// Any other read failure is a 503 rather than a 500: this runs before anything is
// created, so retrying is both safe and the correct advice.
func composeBriefErr(ctx context.Context, projectID, briefID string, err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		slog.WarnContext(ctx, "compose audience master: no such brief to attach to",
			"project_id", projectID, "brief_id", briefID)
		return &explore.NotFoundError{
			Code:    "404",
			Message: "no such brief in this project, so a composed master cannot be attached to it",
		}
	}
	slog.ErrorContext(ctx, "compose audience master: the brief could not be read",
		"project_id", projectID, "brief_id", briefID, "error", err)
	return &explore.ConnServiceUnavailableError{
		Code:    "503",
		Message: "the brief could not be read, so a composed master cannot be attached to it; please retry",
	}
}

// suppressionOrZero flattens the optional suppression for ComposePartialError, whose
// Suppression field is a value rather than a pointer.
func suppressionOrZero(s *audience.ComposedList) audience.ComposedList {
	if s == nil {
		return audience.ComposedList{}
	}
	return *s
}

// recordComposedAudience writes the composed master as the brief's built audience.
//
// One bounded retry, because the expensive and irreversible half (two HubSpot creates)
// has already succeeded and a transient database blip should not cost the operator a
// duplicate contact list to reconcile. It is bounded rather than persistent because the
// caller is holding an HTTP request open, and a partial that names the lists is a far
// better outcome than a timeout that names nothing.
func (s *AudienceExploreService) recordComposedAudience(
	ctx context.Context,
	repo domain.AudienceRepository,
	projectID, briefID string,
	outcome *audience.ComposeOutcome,
	inclusionSummary string,
	includeIDs []string,
) (*model.CampaignAudience, error) {
	a := audienceFromCompose(projectID, briefID, outcome, inclusionSummary)
	// includeIDs is set only by an attach of several existing lists; the outcome's Master is
	// already the first of them, so PlatformMasterListID keeps naming a real recipient list.
	if len(includeIDs) > 0 {
		a.IncludeListIDs = marshalStrings(includeIDs)
	}
	a.CreatedBy = marshalActor(actorFromCtx(ctx))
	if verr := a.Validate(); verr != nil {
		return nil, verr
	}

	var lastErr error
	for attempt := range composeRecordAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(composeRecordRetryDelay):
			}
		}
		created, cerr := repo.CreateAudience(ctx, a)
		if cerr == nil {
			return created, nil
		}
		lastErr = cerr
		// A DEFINITE refusal is not retried. The retry exists to ride out a connection blip, and
		// these two sentinels are the repository's settled answer: the brief was archived between
		// the compose and the record (ErrNotFound), or another build for the same brief and
		// platform already holds the slot (ErrAudienceBuildInFlight). Retrying buys a wasted
		// round trip and 150ms of the caller's deadline.
		//
		// It also mislabels the outcome. The partial this error becomes tells the operator the
		// lists exist and to attach them by hand rather than retry -- true of an archived brief,
		// and wrong for an in-flight build, which is the one case where a LATER retry by the
		// operator would succeed. Breaking out keeps the message attached to the right cause.
		if errors.Is(cerr, domain.ErrNotFound) || errors.Is(cerr, domain.ErrAudienceBuildInFlight) {
			break
		}
	}
	return nil, lastErr
}

func (s *AudienceExploreService) RunAudienceQa(ctx context.Context, p *explore.RunAudienceQaPayload) (*explore.AudienceQaResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	outcome, qerr := explorer.RunQA(ctx, p.ProjectID, p.ListRef, derefStr(p.EventName), derefBool(p.TargetsEu), derefBool(p.TargetsCa))
	if qerr != nil {
		return nil, audienceExploreErr(ctx, "run audience QA", p.ProjectID, qerr)
	}

	// The disambiguation branch returns ONLY the candidates. No id, no checks, no
	// verdict — because QA audited nothing, and a zero-valued verdict beside the
	// candidates would be readable as a PASS on a list it never looked at.
	if outcome.NeedsDisambiguation {
		res := &explore.AudienceQaResult{
			NeedsDisambiguation: true,
			Candidates:          make([]*explore.AudienceQaCandidate, 0, len(outcome.Candidates)),
		}
		for _, c := range outcome.Candidates {
			res.Candidates = append(res.Candidates, &explore.AudienceQaCandidate{
				ListID: c.ListID, Name: c.Name, Size: c.Size,
			})
		}
		return res, nil
	}

	listID, name, url, overall := outcome.ListID, outcome.Name, outcome.HubSpotURL, string(outcome.Overall)
	return &explore.AudienceQaResult{
		ListID:     &listID,
		Name:       &name,
		HubspotURL: &url,
		Checks: &explore.AudienceQaChecks{
			SignalMapping: qaCheckResult(outcome.Checks.SignalMapping),
			Suppression: &explore.AudienceQaSuppressionCheck{
				Verdict:       string(outcome.Checks.Suppression.Verdict),
				Findings:      findingResults(outcome.Checks.Suppression.Findings),
				AppliedGdpr:   outcome.Checks.Suppression.AppliedGDPR,
				AppliedOptOut: outcome.Checks.Suppression.AppliedOptOut,
			},
			ExclusionCompleteness: &explore.AudienceQaExclusionCheck{
				Verdict:        string(outcome.Checks.ExclusionCompleteness.Verdict),
				Findings:       findingResults(outcome.Checks.ExclusionCompleteness.Findings),
				ExclusionCount: int64(outcome.Checks.ExclusionCompleteness.ExclusionCount),
			},
			// OMITTED, not zero-valued, when check 4 did not run. An
			// `AudienceQaCheck{Verdict: ""}` on the wire is a check the client must then
			// decide how to render; absent says plainly that it was not performed.
			CurrentRegistrants: currentRegistrantsResult(outcome.Checks.CurrentRegistrants),
		},
		Findings: findingResults(outcome.Findings),
		Overall:  &overall,
	}, nil
}

// ─── Error mapping ───

// audienceExploreErr maps a read-side failure to the status the contract declares.
//
// The raw error is LOGGED and never returned: it can name the connection store, a
// decryption failure, or a HubSpot path, and these messages are rendered to an
// operator in a browser.
func audienceExploreErr(ctx context.Context, op, projectID string, err error) error {
	slog.WarnContext(ctx, "audience builder request failed", "op", op, "project_id", projectID, "error", err)
	switch {
	case errors.Is(err, audience.ErrListNotFound):
		return &explore.NotFoundError{Code: "404", Message: "no list in this project's HubSpot portal matches that reference"}
	case errors.Is(err, audience.ErrInvalidRequest), errors.Is(err, audience.ErrEventNameUnresolved):
		// 400, not 500: nothing is wrong with the service, and the fix is a different
		// request — a resolvable list reference, or an event URL whose page declares a
		// name. Reported as a server fault, an operator would wait for it to clear.
		//
		// eventurl.ErrEventURLInvalid/ErrEventURLForbidden are deliberately NOT listed
		// here even though they are also 400s: the arm below gives them a more specific,
		// URL-focused message, and listing the same sentinel in two arms would make this
		// one win by switch order and silently shadow the more useful message.
		return &explore.BadRequestError{Code: "400", Message: "the request could not be satisfied as given: check the event URL or list reference"}
	case errors.Is(err, eventurl.ErrEventURLInvalid), errors.Is(err, eventurl.ErrEventURLForbidden):
		// 400: the caller gave a URL this service will not fetch — malformed, or resolving
		// to an address SSRF protection refuses. Reported as a 500 these read as "the
		// service is broken", so an operator retries a URL that can never work.
		// `mapEventURLErr` classifies the same sentinels for /fetch-event-url; it returns
		// briefs.* types, so the arms are mirrored here rather than reused.
		return &explore.BadRequestError{Code: "400", Message: "event URL is invalid, or resolves to an address this service will not connect to"}
	case errors.Is(err, hubspot.ErrSearchIncomplete):
		// 503, not 500: the search reached its scan bound without matching, so the portal did
		// not answer the question — it did not answer "no". Retrying is the right advice and a
		// generic 500 gives the opposite, reading as an outage in this service rather than a
		// bounded read that can succeed. LastSent returns this rather than an empty list
		// precisely so the caller is never told a prior send does not exist on this evidence.
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "the prior-send history could not be read completely; please retry"}
	case errors.Is(err, eventurl.ErrEventURLFetchFailed):
		// 503, not 400: the URL is fine and the origin did not answer. Retrying may work,
		// which is the opposite of the advice a 400 gives.
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "the event page could not be fetched"}
	case errors.Is(err, audience.ErrEventPageUnavailable):
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "this deployment cannot read event pages, so discovery is unavailable"}
	case errors.Is(err, domain.ErrSystemConnectionMissing), errors.Is(err, domain.ErrSystemConnectionNotUsable):
		// Above the ErrNotFound/ErrConnectionNotUsable arms, which creds.go wraps
		// ALONGSIDE these: the project has no connection of its OWN and fell back to
		// the shared LF row, so "reconnect HubSpot for this project" names a repair
		// nobody on the project can perform.
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "the shared LF HubSpot connection is missing or unusable — an operator must repair it"}
	case errors.Is(err, domain.ErrCredentialDecryptionFailed):
		// NOT the 503 below. Retrying cannot help: this is either one corrupted row or
		// a rotated encryption key failing every project at once, and only an operator
		// can tell those apart or fix either.
		return &explore.InternalServerError{Code: "500", Message: "this project's stored HubSpot credentials could not be decrypted — an operator must investigate"}
	case hubspot.IsUnconfirmed(err):
		// An AMBIGUOUS upstream outcome -- a mutating 429/5xx/transport failure, or a
		// 2xx with no id. Both CreateList calls behind compose are non-idempotent, so a
		// list may ALREADY exist. Falling through to the generic 500 below gives text
		// that reads like an ordinary transient error and invites exactly the blind
		// retry that creates a duplicate in a production portal. `unconfirmedNote` in
		// audience_build.go exists because this same defect was fixed once already on
		// the build path; this is the explore path's equivalent.
		return &explore.InternalServerError{
			Code:    "500",
			Message: "HubSpot did not confirm whether this change was applied — check the portal before retrying, as a retry may create a duplicate",
		}
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrConnectionNotUsable):
		// 503 rather than 404: the LIST or email asked about may well exist. What is
		// unavailable is the connection needed to look, which is what /capabilities
		// reports and what the tab's degraded banner already explains.
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "this project has no usable HubSpot connection — connect HubSpot to use the audience builder"}
	default:
		return &explore.InternalServerError{Code: "500", Message: "the audience builder could not complete this request against HubSpot"}
	}
}

// composeErr maps a compose failure, preserving the one case that must not be
// flattened: a suppression list that WAS created while the master was not.
//
// That partial state is returned as the contract's own error type carrying the
// orphaned list, because compose is not idempotent. A caller told only "it failed"
// offers a Retry, and the retry either collides on the suppression list's final name
// or leaves a second one behind in a production portal.
func composeErr(ctx context.Context, projectID string, err error) error {
	var partial *audience.ComposePartialError
	if errors.As(err, &partial) {
		slog.ErrorContext(ctx, "audience compose left orphaned or unconfirmed platform state",
			"project_id", projectID, "suppression_list_id", partial.Suppression.ListID,
			"master_name", partial.MasterName, "error", err)
		// 500, matching `Response("ComposePartial", StatusInternalServerError)` in the
		// design. The body's `code` is documented as the HTTP status, so "409" here left
		// a client reading the status and a client reading the body disagreeing about the
		// same response -- on the one response that must never be blindly retried.
		// 409 is not available to switch the mapping TO: commonBriefErrors already binds
		// StatusConflict to the generic Conflict error, and Goa cannot map two errors to
		// one status. The do-not-retry instruction is carried by the message and by the
		// distinct error type, which is what the UI branches on.
		out := &explore.AudienceComposePartialError{
			Code:    "500",
			Message: composePartialMessage(partial),
		}
		// Suppression is set only when one was actually CONFIRMED created (ListID
		// present). A master-create-only failure (no exclusions requested, or the
		// suppression create itself failed) leaves partial.Suppression zero-valued --
		// sending it anyway would put a suppression object on the wire with blank
		// list_id/name/hubspot_url despite AudienceComposedList declaring all three
		// Required, misreporting a list that does not exist as one the operator must go
		// reconcile in HubSpot. An UNCONFIRMED suppression create (SuppressionUnconfirmed)
		// has no id to give either, so it goes out as SuppressionName instead, mirroring
		// how an unconfirmed master is reported via MasterName rather than Suppression.
		switch {
		case partial.Suppression.ListID != "":
			out.Suppression = composedListResult(&partial.Suppression)
		case partial.SuppressionUnconfirmed && partial.Suppression.Name != "":
			out.SuppressionName = &partial.Suppression.Name
		}
		if partial.MasterName != "" {
			out.MasterName = &partial.MasterName
		}
		// The record-failed shape is the only one carrying a CONFIRMED master. Sending it
		// is what lets the UI say "these lists exist, attach one by hand" instead of
		// "something went wrong" — the difference between a reconcile the operator can
		// finish and a compose they will repeat.
		if partial.Master != nil {
			out.Master = composedListResult(partial.Master)
		}
		return out
	}
	return audienceExploreErr(ctx, "compose audience master", projectID, err)
}

// composePartialMessage distinguishes a CONFIRMED suppression orphan (the master
// create definitely failed) from an UNCONFIRMED master create, and from an
// UNCONFIRMED suppression create (HubSpot may have created either despite the
// error) — the remedy differs: the confirmed case names one list an operator must
// reconcile, the unconfirmed cases tell them to search by name before assuming
// nothing happened.
func composePartialMessage(partial *audience.ComposePartialError) string {
	hasSuppression := partial.Suppression.ListID != "" || partial.Suppression.Name != ""
	switch {
	case partial.Master != nil:
		// Checked first: this shape has a confirmed master AND possibly a confirmed
		// suppression, so every arm below would describe it wrongly as a create that did
		// not finish. Nothing needs reconciling in HubSpot here — the lists are correct
		// and complete; only the link to the campaign is missing.
		return "the master list and its suppression list were created, but attaching them to the campaign failed — " +
			"attach the master list to the campaign's audience manually; do not compose again, the lists already exist"
	case partial.SuppressionUnconfirmed:
		return "the combined suppression list creation is unconfirmed (it may have been created) — " +
			"search HubSpot for it by name before composing again; do not simply retry"
	case partial.MasterName != "" && hasSuppression:
		return "the combined suppression list was created and the master list creation is unconfirmed " +
			"(it may have been created) — verify both in HubSpot before composing again; do not simply retry"
	case partial.MasterName != "":
		return "the master list creation is unconfirmed (it may have been created) — " +
			"search HubSpot for it by name before composing again; do not simply retry"
	default:
		return "the combined suppression list was created but the master list was not — " +
			"reconcile the suppression list in HubSpot before composing again; do not simply retry"
	}
}

// currentRegistrantsResult is check 4 on the wire, or nil when it did not run.
//
// An empty verdict means no event name reached the audit, so there is nothing to report. A
// zero-valued check would arrive as a verdict the client has to interpret, and the one
// interpretation that must never be reached is "passed".
func currentRegistrantsResult(c audience.Check) *explore.AudienceQaCheck {
	if c.Verdict == "" {
		return nil
	}
	return qaCheckResult(c)
}

// ─── Result mapping ───

func eventIdentityResult(id audience.EventIdentity) *explore.AudienceEventIdentity {
	out := &explore.AudienceEventIdentity{EventName: id.Name, EventDates: id.Dates}
	// Absent rather than "" when the brand could not be extracted: the field drives
	// brand-scoped list matching, and an empty token would match every list name.
	if brand := strings.TrimSpace(id.BrandShort); brand != "" {
		out.BrandShort = &brand
	}
	return out
}

func composedListResult(l *audience.ComposedList) *explore.AudienceComposedList {
	if l == nil {
		return nil
	}
	return &explore.AudienceComposedList{
		ListID: l.ListID, Name: l.Name, HubspotURL: l.HubSpotURL, Size: l.Size,
	}
}

func listBriefResults(briefs []audience.ListBrief) []*explore.AudienceListBrief {
	out := make([]*explore.AudienceListBrief, 0, len(briefs))
	for _, b := range briefs {
		row := &explore.AudienceListBrief{
			ListID:  b.ListID,
			Name:    b.Name,
			Size:    b.Size,
			Missing: b.Missing,
		}
		if legacy := strings.TrimSpace(b.ResolvedFromLegacyID); legacy != "" {
			row.ResolvedFromLegacyID = &legacy
		}
		if u := strings.TrimSpace(b.HubSpotURL); u != "" {
			row.HubspotURL = &u
		}
		out = append(out, row)
	}
	return out
}

func qaCheckResult(c audience.Check) *explore.AudienceQaCheck {
	return &explore.AudienceQaCheck{Verdict: string(c.Verdict), Findings: findingResults(c.Findings)}
}

func findingResults(findings []audience.Finding) []*explore.AudienceQaFinding {
	out := make([]*explore.AudienceQaFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, &explore.AudienceQaFinding{
			Severity: string(f.Severity), Message: f.Message, Fix: f.Fix,
		})
	}
	return out
}

// signalStrings renders signal values for the wire. Never nil: an omitted
// missing-signals array and an empty one mean opposite things to the section that
// renders them ("nothing is missing" vs "the field was not populated").
func signalStrings(signals []audience.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, string(s))
	}
	return out
}

// derefBool reads an optional boolean payload field, defaulting to false.
//
// False is the right default for the regulatory targeting flags: a caller that did
// not say the send targets the EU has not asserted that it does, and inventing the
// assertion would make the GDPR check fail lists that need no GDPR suppression.
func derefBool(b *bool) bool {
	return b != nil && *b
}
