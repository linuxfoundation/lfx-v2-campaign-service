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

// DefaultJobRetention is how long a TERMINAL campaign job is kept before pruning.
//
// Deliberately long. A campaign job is the audit trail of real ad spend: it records that a
// brief was dispatched, to which platforms, and what each one returned. Deleting one destroys
// the only in-service record of a paid campaign creation, so the default errs heavily toward
// keeping it — 180 days outlives a quarterly spend review and any realistic
// "why was this campaign created?" investigation, while still bounding a table that is
// otherwise append-only and grows with every dispatch forever.
//
// Operators may shorten it via CAMPAIGN_JOB_RETENTION, but the default must never be the
// aggressive choice: a deployment that never sets the variable is the common case, and the
// cost of keeping a row too long is disk, while the cost of deleting one too early is an
// unrecoverable gap in the spend record.
const DefaultJobRetention = 180 * 24 * time.Hour

// jobPrunePassLimit bounds ONE delete so a first prune over a large backlog cannot hold a long
// transaction, lock the table against live dispatch writes, or spike replication lag. The
// sweeper prunes every pass, so a backlog drains over several passes rather than in one
// statement.
const jobPrunePassLimit = 1000

// terminalJobStatuses is the ALLOW-LIST of statuses a prune may delete.
//
// An allow-list, never `status NOT IN ('queued','running')` or `status != 'running'`. A future
// status added to the CHECK constraint (a 'cancelled', a 'retrying') would be swept in silently
// by any negative predicate — deleting records of real money spent, with no code change and no
// review. Adding one here is a deliberate act; forgetting to is merely conservative, which is
// the correct direction for this table.
//
// Must stay identical to the terminal arm of model.JobStatus.Terminal(); pinned by
// TestTerminalJobStatusesMatchTheDomainVocabulary.
var terminalJobStatuses = []string{
	string(model.JobSucceeded),
	string(model.JobPartial),
	string(model.JobFailed),
}

// pruneTerminalJobsQuery deletes TERMINAL job history only.
//
// queued/running rows are NEVER eligible, at any age. A non-terminal row that is old is not
// stale history — it is a STUCK JOB, and a stuck job is exactly the record someone needs to
// investigate why a dispatch never finished. (The recovery sweeper transitions those to
// 'failed' after staleJobCutoff, at which point they become terminal and start their retention
// window from that transition — so nothing is retained forever, but nothing is deleted while it
// still looks live either.)
//
// Age is measured on updated_at, the moment the job REACHED its terminal state, not created_at.
// A job created months ago but completed yesterday is recent history and must not be pruned on
// the strength of its creation date.
//
// Deleting by id via a bounded, ordered subquery keeps the statement short and its row set
// predictable. The partial index added in migration 000026 (updated_at, WHERE status IN the
// three terminal values) serves the inner SELECT directly; without it this is a full scan of
// the very history the prune exists to bound.
//
// No FOR UPDATE SKIP LOCKED, unlike the outbox DRAIN. The drain needs it because it claims rows
// it will then publish — an at-most-once side effect outside the transaction, so two pods
// claiming the same row would double-publish. A DELETE has no such side effect: it takes its
// own row locks, and if two replicas' sweeps overlap, the second simply finds fewer rows and
// deletes fewer. The outbox PRUNE (pruneQuery) is the right comparison and it does not use
// SKIP LOCKED either. What multi-replica safety requires here is only that the statement be
// bounded and idempotent, which LIMIT + "delete what is already terminal and old" both are.
const pruneTerminalJobsQuery = `DELETE FROM campaign_jobs WHERE id IN (
	SELECT id FROM campaign_jobs
	WHERE status = ANY($1::text[]) AND updated_at < now() - $2::interval
	ORDER BY updated_at
	LIMIT $3
)`

// PruneTerminalJobs deletes TERMINAL jobs whose terminal state is older than olderThan,
// returning the number of rows removed. Non-terminal jobs are never eligible — see
// pruneTerminalJobsQuery.
//
// Without this campaign_jobs grows with EVERY brief dispatch and never shrinks. It also makes
// the stuck-job recovery sweep progressively more expensive: that sweep's partial index covers
// only queued/running rows, but the table it lives on keeps growing, and terminal history is
// pure ballast for every plan that touches it.
//
// A zero or negative olderThan falls back to DefaultJobRetention rather than pruning
// everything: a mis-parsed or unset configuration value must not be readable as "retain
// nothing". Same for limit and jobPrunePassLimit.
func (r *JobRepo) PruneTerminalJobs(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	if olderThan <= 0 {
		olderThan = DefaultJobRetention
	}
	if limit <= 0 {
		limit = jobPrunePassLimit
	}
	tag, err := r.db.Exec(ctx, pruneTerminalJobsQuery, terminalJobStatuses, olderThan.String(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune terminal jobs: %w", err)
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
