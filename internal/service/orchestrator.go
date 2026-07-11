// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"golang.org/x/sync/errgroup"

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
}

// NewOrchestrator constructs an Orchestrator. dispatchers may be empty; a
// platform with no registered dispatcher is recorded as a failed result.
func NewOrchestrator(campaigns domain.CampaignRepository, jobs domain.JobRepository, dispatchers map[model.Provider]PlatformDispatcher) *Orchestrator {
	if dispatchers == nil {
		dispatchers = map[model.Provider]PlatformDispatcher{}
	}
	return &Orchestrator{campaigns: campaigns, jobs: jobs, dispatchers: dispatchers}
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
	job, err := o.jobs.CreateJob(ctx, brief.ID)
	if err != nil {
		return "", err
	}

	dispatchCtx := context.WithoutCancel(ctx)
	go o.run(dispatchCtx, job.ID, brief, platforms, config)

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

			// Idempotency guard FIRST, before resolving the dispatcher: if this
			// brief already has a campaign for this platform with an upstream id, a
			// prior run (or a re-POST of create-campaigns) already created the paid
			// campaign, so reuse it. This must precede the dispatcher lookup so an
			// already-persisted platform is reported ok on retry even if its
			// dispatcher is no longer registered. A not-found is the normal
			// first-time path; a transient lookup error is recorded as a failure
			// rather than risking a duplicate create.
			//
			// This read-before-dispatch check narrows — but does not fully close —
			// the duplicate window: two concurrent create-campaigns requests for the
			// same (brief, platform) can both observe not-found and both dispatch.
			// The (brief_id, platform) unique index keeps persistence single-rowed,
			// but the upstream create is not transactional with it. Fully closing
			// this requires provider-side idempotency keys and single-flight job
			// ownership, tracked in LFXV2-2665. campaign_id below is always the
			// upstream platform id so the field means the same on reuse and create.
			if existing, lerr := o.campaigns.GetCampaignByPlatform(gctx, brief.ID, p); lerr == nil {
				if existing.PlatformCampaignID != "" {
					res.OK = true
					res.CampaignID = existing.PlatformCampaignID
					results[i] = res
					return nil
				}
			} else if !errors.Is(lerr, domain.ErrNotFound) {
				// Log the underlying repository error server-side; return a generic
				// message so raw DB error text isn't surfaced to clients via GetJob.
				slog.ErrorContext(gctx, "existing-campaign lookup failed", "platform", p, "job_id", jobID, "error", lerr)
				res.Error = "could not verify existing campaign"
				results[i] = res
				return nil
			}

			d, ok := o.dispatchers[p]
			if !ok {
				res.Error = "no dispatcher registered for platform"
				results[i] = res
				return nil
			}
			campaign, err := d.Dispatch(gctx, brief, p, config)
			if err != nil {
				res.Error = err.Error()
				results[i] = res
				return nil
			}
			if campaign == nil {
				// Defensive: a dispatcher must return a non-nil campaign on success.
				// A nil campaign with no error would panic on the ownership stamp
				// below (in a detached goroutine); record it as a platform failure.
				res.Error = "dispatcher returned no campaign"
				results[i] = res
				return nil
			}
			if campaign.PlatformCampaignID == "" {
				// A successful dispatch must yield an upstream campaign id; without
				// one the result can't honestly be reported ok (campaign_id is
				// documented as present when ok), and a later retry couldn't
				// recognize it via the idempotency guard. Treat as a failure.
				res.Error = "dispatcher returned no upstream campaign id"
				results[i] = res
				return nil
			}
			// Stamp ownership before persisting so a dispatcher can't cause a
			// campaign to be written with missing/wrong ownership. Set on a
			// fresh reference the dispatcher returned; the invariant is one
			// campaign per (brief, platform) owned by exactly this job.
			campaign.JobID = &jobID
			campaign.BriefID = brief.ID
			campaign.ProjectID = brief.ProjectID
			campaign.Platform = p
			if _, err := o.campaigns.UpsertCampaign(gctx, campaign); err != nil {
				// Log the raw persistence error; return a generic message so DB
				// error text isn't leaked to clients via the job result.
				slog.ErrorContext(gctx, "persist campaign failed", "platform", p, "job_id", jobID, "error", err)
				res.Error = "could not persist campaign"
				results[i] = res
				return nil
			}
			res.OK = true
			res.CampaignID = campaign.PlatformCampaignID
			results[i] = res
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
