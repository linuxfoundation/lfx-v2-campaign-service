// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// BriefReader reads campaign briefs.
type BriefReader interface {
	// GetBrief returns a brief by id (scoped to the project), or ErrNotFound.
	GetBrief(ctx context.Context, projectID, id string) (*model.CampaignBrief, error)
}

// BriefWriter mutates campaign briefs.
type BriefWriter interface {
	// CreateBrief inserts a brief. Returns ErrConflict on the
	// UNIQUE(project_id, event_slug) violation.
	CreateBrief(ctx context.Context, b *model.CampaignBrief) (*model.CampaignBrief, error)
	// ReplaceBrief replaces a brief's mutable fields, gating on expectedVersion.
	ReplaceBrief(ctx context.Context, b *model.CampaignBrief, expectedVersion int64) (*model.CampaignBrief, error)
	// Approve marks a brief approved, recording the actor. It is gated on
	// expectedVersion (optimistic concurrency): approving a stale version returns
	// ErrPreconditionFailed so a concurrent replace can't be approved by accident.
	Approve(ctx context.Context, projectID, id string, by *model.Actor, expectedVersion int64) (*model.CampaignBrief, error)
	// ArchiveBrief soft-archives a brief (status = archived).
	ArchiveBrief(ctx context.Context, projectID, id string) error
}

// BriefRepository is the full persistence port for briefs.
type BriefRepository interface {
	BriefReader
	BriefWriter
}

// CampaignReader reads campaigns.
type CampaignReader interface {
	// GetCampaign returns a single campaign under a brief, or ErrNotFound.
	GetCampaign(ctx context.Context, projectID, briefID, id string) (*model.Campaign, error)
	// GetCampaignByPlatform returns the campaign for a (brief, platform) pair, or
	// ErrNotFound. Used to make dispatch idempotent: a brief already dispatched to
	// a platform must not create a second upstream (paid) campaign on retry. Scoped
	// by projectID for tenant isolation, matching GetCampaign/ClaimCampaignDispatch.
	GetCampaignByPlatform(ctx context.Context, projectID, briefID string, platform model.Provider) (*model.Campaign, error)
	// ClaimCampaignDispatch atomically claims the right to dispatch (brief,
	// platform) by inserting a placeholder campaign row (status 'pending') via
	// INSERT ... ON CONFLICT (brief_id, platform) DO NOTHING. Exactly one worker
	// wins across all replicas — the (brief_id, platform) unique index arbitrates,
	// with no held connection and no blocking lock. It returns:
	//   - claimed=true, row=the pending row  → this worker owns the dispatch;
	//   - claimed=false, row=the existing row → another worker already claimed or
	//     completed it; the caller reuses that row instead of dispatching again.
	// The placeholder row also survives an upstream-create-then-crash, making the
	// orphan recoverable (its status stays 'pending').
	ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, platform model.Provider, jobID string) (claimed bool, row *model.Campaign, err error)
	// DeleteDispatchClaim removes a still-'pending' claim row for (brief, platform)
	// so the pair can be retried after a dispatch fails before the upstream
	// campaign is created. It only deletes rows still in 'pending' status, so it
	// can never remove a real (created) campaign.
	DeleteDispatchClaim(ctx context.Context, briefID string, platform model.Provider) error
}

// CampaignWriter mutates campaigns.
type CampaignWriter interface {
	// UpsertCampaign inserts or updates the campaign row for a (brief, platform).
	// Campaigns are updated in place when a brief changes after they exist.
	UpsertCampaign(ctx context.Context, c *model.Campaign) (*model.Campaign, error)
	// ReplaceCampaign replaces a campaign's mutable fields, gating on version.
	ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64) (*model.Campaign, error)
}

// CampaignRepository is the full persistence port for campaigns.
type CampaignRepository interface {
	CampaignReader
	CampaignWriter
}

// JobRepository persists async dispatch jobs.
type JobRepository interface {
	// CreateJob inserts a queued job for a brief.
	CreateJob(ctx context.Context, briefID string) (*model.CampaignJob, error)
	// CreateJobForApprovedBrief inserts a queued job ONLY if the brief is still
	// approved at expectedVersion, re-verifying the approval atomically with the job
	// insert inside one transaction. This closes the approve→dispatch TOCTOU race:
	// a concurrent ReplaceBrief (which resets the brief to draft and bumps version)
	// or ArchiveBrief committing between the caller's approval read and job creation
	// bumps the brief's version and must prevent the job from being created against
	// that stale approval, returning ErrStaleApproval instead of launching paid
	// campaigns from a stale "approved" snapshot. The guard takes a SELECT ... FOR
	// UPDATE row lock on the brief before the insert so it serializes against any
	// concurrent replace/archive of that row — a lone guarded INSERT ... WHERE
	// EXISTS would NOT, because under READ COMMITTED its snapshot can miss a mutation
	// that commits just before the statement runs (see the implementation for the
	// full isolation reasoning).
	CreateJobForApprovedBrief(ctx context.Context, briefID string, expectedVersion int64) (*model.CampaignJob, error)
	// GetJob returns a job by id, or ErrNotFound.
	GetJob(ctx context.Context, projectID, id string) (*model.CampaignJob, error)
	// UpdateJobStatus sets a job's status (any JobStatus, e.g. running or a
	// terminal succeeded/partial/failed) and its result/error.
	UpdateJobStatus(ctx context.Context, id string, status model.JobStatus, result []byte, jobErr string) error
	// FailStuckJobs marks every non-terminal (queued/running) job as failed with
	// the given error, returning the count. Called on startup to recover jobs
	// orphaned by a pod restart (their in-memory dispatch goroutine is gone).
	FailStuckJobs(ctx context.Context, jobErr string) (int64, error)
}
