// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"

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
	ArchiveBrief(ctx context.Context, projectID, id string, by *model.Actor, indexPayload IndexPayloadFunc) (*model.CampaignBrief, error)
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
	// ListCampaignsForBrief returns all non-deleted campaigns under a brief, or
	// an empty array when the brief exists but has no campaigns. The brief's
	// existence is verified (404 if absent or archived); a campaign under a
	// different project or brief is guaranteed not to appear. Results are ordered
	// deterministically by created_at (ascending), with id as a tie-break.
	ListCampaignsForBrief(ctx context.Context, projectID, briefID string) ([]*model.Campaign, error)
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
	//
	// `by` is the human who asked for the dispatch, stamped as the row's created_by.
	// It is a PARAMETER rather than something the repository reads from ctx because
	// dispatch runs on the orchestrator's root context, long after the request
	// returned — an actor read from this ctx would be nil for every campaign ever
	// created. Orchestrator.Start captures it while the request context is still in
	// hand and threads it down. nil is legitimate: Start reads the actor with
	// attributedActor, which returns nil — after logging a warning — when the request
	// carried no authenticated principal, and NULL means "not recorded", not "nobody".
	ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, platform model.Provider, jobID string, by *model.Actor) (claimed bool, row *model.Campaign, err error)
	// DeleteDispatchClaim removes a still-'pending' claim row for (brief, platform)
	// so the pair can be retried after a dispatch fails before the upstream
	// campaign is created. It only deletes rows still in 'pending' status, so it
	// can never remove a real (created) campaign.
	DeleteDispatchClaim(ctx context.Context, briefID string, platform model.Provider) error
}

// CampaignLockToken is an opaque handle identifying one successful ClaimCampaignVersion call.
// ReleaseCampaignLock and ReleaseCampaignLockAfterCooldown must be passed the exact token
// returned by the claim they are releasing, never a bare campaign ID — that is the whole point
// of the token. Releasing by campaign ID alone cannot tell "the lock I claimed" from "whatever
// lock is in the map right now": if this claimant's session dies during a cooldown and a NEW
// claimant claims and overwrites the same campaign's entry, a delayed release keyed only by
// campaign ID would load and release the successor's live lock instead of a no-op. The token
// carries this claim's own identity so a stale release can never touch a different claim. The
// zero value denotes "no lock held" and is always a safe no-op to release.
type CampaignLockToken struct {
	CampaignID string
	handle     any
}

// NewCampaignLockToken constructs a CampaignLockToken wrapping a repository-internal lock
// handle. Only CampaignWriter implementations, which own the concrete handle type, should call
// this; every other caller must treat CampaignLockToken as opaque.
func NewCampaignLockToken(campaignID string, handle any) CampaignLockToken {
	return CampaignLockToken{CampaignID: campaignID, handle: handle}
}

// Handle returns the opaque lock handle for use by the CampaignWriter implementation that
// created this token.
func (t CampaignLockToken) Handle() any {
	return t.handle
}

// CampaignWriter mutates campaigns.
type CampaignWriter interface {
	// UpsertCampaign inserts or updates the campaign row for a (brief, platform).
	// Campaigns are updated in place when a brief changes after they exist.
	UpsertCampaign(ctx context.Context, c *model.Campaign, indexPayload CampaignIndexPayloadFunc) (*model.Campaign, error)
	// ReplaceCampaign replaces a campaign's mutable fields, gating on version. lockToken is the
	// token returned by ClaimCampaignVersion when the caller holds the claim lock for this
	// campaign (the zero CampaignLockToken otherwise) — implementations that reuse the lock
	// holder's own connection for this write MUST use lockToken's own handle, never a lookup by
	// campaign ID, so a write can never attach to a different claimant's connection.
	ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64, lockToken CampaignLockToken, indexPayload CampaignIndexPayloadFunc) (*model.Campaign, error)
	// ClaimCampaignVersion reserves EXCLUSIVE write ownership of a campaign row, gated on
	// expectedVersion, and returns the row plus a CampaignLockToken. It returns
	// ErrPreconditionFailed if expectedVersion is stale, or ErrNotFound if the row is gone;
	// the two must be told apart while ownership is still held, or a concurrent delete
	// racing the release turns a stale-version caller's 412 into a 404.
	//
	// Ownership is a LOCK, not a version bump. The claim explicitly leaves version
	// UNCHANGED: the increment belongs to ReplaceCampaign, inside the same transaction that
	// writes the outbox event, so the invariant that every campaign write co-commits its
	// index event is preserved. Implementations must therefore hold ownership across the
	// caller's external I/O (the Postgres implementation takes a session advisory lock on a
	// dedicated connection) and must not rely on the version alone to exclude a second
	// writer.
	//
	// Every writer that must do external I/O (an ad-platform call) BETWEEN reading
	// the row and persisting its outcome — currently only BriefService.
	// ToggleCampaignStatus — calls this FIRST, before that I/O, instead of comparing
	// the read-time version in memory. An in-memory comparison only rejects a stale
	// caller; it does nothing to stop a SECOND caller reading the same version and
	// also proceeding to call the platform, so two toggles (or a toggle and an
	// update-campaign) can both pass the check and both mutate the platform before
	// either persists. The claim closes that window by EXCLUSION, not by bumping the
	// version: while one caller holds the claim for a campaign, a second caller's claim
	// FAILS, so the second one is stopped before it ever reaches the platform rather
	// than after. Implementations must not make the loser WAIT: the claim is held across
	// external I/O, so a waiter would tie up whatever resource the claim itself consumes
	// (for Postgres, a second pooled connection) for that whole span, and a burst against
	// one campaign could exhaust a finite pool. Answer immediately with
	// ErrCampaignWriteInProgress, which the service maps to a retryable 409 — not
	// ErrPreconditionFailed, since the loser's expectedVersion may be perfectly current.
	// Do not read this as durable ownership — the exclusion lasts only as
	// long as the implementation's lock (for Postgres, the lifetime of the session
	// holding it), and what makes a lost lock safe is the compare-and-swap in the
	// subsequent ReplaceCampaign, which rejects any writer whose expectedVersion has
	// since moved.
	//
	// BriefService.UpdateCampaign — which has no I/O between its read and its write —
	// also claims first so its single-statement write can never land in the gap
	// between a toggle's claim and that toggle's own post-platform persist; without
	// this, UpdateCampaign's plain ReplaceCampaign could win that gap's freshly
	// bumped version out from under the in-flight toggle, so the toggle's own
	// persist would then lose (a real divergence between the platform and the row,
	// even though every individual write was individually consistent).
	//
	// Callers MUST call ReleaseCampaignLock, passing back the returned token, after claiming.
	// Use defer for guaranteed release, even on panic or cancellation.
	ClaimCampaignVersion(ctx context.Context, projectID, briefID, campaignID string, expectedVersion int64) (*model.Campaign, CampaignLockToken, error)
	// ReleaseCampaignLock releases the advisory lock identified by token, as returned by
	// ClaimCampaignVersion. Callers MUST call this after claiming, either directly or via
	// defer, to allow other writers to proceed.
	//
	// It is a complete no-op only for the zero CampaignLockToken and for an already-released
	// token. When token no longer identifies the lock currently held for its campaign — a
	// SUCCESSOR claimed it after this token's session ended (see CampaignLockToken) — the
	// call must leave that successor's lock and resources alone, but it must still release
	// whatever THIS token holds. Implementations that return early in that case leak their
	// own held resource (in Postgres, a checked-out pool connection) with nothing left
	// referencing it.
	ReleaseCampaignLock(ctx context.Context, token CampaignLockToken) error
	// ReleaseCampaignLockAfterCooldown releases the advisory lock identified by token after
	// cooldown elapses, or immediately once the process starts shutting down — whichever
	// comes first. Used instead of a bare ReleaseCampaignLock call when a caller must hold the
	// lock past its own request's lifetime (see BriefService.ToggleCampaignStatus's UNCONFIRMED
	// path) without leaking the held connection past process shutdown.
	ReleaseCampaignLockAfterCooldown(token CampaignLockToken, cooldown time.Duration)
	// DeleteCampaign SOFT-deletes a campaign (status = 'deleted'), gating on
	// expectedVersion. The row is retained: it holds platform_campaign_id, the only
	// local record of a campaign that may still exist (and still be spending) on the
	// ad platform, so hard-deleting would destroy both the audit trail and the
	// pointer needed to reconcile it. Deleting is LOCAL ONLY — it never contacts the
	// ad platform.
	//
	// Its purpose is to free the (brief_id, platform) slot: the partial unique index
	// added in 000013 excludes deleted rows, so a brief whose campaign was created
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
	//
	// The campaign is ALWAYS co-indexed on delete, just as every other write,
	// so the indexer can remove it and keep search consistent. A nil indexPayload
	// means the caller does not want the delete indexed; the soft delete still commits.
	// `by` is stamped onto updated_by so the audit trail names who retired the row
	// rather than whoever last edited it; nil (an unauthenticated, system-initiated
	// delete) leaves the column at its previous value rather than clearing it.
	DeleteCampaign(ctx context.Context, projectID, briefID, id string, expectedVersion int64, by *model.Actor, indexPayload CampaignIndexPayloadFunc) error
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
