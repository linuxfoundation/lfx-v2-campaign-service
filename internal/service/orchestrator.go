// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
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

	lockErr := o.campaigns.WithDispatchLock(ctx, brief.ID, p, func(ctx context.Context) error {
		// Idempotency guard FIRST, before resolving the dispatcher, so an
		// already-persisted platform is reported ok even if its dispatcher is no
		// longer registered. Under the lock, a concurrent request's persisted row
		// is visible here, closing the duplicate-create window.
		if existing, lerr := o.campaigns.GetCampaignByPlatform(ctx, brief.ID, p); lerr == nil {
			if existing.PlatformCampaignID != "" {
				res.OK = true
				res.CampaignID = existing.PlatformCampaignID
				return nil
			}
		} else if !errors.Is(lerr, domain.ErrNotFound) {
			// Log the underlying repository error server-side; return a generic
			// message so raw DB error text isn't surfaced to clients via GetJob.
			slog.ErrorContext(ctx, "existing-campaign lookup failed", "platform", p, "job_id", jobID, "error", lerr)
			res.Error = "could not verify existing campaign"
			return nil
		}

		d, ok := o.dispatchers[p]
		if !ok {
			res.Error = "no dispatcher registered for platform"
			return nil
		}
		campaign, err := d.Dispatch(ctx, brief, p, config)
		if err != nil {
			// Log the raw dispatcher error (it may carry provider request/response
			// detail or credentials) server-side, and store a stable, client-safe
			// message in the job result, consistent with the persistence/panic paths.
			slog.ErrorContext(ctx, "platform dispatch failed", "platform", p, "job_id", jobID, "error", err)
			res.Error = "platform campaign creation failed"
			return nil
		}
		if campaign == nil {
			// Defensive: a dispatcher must return a non-nil campaign on success.
			res.Error = "dispatcher returned no campaign"
			return nil
		}
		if campaign.PlatformCampaignID == "" {
			// A successful dispatch must yield an upstream campaign id; without one
			// the result can't honestly be reported ok, and a later retry couldn't
			// recognize it via the idempotency guard. Treat as a failure.
			res.Error = "dispatcher returned no upstream campaign id"
			return nil
		}
		// Stamp ownership before persisting so a dispatcher can't cause a campaign
		// to be written with missing/wrong ownership.
		campaign.JobID = &jobID
		campaign.BriefID = brief.ID
		campaign.ProjectID = brief.ProjectID
		campaign.Platform = p
		if _, err := o.campaigns.UpsertCampaign(ctx, campaign); err != nil {
			// Log the raw persistence error; return a generic message so DB error
			// text isn't leaked to clients via the job result. Return the error so
			// the advisory-lock transaction rolls back rather than committing after
			// a failed write.
			slog.ErrorContext(ctx, "persist campaign failed", "platform", p, "job_id", jobID, "error", err)
			res.Error = "could not persist campaign"
			return err
		}
		res.OK = true
		res.CampaignID = campaign.PlatformCampaignID
		return nil
	})
	if lockErr != nil && res.Error == "" {
		// The lock itself failed (couldn't begin/acquire); record a generic failure.
		slog.ErrorContext(ctx, "dispatch lock failed", "platform", p, "job_id", jobID, "error", lockErr)
		res.Error = "could not acquire dispatch lock"
		res.OK = false
	}
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
