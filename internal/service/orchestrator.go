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
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

// maxParallelDispatch bounds concurrent per-platform campaign creation.
const maxParallelDispatch = 5

// CancelGracePeriod is how long Shutdown waits, after cancelling in-flight runs
// on a drain timeout, for them to unwind before it returns (and the pool closes).
// A run cancelled at the drain deadline may, in its worst case, still owe TWO
// detached writes that must complete before the pool closes: persisting a
// just-created upstream campaign (persistResultTimeout) and then writing the
// terminal job status (jobFinalizeTimeout). Sizing the grace to cover both (plus a
// second of slack) keeps the documented invariant honest — both detached writes
// fit inside the grace window rather than racing the pool close. (In practice both
// are single-row statements that finish in milliseconds; the ceilings only bound a
// pathological hang.)
const CancelGracePeriod = jobFinalizeTimeout + persistResultTimeout + time.Second

// claimReleaseTimeout bounds the best-effort pending-claim cleanup, which runs on
// a context detached from the (possibly-cancelled) dispatch context.
const claimReleaseTimeout = 5 * time.Second

// campaignStatusPending is the status of a bare single-flight claim row that has no
// upstream campaign yet. The idempotency fast path must NOT treat a row as a completed
// success just because it carries an upstream id — reuse is gated on a TERMINAL status
// (see isReusableCampaign), not merely on a non-empty id.
const campaignStatusPending = "pending"

// partialOrphanStatuses are the dispatcher-set statuses that describe a RETAINED
// PARTIAL orphan — the upstream campaign was NOT fully created (only a sub-resource
// like the campaign group exists, or the create is ambiguous). The orchestrator
// PRESERVES these on the persisted row (rather than flattening every partial to
// "pending") so the row surfaces WHAT went wrong, and treats them as NON-reusable so a
// retry re-attempts the incomplete create.
// The two literals are exported from the model package (as
// CampaignStatusGroupCreated/CampaignStatusUnconfirmed) so the postgres repo's delete
// guard can share this vocabulary; referenced here rather than re-spelled so the two
// definitions cannot drift apart.
var partialOrphanStatuses = map[string]bool{
	model.CampaignStatusGroupCreated: true,
	model.CampaignStatusUnconfirmed:  true,
}

// preservableErrorStatuses are the dispatcher-set statuses that are PRESERVED on the
// retained-error path rather than flattened to "pending". Two categories:
//   - the partial-orphan statuses above (non-reusable; a retry reconciles), and
//   - created_degraded: the campaign WAS created (a re-dispatch can't repair a degraded
//     sub-step like a short creative count), so it is preserved AND reusable.
//
// This is a CLOSED allowlist, not "any non-empty status": an arbitrary success-looking
// status (e.g. a plain "active" returned when a later step like ad-group creation
// failed) is NOT preserved — that campaign is incomplete and must stay a 'pending'
// reconcilable orphan, not be reused as complete. created_degraded mirrors the dispatch
// package's campaignStatusCreatedDegraded; the group_created/unconfirmed literals are
// drift-guarded by TestPartialOrphanStatusValues in the dispatch package.
var preservableErrorStatuses = map[string]bool{
	model.CampaignStatusGroupCreated:    true,
	model.CampaignStatusUnconfirmed:     true,
	model.CampaignStatusCreatedDegraded: true,
}

// isReusableCampaign reports whether an existing campaign row is a completed upstream
// campaign safe to reuse idempotently. It must carry an upstream id AND be in neither
// the bare-claim status (pending) nor a retained-partial-orphan status
// (group_created/unconfirmed): a row with an empty id (never created), a pending claim,
// or a partial orphan is NOT reused. Non-reuse means the fast path does not report a
// false success — the retained claim then wins the unique-claim conflict on a later
// dispatch and returns "reconciliation required" rather than blind-re-dispatching (which
// could double-create); the row is auto-resumed only once resume support lands
// (LFXV2-2665). Any other non-empty status WITH an id is a completed campaign (created /
// created_degraded / a future terminal status), preserving the original "has id and
// isn't a non-terminal claim/orphan" reuse semantics.
func isReusableCampaign(c *model.Campaign) bool {
	return c != nil &&
		c.PlatformCampaignID != "" &&
		c.Status != campaignStatusPending &&
		!partialOrphanStatuses[c.Status]
}

// dispatchQueueTimeout bounds how long a platform waits for a semaphore slot
// before it's recorded as failed. Without it, a large backlog could keep a job
// queued longer than staleJobCutoff, so the recovery sweep would wrongly fail a
// still-live job. Kept comfortably below staleJobCutoff (15m) even added to
// providerCallTimeout and the finalize write, so a job that is actually
// progressing always reaches a terminal state before it could look stuck.
const dispatchQueueTimeout = 10 * time.Minute

// providerCallTimeout bounds a single provider Dispatch call. The dispatch
// context is otherwise only cancelled at shutdown, so a provider call that hangs
// (unresponsive upstream, dropped connection with no client timeout) would leave
// its job "running" forever and permanently occupy one of the maxParallelDispatch
// semaphore slots. This ceiling guarantees the slot and job are released.
//
// It is a DELIBERATE fail-fast ceiling, NOT the sum of a client's worst-case retry
// policy. Real create flows complete in well under a minute; a client's full 429
// backoff ceiling can exceed this (e.g. the reddit client's worst-case single-create
// wait is ~7m: retryMax*maxRetryWait + attempt timeouts), so on sustained throttling
// this cap truncates the client's best-effort retries and the create is returned as a
// retained partial that the reconcile path handles — preferred over letting one
// throttled create hold a semaphore slot for many minutes. Raising this to cover a
// client's full worst case is NOT free: dispatchQueueTimeout (10m) + providerCallTimeout
// must stay comfortably below staleJobCutoff (15m) so the stale sweeper never fails a
// still-progressing job — 10m + 7m would break that. Rebalancing the queue/stale budget
// to honor the full per-client worst case is an infra-owned timeout review, tracked
// separately.
const providerCallTimeout = 2 * time.Minute

// toggleCallTimeout bounds the SYNCHRONOUS status-toggle platform call, which runs on the
// HTTP request goroutine (unlike the async dispatch above). Without it, a cascade of
// sequential PATCHes — each with its own retry budget — could exceed the server's
// DefaultWriteTimeout (60s), so the platform + DB would change but the response could no
// longer reach the caller (a silent "did it apply?" for the operator). Kept comfortably
// under the 60s write timeout so a failed toggle still returns an error the client receives;
// on timeout the platform call is cancelled and surfaces as UNCONFIRMED (verify/retry).
const toggleCallTimeout = 45 * time.Second

// metricsCallTimeout bounds the SYNCHRONOUS metrics-read platform call, which — like the
// status toggle above — runs on the HTTP request goroutine. A read has no cascade to child
// resources (unlike a toggle, which may sequence PATCHes across a campaign's children), so
// it needs less headroom than toggleCallTimeout while still leaving room under the server's
// DefaultWriteTimeout.
const metricsCallTimeout = 20 * time.Second

// accountsCallTimeout bounds the SYNCHRONOUS account-listing platform call, which — like
// metrics and toggle — runs on the HTTP request goroutine. Account discovery is a pure read
// with no cascade, so it can use the same ceiling as metrics reads.
const accountsCallTimeout = 20 * time.Second

// adoptLookupTimeout bounds the SYNCHRONOUS pre-bind platform lookup: the same kind of call as
// metrics and account discovery, so the same ceiling. Far below the dispatch budget because
// nothing here creates anything upstream for a longer wait to resolve.
const adoptLookupTimeout = 20 * time.Second

// jobFinalizeTimeout bounds the terminal job-status write, which runs on a
// context detached from the dispatch context so a cancelled run still reaches a
// terminal state instead of being stuck queued/running.
const jobFinalizeTimeout = 10 * time.Second

// persistResultTimeout bounds the post-provider persistence upsert that records a
// successfully-created upstream campaign. Like the finalize write it runs on a
// context DETACHED from the dispatch context: once the provider has created the
// paid resource upstream, persisting its id must not be abandoned merely because
// Shutdown cancelled the dispatch context — that would lose the record of a
// campaign that WAS created (an unreconcilable orphan). It is kept well below
// CancelGracePeriod so it completes within the shutdown grace window and can never
// itself hang shutdown. Kept modest (and below jobFinalizeTimeout) so the sum of
// both detached writes fits within CancelGracePeriod without pushing
// ContainerCloseTimeout past the overall shutdown budget (asserted in container
// init()).
const persistResultTimeout = 5 * time.Second

// PlatformDispatcher creates a campaign on one ad platform. Implementations are
// the per-provider adapters (added as those integrations land); the
// orchestrator is agnostic to them.
type PlatformDispatcher interface {
	// Dispatch creates a campaign on the platform and returns the resulting
	// campaign row (platform_campaign_id, status, result populated).
	Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error)
}

// StatusToggler is an OPTIONAL dispatcher capability: pause/resume an existing campaign on
// the platform. Not every platform's dispatcher implements it (see the status-toggle roadmap
// in the architecture doc), so the orchestrator type-asserts for it rather than adding it to
// PlatformDispatcher — a dispatcher that doesn't implement it yields a clean "not supported"
// error (ErrToggleUnsupported → 400).
type StatusToggler interface {
	// ToggleStatus sets the platform campaign's run state. status is
	// model.CampaignRunActive or model.CampaignRunPaused. Returns nil only when the
	// platform confirms the change. campaign is the persisted row so an adapter can reach
	// any child ids it stored at creation (e.g. Reddit persists the ad group + ad ids in
	// Result and must cascade the status to them; a single-node platform like Meta/LinkedIn
	// ignores it and toggles the campaign alone).
	ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error
}

// MetricsReader is an OPTIONAL dispatcher capability: read live performance metrics for an
// existing campaign from the platform. Not every platform's dispatcher implements it, so —
// like StatusToggler — the orchestrator type-asserts for it rather than adding it to
// PlatformDispatcher; a dispatcher that doesn't implement it yields a clean "not supported"
// error (ErrMetricsUnsupported → 400). Unlike ToggleStatus this never mutates platform or DB
// state — it is a pure read, never persisted by the orchestrator.
type MetricsReader interface {
	// ReadMetrics fetches the platform campaign's metrics for window (a closed,
	// platform-agnostic vocabulary — see model.MetricsWindow). campaign is the persisted row
	// so an adapter can reach PlatformCampaignID and any child ids it needs to aggregate.
	ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error)
}

// AccountLister is an OPTIONAL dispatcher capability: enumerate accessible ad accounts for a
// project's stored, encrypted connection. Not every platform's dispatcher implements it, so —
// like StatusToggler and MetricsReader — the orchestrator type-asserts for it rather than
// adding it to PlatformDispatcher; a dispatcher that doesn't implement it yields a clean
// "not supported" error (ErrAccountsUnsupported → 400). This is a pure read that never mutates
// platform or DB state.
type AccountLister interface {
	// ListAccounts enumerates the accessible ad accounts for a project's connection.
	// Returns a list of accessible accounts with minimal identifying information (ID and label).
	//
	// A successful call MUST return a NON-NIL slice, even when the credential reaches zero
	// accounts — return an empty slice, not nil. This deviates from the usual Go convention
	// that nil and empty are interchangeable, deliberately: the caller cannot otherwise tell
	// "the provider authoritatively has nothing for you" from an implementation that fell
	// through a branch and returned its zero values, and the two mean opposite things to an
	// operator choosing an account. ReadAccounts treats (nil, nil) as a contract violation
	// and converts it to a 503 rather than reporting an empty account list as fact.
	ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error)
}

// EmailSearcher is an OPTIONAL dispatcher capability: search the marketing emails reachable
// through a project's stored connection. Discovered by type assertion like StatusToggler,
// MetricsReader and AccountLister; a dispatcher that doesn't implement it yields a clean
// "not supported" error (ErrEmailSearchUnsupported → 400). A pure read that never mutates
// platform or DB state.
//
// Separate from AccountLister rather than folded into it: an ad platform's discovery answers
// which ACCOUNT a credential may act as, and the answer is stored on the connection. This
// answers which EMAIL a campaign should clone, and the answer travels per campaign in the
// dispatch config. Only the email channel has the second question at all.
type EmailSearcher interface {
	// SearchEmails returns the marketing emails whose name or subject matches query,
	// most-recently-updated first. An empty query lists the most recently updated emails.
	//
	// A successful call MUST return a NON-NIL slice, even when nothing matches — return an
	// empty slice, not nil, for the same reason ListAccounts does: the caller cannot
	// otherwise tell "the portal authoritatively has no such email" from an implementation
	// that fell through a branch, and the two mean opposite things to someone staring at an
	// empty picker.
	SearchEmails(ctx context.Context, projectID string, platform model.Provider, query string) ([]model.MarketingEmail, error)
}

// CampaignAdopter is an OPTIONAL dispatcher capability: look a campaign up BY ITS PLATFORM ID,
// so an existing one can be bound to a brief without creating anything. Type-asserted like
// StatusToggler, MetricsReader and AccountLister, so a dispatcher without it yields a clean
// ErrAdoptionUnsupported -> 400. It MUST NOT create, mutate or resurrect anything upstream:
// adoption's safety argument is that the campaign already existed and this service confirmed it.
type CampaignAdopter interface {
	// LookupCampaign reports what the platform holds for platformCampaignID under the
	// project's connection.
	//
	// The (nil, nil) return is MEANINGFUL here — "the platform answered, and no such campaign
	// exists" — unlike MetricsReader and AccountLister, where it is a contract violation. An
	// unverifiable answer must be an ERROR, never a nil ref: a false absence tells an operator
	// "your campaign isn't there" about a campaign sitting on the platform spending money.
	//
	// It MUST resolve the PROJECT's own connection only — never the shared LF system account
	// that every other path falls back to. Under that fallback all such projects share one ad
	// account, so project A could name a campaign project B created there and bind it to its
	// own brief; the account-mismatch guards do not help, because both resolve to the same
	// customer id. A project with no connection of its own must be refused with
	// domain.ErrAdoptionRequiresOwnConnection, which the service maps to an actionable 409.
	//
	// Declining to RESOLVE the fallback, rather than rejecting a value that came from it, is
	// part of the contract: resolution validates and decrypts the LF row before the caller can
	// see where it came from, so consulting it lets that row's defects surface as this call's
	// failure. The service switch has no arm for them, deliberately.
	LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error)
}

// Status-toggle classification sentinels. These distinguish a client/state error (the
// toggle never reached the ad platform) from a real platform-call failure, so the service
// can return an accurate status + message instead of blaming the platform for everything.
// These sentinels are DEFINED in internal/domain (the dependency-free base package) so a
// platform dispatcher can return them directly without importing this orchestration layer;
// they are re-exported here as aliases for the existing service call sites and back-compat.
var (
	// ErrToggleUnsupported: the campaign's platform has no status-toggle capability wired.
	ErrToggleUnsupported = domain.ErrToggleUnsupported
	// ErrCampaignNotProvisioned: the campaign is not fully provisioned for the toggle (no
	// upstream id yet, or a missing child ad group/ad on ACTIVATE). Nothing serviceable to toggle.
	ErrCampaignNotProvisioned = domain.ErrCampaignNotProvisioned
	// ErrMetricsUnsupported: the campaign's platform has no metrics-read capability wired.
	ErrMetricsUnsupported = domain.ErrMetricsUnsupported
	// ErrMetricsWindowUnsupported: this platform IS a MetricsReader but rejects the
	// specific requested window (e.g. X Ads' 7-day cap).
	ErrMetricsWindowUnsupported = domain.ErrMetricsWindowUnsupported
	// ErrCampaignAccountMismatch: the campaign was created under a different ad account
	// than the project's current connection resolves to, so its id cannot be read safely.
	ErrCampaignAccountMismatch = domain.ErrCampaignAccountMismatch

	// ErrAccountsUnsupported: the platform has no account-listing capability wired.
	ErrAccountsUnsupported = domain.ErrAccountsUnsupported

	// ErrEmailSearchUnsupported: the platform has no email-search capability wired.
	ErrEmailSearchUnsupported = domain.ErrEmailSearchUnsupported

	// ErrAdoptionUnsupported: the platform has no campaign-adoption capability wired.
	ErrAdoptionUnsupported = domain.ErrAdoptionUnsupported
	// ErrPlatformCampaignAbsent: the platform answered and holds no such campaign.
	ErrPlatformCampaignAbsent = domain.ErrPlatformCampaignAbsent
	// ErrInvalidPlatformCampaignID: the id cannot name a campaign on that platform.
	ErrInvalidPlatformCampaignID = domain.ErrInvalidPlatformCampaignID
)

// noUpstreamCreator lets a dispatcher signal that a returned error occurred
// BEFORE any upstream (paid) create call — e.g. input validation or config
// errors — so the orchestrator can safely release the claim and allow a retry.
// A plain error (which might follow a timeout that did create a campaign) is
// treated conservatively: the claim is retained to prevent a duplicate.
type noUpstreamCreator interface{ NoUpstreamCreate() bool }

// dispatchErrIsPreCreate reports whether a dispatcher error is known to have
// occurred before any upstream create (safe to release the claim).
func dispatchErrIsPreCreate(err error) bool {
	var n noUpstreamCreator
	return errors.As(err, &n) && n.NoUpstreamCreate()
}

// Orchestrator runs async multi-platform campaign creation for a brief.
// DispatchMetrics records dispatch and job-state observations. It is declared
// HERE, as a narrow interface, rather than importing internal/infrastructure/metrics:
// the orchestrator's dependencies are all interfaces it owns, and a concrete
// import would make every orchestrator test drag in a Prometheus registry.
//
// The implementation must be safe for concurrent use — dispatch runs record from
// one goroutine per platform.
type DispatchMetrics interface {
	RecordDispatch(ctx context.Context, platform model.Provider, outcome string)
	RecordJobTransition(ctx context.Context, status model.JobStatus)
	RecordUpstreamCall(ctx context.Context, platform model.Provider, operation, outcome string, seconds float64)
}

// Dispatch outcome tokens. These MIRROR the constants in
// internal/infrastructure/metrics, which is what bounds the label's cardinality;
// they are duplicated rather than imported to keep this package free of the
// metrics dependency. metrics.safeOutcome collapses anything it does not
// recognise, so a drift here degrades to "unknown" rather than minting series.
const (
	dispatchOutcomeSuccess = "success"
	dispatchOutcomeSkipped = "skipped"
	dispatchOutcomeFailure = "failure"
	dispatchOutcomePanic   = "panic"
)

type Orchestrator struct {
	campaigns   domain.CampaignRepository
	jobs        domain.JobRepository
	dispatchers map[model.Provider]PlatformDispatcher

	// metricsMu guards metrics, which SetMetrics late-binds from the container for
	// the same reason as indexer above. Never nil after construction: defaults to
	// a no-op so every record site can call it unconditionally.
	metricsMu sync.RWMutex
	metrics   DispatchMetrics

	// indexerMu guards indexer, which SetIndexer late-binds from the container.
	// Campaign CREATES land here (dispatchOne persists them), not in BriefService,
	// so without this a new campaign would stay invisible to search until some
	// later update happened to republish it. Never nil: defaults to Noop.
	indexerMu sync.RWMutex
	indexer   indexer.Publisher
	// indexingDisabled is a CONFIGURATION fact (NATS_URL empty), not an observation of the
	// publisher — a Noop also appears when the broker is unreachable. See DisableIndexing.
	indexingDisabled bool

	// wg tracks in-flight dispatch runs so Shutdown can wait for them before the
	// process (and the DB pool) goes away. mu guards the shutting-down flag so a
	// Start racing with Shutdown either registers on wg or is rejected, never
	// launches an untracked goroutine after Shutdown has stopped waiting.
	// rootCtx/rootCancel parent every dispatch run so Shutdown can cancel them if
	// the drain deadline expires (rather than leaving them running against a
	// closing pool).
	mu         sync.Mutex
	wg         sync.WaitGroup
	draining   bool
	rootCtx    context.Context
	rootCancel context.CancelFunc
	// sweeperCtx / sweeperCancel own the background recovery sweeper's lifetime,
	// SEPARATELY from rootCtx (which owns dispatch runs). Cancelled once, guarded
	// by sweeperOnce, at the very START of Shutdown — before the dispatch drain —
	// so:
	//   1. A sweep already blocked in the DB is interrupted PROMPTLY (its query
	//      derives from sweeperCtx, so cancelling it aborts the statement rather
	//      than waiting for it to return on its own).
	//   2. The sweeper's own timeout/cancellation is spent up front and can never
	//      consume any of the dispatch-drain budget — the drain phase (bounded by
	//      drainTimeout) and the sweeper shutdown do not compete for a deadline,
	//      so a maintenance query can't starve healthy in-flight dispatches.
	// The sweeper is still tracked by o.wg, so Shutdown waits for it to return
	// before the caller closes the pool; but because it's cancelled first, that
	// wait is effectively instantaneous and does not eat into the drain window.
	sweeperCtx    context.Context
	sweeperCancel context.CancelFunc
	sweeperOnce   sync.Once
	// sem is a process-wide semaphore bounding concurrent provider dispatches
	// across ALL jobs (a per-job errgroup limit would let N concurrent jobs each
	// get maxParallelDispatch slots, leaving total provider calls unbounded).
	sem chan struct{}
}

// SetIndexer injects the Query Service index publisher. Separate from the constructor
// so existing NewOrchestrator call sites (mostly tests) are unaffected and default to
// Noop, mirroring BriefService.SetIndexer.
func (o *Orchestrator) SetIndexer(p indexer.Publisher) {
	if p == nil {
		return
	}
	o.indexerMu.Lock()
	defer o.indexerMu.Unlock()
	o.indexer = p
}

// Upstream operation tokens. COMPILE-TIME CONSTANTS, never derived from a request,
// a URL or an upstream response — the `operation` label is bounded by this list
// being the only thing passed to recordUpstream.
const (
	opToggleStatus   = "toggle_status"
	opReadMetrics    = "read_metrics"
	opLookupCampaign = "lookup_campaign"
	opListAccounts   = "list_accounts"
	opSearchEmails   = "search_emails"
)

// recordUpstream times one upstream platform call. It is called ONLY after the
// pre-platform guards have passed, so the histogram measures actual network work
// rather than local refusals — a "no dispatcher registered" rejection returns in
// nanoseconds and would drag every latency quantile toward zero.
//
// The outcome is derived from the error's PRESENCE only. The error VALUE never
// becomes a label: upstream errors embed account ids, campaign ids and response
// fragments, all unbounded.
func (o *Orchestrator) recordUpstream(ctx context.Context, platform model.Provider, operation string, start time.Time, err error) {
	outcome := callOutcomeOK
	if err != nil {
		outcome = callOutcomeError
	}
	o.dispatchMetrics().RecordUpstreamCall(ctx, platform, operation, outcome, time.Since(start).Seconds())
}

// Upstream call outcome tokens, mirroring internal/infrastructure/metrics for the
// same reason as the dispatch outcomes above.
const (
	callOutcomeOK    = "ok"
	callOutcomeError = "error"
)

// noopDispatchMetrics is the default recorder, so a call site never needs a nil
// check and an orchestrator built without metrics behaves identically.
type noopDispatchMetrics struct{}

func (noopDispatchMetrics) RecordDispatch(context.Context, model.Provider, string) {}
func (noopDispatchMetrics) RecordJobTransition(context.Context, model.JobStatus)   {}
func (noopDispatchMetrics) RecordUpstreamCall(context.Context, model.Provider, string, string, float64) {
}

// SetMetrics late-binds the metrics recorder from the container. Passing nil
// restores the no-op rather than storing nil, so the record sites stay
// unconditional.
func (o *Orchestrator) SetMetrics(m DispatchMetrics) {
	o.metricsMu.Lock()
	if m == nil {
		m = noopDispatchMetrics{}
	}
	o.metrics = m
	o.metricsMu.Unlock()
}

// dispatchMetrics returns the current recorder under the read lock.
func (o *Orchestrator) dispatchMetrics() DispatchMetrics {
	o.metricsMu.RLock()
	defer o.metricsMu.RUnlock()
	if o.metrics == nil {
		return noopDispatchMetrics{}
	}
	return o.metrics
}

// IndexerIsNoop reports whether this orchestrator would publish nothing. Exported for
// the container's wiring tests — see BriefService.IndexerIsNoop for why this is needed.
func (o *Orchestrator) IndexerIsNoop() bool {
	o.indexerMu.RLock()
	defer o.indexerMu.RUnlock()
	_, isNoop := o.indexer.(indexer.Noop)
	return isNoop
}

// campaignIndexPayload builds the outbox payload for a campaign write, co-committed with the
// row by the repo. Used by EVERY campaign write — create, update and status toggle.
//
// Mixing paths does not work: while creates went through the outbox and updates published
// directly, a replayed create could land AFTER a newer update or toggle and overwrite it in the
// index, leaving search stale until some later write happened to repair it. One ordered sequence
// per row removes that, exactly as for briefs.
//
// It carries NO bearer token. Campaign creation is ASYNC — the dispatch runs on the
// orchestrator's root context, long after the request returned — so a captured JWT could be
// EXPIRED by publish time, and the indexer rejects an expired token. With no outbox row there
// was nothing to retry, so a slow dispatch left a new campaign permanently unsearchable. The
// relay stamps a service credential at publish time instead, which also keeps a live credential
// out of a JSONB table retained for audit with no pruning.
func (o *Orchestrator) campaignIndexPayload(action string) domain.CampaignIndexPayloadFunc {
	// Nothing is enqueued when indexing is DELIBERATELY disabled (NATS_URL=""). Gated on the
	// CONFIG flag, not on the publisher type: a Noop also appears when the broker is merely
	// unreachable, and skipping the enqueue then would permanently lose the write. See
	// BriefService.DisableIndexing.
	if o.indexingIsDisabled() {
		return nil
	}
	return func(c *model.Campaign) ([]byte, error) {
		return json.Marshal(indexer.NewTransaction(
			action, indexer.ObjectTypeCampaign,
			c.ID, c.ProjectID, "",
			campaignDoc(campaignResult(c)), c.CampaignName,
		))
	}
}

// DisableIndexing marks indexing as DELIBERATELY off (NATS_URL empty), so dispatch writes skip
// the outbox. A configuration fact, NOT an observation of the publisher — see
// BriefService.DisableIndexing for why that distinction prevents permanent data loss.
func (o *Orchestrator) DisableIndexing() {
	o.indexerMu.Lock()
	defer o.indexerMu.Unlock()
	o.indexingDisabled = true
}

func (o *Orchestrator) indexingIsDisabled() bool {
	o.indexerMu.RLock()
	defer o.indexerMu.RUnlock()
	return o.indexingDisabled
}

// NewOrchestrator constructs an Orchestrator. dispatchers may be empty; a
// platform with no registered dispatcher is recorded as a failed result.
func NewOrchestrator(campaigns domain.CampaignRepository, jobs domain.JobRepository, dispatchers map[model.Provider]PlatformDispatcher) *Orchestrator {
	if dispatchers == nil {
		dispatchers = map[model.Provider]PlatformDispatcher{}
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	return &Orchestrator{
		campaigns:     campaigns,
		jobs:          jobs,
		dispatchers:   dispatchers,
		indexer:       indexer.Noop{},
		metrics:       noopDispatchMetrics{},
		rootCtx:       rootCtx,
		rootCancel:    rootCancel,
		sweeperCtx:    sweeperCtx,
		sweeperCancel: sweeperCancel,
		sem:           make(chan struct{}, maxParallelDispatch),
	}
}

// recoverySweepInterval is how often the background sweeper re-scans for stuck
// jobs. The startup scan alone can't recover a job orphaned by a crash less than
// staleJobCutoff ago (it's too young to be considered stuck at boot and is never
// re-examined); a periodic sweep eventually catches it. Kept well below
// staleJobCutoff so a newly-stuck job is picked up within roughly one cutoff
// window rather than only on the next restart.
const recoverySweepInterval = 5 * time.Minute

// StartRecoverySweeper launches a background goroutine that periodically fails
// jobs stuck past staleJobCutoff, complementing the one-time startup scan so a
// job orphaned by a crash younger than the cutoff is still eventually recovered.
//
// The goroutine is tracked by wg (so Shutdown waits for it before the pool
// closes) but its lifetime is owned by sweeperCtx, NOT rootCtx: Shutdown cancels
// sweeperCtx first, before draining dispatch runs. Because the in-flight sweep's
// query derives from sweeperCtx, cancelling it interrupts a sweep already
// blocked in the DB promptly, and it does so up front so the sweeper's own
// shutdown never competes with the dispatch-drain budget. Call once after
// construction.
func (o *Orchestrator) StartRecoverySweeper() {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		ticker := time.NewTicker(recoverySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-o.sweeperCtx.Done():
				return
			case <-ticker.C:
				// Bound each sweep so a slow DB can't wedge the goroutine, but derive it
				// from sweeperCtx (do NOT detach) so cancelling sweeperCtx at Shutdown
				// interrupts a sweep already blocked mid-statement rather than letting it
				// run to its own timeout against a closing pool.
				sctx, cancel := context.WithTimeout(o.sweeperCtx, jobFinalizeTimeout)
				n, err := o.jobs.FailStuckJobs(sctx, "job did not complete before a service restart")
				cancel()
				if err != nil {
					// A cancellation here is the expected outcome when Shutdown interrupts
					// an in-flight sweep, not a real failure — don't log it as an error.
					if o.sweeperCtx.Err() == nil {
						slog.ErrorContext(o.sweeperCtx, "periodic stuck-job sweep failed", "error", err)
					}
				} else if n > 0 {
					slog.InfoContext(o.sweeperCtx, "periodic stuck-job sweep recovered jobs", "count", n)
				}
			}
		}
	}()
}

// Shutdown drains in-flight dispatch runs so a graceful shutdown doesn't close
// the database pool out from under a dispatch that already created an upstream
// campaign but hasn't persisted it yet. After Shutdown is called, Start rejects
// new work.
//
// The two phases have SEPARATE budgets so neither starves the other:
//   - Clean drain waits up to drainTimeout for all runs to finish on their own.
//   - If that elapses, in-flight runs are cancelled (via the shared root
//     context) and Shutdown then waits a post-cancel grace for them to observe
//     cancellation and finalize before returning (and the caller closes the
//     pool). That grace is bounded by CancelGracePeriod AND by whatever the
//     outer ctx still allows, whichever is sooner — so the grace timer can
//     never push total shutdown past the budget the caller reserved.
//
// ctx is the overall budget for BOTH phases (drain + grace); the caller sizes it
// as dispatchDrainTimeout + CancelGracePeriod. Passing ctx already limited to
// only drainTimeout would leave no room for the grace and defeat its purpose.
func (o *Orchestrator) Shutdown(ctx context.Context, drainTimeout time.Duration) error {
	o.mu.Lock()
	o.draining = true
	o.mu.Unlock()

	// Cancel the periodic recovery sweeper FIRST, before the dispatch drain. It's
	// maintenance, not in-flight work, so it stops immediately; cancelling its
	// dedicated context also interrupts any sweep currently blocked in the DB
	// (see StartRecoverySweeper). Doing this up front means the sweeper's own
	// shutdown never draws down the dispatch-drain budget, and wg.Wait below then
	// blocks only on real dispatch runs. Safe whether or not the sweeper started.
	o.sweeperOnce.Do(o.sweeperCancel)

	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()

	// Phase 1: clean drain, bounded by drainTimeout and the outer ctx.
	drainCtx, cancelDrain := context.WithTimeout(ctx, drainTimeout)
	defer cancelDrain()
	select {
	case <-done:
		o.rootCancel()
		return nil
	case <-drainCtx.Done():
		if ctx.Err() != nil {
			// The OUTER budget (not just the drain window) is exhausted: cancel and
			// return without a grace wait we have no budget for.
			o.rootCancel()
			return ctx.Err()
		}
	}

	// Phase 2: drain deadline hit but outer budget remains. Cancel in-flight runs
	// and give them a post-cancel grace to unwind before the pool is closed.
	o.rootCancel()
	graceDur := CancelGracePeriod
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < graceDur {
			graceDur = remaining
		}
	}
	if graceDur <= 0 {
		return context.DeadlineExceeded
	}
	grace := time.NewTimer(graceDur)
	defer grace.Stop()
	select {
	case <-done:
		return nil
	case <-grace.C:
		return context.DeadlineExceeded
	case <-ctx.Done():
		// Caller cancelled (not just a deadline we already accounted for): stop
		// waiting immediately rather than blocking out the whole grace window.
		return ctx.Err()
	}
}

// platformResult is the per-platform outcome recorded in the job result.
//
// Skipped marks the single-flight SKIP case: another concurrent dispatch owns
// this (brief, platform) pair, so THIS request did not create the campaign and
// its outcome is owned by that other dispatch. A skip is neither a success nor a
// failure for this job — recording it as a failure would falsely make the
// aggregate job terminal-failed/partial even when the owning dispatch succeeds
// (and GetJob only decodes the stored result; it never re-checks the campaign
// row). aggregateStatus therefore excludes skipped platforms from the failure
// tally, and a job with only skipped platforms terminalizes as succeeded (nothing
// revisits a running job — GetJob only reads it — so leaving it running would
// strand it until the staleness sweeper failed it, turning a correct deferral into
// a spurious failure). Full cross-request reconciliation (this job actively
// adopting the owner's result) is tracked under LFXV2-2665.
type platformResult struct {
	Platform   string `json:"platform"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	CampaignID string `json:"campaign_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Start creates a queued job for the brief and launches dispatch asynchronously,
// returning the job id immediately. The caller polls GetJob for progress.
//
// approvedVersion is the brief version the caller observed as 'approved'. Job
// creation is gated on the brief still being approved at that exact version
// (CreateJobForApprovedBrief), closing the approve→dispatch TOCTOU race: a
// concurrent ReplaceBrief (resets to draft) or ArchiveBrief committing between the
// caller's approval read and job creation bumps the brief's version; the guard
// (a SELECT ... FOR UPDATE re-check inside the job-insert transaction) then fails
// and Start returns domain.ErrStaleApproval (mapped to 409) rather than launching
// paid campaigns from a stale "approved" snapshot.
//
// The dispatch goroutine runs under the orchestrator's root context (not the
// request context), so it survives the request ending but can still be cancelled
// by Shutdown when the drain deadline expires.
// It takes NO caller token. Dispatch results are indexed by co-committing an outbox row (see
// campaignIndexPayload), which the relay publishes with the SERVICE credential — deliberately,
// because by publish time the originating request is long gone and its JWT may have expired.
//
// The ACTOR, unlike the token, IS captured — here, from the request context, because this is
// the last point at which it exists. The decoded value (name, email, principal) has no expiry,
// so none of the reasoning that rules out capturing the JWT applies to it. Every campaign row
// this run writes attributes to that person: they authorized the spend, which is the question
// created_by exists to answer, and it is not the same question as "who was authenticated when
// some later goroutine got around to writing the row".
func (o *Orchestrator) Start(ctx context.Context, brief *model.CampaignBrief, approvedVersion int64, platforms []model.Provider, config json.RawMessage) (string, error) {
	// Register the run with the drain WaitGroup under the lock so a concurrent
	// Shutdown can't start waiting between the draining check and wg.Add (which
	// would let an untracked goroutine outlive Shutdown).
	o.mu.Lock()
	if o.draining {
		o.mu.Unlock()
		return "", &briefs.ConnServiceUnavailableError{Code: "503", Message: "service is shutting down; try again"}
	}
	o.wg.Add(1)
	o.mu.Unlock()

	// Create the job ONLY if the brief is still approved at the version the caller
	// read. This re-verifies approval atomically with job creation, so a concurrent
	// replace/archive that commits in the approve→dispatch window causes this
	// request to FAIL (ErrConflict → 409) instead of dispatching from stale state.
	job, err := o.jobs.CreateJobForApprovedBrief(ctx, brief.ID, approvedVersion)
	if err != nil {
		o.wg.Done()
		return "", err
	}

	// Defensively copy the caller-owned slices before handing them to the async
	// goroutine, so a caller that reuses/mutates its platforms slice or config
	// bytes after Start returns can't race the dispatch run.
	platformsCopy := append([]model.Provider(nil), platforms...)
	configCopy := append(json.RawMessage(nil), config...)

	// Parent the run on the orchestrator's root context (not the request context),
	// so it survives the request ending but can still be cancelled by Shutdown if
	// the drain deadline expires.
	dispatchCtx := o.rootCtx
	// Read the actor from the REQUEST ctx, before it goes out of scope. Below this line
	// the only context in play is o.rootCtx, which carries no actor.
	by := attributedActor(ctx, "dispatch campaign brief")
	go func() {
		defer o.wg.Done()
		o.run(dispatchCtx, job.ID, brief, platformsCopy, configCopy, by)
	}()

	return job.ID, nil
}

// run performs the parallel per-platform dispatch and finalizes the job.
func (o *Orchestrator) run(ctx context.Context, jobID string, brief *model.CampaignBrief, platforms []model.Provider, config json.RawMessage, by *model.Actor) {
	// Mark the job running. Don't abort dispatch on failure (the work should still
	// proceed and the final status write will correct it), but log it — silently
	// dropping this can leave a job stuck at "queued" in the client's view.
	if err := o.jobs.UpdateJobStatus(ctx, jobID, model.JobRunning, nil, ""); err != nil {
		slog.ErrorContext(ctx, "failed to mark campaign job running", "job_id", jobID, "error", err)
	}
	// Recorded after the write is ATTEMPTED rather than only on success: the job is
	// running either way (dispatch proceeds below regardless), so gating this on the
	// write would under-count running jobs during a database blip and make the
	// transition counters disagree with the work actually performed.
	o.dispatchMetrics().RecordJobTransition(ctx, model.JobRunning)

	results := make([]platformResult, len(platforms))
	g, gctx := errgroup.WithContext(ctx)

	for i, p := range platforms {
		i, p := i, p
		g.Go(func() (rerr error) {
			// A single platform failure must not cancel the others, so we never
			// return a non-nil error from the group; failures are recorded.
			res := platformResult{Platform: string(p)}

			// Bound concurrent dispatches process-wide (across all jobs) via the
			// shared semaphore. Honor cancellation so a draining shutdown doesn't
			// block here, and bound the wait so a large backlog can't keep a job
			// queued so long it looks stuck to the recovery sweep (which would then
			// wrongly fail a still-live job). If no slot frees in time, record this
			// platform as failed and let the job finalize promptly.
			queueCtx, cancelQueue := context.WithTimeout(gctx, dispatchQueueTimeout)
			select {
			case o.sem <- struct{}{}:
				cancelQueue()
				defer func() { <-o.sem }()
			case <-queueCtx.Done():
				cancelQueue()
				if gctx.Err() != nil {
					res.Error = "dispatch cancelled"
				} else {
					res.Error = "dispatch queue timed out waiting for a slot"
				}
				results[i] = res
				o.dispatchMetrics().RecordDispatch(gctx, p, dispatchOutcomeFailure)
				return nil
			}

			// dispatched records that dispatchPlatform RETURNED and its result was
			// already stored in results[i]. It is what makes the recover arm below
			// able to tell "the dispatch itself panicked" from "the dispatch
			// succeeded and something after it panicked" — see there for why the
			// distinction decides whether a paid campaign gets reported as failed.
			dispatched := false

			// Recover from a panic in a dispatcher (or future code here): a panic
			// in this detached goroutine would otherwise crash the whole process
			// mid-job. Record it as a platform failure and keep the group intact.
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(gctx, "panic during platform dispatch", "platform", p, "job_id", jobID, "panic", r)
					// Only synthesize a failure when the dispatch had NOT already
					// completed. A panic raised AFTER dispatchPlatform returned (the
					// outcome-recording call below is the only such code today) must
					// not rewrite a stored success: the campaign really was created
					// upstream, and reporting it failed would invite a reconcile or
					// retry that could double-create a PAID campaign. Losing the
					// metric for that platform is the strictly cheaper failure.
					//
					// rerr is cleared on BOTH arms: recovering but returning the panic
					// as the group error would cancel every sibling platform's context,
					// which is the crash this recovery exists to prevent.
					rerr = nil
					if dispatched {
						return
					}
					res.OK = false
					res.Error = "internal error during dispatch"
					results[i] = res
					// A panic gets its OWN outcome rather than folding into "failure":
					// it is a bug in this service, not an upstream platform refusing a
					// campaign, and the two want different responses from whoever is
					// on call. The recovered value is NEVER used as a label — it is
					// unbounded and can embed ids.
					o.dispatchMetrics().RecordDispatch(gctx, p, dispatchOutcomePanic)
					rerr = nil
				}
			}()

			// Deliberately a NEW variable rather than assigning the outer res, which
			// the recover arm rewrites into a failure. Keeping the completed dispatch
			// out of res, together with the `dispatched` flag the recover arm checks,
			// is what stops a panic raised AFTER this point (the recording call below)
			// from turning a created-upstream campaign into a failed result.
			done := o.dispatchPlatform(gctx, jobID, brief, p, config, by)
			results[i] = done
			// Set BEFORE the recording call: it is the only code that can panic after
			// the result is stored, and the flag is what tells the recover arm not to
			// overwrite it.
			dispatched = true
			o.dispatchMetrics().RecordDispatch(gctx, p, dispatchOutcomeFor(done))
			return nil
		})
	}
	// Wait for all dispatches to finish. Each goroutine returns nil and records
	// per-platform failures in results, so g.Wait() is expected to be nil; it is
	// checked only as a defensive guard in case a future change starts returning
	// an error from a Go func (errgroup.Wait returns the first such error).
	if werr := g.Wait(); werr != nil {
		slog.ErrorContext(ctx, "campaign dispatch returned an error", "job_id", jobID, "error", werr)
	}

	// Finalize on a context detached from cancellation: on the drain-timeout path
	// Shutdown cancels this run's ctx, and using it for the terminal write would
	// guarantee the write fails and leave the job stuck non-terminal. A bounded
	// detached context lets the job always reach a terminal state.
	finCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobFinalizeTimeout)
	defer cancel()

	// The TERMINAL transitions below are recorded only on a SUCCESSFUL status write,
	// the opposite of the RUNNING transition above, which is deliberately recorded on
	// attempt. The asymmetry is the point: campaign_job_transitions_total exists so a
	// stuck job shows up as a gap between the running count and the terminal count.
	// A job whose terminal write failed is still `running` in the database and IS the
	// stuck job the alert hunts, so counting its terminal here would close the gap for
	// exactly the rows the metric exists to expose. Such rows are terminalized later by
	// the recovery sweeper (FailStuckJobs), and the gap stays open until they are.
	status := aggregateStatus(results)
	payload, err := json.Marshal(results)
	if err != nil {
		// Don't store a null result (which would make the job unpollable);
		// record the marshal failure in the job's error field and fail the job.
		slog.ErrorContext(finCtx, "failed to marshal job result", "job_id", jobID, "error", err)
		o.terminalize(finCtx, jobID, model.JobFailed, nil, "failed to serialize job result: "+err.Error())
		return
	}
	o.terminalize(finCtx, jobID, status, payload, "")
}

// terminalize writes a job's TERMINAL status and records the transition only if
// that write succeeded. Both finalize paths (the normal one and the marshal
// failure) go through it so the metric can never be recorded for a status that
// did not persist — see the asymmetry note at the finalize site for why the
// terminal transition is guarded where the RUNNING one is not.
func (o *Orchestrator) terminalize(ctx context.Context, jobID string, status model.JobStatus, payload []byte, jobErr string) {
	if err := o.jobs.UpdateJobStatus(ctx, jobID, status, payload, jobErr); err != nil {
		slog.ErrorContext(ctx, "failed to finalize campaign job", "job_id", jobID, "error", err)
		return
	}
	o.dispatchMetrics().RecordJobTransition(ctx, status)
}

// dispatchOutcomeFor maps a platform result onto the CLOSED outcome enum. Skipped
// is checked before OK because a skipped platform is reported with OK=true (no
// campaign was created, so counting it as a success would inflate the dispatch
// success rate with work never attempted).
func dispatchOutcomeFor(res platformResult) string {
	switch {
	case res.Skipped:
		return dispatchOutcomeSkipped
	case res.OK:
		return dispatchOutcomeSuccess
	default:
		return dispatchOutcomeFailure
	}
}

// dispatchPlatform creates (or reuses) the campaign for a single platform.
// Single-flight is enforced by an atomic claim row (ClaimCampaignDispatch:
// INSERT ... ON CONFLICT (brief_id, platform) DO NOTHING) — no held connection,
// no blocking lock — so two concurrent create-campaigns for the same pair cannot
// both create an upstream campaign: exactly one wins the claim (the unique index
// arbitrates); the other reuses the existing row or, if it's still pending, is
// reported in-progress. campaign_id is always the upstream platform id, so the
// field means the same on the reuse and create paths.

// googleAdsChannelSearchName is the `channel` value that means the default Search
// campaign. Duplicated from internal/dispatch rather than imported: dispatch imports
// this package, so the dependency cannot run the other way.
const googleAdsChannelSearchName = "search"

// googleAdsChannelDemandGenName mirrors internal/dispatch's googleAdsChannelDemandGen for
// the same reason googleAdsChannelSearchName mirrors its sibling: dispatch imports this
// package, so the dependency cannot run the other way.
const googleAdsChannelDemandGenName = "demand-gen"

// variantForDispatch reads WHICH of a platform's campaign types this dispatch is for,
// out of the same config envelope the dispatcher will read.
//
// It exists because (brief, platform) is not a unique slot for Google: its UI offers
// Search and Demand Gen as simultaneous checkboxes (Performance Max next), so one
// brief legitimately holds several google-ads campaigns. Without this, the
// idempotency fast path below matched the FIRST google-ads campaign for the brief and
// reported a Demand Gen dispatch as already done — returning the Search campaign's id
// without ever calling Google. Observed 2026-08-13.
//
// Every other provider returns VariantDefault: Meta's and Reddit's `objective`
// configures a single campaign rather than multiplying it, and the rest have no such
// concept. Their slot key is unchanged in behaviour, just spelled with a third column.
//
// A MALFORMED or unknown value resolves to VariantInvalid, which dispatchPlatform
// refuses BEFORE the idempotency lookup and the claim — so it writes no row and cannot
// collide with a real campaign's slot. Defaulting a typo would spend one channel's
// budget on another, and routing it through a shared slot let two concurrent malformed
// requests collide into a false success (see the refusal in dispatchPlatform).
func variantForDispatch(p model.Provider, config json.RawMessage) string {
	if p != model.ProviderGoogleAds || len(config) == 0 {
		return model.VariantDefault
	}
	var envelope struct {
		GoogleAds struct {
			Channel string `json:"channel"`
		} `json:"googleAdsConfig"`
	}
	if err := json.Unmarshal(config, &envelope); err != nil {
		// Undecodable config must NOT claim the default slot. The intent was to let the
		// dispatcher report the specific decode error, but the idempotency lookup runs FIRST:
		// when a Search/default campaign already exists for this brief, the malformed request
		// matched that row, was reported as a reused success, and the dispatcher never ran —
		// so the error it was supposed to surface never surfaced, and the caller was told a
		// campaign it never validly asked for had been created.
		//
		// VariantInvalid is a slot no create ever writes, so the lookup always misses and the
		// dispatch proceeds to the decode error. It cannot collide with a real campaign.
		return model.VariantInvalid
	}
	ch := strings.ToLower(strings.TrimSpace(envelope.GoogleAds.Channel))
	// Explicit "search" and an ABSENT channel dispatch the identical Search campaign, so
	// they must share a slot. They did not: absence normalized to 'default' (what every
	// pre-000021 row was backfilled to) while "search" claimed a slot of its own — so the
	// updated UI, which now names the channel explicitly, would miss the existing row for
	// a brief created before it and create a SECOND paid Search campaign.
	//
	// Collapsing to 'default' rather than migrating the old rows to 'search' keeps the
	// backfill honest: 'default' means "this platform's only campaign", which is exactly
	// what those rows are.
	if ch == googleAdsChannelSearchName {
		return model.VariantDefault
	}
	// An UNSUPPORTED channel must not land on a real slot. `NormalizeVariant` is a
	// pass-through for any non-empty value, so a caller sending `channel:"default"` — valid
	// JSON, not an accepted Google channel — resolved to the Search slot: the idempotency
	// fast path then found that brief's existing Search campaign and returned its id as a
	// SUCCESS, so the dispatcher never ran and never rejected the unsupported channel. The
	// caller was told a campaign it never validly asked for had been created.
	//
	// This is the same shape as the undecodable-config case above and takes the same answer:
	// a slot no create path writes, so the lookup always misses and the dispatch proceeds to
	// the dispatcher's own "unsupported channel" error. Validating HERE instead would
	// duplicate the dispatcher's channel list in a second place and let the two drift.
	if !googleAdsChannelIsSupported(ch) {
		return model.VariantInvalid
	}
	return model.NormalizeVariant(ch)
}

// googleAdsChannelIsSupported reports whether a channel string names a Google Ads campaign
// type this service creates. It deliberately mirrors — rather than re-derives — the
// dispatcher's switch: the dispatcher owns the decision and produces the caller-facing
// error, and this exists only so an unsupported value cannot be filed under a slot a real
// campaign occupies before that error is reached.
func googleAdsChannelIsSupported(ch string) bool {
	// An ABSENT channel is supported and means Search — that is what every brief created
	// before the channel field existed sends, and the dispatcher's own switch has the same
	// empty-string arm. Treating it as unsupported would file every one of those on
	// VariantInvalid and refuse the platform's most common create.
	return ch == "" || ch == googleAdsChannelSearchName || ch == googleAdsChannelDemandGenName
}

func (o *Orchestrator) dispatchPlatform(ctx context.Context, jobID string, brief *model.CampaignBrief, p model.Provider, config json.RawMessage, by *model.Actor) platformResult {
	res := platformResult{Platform: string(p)}

	// Fast path: if this pair already has a completed campaign (upstream id set),
	// reuse it — idempotent, and valid even if no dispatcher is registered for the
	// platform anymore. A real DB error here (anything other than "no such row") must
	// NOT be swallowed as "no existing campaign": proceeding to claim/dispatch when an
	// existing campaign simply couldn't be loaded risks a duplicate upstream create.
	// Only ErrNotFound is a clean "nothing yet, proceed".
	variant := variantForDispatch(p, config)
	// An invalid variant is refused HERE, before the lookup and the claim, rather than
	// being routed through a shared VariantInvalid slot.
	//
	// Routing it through the slot was wrong even though no CREATE writes that slot: the
	// CLAIM does. ClaimCampaignDispatch inserts a pending row keyed on the variant, so two
	// concurrent malformed requests both derive VariantInvalid, one wins the claim and the
	// other is marked Skipped — and aggregateStatus excludes skipped platforms from the
	// failure tally, so a wholly-skipped job terminalizes as SUCCEEDED. The caller sends
	// invalid config and is told it worked. A stranded _invalid claim (see the KNOWN
	// RESIDUAL GAP note on the skip path) makes that permanent rather than a one-off race.
	//
	// Refusing before any row is written removes the shared slot entirely, so there is
	// nothing to collide over and the caller gets the decode error it should have had.
	if variant == model.VariantInvalid {
		// VariantInvalid has TWO causes and they are not the same problem for the caller:
		// a config that did not decode at all, and one that decoded but named a channel
		// this service does not support. Reporting both as "unsupported channel" sends
		// someone hunting a channel value in a payload that never parsed. `config` is
		// still in hand here, so the cause is re-derived rather than threaded through a
		// second sentinel.
		var probe map[string]json.RawMessage
		if jerr := json.Unmarshal(config, &probe); jerr != nil {
			res.Error = fmt.Sprintf("invalid platform config for %s: config could not be decoded", p)
		} else {
			res.Error = fmt.Sprintf("invalid platform config for %s: unsupported channel", p)
		}
		return res
	}
	existing, lerr := o.campaigns.GetCampaignByPlatform(ctx, brief.ProjectID, brief.ID, p, variant)
	switch {
	case lerr == nil && isReusableCampaign(existing):
		// A COMPLETED campaign (created / created_degraded — both terminal, since a
		// re-dispatch can't repair a degraded sub-step). Reuse it idempotently. A
		// retained partial orphan (group_created/unconfirmed, or an id-less row) is NOT
		// terminal, so it falls through to the claim/reconcile path below.
		res.OK = true
		res.CampaignID = existing.PlatformCampaignID
		return res
	case lerr == nil:
		// A row exists but is not a completed campaign — either it has no upstream id
		// yet (a prior pending/failed attempt) OR it is a retained partial orphan
		// (pending status WITH an upstream id, recorded for reconciliation after a
		// mid-flow failure). In both cases fall through to the claim path, which
		// reconciles it rather than falsely reporting the pending orphan as a success.
	case errors.Is(lerr, domain.ErrNotFound):
		// No campaign for this pair yet — fall through to claim/dispatch.
	default:
		// A transient/real DB failure — surface it as a platform failure rather than
		// dispatching blind (which could duplicate an existing-but-unloaded campaign).
		slog.ErrorContext(ctx, "idempotency lookup failed", "platform", p, "job_id", jobID, "error", lerr)
		res.Error = "could not check for an existing campaign"
		return res
	}

	// Resolve the dispatcher BEFORE claiming: a "no dispatcher" outcome must not
	// leave a permanent pending claim that blocks the pair forever (which, with
	// the currently-empty dispatcher map, would happen on every request).
	d, ok := o.dispatchers[p]
	if !ok {
		res.Error = "no dispatcher registered for platform"
		return res
	}

	// Single-flight claim: atomically insert a 'pending' placeholder for (brief,
	// platform). Exactly one worker across all replicas wins (the unique index
	// arbitrates) — no held connection, no blocking lock.
	claimed, existing, err := o.campaigns.ClaimCampaignDispatch(ctx, brief.ProjectID, brief.ID, p, variant, jobID, by)
	if err != nil {
		slog.ErrorContext(ctx, "claim dispatch failed", "platform", p, "job_id", jobID, "error", err)
		res.Error = "could not claim campaign dispatch"
		return res
	}
	if !claimed {
		// Another worker owns (or already completed) this pair.
		if isReusableCampaign(existing) {
			// Already created upstream AND completed (created / created_degraded) —
			// reuse it (idempotent). A non-terminal row WITH an id (a retained partial
			// orphan) is not a success, so it falls through to the skip path below rather
			// than being reported as a completed campaign.
			res.OK = true
			res.CampaignID = existing.PlatformCampaignID
			return res
		}
		// A retained partial ORPHAN is distinguishable from a claim held by a still-
		// running worker: a prior dispatch failed mid-flow after (maybe) creating the
		// upstream campaign, and NOTHING will revisit it. It presents in two shapes, both
		// of which must be caught so aggregateStatus can't mark an all-skipped retry job
		// `succeeded` and hide the orphan:
		//   1. a non-empty PlatformCampaignID (the created upstream id was recorded), or
		//   2. an id-less partial that still carries a Result reconcile blob (the
		//      ambiguous-create / group-orphan case — the id is empty by design but the
		//      row was persisted WITH Result so it's reconcilable; see the retain branch
		//      persist condition, which keeps such a row when len(Result) > 0).
		// Either shape is reported as a FAILURE (reconciliation required), NOT a skip.
		// (Automatic name-based reconciliation is tracked in LFXV2-2665.)
		if existing != nil && (existing.PlatformCampaignID != "" || len(existing.Result) > 0) {
			slog.ErrorContext(ctx, "retained partial orphan found on retry; reconciliation required",
				"platform", p, "job_id", jobID, "platform_campaign_id", existing.PlatformCampaignID, "has_result", len(existing.Result) > 0)
			res.CampaignID = existing.PlatformCampaignID
			res.Error = "a prior dispatch left an incomplete campaign upstream; reconciliation required"
			return res
		}
		// A BARE pending claim (no upstream id AND no Result blob): typically a claim
		// held by another still-running worker, so we treat it as SKIPPED rather than
		// re-dispatching (the point of the claim). Recording a skip as a failure would
		// falsely drive THIS job to terminal failed/partial even when the owner succeeds
		// (GetJob only decodes the stored result and never re-checks the campaign row, so
		// the false failure would be permanent). aggregateStatus excludes skipped
		// platforms from the failure tally and returns succeeded for a wholly-skipped job.
		//
		// KNOWN RESIDUAL GAP: a bare pending claim can ALSO be a terminally-stranded row
		// — e.g. a dispatcher that returned (nil, err) after possibly creating a campaign
		// but with no reconcile detail, a failed partial persist, or a panic after the
		// claim. Those are indistinguishable HERE from a live owner without a claim
		// lease/timestamp, so such a stranded claim can still let an all-skipped retry
		// report succeeded. Disambiguating an active owner from a terminal/unknown claim
		// requires the claim-lease (claimed_at) reconciliation work tracked in LFXV2-2665;
		// this PR closes the id-carrying and Result-carrying cases, which are the ones
		// that persist a reconcilable signal.
		res.Skipped = true
		res.Error = "skipped: another concurrent dispatch owns this platform"
		return res
	}

	// We own the claim (a 'pending' row now exists). If we fail BEFORE the
	// upstream campaign is created, release the pending claim so the pair isn't
	// blocked and can be retried. Once the upstream campaign exists, we do NOT
	// release (the row is the record of the created campaign / recoverable orphan).
	releaseClaim := func() {
		// Use a fresh bounded context, not the dispatch ctx: on shutdown/timeout the
		// dispatch ctx is already cancelled, and reusing it would make the cleanup
		// DELETE fail and leak the pending claim exactly when we most need to free it.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimReleaseTimeout)
		defer cancel()
		if derr := o.campaigns.DeleteDispatchClaim(rctx, brief.ID, p, variant); derr != nil {
			slog.ErrorContext(rctx, "failed to release pending dispatch claim", "platform", p, "job_id", jobID, "error", derr)
		}
	}

	// Bound the provider call so a hung upstream can't hold this job "running" and
	// its semaphore slot indefinitely. Derived from the dispatch ctx so a shutdown
	// cancel still propagates, but with its own ceiling.
	callCtx, cancelCall := context.WithTimeout(ctx, providerCallTimeout)
	defer cancelCall()
	campaign, derr := d.Dispatch(callCtx, brief, p, config)
	if derr != nil {
		// A dispatch error usually does NOT prove the provider rejected the create —
		// a timeout or dropped connection can leave a campaign created upstream — so
		// by default we RETAIN the pending claim to block a blind retry from
		// double-creating. The exception: a dispatcher can signal (via
		// NoUpstreamCreate) that the error occurred before any create call (e.g.
		// input/config validation), in which case releasing the claim to allow a
		// retry is safe.
		//
		// `campaign == nil` is an INDEPENDENT precondition on that release, not a
		// restatement of the error check. NoUpstreamCreate is a claim the DISPATCHER
		// makes about its own error; a non-nil campaign is evidence from the same
		// return that something was built for an upstream resource. When the two
		// disagree, the evidence wins and the claim is RETAINED — releasing it would
		// free the (brief, platform) slot and authorize a duplicate PAID create,
		// which is the one failure this claim exists to prevent.
		//
		// The condition keys on `campaign == nil` ALONE, deliberately not on
		// campaign.PlatformCampaignID != "". An id-less partial is exactly the
		// group-orphan / ambiguous-create shape the retain branch below persists via
		// its `len(campaign.Result) > 0` arm: the id is empty BY DESIGN while a real
		// upstream resource exists. Testing the id would route those straight back
		// into the release path and reintroduce the double-create.
		//
		// No dispatcher returns this pairing today (every preCreateError is returned
		// with a nil campaign), so this is a guard against a future adapter, not a
		// live bug. It is cheap, and the failure it prevents costs real money.
		if dispatchErrIsPreCreate(derr) && campaign == nil {
			// The reason token is carried on THIS branch only, and only for the errors it
			// has a vocabulary for. Pre-create is where the connection faults land — a
			// resolve that could not produce a usable client never reached the provider —
			// but it is NOT only connection faults: dispatchErrIsPreCreate keys on
			// NoUpstreamCreate, which a malformed platform config, a brief-validation
			// failure and a connection-repository error all set too. unusableConnectionReason
			// is defined over ErrConnectionNotUsable chains, so those would every one log
			// "unclassified".
			//
			// Hence the errors.Is gate rather than an unconditional attribute. A field that
			// is constant is not a field, and one that is constant only where nothing can be
			// learned is worse, because it reads like a classification was attempted and
			// came back empty — which is exactly what a reason-based alert would then be
			// filtering on. Omitting the attribute says "not classified here"; emitting
			// "unclassified" says "classified, and the answer is nothing". The retained
			// branches carry no reason for the same reason: there the error describes the
			// provider, not the connection.
			//
			// Without this the token reached NOTHING. res.Error below collapses every
			// dispatcher error to one string, and this line logged only err.Error() — so the
			// classification that several dispatchers and the Meta credentials-only
			// bootstrap all cite as the operator's signal existed solely in prose that
			// promised it. A structured reason is what makes "the operator learns
			// account_not_selected" a true statement about an ASYNC dispatch, which is the
			// only path on which a campaign is ever created.
			// The reason arm logs the token INSTEAD of the cause, not alongside it, and
			// that substitution is the whole point of the vocabulary existing. Two
			// conditions in it are detected by decoding the DECRYPTED credential blob
			// (credentials_undecodable, credentials_incomplete), and an encoding/json
			// error quotes its input — so rendering derr here would put decrypted
			// credential material into the log on a path an operator is expected to read.
			// internal/dispatch/creds.go says so at the point the 400 is built ("it logs a
			// fixed reason token and nothing else ... Do not 'restore' logging of the cause
			// on the 400 path"), and unusableConnectionReason's own doc comment gives the
			// same reason for existing at all. The retained-claim branches below and the
			// else arm here keep the cause because those errors describe the PROVIDER's
			// response, which never passed through the credential blob.
			//
			// The system arm sits ABOVE the broad one, for the reason brief.go:577
			// states at the synchronous split: systemScoped WRAPS rather than
			// replaces, so errors.Is still reports ErrConnectionNotUsable and a broad
			// match would win. Collapsing the two loses the only thing that says WHO
			// can fix it — a broken LF fallback row would read exactly like one
			// project's misconfigured connection, when domain.ErrSystemConnectionNotUsable
			// is defined as the operator's page and means every project without its own
			// connection is failing. Suppressing the cause is what the arm is for;
			// suppressing the scope was not.
			//
			// The scope is carried by the MESSAGE rather than by a new attribute
			// because that is how both synchronous splits already carry it
			// (brief.go:577, brief.go:914, connection.go:276) and this line pages the
			// same operator with the same distinction.
			//
			// project_id is on every arm because `run` is parented on o.rootCtx
			// (Start, above: "the only context in play is o.rootCtx"), so these
			// records inherit NO request-scoped fields — unlike the synchronous
			// counterpart at connection.go:312, which gets its project from the
			// request. Without it, `reason=account_not_selected` names a defect
			// but not the connection to repair, and an operator has to resolve the
			// job id against the database to act. The slug is safe to log: it is a
			// URL path segment, not credential-derived, and the synchronous split
			// already logs it beside the same reason token.
			const preCreateMsg = "platform dispatch failed before upstream create (claim released)"
			switch {
			case errors.Is(derr, domain.ErrSystemConnectionNotUsable):
				slog.ErrorContext(ctx, "the LF system connection is not usable; platform campaign creation is failing for every project without its own connection (claim released)",
					"platform", p, "job_id", jobID, "project_id", brief.ProjectID,
					"reason", unusableConnectionReason(derr))
			case errors.Is(derr, domain.ErrConnectionNotUsable):
				slog.ErrorContext(ctx, preCreateMsg,
					"platform", p, "job_id", jobID, "project_id", brief.ProjectID,
					"reason", unusableConnectionReason(derr))
			default:
				slog.ErrorContext(ctx, preCreateMsg,
					"platform", p, "job_id", jobID, "project_id", brief.ProjectID, "error", derr)
			}
			releaseClaim()
		} else {
			// The claim is RETAINED (outcome unknown, blind retry could double-create).
			// The platform clients' partial-result contract lets Dispatch return a
			// non-nil campaign carrying the created upstream id ALONGSIDE the error
			// (the campaign POST succeeded but a later step failed) OR an id-less
			// partial that still carries a Result reconcile blob (an ambiguous create /
			// group-orphan, where the id is empty by design but Result holds the
			// reconcile detail — e.g. a campaign name for a name-based lookup). In BOTH
			// cases, persist the row so the orphan is RECONCILABLE later instead of
			// leaving an anonymous claim that a retry can't distinguish from a live
			// concurrent dispatch. Persist on a context DETACHED from the dispatch ctx
			// (mirroring the success path) so a shutdown-cancelled dispatch ctx can't
			// drop the record of a paid campaign that actually exists.
			if campaign != nil && (campaign.PlatformCampaignID != "" || len(campaign.Result) > 0) {
				campaign.JobID = &jobID
				// Stamp the SAME variant the claim used. A dispatcher does not know which
				// slot it was claimed for, so without this the upsert writes 'default' via
				// NormalizeVariant while the claim holds 'demand-gen' — the conflict target
				// then misses the claimed row and INSERTs a second one.
				campaign.Variant = variant
				campaign.BriefID = brief.ID
				campaign.ProjectID = brief.ProjectID
				campaign.Platform = p
				// Both, not just UpdatedBy. Reaching here means we OWN the claim, so a
				// live 'pending' row for this (brief, platform) existed a moment ago and
				// this upsert normally takes the conflict arm. That covers the
				// re-dispatch-after-delete case too, which is NOT an INSERT-arm case:
				// DeleteCampaign refuses a 'pending' row outright, so only a settled row
				// can be soft-deleted, and the ClaimCampaignDispatch above then inserts a
				// FRESH live row (the deleted one sits outside the partial unique index) —
				// which is the statement that stamps the new campaign's created_by, and
				// this upsert conflicts with it.
				//
				// CreatedBy is set for the one case that does reach the INSERT arm: the
				// claim row disappearing between that call and this one — an operator
				// clearing what looked like a stuck claim (see StuckDispatchClaims), or a
				// concurrent DeleteDispatchClaim. On the conflict arm the query leaves the
				// column alone, so passing it costs nothing; omitting it would leave a
				// campaign with no recorded author on the one path that creates a row
				// without a live claim in front of it.
				campaign.CreatedBy = by
				campaign.UpdatedBy = by
				// Decide the persisted status. Preserve a dispatcher-set status that
				// carries real meaning; otherwise flatten to 'pending' so the row can't read
				// as complete. Two kinds of status are preserved:
				//   - a partial-orphan status (group_created / unconfirmed): the row surfaces
				//     WHAT went wrong and stays NON-reusable (isReusableCampaign excludes it),
				//     so a retry reconciles rather than false-succeeding.
				//   - a terminal status WITH a real upstream id (e.g. created_degraded, which
				//     LinkedIn returns alongside an error when the campaign WAS created but
				//     fewer creatives than requested landed): the campaign genuinely exists
				//     and a re-dispatch can't repair the degraded sub-step, so it is preserved
				//     AND remains reusable (isReusableCampaign accepts it) — flattening it to
				//     'pending' would hide a real paid campaign and force a needless retry.
				// Anything outside the closed preservableErrorStatuses allowlist — empty, or a
				// plain success-looking status like "active" returned when a later step
				// failed — is flattened, so an incomplete campaign can't read as complete.
				if !preservableErrorStatuses[campaign.Status] {
					campaign.Status = campaignStatusPending
				}
				persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
				// Index the RETAINED PARTIAL too: it is a real row an operator must be able to
				// find in order to reconcile it. Indexing only clean successes would hide
				// exactly the campaigns that need attention.
				if _, perr := o.campaigns.UpsertCampaign(persistCtx, campaign, o.campaignIndexPayload(indexer.ActionCreated)); perr != nil {
					slog.ErrorContext(ctx, "failed to record partial upstream campaign on retained pending claim",
						"platform", p, "job_id", jobID, "platform_campaign_id", campaign.PlatformCampaignID, "has_result", len(campaign.Result) > 0, "error", perr)
				} else {
					slog.ErrorContext(ctx, "platform dispatch failed after (possible) upstream create; recorded orphan on retained pending claim",
						"platform", p, "job_id", jobID, "platform_campaign_id", campaign.PlatformCampaignID, "has_result", len(campaign.Result) > 0, "error", derr)
				}
				cancelPersist()
			} else {
				slog.ErrorContext(ctx, "platform dispatch failed (claim retained; outcome unknown)", "platform", p, "job_id", jobID, "error", derr)
			}
		}
		res.Error = "platform campaign creation failed"
		return res
	}
	if campaign == nil {
		// A (nil, nil) result is ambiguous: it does NOT prove no upstream campaign
		// was created (a dispatcher could create the campaign, then fail to build
		// its return value). Treat it like the ambiguous error path — RETAIN the
		// claim so a blind retry can't double-create; the pending row flags the
		// pair for reconciliation.
		slog.ErrorContext(ctx, "dispatcher returned no campaign (claim retained; outcome unknown)", "platform", p, "job_id", jobID)
		res.Error = "dispatcher returned no campaign"
		return res
	}
	if campaign.PlatformCampaignID == "" {
		// An empty upstream id is likewise ambiguous (the create may have happened
		// but the id wasn't captured), so RETAIN the claim rather than releasing it
		// and risking a duplicate on retry.
		slog.ErrorContext(ctx, "dispatcher returned no upstream campaign id (claim retained; outcome unknown)", "platform", p, "job_id", jobID)
		res.Error = "dispatcher returned no upstream campaign id"
		return res
	}
	// Stamp ownership, then update the claimed row in place (Upsert on the same
	// (brief, platform, variant) fills in the real upstream id and status).
	campaign.JobID = &jobID
	// The SAME variant the claim used — see the retained-partial path above.
	campaign.Variant = variant
	campaign.BriefID = brief.ID
	campaign.ProjectID = brief.ProjectID
	campaign.Platform = p
	campaign.CreatedBy = by
	campaign.UpdatedBy = by
	// Persist the successful result on a context DETACHED from the dispatch ctx.
	// The upstream (paid) campaign now EXISTS; on the phase-two shutdown path
	// rootCancel has already cancelled the dispatch ctx, and reusing it here would
	// make pgx reject the upsert immediately — losing the record of a campaign that
	// was actually created (an unreconcilable orphan) even though Shutdown is still
	// inside its grace window. A bounded detached context (persistResultTimeout,
	// sized to fit within CancelGracePeriod) lets the persist complete during grace
	// while still bounding it so it can never hang shutdown.
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancelPersist()
	if _, uerr := o.campaigns.UpsertCampaign(persistCtx, campaign, o.campaignIndexPayload(indexer.ActionCreated)); uerr != nil {
		// The upstream (paid) campaign was created but recording it failed. The
		// 'pending' claim row remains, so this is recoverable/reconcilable out of
		// band and a duplicate can't be created behind the claim. Log the raw error
		// and the orphaned upstream id; keep the id in the result.
		slog.ErrorContext(ctx, "persist campaign failed after upstream create — pending claim retained",
			"platform", p, "job_id", jobID, "platform_campaign_id", campaign.PlatformCampaignID, "error", uerr)
		res.Error = "created upstream campaign but failed to record it; see logs"
		res.CampaignID = campaign.PlatformCampaignID
		return res
	}
	// The index message co-committed with the row (see campaignIndexPayload), so the campaign
	// is searchable once the relay delivers it — no dependency on this goroutine's token still
	// being valid, and no lost message if the process dies here.

	res.OK = true
	res.CampaignID = campaign.PlatformCampaignID
	return res
}

// aggregateStatus folds per-platform outcomes into the job's status.
//
// Skipped platforms (single-flight SKIP: another dispatch owns the pair) are NOT
// counted as failures — a skip is a deferral to that owner, not this job's outcome
// and not this job's work to finish. If a platform was skipped and none of THIS
// job's own dispatches failed, the job terminalizes as SUCCEEDED: it is neither a
// failure (spurious) nor left non-terminal (a stuck `running` job is never
// revisited — GetJob only reads it and the recovery sweeper would eventually fail
// it after staleJobCutoff). The per-platform result carries Skipped=true so a
// caller can see which platforms a concurrent request handled; full cross-request
// adoption of the owner's result is tracked under LFXV2-2665.
func aggregateStatus(results []platformResult) model.JobStatus {
	var ok, failed, skipped int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.OK:
			ok++
		default:
			failed++
		}
	}
	switch {
	case failed > 0 && ok == 0 && skipped == 0:
		// Every platform this job actually dispatched failed.
		return model.JobFailed
	case failed > 0:
		// A real mix of success and failure (any skips remain pending but at least
		// one platform definitively failed — surface that as partial rather than
		// hiding it behind a still-running status).
		return model.JobPartial
	case skipped > 0:
		// No failures, and every platform this job actually dispatched succeeded; the
		// remaining pair(s) were SKIPPED because a concurrent dispatch already owns
		// the (brief, platform) claim and is creating them. A skip is a deferral to
		// that owner, NOT this job's failure and NOT this job's work to finish — the
		// owner's own job records the real per-platform result. So this job is
		// TERMINAL: return succeeded rather than staying `running` forever (nothing
		// revisits a running job — GetJob only reads it and the recovery sweeper would
		// eventually fail it after staleJobCutoff, turning a correct deferral into a
		// spurious failure). The per-platform result already carries Skipped=true so a
		// caller can see which platforms were handled by a concurrent request; full
		// cross-request adoption of the owner's outcome is tracked under LFXV2-2665.
		return model.JobSucceeded
	default:
		return model.JobSucceeded
	}
}

// ToggleCampaignStatus pauses or resumes an already-created campaign on its ad platform.
// It looks up the campaign's dispatcher, requires that dispatcher to implement
// StatusToggler (else ErrToggleUnsupported), and delegates the platform call. The caller
// (the service) updates the persisted row only after this returns nil. platformCampaignID
// is the campaign's stored upstream id; status is model.CampaignRunActive/Paused.
func (o *Orchestrator) ToggleCampaignStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	// Pre-platform guards return classifiable sentinels: these NEVER contact the ad
	// platform, so the caller must NOT report them as a platform failure.
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return ErrCampaignNotProvisioned
	}
	d, ok := o.dispatchers[platform]
	if !ok {
		// No dispatcher registered is, from the caller's view, the same as "toggle not
		// supported for this platform" — neither reaches the platform.
		return fmt.Errorf("%w: no dispatcher registered for platform %s", ErrToggleUnsupported, platform)
	}
	toggler, ok := d.(StatusToggler)
	if !ok {
		return fmt.Errorf("%w: %s", ErrToggleUnsupported, platform)
	}
	// Any error from here is from the platform call itself, from the dispatcher's own
	// pre-flight cred resolution, or from its CONNECTION-STATE checks (connection not ACTIVE,
	// undecodable or incomplete credentials, missing account id) — the ad platform is never
	// contacted for the last group.
	//
	// The classification of that last group is uniform across dispatchers as of LFXV2-3196;
	// the paragraph further down names each adapter and its helper. Google Ads, Reddit,
	// X/Twitter and Microsoft Ads were the first four to tag the preflight failures REACHABLE
	// HERE with domain.ErrConnectionNotUsable plus a reason sentinel, and Meta then LinkedIn
	// followed. Google Ads tags every one of its
	// CONNECTION-STATE checks (internal/dispatch/googleads.go): the three in
	// validateGoogleAdsCredentials, the missing-account guard in validateGoogleAdsConnection —
	// both of which resolveGoogleAdsClient runs — and the stored-login_customer_id check in
	// validatedLoginCustomerID; all five run on this path. The other three adapters tag the
	// same four defects in each one's shared resolve/validate helper (LFXV2-3069 part 1). The
	// caller maps them to 409 — correct, because none of them improves with time.
	//
	// The middle group — cred RESOLUTION, credsSource.resolve — is deliberately not covered by
	// that statement, on Google Ads or anywhere else. Three of its returns carry no
	// ErrConnectionNotUsable at all (internal/dispatch/creds.go): a missing connection row keeps
	// domain.ErrNotFound, a repository failure keeps only the wrapped repo error, and a GCM
	// AUTHENTICATION failure carries domain.ErrCredentialDecryptionFailed. Only the
	// row-is-provably-bad returns (no stored credentials, ErrCredentialsMalformed) are tagged.
	//
	// What the TOGGLE CALLER does with those three is no longer uniform, as of LFXV2-3065.
	// ToggleCampaignStatus's switch (internal/service/brief.go) now matches two of them:
	// ErrNotFound answers 404 (no connection row exists and the system fallback did not cover
	// it — nothing to repair, so the caller is told to connect) and ErrCredentialDecryptionFailed
	// answers 500 (the application's key no longer matches the stored blob, which is an
	// operator's repair and not the caller's). GetCampaignMetrics classifies the same two
	// identically, since both resolve through this same credsSource.
	//
	// Only the BARE REPOSITORY ERROR still lands in default and returns 503, and that one is
	// correct there: a DB blip genuinely is transient, which is the answer the other two no
	// longer share.
	//
	// The rough edge this paragraph used to describe — "permanent defects answered 503, which
	// invites a retry that cannot succeed" — is the defect LFXV2-3065 closed.
	//
	// The manager-id check is the newest of the five and was for a while NOT reachable here:
	// it sat inline in the discovery resolver, so a malformed stored value reached this path
	// unclassified and fell through to 503. LFXV2-3052 hoisted it into a helper both resolvers
	// call, which is why the list above is once again the whole list rather than a subset.
	//
	// Every toggle-capable adapter now tags: Google Ads, Microsoft and X through their
	// validate<Provider>Connection helpers, Reddit inline in resolveRedditClient, Meta in
	// resolveMetaCredentials (internal/dispatch/meta.go), and LinkedIn in
	// resolveLinkedInCredentials (internal/dispatch/linkedin.go).
	//
	// LinkedIn was the last one, and closing it (LFXV2-3196, the remainder of LFXV2-3069
	// part 2) was an extraction rather than an annotation: ToggleStatus validated the
	// connection inline across four checks (inactive status, credential decode, incomplete
	// credentials, missing account id) with no shared helper to put the tagging in. The new
	// helper serves ToggleStatus and ReadMetrics, the two paths that reach this classification.
	// Dispatch keeps its own inline validation, because its failures are wrapped in
	// notCreated() to release the dispatch claim — a contract this helper does not carry.
	//
	// Meta's tagging deliberately stops short of a missing account id HERE, because
	// ToggleStatus never reads AccountConfig.AccountID (a status update targets the campaign
	// node by id); that guard is requireMetaAccountID and lives only on the Dispatch path.
	//
	// Bound the
	// whole (possibly multi-PATCH, each with its own retry budget) cascade with a total
	// deadline UNDER the HTTP write timeout, so a slow toggle is cancelled and returned to the
	// caller as an error rather than mutating the platform after the response can no longer be
	// delivered. A context deadline surfaces as UNCONFIRMED (the caller reports verify/retry).
	callCtx, cancel := context.WithTimeout(ctx, toggleCallTimeout)
	defer cancel()
	start := time.Now()
	terr := toggler.ToggleStatus(callCtx, projectID, platform, campaign, status)
	o.recordUpstream(ctx, platform, opToggleStatus, start, terr)
	return terr
}

// ReadCampaignMetrics fetches live performance metrics for one campaign from its ad
// platform. It never mutates the platform or the DB — a pure read, not persisted here.
func (o *Orchestrator) ReadCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	// Pre-platform guard, same rationale as ToggleCampaignStatus: never contact the ad
	// platform for a campaign with nothing provisioned upstream yet.
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, ErrCampaignNotProvisioned
	}
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrMetricsUnsupported, platform)
	}
	reader, ok := d.(MetricsReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrMetricsUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, metricsCallTimeout)
	defer cancel()
	start := time.Now()
	m, rerr := reader.ReadMetrics(callCtx, projectID, platform, campaign, window)
	o.recordUpstream(ctx, platform, opReadMetrics, start, rerr)
	if rerr != nil {
		return nil, rerr
	}
	if m == nil {
		// A MetricsReader returning (nil, nil) is a contract violation, not success — the
		// caller (GetCampaignMetrics) dereferences the result unconditionally on a nil error,
		// same as the dispatch path already guards against a nil successful Dispatch result
		// (see the analogous check for CampaignBrief dispatch). Convert it into an ordinary
		// error so the handler returns its declared 503 instead of panicking the request.
		return nil, fmt.Errorf("%s metrics reader returned a nil result with no error", platform)
	}
	return m, nil
}

// LookupPlatformCampaign confirms that platformCampaignID names a real campaign under the
// project's own connection. It never mutates the platform or the DB, and returns
// ErrPlatformCampaignAbsent — not (nil, nil) — when the platform answers that no such campaign
// exists, so "absent" never looks like "we could not tell".
func (o *Orchestrator) LookupPlatformCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	// Guarded here as well as at the transport layer: an empty id is a lookup for "any
	// campaign", and on a platform whose filter degrades to "unfiltered" that returns SOMEBODY
	// ELSE'S campaign as the adoption target. Unreachable over HTTP today, so a bare error
	// rather than a 400 sentinel — a programming fault, not something callers can provoke.
	if strings.TrimSpace(platformCampaignID) == "" {
		return nil, fmt.Errorf("lookup platform campaign: no platform campaign id given for %s", platform)
	}
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrAdoptionUnsupported, platform)
	}
	adopter, ok := d.(CampaignAdopter)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAdoptionUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, adoptLookupTimeout)
	defer cancel()
	start := time.Now()
	ref, err := adopter.LookupCampaign(callCtx, projectID, platform, platformCampaignID)
	o.recordUpstream(ctx, platform, opLookupCampaign, start, err)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, fmt.Errorf("%w: %s campaign %s", ErrPlatformCampaignAbsent, platform, platformCampaignID)
	}
	// The returned identity must be the requested one. An empty id is the obvious case — the
	// row would claim an upstream campaign with no way to reach it — but a DIFFERENT non-empty
	// id is the dangerous one: binding it means the caller asked to adopt campaign X and this
	// service bound campaign Y, a real paid campaign nobody named, under a 201. That is the
	// exact outcome verify-before-bind exists to prevent, and it is reachable from the ordinary
	// failure of an id filter that degrades to unfiltered — the adapter is expected to catch
	// that, and this is the check that holds if the adapter's does not. A mismatch is not an
	// absence: nothing here establishes that campaign X is missing, so it must be unverifiable
	// rather than a 404 that invites the caller to create a duplicate.
	//
	// Compared after TrimSpace on both sides and nothing else. Any looser comparison would be
	// this service inventing an equivalence between two platform ids, which is the platform's
	// vocabulary, not ours.
	if got := strings.TrimSpace(ref.ID); got != strings.TrimSpace(platformCampaignID) {
		if got == "" {
			return nil, fmt.Errorf("%s campaign lookup returned a campaign with no id for %s", platform, platformCampaignID)
		}
		return nil, fmt.Errorf("%s campaign lookup for %s returned campaign %s; the id filter was not honoured, so nothing about this response can be trusted", platform, platformCampaignID, got)
	}
	return ref, nil
}

// ReadAccounts enumerates the accessible ad accounts for a project's stored connection.
// It never mutates the platform or the DB — a pure read, not persisted here.
func (o *Orchestrator) ReadAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrAccountsUnsupported, platform)
	}
	lister, ok := d.(AccountLister)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAccountsUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	accounts, aerr := lister.ListAccounts(callCtx, projectID, platform)
	o.recordUpstream(ctx, platform, opListAccounts, start, aerr)
	if aerr != nil {
		return nil, aerr
	}
	if accounts == nil {
		// An AccountLister returning (nil, nil) is a contract violation, not success. Nothing
		// downstream would CRASH on it — len and range are nil-safe, and the handler's
		// conversion loop would happily produce an empty `[]`. That is exactly the problem:
		// the caller cannot tell "this credential reaches zero ad accounts" from "the lister
		// silently gave up", and would render an empty account picker with no error, sending
		// the operator to look for a permissions problem at the provider that does not exist.
		// Failing loudly here is what keeps the empty list meaningful — every lister on this
		// path builds its slice with make(..., 0, n) precisely so an empty answer stays
		// distinguishable from no answer.
		return nil, fmt.Errorf("%s account lister returned a nil result with no error", platform)
	}
	return accounts, nil
}

// SearchEmails returns the marketing emails reachable through the project's stored connection
// whose name or subject matches query, most-recently-updated first.
//
// Shares ReadAccounts' shape deliberately, including the (nil, nil) guard: an empty picker that
// silently means "the searcher gave up" sends an operator hunting for a permissions problem in
// their portal that does not exist.
//
// It shares `accountsCallTimeout` too, but NOT because the work is equivalent. `SearchEmails`
// walks cursor pages and can issue up to `maxListPages` (200) SEQUENTIAL HubSpot requests, so
// the 20s here bounds a whole paginated walk rather than a single read — an earlier version of
// this comment called it "one bounded upstream read", which describes ReadAccounts and not this.
//
// The deadline is shared anyway because what it protects is the same: a request path where the
// page cannot render until this answers, so the ceiling has to be a human's patience, not the
// walk's natural length. The consequence is worth stating rather than eliding — a portal large
// enough to need many pages will hit the deadline MID-WALK and surface as a failure, not as a
// truncated list. That is the correct direction (a silently short picker is worse than an
// error), but it means the practical page ceiling is whatever fits in 20s, well under 200.
func (o *Orchestrator) SearchEmails(ctx context.Context, projectID string, platform model.Provider, query string) ([]model.MarketingEmail, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrEmailSearchUnsupported, platform)
	}
	searcher, ok := d.(EmailSearcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrEmailSearchUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	emails, serr := searcher.SearchEmails(callCtx, projectID, platform, query)
	o.recordUpstream(ctx, platform, opSearchEmails, start, serr)
	if serr != nil {
		return nil, serr
	}
	if emails == nil {
		return nil, fmt.Errorf("%s email searcher returned a nil result with no error", platform)
	}
	return emails, nil
}
