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
// The message contains only safe, redacted text. Raw error details from Snowflake, HubSpot,
// and decryption sources are logged server-side via slog and never exposed in the public API
// response. Reconciliation IDs and the UNCONFIRMED marker (if present) are preserved as they
// are safe and necessary for operators to reconcile orphaned state.
func audienceBuildErr(err error) error {
	msg := "the audience build failed upstream"
	if safe := reconciliationText(err); safe != "" {
		msg += ": " + safe
	}
	return &audiences.InternalServerError{
		Code:    "500",
		Message: msg,
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
//
// The message contains only safe, redacted text. Raw pgx and driver errors are logged server-side
// via slog and never exposed in the public API response. Reconciliation IDs (list ids) are
// preserved as they are essential for operators to reconcile orphaned HubSpot lists.
func audiencePersistErr(err error) error {
	msg := "the audience lists were created but recording them failed"
	if safe := reconciliationText(err); safe != "" {
		msg += ": " + safe
	}
	return &audiences.InternalServerError{
		Code:    "500",
		Message: msg,
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

	// CLAIM FIRST — before the brief read, not merely before the warehouse read. The partial
	// unique index from migration 000018 serializes only builds whose rows OVERLAP, so the
	// lease covers exactly the interval between this insert and the row leaving `building`.
	// EVERY blocking step ahead of the insert is therefore a window in which a second request
	// for the same brief finds nothing to conflict with: if it is delayed there past the first
	// request's entire build, it claims cleanly against a now-`built` row and goes on to create
	// a whole second set of HubSpot lists. Resolving past editions first — a Snowflake
	// round-trip — made that window seconds wide; reading the brief first makes it a database
	// round-trip. Neither is a bound, only a size, so the claim goes first and the ordering
	// stops depending on how fast the steps ahead of it happen to be.
	//
	// The claim gates itself on the brief being APPROVED (the plain create only checks
	// `status <> 'archived'`) and reports the version it observed under its own row lock. That
	// is why it needs no expected version from here: there is no earlier read to have pinned,
	// which is the whole point of it running first. Recording the intent up front also does
	// what the original ordering was written for — an interrupted build is a visible
	// `building` row rather than a silent gap.
	row := &model.CampaignAudience{
		ProjectID: p.ProjectID,
		BriefID:   p.BriefID,
		Platform:  model.ProviderHubSpot,
		Status:    model.AudienceBuilding,
		CreatedBy: marshalActor(actorFromCtx(ctx)),
	}
	created, approvedVersion, cerr := repo.CreateAudienceForApprovedBrief(ctx, row)
	if cerr != nil {
		return nil, refusedClaimErr(ctx, briefs, p, cerr)
	}

	// From here every early return must RELEASE the claim. Nothing has been created upstream
	// yet on any of these paths, so there is nothing to reconcile first — and a `building` row
	// left behind by a request that has given up blocks every later build of this brief behind
	// a 409 until an operator intervenes.
	brief, berr := briefs.GetBrief(ctx, p.ProjectID, p.BriefID)
	if berr != nil {
		releaseUnstartedClaim(ctx, repo, created, berr)
		return nil, mapAudienceErr(berr)
	}

	// Same lifecycle guard the campaign-creation path applies (brief.go). Building creates
	// REAL HubSpot lists and makes the brief sendable, so a draft must not reach it — the
	// event details it would be built from are still being edited. The claim already refused a
	// brief that was not approved when it locked the row, so this can only fire if the brief
	// was retracted in the moment since; it is kept because the alternative is a guard whose
	// correctness depends on a second component's implementation.
	if brief.Status != model.BriefApproved {
		verr := fmt.Errorf("brief must be approved before building its audience (it is %s)", brief.Status)
		releaseUnstartedClaim(ctx, repo, created, verr)
		return nil, audienceValidationErr(verr)
	}

	details, derr := decodeEventDetails(brief)
	if derr != nil {
		releaseUnstartedClaim(ctx, repo, created, derr)
		return nil, audienceValidationErr(derr)
	}

	// Past editions are resolved under the claim. Nothing has been created upstream yet, so a
	// warehouse failure here still cannot leave half-built lists in the portal.
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
			"brief_id", p.BriefID, "event", details.EventName, "error", audience.SafeErrorCause(rerr))
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
	// Rebuild the plan with the row id as BuildRef so THIS build's list names cannot collide
	// with a previous build's for the same brief — HubSpot list names are portal-global, and a
	// collision would silently adopt the older build's lists.
	planInput.BuildRef = created.ID
	plan, perr := audience.BuildPlan(planInput)
	if perr != nil {
		releaseUnstartedClaim(ctx, repo, created, perr)
		return nil, audienceValidationErr(perr)
	}

	// LAST thing before the first upstream call: confirm the brief is STILL approved at the
	// version the claim locked. The claim's own gate proves the brief was approved when the
	// lease was taken, which is now BEFORE the warehouse round-trip rather than after it — so
	// on its own it no longer says anything about the brief at the moment lists are created. A
	// ReplaceBrief landing during that round-trip would otherwise build real HubSpot lists from
	// an approval the operator has since withdrawn, which is the case the gate exists for. The
	// two guards are not redundant: the claim's serializes builds, this one dates the approval.
	if serr := confirmStillApproved(ctx, briefs, p.ProjectID, p.BriefID, approvedVersion); serr != nil {
		releaseUnstartedClaim(ctx, repo, created, serr)
		return nil, mapAudienceErr(serr)
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
			return nil, audienceBuildErr(unconfirmedNote(unrecordedListsErr(buildErr, created.ID, ids, true), ambiguous))
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
		return nil, audiencePersistErr(unrecordedListsErr(uerr, created.ID, reported, false))
	}
	return audienceResult(updated), nil
}

// refusedClaimErr renders a refused claim as the error the CALLER's situation warrants.
//
// The claim gates on approval itself, so a brief that was never approved and a brief that
// moved mid-build both come back as domain.ErrStaleApproval — a 409 whose message is about
// versions. That is right for the race and wrong for the ordinary case, which is someone
// building a draft: they need a 400 naming the status and what to do about it, not a conflict
// implying they collided with somebody. Since the claim runs first there is no earlier read to
// tell the two apart, so this one is done here, on the failure path only.
//
// A brief that cannot be re-read falls through to the generic mapping. Guessing 400 there
// would blame the caller for the service's own inability to look.
func refusedClaimErr(ctx context.Context, briefs domain.BriefRepository, p *audiences.BuildAudiencePayload, cerr error) error {
	if errors.Is(cerr, domain.ErrStaleApproval) {
		if brief, berr := briefs.GetBrief(ctx, p.ProjectID, p.BriefID); berr == nil && brief.Status != model.BriefApproved {
			return audienceValidationErr(fmt.Errorf("brief must be approved before building its audience (it is %s)", brief.Status))
		}
	}
	return mapAudienceErr(cerr)
}

// confirmStillApproved re-reads the brief and reports domain.ErrStaleApproval unless it is
// still approved at expectedVersion. A read failure is reported as-is and is NOT treated as
// "probably still fine": the caller is about to create real HubSpot lists, and the only safe
// reading of "could not check" is that the check did not pass.
func confirmStillApproved(ctx context.Context, briefs domain.BriefRepository, projectID, briefID string, expectedVersion int64) error {
	brief, err := briefs.GetBrief(ctx, projectID, briefID)
	if err != nil {
		return err
	}
	if brief.Status != model.BriefApproved || brief.Version != expectedVersion {
		return domain.ErrStaleApproval
	}
	return nil
}

// releaseUnstartedClaim marks a just-inserted `building` row FAILED when the build never
// reached HubSpot. The row is the audience-build lease: holding it after the request has given
// up blocks every later build of the same brief behind a 409 until an operator intervenes, and
// unlike the partial-build paths there is nothing upstream to reconcile first. Detached and
// bounded for the same reason the other persists are — a client disconnect must not be the
// reason a lease stays held.
func releaseUnstartedClaim(ctx context.Context, repo domain.AudienceRepository, row *model.CampaignAudience, cause error) {
	row.Status = model.AudienceFailed
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), audiencePersistTimeout)
	defer cancel()
	if _, uerr := repo.UpdateAudience(relCtx, row, row.Version); uerr != nil {
		// Best effort by construction: the caller is already returning an error, so there is
		// nothing better to do than name the row an operator will have to fail by hand.
		slog.ErrorContext(ctx, "failed to release an audience build claim that never started",
			"audience_id", row.ID, "cause", audience.SafeErrorCause(cause), "error", uerr)
	}
}

// reconciliationDetail carries the facts about a build outcome that must reach the caller — which
// HubSpot lists exist and must be reconciled, whether the outcome is confirmed, and (only when
// exposeCause says it is safe) the underlying cause's own text.
//
// This replaces an earlier design where unrecordedListsErr/unconfirmedNote used fmt.Errorf("%w
// (...)", cause, ...) and audienceBuildErr/audiencePersistErr decided whether the resulting
// combined message was safe to return by checking whether it CONTAINED a marker substring like
// "recording the result failed". Because %w places cause's own text first, and unrecordedListsErr
// always appends that marker itself, the check was tautological — true for every error that went
// through the wrapper, regardless of what cause actually was. cause was buildErr (a HubSpot/
// Snowflake failure, already redacted by its own client's safeCause before it reaches here — see
// hubspot.Client) in the build-failure path, but the raw *pgx/driver* error in the persist-failure
// path — and the same tautological check let both through identically. exposeCause makes that
// distinction explicit and structural instead of inferred from text, and errors.As (rather than
// substring-matching err.Error()) is what proves an error actually came from these wrappers.
type reconciliationDetail struct {
	cause       error
	exposeCause bool // true only when cause is known pre-redacted (a builder/platform failure)
	audienceID  string
	ids         []string
	unconfirmed bool
}

func (d *reconciliationDetail) Error() string { return d.safeText() }

func (d *reconciliationDetail) Unwrap() error { return d.cause }

// safeText renders the text safe to expose publicly: cause's own text ONLY when exposeCause is
// true, plus the curated reconciliation facts (list ids, the UNCONFIRMED marker).
func (d *reconciliationDetail) safeText() string {
	var parts []string
	if d.exposeCause && d.cause != nil {
		parts = append(parts, d.cause.Error())
	}
	if d.unconfirmed {
		parts = append(parts, "UNCONFIRMED: the list may already exist in HubSpot — verify before "+
			"retrying, a blind retry can duplicate it")
	}
	if len(d.ids) > 0 {
		parts = append(parts, fmt.Sprintf("recording the result failed, so audience %s still reads "+
			"'building' with no summary; these HubSpot lists EXIST and must be reconciled before "+
			"retrying: %s", d.audienceID, strings.Join(d.ids, ", ")))
	}
	return strings.Join(parts, " ")
}

// reconciliationText extracts the text a *reconciliationDetail carries that is safe to expose, via
// errors.As rather than substring-matching err.Error(). Returns "" for any error that is not (or
// does not wrap) a *reconciliationDetail, or that carries nothing to show — callers fall back to
// their own generic, fixed message in that case.
func reconciliationText(err error) string {
	var d *reconciliationDetail
	if !errors.As(err, &d) {
		return ""
	}
	return d.safeText()
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
// Idempotent: mutates an existing *reconciliationDetail in place (preserving its cause and any
// ids already attached by unrecordedListsErr) rather than wrapping again. Only ever called with a
// buildErr-derived err (see call sites), so a freshly-created detail exposes cause.
func unconfirmedNote(err error, ambiguous bool) error {
	if !ambiguous {
		return err
	}
	var d *reconciliationDetail
	if errors.As(err, &d) {
		d.unconfirmed = true
		return d
	}
	return &reconciliationDetail{cause: err, exposeCause: true, unconfirmed: true}
}

// unrecordedListsErr carries the ids of HubSpot lists that EXIST upstream but could not be
// recorded, because the write that would have recorded them failed.
//
// Used on both exits, for the same reason and with different urgency:
//
//   - the PARTIAL path, where the build broke midway and the record of what it left behind also
//     broke — cause is buildErr, already redacted by the platform client, so exposeCause must be
//     true; and
//   - the SUCCESS path, where every list including the MASTER was created and only the write
//     failed — worse, because a blind retry then duplicates the entire set — but cause is a raw
//     repository error, so exposeCause must be false.
//
// Either way the row is stuck 'building' with no summary, so the ids survive only in the logs and
// in this error's curated (reconciliationText) form.
func unrecordedListsErr(cause error, audienceID string, ids []string, exposeCause bool) error {
	if len(ids) == 0 {
		// Nothing upstream to reconcile, so there is nothing to carry — do not inflate the
		// message with a reconcile instruction that names no lists.
		return cause
	}
	var d *reconciliationDetail
	if errors.As(cause, &d) {
		d.audienceID = audienceID
		d.ids = ids
		return d
	}
	return &reconciliationDetail{cause: cause, exposeCause: exposeCause, audienceID: audienceID, ids: ids}
}

// safeBuildCause renders a redacted description of a builder failure (HubSpot list creation) for
// persistence in InclusionSummary, which a later GET reads back through the public API.
//
// A builder failure is not always an HTTP transport error already redacted by the hubspot
// client's own safeCause: it can fail before any request — e.g. resolving stored HubSpot
// credentials from the repository — and that error text has not passed through any redaction.
// Collapsing unknown causes to a generic description here, rather than persisting err.Error()
// verbatim, keeps a credential-store or driver error from reaching a stored, publicly-readable
// field. Mirrors audience.SafeErrorCause's pattern for warehouse errors.
func safeBuildCause(err error) string {
	if err == nil {
		return "build failed"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "build request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "build request deadline exceeded"
	}
	if hubspot.IsUnconfirmed(err) || errors.Is(err, errUnconfirmedCreate) {
		return "build outcome unconfirmed (the request may have succeeded upstream)"
	}
	return "build failed (see server logs for details)"
}

// partialSummary records what a failed build actually left upstream. The plan summary alone
// describes what was INTENDED, which is misleading after a failure — an operator needs the ids
// of the lists that exist in order to reconcile them before retrying.
func partialSummary(planSummary string, ids []string, buildErr error) string {
	var b strings.Builder
	b.WriteString(planSummary)
	b.WriteString("\nBuild incomplete: ")
	b.WriteString(safeBuildCause(buildErr))
	if len(ids) == 0 {
		// Distinguish unconfirmed (list may exist) from a definite zero-create (no list exists).
		// The row stays BUILDING in both cases, but the summary must clarify the reconciliation path.
		ambiguous := hubspot.IsUnconfirmed(buildErr) || errors.Is(buildErr, errUnconfirmedCreate)
		if ambiguous {
			b.WriteString("\nNo confirmed list ID returned, but the request may have succeeded upstream.")
		} else {
			b.WriteString("\nNo HubSpot lists were created.")
		}
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
// itself (event names normally carry it). When neither yields a year isSupportedYear accepts —
// including a well-formed but out-of-range one like "9999" from a hand-edited details field —
// the family is returned unchanged with an empty year. The builder then resolves no editions
// rather than guessing, since a wrong year excludes the wrong edition; an out-of-range one
// excludes NOTHING, which is worse because it looks like a successful build.
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
	if !isSupportedYear(year) {
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
		if !isSupportedYear(c) {
			continue
		}
		// Reject a longer digit run (e.g. an id) that merely contains four digits.
		if (i == 0 || s[i-1] < '0' || s[i-1] > '9') && (i+4 == len(s) || s[i+4] < '0' || s[i+4] > '9') {
			return c
		}
	}
	return ""
}

// isSupportedYear reports whether s is a 4-digit year in the 19xx/20xx range.
//
// The range is not decoration, and it is deliberately the FULL two-byte prefix rather than a
// first-digit check (which would accept 1000-2999). yearInName can only ever EXTRACT a 19xx/20xx
// year from an event name, so a year outside that range is not comparable with the years it
// is compared AGAINST. Above the range a currentYear of "9999" leaves every real edition
// strictly below it and the exclusion never fires — "past editions only" quietly starts
// returning future ones; below it ("0202") every edition is excluded and the resolve returns
// nothing. The two predicates must be one, which is why the range lives here rather than at
// each comparison.
func isSupportedYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	if s[0:2] != "19" && s[0:2] != "20" {
		return false
	}
	return s[2] >= '0' && s[2] <= '9' && s[3] >= '0' && s[3] <= '9'
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
