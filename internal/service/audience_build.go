// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// AudienceBuilder performs the platform-side half of a build: resolving an event's past
// editions and creating the HubSpot lists. Declared as an interface here (rather than taking
// the concrete snowflake/hubspot clients) so the orchestration is testable without a warehouse
// or a live portal — and so a deployment with neither configured degrades to a typed error
// instead of a nil dereference.
type AudienceBuilder interface {
	// ResolvePastEditions returns the VERBATIM names of this event's past editions. The names
	// are used as exact HubSpot filter values, so an implementation must not guess or
	// normalise them. Returning an empty slice is valid: a first-time event has none.
	//
	// eventTerm must be the year-FREE event family ("KubeCon Korea", not "KubeCon Korea 2026").
	// The warehouse query matches the term and EXCLUDES currentYear, so a term containing that
	// year is unsatisfiable and silently returns nothing. Callers use eventFamily() rather than
	// relying on an implementation to strip it.
	ResolvePastEditions(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]string, error)
	// CreateList creates one DYNAMIC contact list and returns its platform id.
	// CreateList creates one DYNAMIC contact list in the PROJECT's portal and returns its
	// platform id. projectID is explicit rather than context-carried: HubSpot credentials are
	// stored per project, so building in the wrong portal is a silent, damaging failure.
	CreateList(ctx context.Context, projectID, name string, filter json.RawMessage) (string, error)
	// BeginBuild returns a context scoped to ONE build. Every CreateList made with it shares
	// one resolved platform client per project — a build creates several lists and they must
	// all land in the same portal, or the master references ids that do not exist together.
	//
	// Scoping to the build (rather than caching on the implementation) is deliberate: a
	// long-lived cache would pin a credential that has since been rotated or revoked.
	BeginBuild(ctx context.Context) context.Context
}

// audiencePersistTimeout bounds the post-create writes, which run on a context detached from
// the request so a disconnect cannot orphan HubSpot lists that already exist. Short: these are
// single-row updates, and the ceiling only bounds a pathological hang.
const audiencePersistTimeout = 10 * time.Second

// errUnconfirmedCreate marks a create whose outcome is genuinely UNKNOWN — a 2xx carrying no
// list id. It is a SENTINEL rather than a message convention because the failed-vs-building
// decision depends on it: hubspot.IsUnconfirmed cannot classify this case (the client returned
// no typed error at all), so without it a list that MAY exist upstream would be recorded as
// "nothing was created" and an operator would skip reconciling it.
var errUnconfirmedCreate = errors.New("the create returned no list id")

// audienceUnavailableErr is the typed 503 returned when the pieces BuildAudience needs (the
// brief repository, the Snowflake/HubSpot builder) are not wired. Mirrors the CRUD routes'
// unavailable mode: the route stays mounted and returns the contract's 503 rather than a bare
// 404 or a nil-dereference panic.
func audienceUnavailableErr() error {
	return &audiences.ConnServiceUnavailableError{
		Code:    "503",
		Message: "audience building is not configured (requires the brief repository and the HubSpot/Snowflake clients)",
	}
}

// audienceBuildErr maps a platform-side build failure to the typed InternalServerError.
//
// The Code is "500", matching the status Goa actually encodes for this type. An earlier version
// set "502" to signal an upstream failure, but Goa maps InternalServerError to
// StatusInternalServerError regardless of the string — so clients got a 500 carrying a body
// claiming 502, which is worse than either alone. The upstream nature of the failure is carried
// in the MESSAGE, where it does not contradict the status line.
//
// Upstream error text can carry request ids but not credentials — the clients redact those.
func audienceBuildErr(err error) error {
	return &audiences.InternalServerError{
		Code:    "500",
		Message: "the audience build failed upstream: " + err.Error(),
	}
}

// audiencePersistErr is audienceBuildErr's counterpart for the opposite failure: the upstream
// build SUCCEEDED and the local write did not.
//
// Reusing audienceBuildErr here would prefix "failed upstream" onto a failure that was not
// upstream at all, sending an operator to check HubSpot when HubSpot is the one system known to
// be fine — and contradicting the neutral wording of the wrapped unrecordedListsErr, which is
// telling them the lists EXIST. The distinction matters most in exactly this case, because the
// remedy is to reconcile listed ids rather than to investigate the platform.
func audiencePersistErr(err error) error {
	return &audiences.InternalServerError{
		Code:    "500",
		Message: "the audience lists were created but recording them failed: " + err.Error(),
	}
}

// SetBuilder injects the platform-side builder. Opt-in like the other late-bound dependencies
// so the ~existing NewAudienceService call sites are unaffected.
func (s *AudienceService) SetBuilder(b AudienceBuilder) {
	if b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.builder = b
}

// BuilderIsSet reports whether the platform-side builder was injected. Exported ONLY so the
// container's wiring tests can assert injection directly: BuildAudience returns the same typed
// 503 whether the repo or the builder is missing, so an error-based assertion cannot tell a
// wired service from an unwired one.
func (s *AudienceService) BuilderIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.builder != nil
}

// briefEventDetails is the slice of a brief's opaque EventDetails blob this build needs.
// EventDetails is deliberately schemaless (see internal/dispatch/reddit.go), so this decodes
// opportunistically and validates what it found rather than assuming a shape.
type briefEventDetails struct {
	EventName string `json:"eventName"`
	Country   string `json:"country"`
	Location  string `json:"location"`
	Year      string `json:"year"`
}

// BuildAudience derives a brief's HubSpot audience and records it.
//
// The email channel cannot dispatch until an audience reaches "built" (the HubSpot dispatcher
// refuses a brief whose audience is unbuilt or carries no master list), so this is the step
// that takes an approved brief from "planned" to "sendable".
//
// Ordering is deliberate: the audience row is created as BUILDING *before* any HubSpot call,
// so a crash mid-build leaves a visible building row rather than an invisible gap plus orphan
// lists in the portal.
func (s *AudienceService) BuildAudience(ctx context.Context, p *audiences.BuildAudiencePayload) (*audiences.Audience, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	briefs, builder, err := s.buildDeps()
	if err != nil {
		return nil, err
	}

	brief, berr := briefs.GetBrief(ctx, p.ProjectID, p.BriefID)
	if berr != nil {
		return nil, mapAudienceErr(berr)
	}

	// Same lifecycle guard the campaign-creation path applies (brief.go). Building creates
	// REAL HubSpot lists and makes the brief sendable, so a draft must not reach it — the
	// event details it would be built from are still being edited.
	// Capture the version the approval was observed at; the insert below is gated on it.
	approvedVersion := brief.Version
	if brief.Status != model.BriefApproved {
		return nil, audienceValidationErr(fmt.Errorf("brief must be approved before building its audience (it is %s)", brief.Status))
	}

	details, derr := decodeEventDetails(brief)
	if derr != nil {
		return nil, audienceValidationErr(derr)
	}

	// Resolve past editions BEFORE creating anything. A warehouse failure here must not leave
	// half-built lists in the portal, and the names it returns are the only acceptable source
	// for the group-5/7 filters.
	// Pass the year-FREE family term. The event name normally carries its year, and the
	// warehouse both matches the term and excludes the year — so sending the full name asks for
	// rows containing 2026 that do not contain 2026, which matches nothing and degrades every
	// returning event to a country-only audience while reporting success.
	family, year := eventFamily(details.EventName, details.Year)
	editions, rerr := builder.ResolvePastEditions(ctx, family, details.Location, year)
	if rerr != nil {
		// Degrade rather than fail: group 4 needs no editions, so a warehouse outage still
		// yields a usable (narrower) audience. rerr is carried into the plan (NOT just logged)
		// so the stored InclusionSummary says "could not read the history" rather than the
		// first-time-event note — the log line rotates away, the summary does not.
		slog.WarnContext(ctx, "could not resolve past editions; building a country-only audience",
			"brief_id", p.BriefID, "event", details.EventName, "error", rerr)
		editions = nil
	}

	// A blank location means ResolvePastEventNames ran WITHOUT its location predicate, matching
	// the event family alone. For a multi-city family ("Open Source Summit") that can resolve
	// other cities' editions. It is recorded rather than refused: the resolved names are only
	// ever used ANDed with the host country (group 5) or region (group 7), so a stray edition
	// widens the audience to family alumni already in the target geography instead of reaching
	// outside it — whereas refusing would discard a correct returning-event audience every time
	// a brief omits `location`.
	unnarrowed := strings.TrimSpace(details.Location) == "" && len(editions) > 0
	if unnarrowed {
		// Cap logged editions to prevent a broad match from truncating the warning in log aggregators.
		logEditions := editions
		if len(logEditions) > 10 {
			logEditions = logEditions[:10]
		}
		slog.WarnContext(ctx, "resolved past editions without a location predicate; they may span cities",
			"brief_id", p.BriefID, "event", details.EventName, "edition_count", len(editions), "editions_sample", strings.Join(logEditions, ","))
	}

	planInput := audience.PlanInput{
		PastEditionsErr:    rerr,
		EventName:          details.EventName,
		Country:            details.Country,
		PastEditions:       editions,
		EditionsUnnarrowed: unnarrowed,
	}
	// Validate the plan BEFORE creating the row: a brief that cannot be planned must not leave
	// a building row behind. The plan is rebuilt below with the row id as its BuildRef.
	if _, perr := audience.BuildPlan(planInput); perr != nil {
		return nil, audienceValidationErr(perr)
	}

	// Record the intent first (status defaults to building), so an interrupted build is
	// visible and reconcilable instead of silently absent.
	row := &model.CampaignAudience{
		ProjectID: p.ProjectID,
		BriefID:   p.BriefID,
		Platform:  model.ProviderHubSpot,
		Status:    model.AudienceBuilding,
		CreatedBy: marshalActor(actorFromCtx(ctx)),
	}
	// Gate the insert on the brief STILL being approved at the version read above. Between
	// that check and here we resolved past editions (a warehouse round-trip), so a concurrent
	// ReplaceBrief can have reset the brief to draft and bumped its version — and the plain
	// create only checks `status <> 'archived'`, so the build would go on to create REAL
	// HubSpot lists from a stale approved snapshot. Mirrors CreateJobForApprovedBrief.
	created, cerr := repo.CreateAudienceForApprovedBrief(ctx, row, approvedVersion)
	if cerr != nil {
		return nil, mapAudienceErr(cerr)
	}

	// Rebuild the plan with the row id as BuildRef so THIS build's list names cannot collide
	// with a previous build's for the same brief — HubSpot list names are portal-global, and a
	// collision would silently adopt the older build's lists.
	planInput.BuildRef = created.ID
	plan, perr := audience.BuildPlan(planInput)
	if perr != nil {
		return nil, audienceValidationErr(perr)
	}

	// Scope the builder's client cache to THIS build (see dispatch.BeginBuild): all of a
	// build's lists must land in one portal, but a credential rotated between builds must be
	// picked up by the next one.
	master, ids, buildErr := createPlanLists(builder.BeginBuild(ctx), builder, p.ProjectID, plan)
	summary := plan.InclusionSummary()

	if buildErr != nil {
		// Two DIFFERENT outcomes, and telling an operator the wrong one costs real time:
		//
		//   - Nothing was created and the failure is DEFINITE (bad credentials, a plain 4xx):
		//     there is no upstream state, so mark it FAILED. Leaving it building would send
		//     someone hunting for portal orphans that do not exist.
		//   - Anything was created, or the outcome is UNCONFIRMED: keep it BUILDING, because a
		//     list may exist upstream and a blind retry would duplicate it.
		//
		// Either way the created ids are RECORDED. They are the only confirmed handles to the
		// lists that do exist; discarding them (as this originally did) makes the row
		// unreconcilable — the operator knows a build broke but not what it left behind.
		created.InclusionSummary = partialSummary(summary, ids, buildErr)
		ambiguous := hubspot.IsUnconfirmed(buildErr) || errors.Is(buildErr, errUnconfirmedCreate)
		if len(ids) == 0 && !ambiguous {
			created.Status = model.AudienceFailed
		}
		// Detached for the same reason as the success path below: lists may exist upstream and
		// this row is the only record of them.
		partialCtx, cancelPartial := context.WithTimeout(context.WithoutCancel(ctx), audiencePersistTimeout)
		defer cancelPartial()
		if _, uerr := repo.UpdateAudience(partialCtx, created, created.Version); uerr != nil {
			slog.ErrorContext(ctx, "failed to record a partial audience build",
				"audience_id", created.ID, "created_lists", strings.Join(ids, ","), "error", uerr)
			// The persist failed, so the row stays 'building' with an EMPTY inclusion_summary
			// while REAL HubSpot lists exist — precisely the unreconcilable state the comment
			// above says is fixed. The API response is now the ONLY channel carrying the ids, so
			// put them in it. Without this the operator learns a build broke and has no handle on
			// what it left behind, and a blind retry duplicates every list.
			return nil, audienceBuildErr(unconfirmedNote(unrecordedListsErr(buildErr, created.ID, ids), ambiguous))
		}
		return nil, audienceBuildErr(unconfirmedNote(buildErr, ambiguous))
	}

	created.PlatformMasterListID = master
	created.SuppressionListIDs = nil
	created.InclusionSummary = summary
	created.Status = model.AudienceBuilt
	if verr := created.Validate(); verr != nil {
		return nil, audienceValidationErr(verr)
	}

	// DETACHED context: the HubSpot master and inclusion lists already exist upstream, so a
	// client disconnect between the final create and this write would make pgx skip it and
	// leave a real master list orphaned with the row still 'building' — a build that succeeded
	// on the platform and looks failed in the database. Same reasoning as the orchestrator's
	// post-create persist. Bounded so it cannot hang shutdown.
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), audiencePersistTimeout)
	defer cancelPersist()

	updated, uerr := repo.UpdateAudience(persistCtx, created, created.Version)
	if uerr != nil {
		// The lists EXIST upstream but the row does not reflect them. Log the ids so the
		// portal state can be reconciled by hand rather than orphaned silently.
		slog.ErrorContext(ctx, "audience lists created but recording them failed",
			"audience_id", created.ID, "master_list_id", master, "list_ids", strings.Join(ids, ","),
			"error", uerr)
		// Return the ids too, for the same reason the partial path does — and more urgently.
		// mapAudienceErr has no case for a DB error, so it falls through to a bare "an internal
		// server error occurred" carrying nothing. Here the MASTER exists as well as every
		// inclusion list, so a blind retry duplicates the whole set, not just part of it. The
		// slog line above is not enough: it is not visible to the caller who has to decide
		// whether to retry.
		//
		// createPlanLists returns the master as the LAST element of ids, so ids already covers
		// it — appending master again would name it twice in the operator's message. Guarded
		// rather than assumed, because that is an easy invariant to break from the other side.
		reported := ids
		if !slices.Contains(reported, master) {
			reported = append(slices.Clone(ids), master)
		}
		return nil, audiencePersistErr(unrecordedListsErr(uerr, created.ID, reported))
	}
	return audienceResult(updated), nil
}

// unconfirmedNote makes an AMBIGUOUS outcome say so in the response body.
//
// The row-state logic classifies ambiguity via hubspot.IsUnconfirmed, but that classification
// used to stop at the row: only the 2xx-no-id sentinel (errUnconfirmedCreate) spelled out
// "verify before retrying" in its own message. The other three ambiguous sources — a mutating
// 429, a mutating 5xx, and a mutating transport failure — surfaced as a plain 500 whose text
// reads like an ordinary transient error, inviting exactly the blind retry that duplicates a
// list HubSpot may already have created.
//
// Idempotent: the sentinel's message already carries the warning, so it is not repeated.
func unconfirmedNote(err error, ambiguous bool) error {
	if !ambiguous || strings.Contains(err.Error(), "UNCONFIRMED") {
		return err
	}
	return fmt.Errorf("%w (UNCONFIRMED: the list may already exist in HubSpot — verify before "+
		"retrying, a blind retry can duplicate it)", err)
}

// unrecordedListsErr carries the ids of HubSpot lists that EXIST upstream but could not be
// recorded, because the write that would have recorded them failed.
//
// Used on both exits, for the same reason and with different urgency:
//
//   - the PARTIAL path, where the build broke midway and the record of what it left behind also
//     broke, and
//   - the SUCCESS path, where every list including the MASTER was created and only the write
//     failed — worse, because a blind retry then duplicates the entire set.
//
// Either way the row is stuck 'building' with no summary, so the ids survive only in the logs
// and in this error. audienceBuildErr interpolates err.Error() into the 500 body, which makes
// the API response the operator's one reliable handle on the orphaned lists. The wording is
// deliberately neutral about WHICH step failed, since the wrapped error already says that.
func unrecordedListsErr(cause error, audienceID string, ids []string) error {
	if len(ids) == 0 {
		// Nothing upstream to reconcile, so there is nothing to carry — do not inflate the
		// message with a reconcile instruction that names no lists.
		return cause
	}
	return fmt.Errorf("%w (recording the result failed, so audience %s still reads 'building' "+
		"with no summary; these HubSpot lists EXIST and must be reconciled before retrying: %s)",
		cause, audienceID, strings.Join(ids, ", "))
}

// partialSummary records what a failed build actually left upstream. The plan summary alone
// describes what was INTENDED, which is misleading after a failure — an operator needs the ids
// of the lists that exist in order to reconcile them before retrying.
func partialSummary(planSummary string, ids []string, buildErr error) string {
	var b strings.Builder
	b.WriteString(planSummary)
	b.WriteString("\nBuild incomplete: ")
	b.WriteString(buildErr.Error())
	if len(ids) == 0 {
		b.WriteString("\nNo HubSpot lists were created.")
		return b.String()
	}
	b.WriteString("\nHubSpot lists ALREADY CREATED (reconcile these before retrying): ")
	b.WriteString(strings.Join(ids, ", "))
	return b.String()
}

// createPlanLists creates every planned inclusion list, then creates the MASTER list as their
// union and returns its id.
//
// The master is what the email dispatcher sends to — it reads only platform_master_list_id — so
// it MUST be a union. Recording one inclusion list as the master would create the others in the
// portal and never email them: a build that reports success while reaching a fraction of the
// intended people.
//
// It stops at the FIRST failure and returns the ids created so far, so a partial build records
// what exists upstream instead of creating more state it cannot recover.
func createPlanLists(ctx context.Context, b AudienceBuilder, projectID string, plan *audience.Plan) (master string, ids []string, err error) {
	for _, l := range plan.Lists {
		id, cerr := b.CreateList(ctx, projectID, l.Name, l.Filter)
		if cerr != nil {
			return "", ids, fmt.Errorf("create list %q: %w", l.Name, cerr)
		}
		if strings.TrimSpace(id) == "" {
			// A 2xx with no id is UNCONFIRMED, not success: the list may exist. Fail here so a
			// retry verifies by name rather than blind-creating a duplicate.
			return "", ids, fmt.Errorf("create list %q %w (UNCONFIRMED: verify before retrying)", l.Name, errUnconfirmedCreate)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", nil, errors.New("the plan produced no lists")
	}

	masterFilter, ferr := audience.MasterListFilter(ids)
	if ferr != nil {
		return "", ids, ferr
	}
	masterName := plan.MasterName()
	masterID, merr := b.CreateList(ctx, projectID, masterName, masterFilter)
	if merr != nil {
		return "", ids, fmt.Errorf("create master list %q: %w", masterName, merr)
	}
	if strings.TrimSpace(masterID) == "" {
		return "", ids, fmt.Errorf("create master list %q %w (UNCONFIRMED: verify before retrying)", masterName, errUnconfirmedCreate)
	}
	return masterID, append(ids, masterID), nil
}

// eventFamily splits an event name into its year-free family term and the edition year.
//
// The year is taken from the brief's details when present, otherwise derived from the name
// itself (event names normally carry it). When neither yields a 4-digit year the family is
// returned unchanged with an empty year — the builder then resolves no editions rather than
// guessing, since a wrong year excludes the wrong edition.
func eventFamily(eventName, detailYear string) (family, year string) {
	// The NAME wins when it carries a year. The name is what the search term is built from, so
	// a detail year that disagrees with it is self-defeating: for "KubeCon Korea 2026" with a
	// stale year=2025, the query keeps 2026 in the term while excluding 2025 — returning the
	// CURRENT edition as a past one and building an audience from people already registered.
	// The details field can go stale independently (it is edited by hand); the year embedded in
	// the name cannot disagree with the name.
	year = yearInName(eventName)
	if year == "" {
		year = strings.TrimSpace(detailYear)
	}
	if !isFourDigitYear(year) {
		year = ""
	}
	if year == "" {
		return strings.TrimSpace(eventName), ""
	}
	family = strings.TrimSpace(strings.ReplaceAll(eventName, year, ""))
	if family == "" {
		family = strings.TrimSpace(eventName)
	}
	return family, year
}

// yearInName extracts a standalone 4-digit 19xx/20xx year from an event name.
func yearInName(s string) string {
	for i := 0; i+4 <= len(s); i++ {
		c := s[i : i+4]
		if !isFourDigitYear(c) || (c[0] != '1' && c[0] != '2') {
			continue
		}
		// Reject a longer digit run (e.g. an id) that merely contains four digits.
		if (i == 0 || s[i-1] < '0' || s[i-1] > '9') && (i+4 == len(s) || s[i+4] < '0' || s[i+4] > '9') {
			return c
		}
	}
	return ""
}

func isFourDigitYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// decodeEventDetails pulls the fields the build needs out of the brief's opaque blobs. It
// mirrors internal/dispatch's decodeBriefFields: EventDetails is the primary source, Copy is a
// fallback, and a blob that isn't this shape is skipped rather than failing the request.
func decodeEventDetails(b *model.CampaignBrief) (briefEventDetails, error) {
	var out briefEventDetails
	for _, blob := range []json.RawMessage{b.EventDetails, b.Copy} {
		if len(blob) == 0 {
			continue
		}
		var partial briefEventDetails
		if err := json.Unmarshal(blob, &partial); err != nil {
			continue
		}
		if out.EventName == "" {
			out.EventName = strings.TrimSpace(partial.EventName)
		}
		if out.Country == "" {
			out.Country = strings.TrimSpace(partial.Country)
		}
		if out.Location == "" {
			out.Location = strings.TrimSpace(partial.Location)
		}
		if out.Year == "" {
			out.Year = strings.TrimSpace(partial.Year)
		}
	}
	if out.EventName == "" {
		return out, fmt.Errorf("brief %s has no eventName in its details; an audience cannot be named or scoped without it", b.ID)
	}
	if out.Country == "" {
		return out, fmt.Errorf("brief %s has no country in its details; every inclusion list is country-scoped", b.ID)
	}
	return out, nil
}

// buildDeps returns the brief repo and builder, or a typed error when either is missing.
func (s *AudienceService) buildDeps() (domain.BriefRepository, AudienceBuilder, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.briefs == nil {
		return nil, nil, audienceUnavailableErr()
	}
	if s.builder == nil {
		return nil, nil, audienceUnavailableErr()
	}
	return s.briefs, s.builder, nil
}
