// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
// semaphore slots. This ceiling guarantees the slot and job are released. It is
// generous: real ad-platform create flows are multi-request but complete in well
// under a minute.
const providerCallTimeout = 2 * time.Minute

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
type Orchestrator struct {
	campaigns   domain.CampaignRepository
	jobs        domain.JobRepository
	dispatchers map[model.Provider]PlatformDispatcher

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
type platformResult struct {
	Platform   string `json:"platform"`
	OK         bool   `json:"ok"`
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
// caller's approval read and this insert bumps the brief's version, so the
// guarded insert affects zero rows and Start returns ErrConflict (409) rather
// than launching paid campaigns from a stale "approved" snapshot.
//
// The dispatch goroutine runs under the orchestrator's root context (not the
// request context), so it survives the request ending but can still be cancelled
// by Shutdown when the drain deadline expires.
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
	go func() {
		defer o.wg.Done()
		o.run(dispatchCtx, job.ID, brief, platformsCopy, configCopy)
	}()

	return job.ID, nil
}

// run performs the parallel per-platform dispatch and finalizes the job.
func (o *Orchestrator) run(ctx context.Context, jobID string, brief *model.CampaignBrief, platforms []model.Provider, config json.RawMessage) {
	// Mark the job running. Don't abort dispatch on failure (the work should still
	// proceed and the final status write will correct it), but log it — silently
	// dropping this can leave a job stuck at "queued" in the client's view.
	if err := o.jobs.UpdateJobStatus(ctx, jobID, model.JobRunning, nil, ""); err != nil {
		slog.ErrorContext(ctx, "failed to mark campaign job running", "job_id", jobID, "error", err)
	}

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
				return nil
			}

			// Recover from a panic in a dispatcher (or future code here): a panic
			// in this detached goroutine would otherwise crash the whole process
			// mid-job. Record it as a platform failure and keep the group intact.
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(gctx, "panic during platform dispatch", "platform", p, "job_id", jobID, "panic", r)
					res.OK = false
					res.Error = "internal error during dispatch"
					results[i] = res
					rerr = nil
				}
			}()

			results[i] = o.dispatchPlatform(gctx, jobID, brief, p, config)
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

	status := aggregateStatus(results)
	payload, err := json.Marshal(results)
	if err != nil {
		// Don't store a null result (which would make the job unpollable);
		// record the marshal failure in the job's error field and fail the job.
		slog.ErrorContext(finCtx, "failed to marshal job result", "job_id", jobID, "error", err)
		if uerr := o.jobs.UpdateJobStatus(finCtx, jobID, model.JobFailed, nil, "failed to serialize job result: "+err.Error()); uerr != nil {
			slog.ErrorContext(finCtx, "failed to finalize campaign job", "job_id", jobID, "error", uerr)
		}
		return
	}
	if err := o.jobs.UpdateJobStatus(finCtx, jobID, status, payload, ""); err != nil {
		slog.ErrorContext(finCtx, "failed to finalize campaign job", "job_id", jobID, "error", err)
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
func (o *Orchestrator) dispatchPlatform(ctx context.Context, jobID string, brief *model.CampaignBrief, p model.Provider, config json.RawMessage) platformResult {
	res := platformResult{Platform: string(p)}

	// Fast path: if this pair already has a completed campaign (upstream id set),
	// reuse it — idempotent, and valid even if no dispatcher is registered for the
	// platform anymore.
	if existing, lerr := o.campaigns.GetCampaignByPlatform(ctx, brief.ID, p); lerr == nil && existing.PlatformCampaignID != "" {
		res.OK = true
		res.CampaignID = existing.PlatformCampaignID
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
	claimed, existing, err := o.campaigns.ClaimCampaignDispatch(ctx, brief.ProjectID, brief.ID, p, jobID)
	if err != nil {
		slog.ErrorContext(ctx, "claim dispatch failed", "platform", p, "job_id", jobID, "error", err)
		res.Error = "could not claim campaign dispatch"
		return res
	}
	if !claimed {
		// Another worker owns (or already completed) this pair.
		if existing != nil && existing.PlatformCampaignID != "" {
			// Already created upstream — reuse it (idempotent).
			res.OK = true
			res.CampaignID = existing.PlatformCampaignID
			return res
		}
		// Another worker holds the pending claim: don't dispatch again (the point of
		// the claim). This job did not create the campaign, so it's recorded as not
		// ok (OK stays false) — which aggregates to failed/partial for THIS job —
		// with a message making clear it's owned by a concurrent run, not a real
		// failure. The other run will complete it; a poll of that run (or a re-poll
		// after it finishes) reflects the true outcome.
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
		if derr := o.campaigns.DeleteDispatchClaim(rctx, brief.ID, p); derr != nil {
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
		if dispatchErrIsPreCreate(derr) {
			slog.ErrorContext(ctx, "platform dispatch failed before upstream create (claim released)", "platform", p, "job_id", jobID, "error", derr)
			releaseClaim()
		} else {
			slog.ErrorContext(ctx, "platform dispatch failed (claim retained; outcome unknown)", "platform", p, "job_id", jobID, "error", derr)
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
	// (brief, platform) fills in the real upstream id and status).
	campaign.JobID = &jobID
	campaign.BriefID = brief.ID
	campaign.ProjectID = brief.ProjectID
	campaign.Platform = p
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
	if _, err := o.campaigns.UpsertCampaign(persistCtx, campaign); err != nil {
		// The upstream (paid) campaign was created but recording it failed. The
		// 'pending' claim row remains, so this is recoverable/reconcilable out of
		// band and a duplicate can't be created behind the claim. Log the raw error
		// and the orphaned upstream id; keep the id in the result.
		slog.ErrorContext(ctx, "persist campaign failed after upstream create — pending claim retained",
			"platform", p, "job_id", jobID, "platform_campaign_id", campaign.PlatformCampaignID, "error", err)
		res.Error = "created upstream campaign but failed to record it; see logs"
		res.CampaignID = campaign.PlatformCampaignID
		return res
	}
	res.OK = true
	res.CampaignID = campaign.PlatformCampaignID
	return res
}

// aggregateStatus folds per-platform outcomes into the job's terminal status.
func aggregateStatus(results []platformResult) model.JobStatus {
	var ok, failed int
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			failed++
		}
	}
	switch {
	case failed == 0:
		return model.JobSucceeded
	case ok == 0:
		return model.JobFailed
	default:
		return model.JobPartial
	}
}
