// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// staleJobCutoff is how long a queued/running job must have been idle (no update)
// before startup recovery treats it as orphaned. It must exceed the longest
// realistic dispatch so a job still being actively worked by another replica
// during a rolling deploy is never failed out from under it.
const staleJobCutoff = 15 * time.Minute

// JobRepo is a pgx-backed implementation of domain.JobRepository.
type JobRepo struct {
	db *Pool
}

// NewJobRepo returns a JobRepo backed by pool.
func NewJobRepo(pool *Pool) *JobRepo { return &JobRepo{db: pool} }

var _ domain.JobRepository = (*JobRepo)(nil)

const jobCols = `id::text, brief_id::text, status, result, error, created_at, updated_at, expires_at`

// jobColsPrefixed is jobCols with a `j.` table alias, for queries that JOIN
// campaign_jobs (aliased j) against campaign_briefs.
const jobColsPrefixed = `j.id::text, j.brief_id::text, j.status, j.result, j.error, j.created_at, j.updated_at, j.expires_at`

// CreateJob inserts a queued job for a brief.
func (r *JobRepo) CreateJob(ctx context.Context, briefID string) (*model.CampaignJob, error) {
	q := `INSERT INTO campaign_jobs (brief_id) VALUES ($1) RETURNING ` + jobCols
	j, err := scanJob(r.db.QueryRow(ctx, q, briefID))
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

// CreateJobForApprovedBrief inserts a queued job only if the brief is still
// approved at expectedVersion, closing the approve→dispatch TOCTOU race described
// on the port: a concurrent ReplaceBrief (resets to 'draft', version+1) or
// ArchiveBrief ('archived', version+1) committing in the window must prevent the
// job from being created against the now-stale approval.
//
// Isolation reasoning — why a single guarded INSERT ... WHERE EXISTS is NOT
// enough. Under PostgreSQL's default READ COMMITTED, each statement takes a fresh
// snapshot at command start. The EXISTS subquery of a lone INSERT reads that
// snapshot; a concurrent ReplaceBrief/ArchiveBrief that COMMITS between the
// snapshot and the insert's row-visibility check is not seen by the EXISTS (it
// still sees the old approved version), so the job would be created from a
// stale-approved brief. The single-statement atomicity only rules out a commit
// interleaving WITHIN the statement's snapshot — it does not serialize against a
// mutation that commits just before the statement runs but after the caller's
// approval read.
//
// Fix: take a row lock on the brief inside ONE transaction BEFORE the insert.
// SELECT ... FOR UPDATE acquires a row-level exclusive lock on the brief row and
// re-reads its CURRENT committed state (FOR UPDATE always sees the latest
// committed row version, waiting out any in-flight writer that holds it). Any
// concurrent ReplaceBrief/ArchiveBrief UPDATEs campaign_briefs by id (see
// brief_repo.go — all three bump version on the same row), so it must acquire the
// same row lock: it either committed before our FOR UPDATE (then our re-read sees
// the bumped version and the check fails → ErrStaleApproval) or it blocks on our
// lock until this transaction commits the job (then it proceeds afterward, but
// the job was created while the brief was genuinely still approved at the read
// version, which is correct). The check-then-insert is therefore atomic with
// respect to brief mutations. Returns domain.ErrStaleApproval when the locked row
// fails the status/version check (mapped to 409).
func (r *JobRepo) CreateJobForApprovedBrief(ctx context.Context, briefID string, expectedVersion int64) (*model.CampaignJob, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create job for approved brief: begin tx: %w", err)
	}
	// Roll back unless we explicitly commit. A no-op after a successful Commit
	// (pgx returns ErrTxClosed, which we ignore) — this guards every error path.
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the brief row and read its current committed status/version. FOR UPDATE
	// serializes this against a concurrent replace/archive that touches the same
	// row: whichever transaction acquires the lock first runs to completion before
	// the other observes the row, so the check below cannot straddle a commit.
	var (
		status  string
		version int64
	)
	lockQ := `SELECT status, version FROM campaign_briefs WHERE id = $1 FOR UPDATE`
	if serr := tx.QueryRow(ctx, lockQ, briefID).Scan(&status, &version); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			// The brief does not exist at all; treat it as a stale approval (there is
			// nothing approved at expectedVersion to dispatch from).
			return nil, domain.ErrStaleApproval
		}
		return nil, fmt.Errorf("create job for approved brief: lock brief: %w", serr)
	}
	if status != "approved" || version != expectedVersion {
		// A concurrent replace/archive committed before we took the lock (or the
		// brief was never approved at this version). Surface the state-conflict
		// sentinel (not the generic uniqueness ErrConflict) so the service can tell
		// the client to refresh and re-approve rather than reporting "already exists".
		return nil, domain.ErrStaleApproval
	}

	insertQ := `INSERT INTO campaign_jobs (brief_id) VALUES ($1) RETURNING ` + jobCols
	j, err := scanJob(tx.QueryRow(ctx, insertQ, briefID))
	if err != nil {
		return nil, fmt.Errorf("create job for approved brief: insert job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create job for approved brief: commit: %w", err)
	}
	return j, nil
}

// GetJob returns a job by id.
func (r *JobRepo) GetJob(ctx context.Context, projectID, id string) (*model.CampaignJob, error) {
	// Scope the lookup to the caller's project by joining through the owning
	// brief: a job UUID alone must not expose a job belonging to another project
	// (tenant isolation — the route is /projects/{project_id}/jobs/{job_id}).
	q := `SELECT ` + jobColsPrefixed + ` FROM campaign_jobs j
		JOIN campaign_briefs b ON b.id = j.brief_id
		WHERE j.id=$1 AND b.project_id=$2`
	j, err := scanJob(r.db.QueryRow(ctx, q, id, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}

// UpdateJobStatus sets a job's status and result/error.
func (r *JobRepo) UpdateJobStatus(ctx context.Context, id string, status model.JobStatus, result []byte, jobErr string) error {
	q := `UPDATE campaign_jobs SET status=$1, result=$2, error=$3, updated_at=now() WHERE id=$4`
	tag, err := r.db.Exec(ctx, q, string(status), nullBytes(result), nullStr(jobErr), id)
	if err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// FailStuckJobs marks non-terminal jobs older than staleJobCutoff as failed. Run
// on startup AND periodically (see Orchestrator.StartRecoverySweeper): a
// queued/running job's dispatch goroutine lives only in the process that created
// it, so after a crash such a job would otherwise stay non-terminal forever.
// Fail-forward (rather than resume) because a partially-dispatched job cannot be
// safely re-driven without provider-side idempotency keys.
//
// The age cutoff must exceed the longest a LIVE job can go without a status
// write, or the sweep would wrongly fail an in-progress job. The orchestrator
// bounds that: a platform waits at most dispatchQueueTimeout for a slot, then
// the provider call is bounded by providerCallTimeout, then a terminal write
// follows — all well within staleJobCutoff (15m). During a rolling deploy an old
// pod can still be dispatching a recently-created job while a new pod boots, so
// only jobs idle (no update) beyond the cutoff are treated as orphaned.
func (r *JobRepo) FailStuckJobs(ctx context.Context, jobErr string) (int64, error) {
	q := `UPDATE campaign_jobs SET status='failed', error=$1, updated_at=now()
		WHERE status IN ('queued','running')
		  AND updated_at < now() - make_interval(secs => $2)`
	tag, err := r.db.Exec(ctx, q, nullStr(jobErr), staleJobCutoff.Seconds())
	if err != nil {
		return 0, fmt.Errorf("fail stuck jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanJob(row pgx.Row) (*model.CampaignJob, error) {
	var (
		j        model.CampaignJob
		status   string
		jobError *string
	)
	if err := row.Scan(&j.ID, &j.BriefID, &status, &j.Result, &jobError, &j.CreatedAt, &j.UpdatedAt, &j.ExpiresAt); err != nil {
		return nil, err
	}
	j.Status = model.JobStatus(status)
	if jobError != nil {
		j.Error = *jobError
	}
	return &j, nil
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
