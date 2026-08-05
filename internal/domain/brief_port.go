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
	// FindBriefByEventSlug returns the non-archived brief for (projectID, eventSlug), or
	// ErrNotFound when none exists. ErrNotFound is an ORDINARY outcome here, not a failure:
	// it is how the caller learns this event has no brief yet and one should be generated.
	FindBriefByEventSlug(ctx context.Context, projectID, eventSlug string) (*model.CampaignBrief, error)
}

// BriefWriter mutates campaign briefs.
type BriefWriter interface {
	// CreateBrief inserts a brief. Returns ErrConflict on the
	// UNIQUE(project_id, event_slug) violation.
	CreateBrief(ctx context.Context, b *model.CampaignBrief, indexPayload IndexPayloadFunc) (*model.CampaignBrief, error)
	// ReplaceBrief replaces a brief's mutable fields, gating on expectedVersion.
	ReplaceBrief(ctx context.Context, b *model.CampaignBrief, expectedVersion int64, indexPayload IndexPayloadFunc) (*model.CampaignBrief, error)
	// Approve marks a brief approved, recording the actor. It is gated on
	// expectedVersion (optimistic concurrency): approving a stale version returns
	// ErrPreconditionFailed so a concurrent replace can't be approved by accident.
	Approve(ctx context.Context, projectID, id string, by *model.Actor, expectedVersion int64, indexPayload IndexPayloadFunc) (*model.CampaignBrief, error)
	// ArchiveBrief soft-archives a brief (status = archived) and RETURNS the archived row.
	// Returning it (rather than just an error) is what lets the caller index the state that
	// was actually committed: a concurrent ReplaceBrief/Approve can commit between a
	// read-then-archive pair, so a separately-read snapshot would publish stale content and a
	// version that never existed.
	ArchiveBrief(ctx context.Context, projectID, id string, indexPayload IndexPayloadFunc) (*model.CampaignBrief, error)
}

// CampaignIndexPayloadFunc builds the index message for a campaign that has just been written,
// co-committed with the row like IndexPayloadFunc does for briefs.
//
// Campaign creation is ASYNC: the dispatch runs on the orchestrator's root context, long after
// the request returned. Publishing directly with the caller's captured JWT could therefore fail
// on an EXPIRED token — and with no outbox row there was nothing to retry, leaving a new
// campaign permanently unsearchable. The relay stamps a service credential at publish time.
type CampaignIndexPayloadFunc func(*model.Campaign) ([]byte, error)

// IndexPayloadFunc builds the index message for a brief that has just been written. It is
// invoked INSIDE the write transaction and its result is enqueued to the outbox alongside the
// row, so the message co-commits with the change it describes.
//
// EVERY brief mutation takes one — not just the terminal archive. A direct post-commit publish
// cannot be ordered against an outbox replay: a replace could commit, stall before publishing,
// and land its update AFTER an archive was replayed and retired, resurrecting a deleted brief in
// the index. Routing all writes through the outbox gives each row ONE ordered sequence, which is
// also why this is safe across replicas — ordering comes from the table, not from any
// process-local lock.
//
// A nil func means the caller does not want the write indexed; the write still commits.
type IndexPayloadFunc func(*model.CampaignBrief) ([]byte, error)

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
	UpsertCampaign(ctx context.Context, c *model.Campaign, indexPayload CampaignIndexPayloadFunc) (*model.Campaign, error)
	// ReplaceCampaign replaces a campaign's mutable fields, gating on version.
	// ReplaceCampaign replaces a campaign's mutable fields, gating on version.
	ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64, indexPayload CampaignIndexPayloadFunc) (*model.Campaign, error)
	// DeleteCampaign SOFT-deletes a campaign (status = 'deleted'), gating on
	// expectedVersion. The row is retained: it holds platform_campaign_id, the only
	// local record of a campaign that may still exist (and still be spending) on the
	// ad platform, so hard-deleting would destroy both the audit trail and the
	// pointer needed to reconcile it. Deleting is LOCAL ONLY — it never contacts the
	// ad platform.
	//
	// Its purpose is to free the (brief_id, platform) slot: the partial unique index
	// added in 000010 excludes deleted rows, so a brief whose campaign was created
	// with the wrong budget (or whose upstream create failed ambiguously) can be
	// re-dispatched to that platform instead of being blocked forever.
	//
	// Returns ErrNotFound if absent or already deleted, ErrConflict if the campaign
	// is mid-dispatch (status 'pending' — an active claim owned by an in-flight
	// dispatch, whose deletion could let a concurrent claim double-create upstream),
	// and ErrPreconditionFailed on a version mismatch. The guards are evaluated under
	// a SELECT ... FOR UPDATE row lock, which is required to close the TOCTOU race
	// against a concurrent claim/finalize under READ COMMITTED (see the
	// implementation for the full isolation reasoning).
	DeleteCampaign(ctx context.Context, projectID, briefID, id string, expectedVersion int64) error
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
