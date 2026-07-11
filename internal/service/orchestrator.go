// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// maxParallelDispatch bounds concurrent per-platform campaign creation.
const maxParallelDispatch = 5

// PlatformDispatcher creates a campaign on one ad platform. Implementations are
// the per-provider adapters (added as those integrations land); the
// orchestrator is agnostic to them.
type PlatformDispatcher interface {
	// Dispatch creates a campaign on the platform and returns the resulting
	// campaign row (platform_campaign_id, status, result populated).
	Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error)
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
	mu       sync.Mutex
	wg       sync.WaitGroup
	draining bool
}

// NewOrchestrator constructs an Orchestrator. dispatchers may be empty; a
// platform with no registered dispatcher is recorded as a failed result.
func NewOrchestrator(campaigns domain.CampaignRepository, jobs domain.JobRepository, dispatchers map[model.Provider]PlatformDispatcher) *Orchestrator {
	if dispatchers == nil {
		dispatchers = map[model.Provider]PlatformDispatcher{}
	}
	return &Orchestrator{campaigns: campaigns, jobs: jobs, dispatchers: dispatchers}
}

// Shutdown waits (bounded by ctx) for in-flight dispatch runs to finish, so a
// graceful shutdown doesn't close the database pool out from under a dispatch
// that already created an upstream campaign but hasn't persisted it yet. After
// Shutdown is called, Start rejects new work. Returns ctx.Err() if the deadline
// elapses before all runs complete.
func (o *Orchestrator) Shutdown(ctx context.Context) error {
	o.mu.Lock()
	o.draining = true
	o.mu.Unlock()

	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
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
// The dispatch goroutine uses context.WithoutCancel so it survives the request
// context ending, matching the documented async model.
func (o *Orchestrator) Start(ctx context.Context, brief *model.CampaignBrief, platforms []model.Provider, config json.RawMessage) (string, error) {
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

	job, err := o.jobs.CreateJob(ctx, brief.ID)
	if err != nil {
		o.wg.Done()
		return "", err
	}

	dispatchCtx := context.WithoutCancel(ctx)
	go func() {
		defer o.wg.Done()
		o.run(dispatchCtx, job.ID, brief, platforms, config)
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
	g.SetLimit(maxParallelDispatch)

	for i, p := range platforms {
		i, p := i, p
		g.Go(func() (rerr error) {
			// A single platform failure must not cancel the others, so we never
			// return a non-nil error from the group; failures are recorded.
			res := platformResult{Platform: string(p)}

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

	status := aggregateStatus(results)
	payload, err := json.Marshal(results)
	if err != nil {
		// Don't store a null result (which would make the job unpollable);
		// record the marshal failure in the job's error field and fail the job.
		slog.ErrorContext(ctx, "failed to marshal job result", "job_id", jobID, "error", err)
		if uerr := o.jobs.UpdateJobStatus(ctx, jobID, model.JobFailed, nil, "failed to serialize job result: "+err.Error()); uerr != nil {
			slog.ErrorContext(ctx, "failed to finalize campaign job", "job_id", jobID, "error", uerr)
		}
		return
	}
	if err := o.jobs.UpdateJobStatus(ctx, jobID, status, payload, ""); err != nil {
		slog.ErrorContext(ctx, "failed to finalize campaign job", "job_id", jobID, "error", err)
	}
}

// dispatchPlatform creates (or reuses) the campaign for a single platform. The
// idempotency read, the upstream create, and the persist all run under a
// cross-replica advisory lock keyed on (brief, platform), so two concurrent
// create-campaigns requests for the same pair cannot both create an upstream
// campaign: the second waits on the lock, then observes the first's persisted
// row and reuses it. campaign_id is always the upstream platform id, so the
// field means the same on the reuse and create paths.
func (o *Orchestrator) dispatchPlatform(ctx context.Context, jobID string, brief *model.CampaignBrief, p model.Provider, config json.RawMessage) platformResult {
	res := platformResult{Platform: string(p)}

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
		// Still pending elsewhere: don't dispatch again (that's the whole point of
		// the claim). Report it as in-progress rather than a failure or a duplicate.
		res.Error = "another dispatch for this platform is already in progress"
		return res
	}

	// We own the claim (a 'pending' row now exists). From here, any early return
	// leaves that pending row behind; it is the recoverable record of an
	// in-progress/failed dispatch (recovered by the startup scan) and blocks a
	// concurrent duplicate.
	d, ok := o.dispatchers[p]
	if !ok {
		res.Error = "no dispatcher registered for platform"
		return res
	}
	campaign, derr := d.Dispatch(ctx, brief, p, config)
	if derr != nil {
		// Log the raw dispatcher error (it may carry provider request/response
		// detail or credentials) server-side; store a stable, client-safe message.
		slog.ErrorContext(ctx, "platform dispatch failed", "platform", p, "job_id", jobID, "error", derr)
		res.Error = "platform campaign creation failed"
		return res
	}
	if campaign == nil {
		res.Error = "dispatcher returned no campaign"
		return res
	}
	if campaign.PlatformCampaignID == "" {
		res.Error = "dispatcher returned no upstream campaign id"
		return res
	}
	// Stamp ownership, then update the claimed row in place (Upsert on the same
	// (brief, platform) fills in the real upstream id and status).
	campaign.JobID = &jobID
	campaign.BriefID = brief.ID
	campaign.ProjectID = brief.ProjectID
	campaign.Platform = p
	if _, err := o.campaigns.UpsertCampaign(ctx, campaign); err != nil {
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
