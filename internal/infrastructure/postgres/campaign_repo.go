// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// CampaignRepo is a pgx-backed implementation of domain.CampaignRepository.
type CampaignRepo struct {
	db *Pool
}

// claimRollbackTimeout bounds the best-effort rollback of a just-inserted pending
// claim when the follow-up read fails; it runs on a context detached from the
// (possibly-cancelled) request context.
const claimRollbackTimeout = 5 * time.Second

// stuckClaimReportAge is how old a still-'pending' campaign row must be before it is REPORTED
// as stuck. Named for reporting, not staleness: "stale" implies expiry/reclaim semantics that
// this code deliberately does not have, and that wrong assumption is exactly what an earlier
// revision of this change got wrong.
//
// Deliberately NOT auto-reclaimed: 'pending' is overloaded. It marks both a claim that is
// merely in flight AND an AMBIGUOUS dispatch outcome, which the orchestrator persists as
// 'pending' precisely because the provider MAY already have created a paid campaign
// (see the retained-claim path in service.dispatchOne). No column distinguishes the two, so
// a time-based reclaim would eventually authorize a duplicate paid create on a campaign that
// already exists upstream — the exact failure the claim exists to prevent. Safe automatic
// recovery needs provider idempotency keys or an authoritative reconcile first (LFXV2-2665);
// until then a human decides, and this threshold is what tells them there is something to
// decide about.
//
// The value is derived, not guessed: a claim is taken AFTER the dispatch semaphore is
// acquired, and the provider call it wraps is hard-bounded by the orchestrator's
// providerCallTimeout (2m), so a healthy claim never lives this long. The 2x headroom covers
// the post-call persist/finalize write plus replica clock skew.
const stuckClaimReportAge = 4 * time.Minute

// DefaultStuckClaimLimit bounds StuckDispatchClaims when a caller passes no limit, so a
// diagnostic query can never scan or allocate without an upper bound. Exported so a caller
// can tell whether a returned batch SATURATED the cap (i.e. more rows exist).
const DefaultStuckClaimLimit = 100

// NewCampaignRepo returns a CampaignRepo backed by pool.
func NewCampaignRepo(pool *Pool) *CampaignRepo { return &CampaignRepo{db: pool} }

var _ domain.CampaignRepository = (*CampaignRepo)(nil)

// claimCampaignDispatchQuery inserts the placeholder 'pending' claim row.
//
// The conflict target carries the partial index's predicate (`WHERE status <>
// 'deleted'`) because 000013/000014 replaced the full UNIQUE (brief_id, platform)
// constraint with a partial unique index over LIVE rows only. Postgres infers the
// arbiter index by matching the conflict target AND its predicate, so a bare
// `ON CONFLICT (brief_id, platform)` now matches no index and fails at runtime with
// "there is no unique or exclusion constraint matching the ON CONFLICT
// specification". The predicate is also what makes a soft-deleted campaign's slot
// reusable: a deleted row sits outside the index, so it does not conflict and this
// INSERT wins the claim cleanly. Pinned by
// TestCampaignRepo_OnConflictCarriesLivePredicate.
const claimCampaignDispatchQuery = `INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status)
	VALUES ($1, $2, $3, $4, '', 'pending')
	ON CONFLICT (brief_id, platform) WHERE status <> 'deleted' DO NOTHING`

// ClaimCampaignDispatch atomically claims the right to dispatch (brief, platform)
// by inserting a placeholder 'pending' campaign row. The (brief_id, platform)
// unique index makes the claim single-winner across all replicas without holding
// a connection or a blocking lock.
//
// The claim is held until explicitly released and is NOT reclaimed on a timer — see
// stuckClaimReportAge for why a time-based takeover would be unsafe. The consequence is that a
// dispatcher which dies between claiming and releasing strands a 'pending' row that blocks
// that (brief, platform) until a human intervenes; StuckDispatchClaims surfaces those rows.
//
// RowsAffected()==1 means this caller won the claim; 0 means the pair is already claimed or
// already has a campaign. No RETURNING is used because ON CONFLICT DO NOTHING returns no row
// on conflict, so we detect the winner via RowsAffected and then read the current row.
func (r *CampaignRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, platform model.Provider, jobID string) (bool, *model.Campaign, error) {
	tag, err := r.db.Exec(ctx, claimCampaignDispatchQuery, projectID, briefID, jobID, string(platform))
	if err != nil {
		return false, nil, fmt.Errorf("claim campaign dispatch: %w", err)
	}
	claimed := tag.RowsAffected() == 1

	row, gerr := r.GetCampaignByPlatform(ctx, projectID, briefID, platform)
	if gerr != nil {
		// The row must exist now (we or someone else just wrote it); a read failure
		// here is a genuine error. If WE just inserted the pending row, roll it back
		// (best effort) so a failed claim doesn't leave a pending row that blocks the
		// pair forever; report claimed=false so the caller treats it as a clean
		// failure with nothing to release.
		if claimed {
			// Roll back on a context detached from ctx: the read likely failed
			// BECAUSE ctx was cancelled, and reusing it for the DELETE would fail
			// too, leaking the just-committed placeholder.
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimRollbackTimeout)
			if derr := r.DeleteDispatchClaim(rbCtx, briefID, platform); derr != nil {
				cancel()
				// Double failure: both the post-insert read AND the rollback delete
				// failed, so a 'pending' placeholder is orphaned and will block every
				// future claim for this (brief, platform) — no sweeper reaps pending
				// campaigns rows. This is a rare double-fault, but its blast radius is
				// total for the pair, so log at ERROR with enough context to alert and
				// reconcile manually (delete the stuck row) rather than swallowing it.
				slog.ErrorContext(ctx, "orphaned pending campaign claim: read-after-claim AND rollback both failed; manual cleanup required",
					"project_id", projectID, "brief_id", briefID, "platform", string(platform), "job_id", jobID,
					"read_err", gerr.Error(), "rollback_err", derr.Error())
				return false, nil, fmt.Errorf("read campaign after claim: %w (and failed to roll back pending claim: %v)", gerr, derr)
			}
			cancel()
		}
		return false, nil, fmt.Errorf("read campaign after claim: %w", gerr)
	}
	return claimed, row, nil
}

// StuckDispatchClaims returns 'pending' campaign rows older than stuckClaimReportAge, OLDEST
// first. It is READ-ONLY: nothing here reclaims, deletes, or redispatches.
//
// Cardinality: it returns up to limit+1 rows, NOT limit. The extra row is a deliberate
// truncation probe — receiving limit+1 is how a caller distinguishes "exactly limit are stuck"
// from "at least limit are stuck and the true total is unknown and larger", which matters
// because a flat count would understate a crash-looping incident. Callers must therefore treat
// len(result) > limit as the truncated signal and slice back to limit before reporting a count
// (see scanStuckDispatchClaims in internal/container). Passing limit <= 0 uses
// DefaultStuckClaimLimit, so the probe row makes that case return up to
// DefaultStuckClaimLimit+1.
//
// Oldest-first is deliberate and interacts with the cap: a row seconds past the threshold may
// still be a slow-but-live owner, whereas one stuck for days is unambiguously dead and is what
// an operator should clear first. Newest-first would invert that triage order AND, once the
// cap saturates, silently drop the longest-stuck rows — the worst cases — entirely.
//
// It exists because a stuck claim is otherwise INVISIBLE. A pod that crashes or is evicted
// between claiming and releasing strands a 'pending' row, and since the claim is
// ON CONFLICT (brief_id, platform) that row blocks every future dispatch for the pair — with
// no signal anywhere that it happened. Operators currently discover it only when someone
// reports that a campaign will not dispatch.
//
// Reporting rather than auto-reclaiming is deliberate: see stuckClaimReportAge. A row returned here
// may be an abandoned claim (safe to delete) OR an ambiguous outcome where a paid campaign
// already exists upstream (deleting it would authorize a duplicate). Distinguishing them
// requires checking the ad platform, so a human decides until provider idempotency or
// reconcile lands (LFXV2-2665).
func (r *CampaignRepo) StuckDispatchClaims(ctx context.Context, limit int) ([]*model.Campaign, error) {
	if limit <= 0 {
		limit = DefaultStuckClaimLimit
	}
	// limit+1 so the caller can tell "exactly limit rows" from "at least limit, truncated".
	// A bad deploy that crash-loops mid-dispatch is exactly the incident where reporting a
	// flat count=100 would understate a possibly-thousands-row outage.
	// make_interval(secs => $1), NOT $1::interval with a Go duration string. Postgres does
	// accept the "4m0s" Go renders for this particular value, so this is not a bug fix at
	// the current constant — it removes a standing dependency on Go's duration formatting
	// happening to match Postgres's interval grammar, which nothing enforces and which does
	// diverge: Go renders sub-microsecond durations as "100ns" and microseconds with a
	// Unicode mu ("1µs"), both of which Postgres rejects outright, and it truncates
	// nanosecond precision ("1.000000001s" parses as 1s). Changing stuckClaimReportAge to
	// such a value would break the scan at runtime, not at compile time. Binding numeric
	// seconds sidesteps the grammar entirely and matches JobRepo.FailStuckJobs.
	q := `SELECT ` + campaignCols + ` FROM campaigns
		WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at ASC
		LIMIT $2`
	rows, err := r.db.Query(ctx, q, stuckClaimReportAge.Seconds(), limit+1)
	if err != nil {
		return nil, fmt.Errorf("list stuck dispatch claims: %w", err)
	}
	defer rows.Close()

	var out []*model.Campaign
	for rows.Next() {
		c, serr := scanCampaign(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan stuck dispatch claim: %w", serr)
		}
		out = append(out, c)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate stuck dispatch claims: %w", rerr)
	}
	return out, nil
}

// DeleteDispatchClaim removes a still-'pending' claim row so a failed dispatch
// doesn't permanently block the (brief, platform) pair. The status guard means
// it can only ever delete a placeholder claim, never a created campaign.
func (r *CampaignRepo) DeleteDispatchClaim(ctx context.Context, briefID string, platform model.Provider) error {
	q := `DELETE FROM campaigns WHERE brief_id=$1 AND platform=$2 AND status='pending'`
	if _, err := r.db.Exec(ctx, q, briefID, string(platform)); err != nil {
		return fmt.Errorf("delete dispatch claim: %w", err)
	}
	return nil
}

const campaignCols = `id::text, project_id::text, brief_id::text, job_id::text, platform, platform_campaign_id, campaign_name,
	status, budget_amount, budget_type, start_date, end_date, config_snapshot, result, version,
	created_at, updated_at`

// getCampaignQuery and getCampaignByPlatformQuery both exclude soft-deleted rows;
// pinned by TestCampaignRepo_ReadsExcludeSoftDeleted.
const getCampaignQuery = `SELECT ` + campaignCols + ` FROM campaigns
	WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND status <> 'deleted'`

// getCampaignByPlatformQuery's exclusion of deleted rows is load-bearing for
// dispatch, not just cosmetic: the orchestrator uses this lookup to decide whether a
// (brief, platform) pair has ALREADY been dispatched. A deleted campaign's slot is
// free by design, so it must read as "not dispatched" — otherwise the idempotency
// check would see the deleted row and refuse the re-dispatch that deleting the
// campaign exists to enable. At most one LIVE row can match (the partial unique
// index), so this still returns at most one row.
const getCampaignByPlatformQuery = `SELECT ` + campaignCols + ` FROM campaigns
	WHERE brief_id=$1 AND platform=$2 AND project_id=$3 AND status <> 'deleted'`

// GetCampaign returns a single campaign under a brief. Soft-deleted campaigns are
// invisible to reads: a deleted campaign returns ErrNotFound (404), matching
// domain.ErrNotFound's contract ("does not exist, or has been soft-deleted") and
// the brief layer, where an archived brief is likewise unreadable. The row is
// retained in the table for audit and for its platform_campaign_id, not for serving.
func (r *CampaignRepo) GetCampaign(ctx context.Context, projectID, briefID, id string) (*model.Campaign, error) {
	c, err := scanCampaign(r.db.QueryRow(ctx, getCampaignQuery, id, briefID, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return c, nil
}

// GetCampaignByPlatform returns the campaign for a (brief, platform) pair. The
// (brief_id, platform) pair is unique, so at most one row matches. It is scoped by
// project_id for tenant isolation (defense-in-depth), matching GetCampaign and
// ClaimCampaignDispatch — brief_id is a globally-unique UUID, so this guards a
// future direct caller from reading across tenants with an attacker-influenced
// briefID.
func (r *CampaignRepo) GetCampaignByPlatform(ctx context.Context, projectID, briefID string, platform model.Provider) (*model.Campaign, error) {
	c, err := scanCampaign(r.db.QueryRow(ctx, getCampaignByPlatformQuery, briefID, string(platform), projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get campaign by platform: %w", err)
	}
	return c, nil
}

// upsertCampaignQuery's conflict target carries the partial index's predicate — see
// claimCampaignDispatchQuery for why it is mandatory. A soft-deleted row sits outside
// the index, so an upsert after a delete INSERTs a fresh campaign rather than
// resurrecting the deleted one; the deleted row is preserved as the audit trail of
// whatever may still exist upstream.
const upsertCampaignQuery = `INSERT INTO campaigns
	(project_id, brief_id, job_id, platform, platform_campaign_id, campaign_name, status,
	 budget_amount, budget_type, start_date, end_date, config_snapshot, result)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	ON CONFLICT (brief_id, platform) WHERE status <> 'deleted' DO UPDATE SET
		job_id=EXCLUDED.job_id, platform_campaign_id=EXCLUDED.platform_campaign_id,
		campaign_name=EXCLUDED.campaign_name, status=EXCLUDED.status,
		budget_amount=EXCLUDED.budget_amount, budget_type=EXCLUDED.budget_type,
		start_date=EXCLUDED.start_date, end_date=EXCLUDED.end_date,
		config_snapshot=EXCLUDED.config_snapshot, result=EXCLUDED.result,
		version=campaigns.version+1, updated_at=now()
	RETURNING ` + campaignCols

// replaceCampaignQuery refuses to touch a soft-deleted row, so a stale client cannot
// mutate (and effectively resurrect) a campaign that has been deleted.
const replaceCampaignQuery = `UPDATE campaigns SET
	campaign_name=$1, status=$2, budget_amount=$3, budget_type=$4, start_date=$5, end_date=$6,
	config_snapshot=$7, result=$8, version=version+1, updated_at=now()
	WHERE id=$9 AND brief_id=$10 AND project_id=$11 AND version=$12
	  AND status <> 'deleted'`

// UpsertCampaign inserts or updates the (brief, platform) campaign row. On
// conflict it updates in place (a brief change after campaigns exist).
func (r *CampaignRepo) UpsertCampaign(ctx context.Context, c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert campaign: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, upsertCampaignQuery,
		c.ProjectID, c.BriefID, c.JobID, string(c.Platform), nullStr(c.PlatformCampaignID),
		c.CampaignName, c.Status, c.BudgetAmount, budgetTypeArg(c.BudgetType),
		c.StartDate, c.EndDate, nullJSON(c.ConfigSnapshot), nullJSON(c.Result),
	)
	upserted, err := scanCampaign(row)
	if err != nil {
		return nil, fmt.Errorf("upsert campaign: %w", err)
	}
	if eerr := enqueueCampaignIndex(ctx, tx, upserted, indexPayload); eerr != nil {
		return nil, fmt.Errorf("upsert campaign: %w", eerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("upsert campaign: commit: %w", cerr)
	}
	return upserted, nil
}

// enqueueCampaignIndex writes a campaign's index message to the outbox inside tx.
//
// EVERY campaign write co-commits, exactly as the brief writes do. Mixing paths does not work:
// while creates went through the outbox and updates published directly, a replayed create could
// land AFTER a newer update or status toggle and overwrite it in the index, leaving search stale
// until some later write happened to repair it. One ordered sequence per row removes that.
//
// It also removes a second hazard specific to campaigns: creation is ASYNC, so the caller's JWT
// could be EXPIRED by publish time and the message rejected outright, with no row to retry.
//
// A nil builder means the caller does not want this write indexed; the write still commits.
func enqueueCampaignIndex(ctx context.Context, tx pgx.Tx, c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) error {
	if indexPayload == nil {
		return nil
	}
	payload, err := indexPayload(c)
	if err != nil {
		return fmt.Errorf("build index payload: %w", err)
	}
	return enqueueIndexMessage(ctx, tx, indexObjectTypeCampaign, c.ID, payload)
}

// indexObjectTypeCampaign mirrors indexer.ObjectTypeCampaign. Duplicated rather than imported
// for the same reason as indexObjectTypeBrief: the postgres package must not depend on the
// indexer. Pinned by a test.
const indexObjectTypeCampaign = "campaign"

// ReplaceCampaign replaces mutable fields, gating on expectedVersion.
func (r *CampaignRepo) ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	// RETURNING keeps the write and the snapshot that gets indexed in ONE statement, so the
	// outbox payload is exactly what committed. The prior implementation re-read via
	// GetCampaign after the UPDATE, which could observe a LATER concurrent write and index a
	// snapshot this call never produced.
	q := `UPDATE campaigns SET
		campaign_name=$1, status=$2, budget_amount=$3, budget_type=$4, start_date=$5, end_date=$6,
		config_snapshot=$7, result=$8, version=version+1, updated_at=now()
		WHERE id=$9 AND brief_id=$10 AND project_id=$11 AND version=$12
		RETURNING ` + campaignCols
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("replace campaign: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	updated, err := scanCampaign(tx.QueryRow(ctx, q,
		c.CampaignName, c.Status, c.BudgetAmount, budgetTypeArg(c.BudgetType), c.StartDate, c.EndDate,
		nullJSON(c.ConfigSnapshot), nullJSON(c.Result), c.ID, c.BriefID, c.ProjectID, expectedVersion,
	))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("replace campaign: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish missing from stale version THROUGH THIS TRANSACTION. Calling GetCampaign
		// would acquire a SECOND pool connection while this one still holds the first — with a
		// saturated pool (pool_max_conns=1 makes it certain) an ordinary stale-version request
		// would block until its context expired instead of returning 412.
		var exists bool
		eq := `SELECT EXISTS (SELECT 1 FROM campaigns WHERE id = $1 AND brief_id = $2 AND project_id = $3)`
		if serr := tx.QueryRow(ctx, eq, c.ID, c.BriefID, c.ProjectID).Scan(&exists); serr != nil {
			// Surface a transient read error rather than masking it as a precondition failure,
			// which would make the caller retry with a fresh ETag instead of backing off.
			return nil, fmt.Errorf("replace campaign: classify guarded update: %w", serr)
		}
		if !exists {
			return nil, domain.ErrNotFound
		}
		return nil, domain.ErrPreconditionFailed
	}
	if eerr := enqueueCampaignIndex(ctx, tx, updated, indexPayload); eerr != nil {
		return nil, fmt.Errorf("replace campaign: %w", eerr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("replace campaign: commit: %w", cerr)
	}
	return updated, nil
}

// deleteCampaignLockQuery takes the row lock and reads the state the guards check.
// FOR UPDATE is required, not decorative: see DeleteCampaign's isolation reasoning.
// Pinned by TestDeleteCampaign_LocksRowBeforeGuards.
const deleteCampaignLockQuery = `SELECT status, version FROM campaigns
	WHERE id=$1 AND brief_id=$2 AND project_id=$3 FOR UPDATE`

// deleteCampaignQuery performs the SOFT delete. It must never become a DELETE FROM:
// the row holds platform_campaign_id, the only local pointer to a campaign that may
// still exist and still be spending upstream. Pinned by
// TestDeleteCampaign_IsSoftDelete.
const deleteCampaignQuery = `UPDATE campaigns SET status='deleted', version=version+1, updated_at=now()
	WHERE id=$1`

// DeleteCampaign soft-deletes a campaign (status = 'deleted'), gating on
// expectedVersion. The row is RETAINED: it carries platform_campaign_id, the only
// local record of a campaign that may still exist — and still be spending — on the
// ad platform. Hard-deleting would destroy both the audit trail and the sole
// pointer needed to reconcile it upstream. The partial unique index from 000013
// excludes deleted rows, so this frees the (brief_id, platform) slot for a
// legitimate re-dispatch.
//
// Isolation reasoning — why this needs a transaction and a row lock rather than a
// single guarded UPDATE. A 'pending' campaign is an ACTIVE dispatch claim: an
// in-flight dispatch owns it and is about to write the real campaign (and may have
// already created a paid campaign upstream). Deleting it would free the slot while
// that dispatch is still running, letting a concurrent claim win the same pair and
// double-create upstream — and the finalizing dispatch would then UPDATE a row it
// no longer exclusively owns. So 'pending' must be refused.
//
// A lone `UPDATE ... WHERE status <> 'pending'` does NOT close this race. Under
// PostgreSQL's default READ COMMITTED each statement takes a fresh snapshot at
// command start, so a concurrent ClaimCampaignDispatch that COMMITS its 'pending'
// insert just before this statement runs is invisible to the predicate's snapshot;
// worse, the claim INSERTs a new row rather than updating this one, so there is no
// row-level conflict for Postgres to serialize on at all.
//
// Fix: SELECT ... FOR UPDATE inside one transaction. The lock re-reads the row's
// CURRENT committed state (FOR UPDATE always sees the latest committed version,
// waiting out any in-flight writer holding it) and serializes this against the
// dispatch path, which UPDATEs the same row by id when it finalizes — that writer
// either committed before our lock (our re-read then sees its real status, not a
// stale 'pending') or blocks until we commit. The status/version check and the
// soft delete are therefore atomic with respect to dispatch.
//
// Returns domain.ErrNotFound if the campaign is absent or already deleted,
// domain.ErrConflict if its status is an unresolved reconciliation marker (a
// mid-dispatch 'pending' claim, or a 'group_created'/'unconfirmed' partial orphan —
// see model.CampaignStatusNeedsReconciliation), and domain.ErrPreconditionFailed on a
// version mismatch.
func (r *CampaignRepo) DeleteCampaign(ctx context.Context, projectID, briefID, id string, expectedVersion int64, indexPayload domain.CampaignIndexPayloadFunc) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete campaign: begin tx: %w", err)
	}
	// Roll back unless we explicitly commit. A no-op after a successful Commit
	// (pgx returns ErrTxClosed, which we ignore) — this guards every error path.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		status  string
		version int64
	)
	if serr := tx.QueryRow(ctx, deleteCampaignLockQuery, id, briefID, projectID).Scan(&status, &version); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("delete campaign: lock campaign: %w", serr)
	}
	// Already deleted reads as absent, matching GetCampaign — a second DELETE of the
	// same campaign is a 404, not a silent success that would imply it deleted
	// something.
	if status == model.CampaignStatusDeleted {
		return domain.ErrNotFound
	}
	// Refuse to retire a row whose status is an unresolved reconciliation marker:
	// 'pending' (a live dispatch claim, or one that died mid-flight) and the partial
	// orphans 'group_created'/'unconfirmed'. Soft-deleting overwrites status with
	// 'deleted', which both erases the only local record of WHAT went wrong and frees
	// the (brief, platform) slot — so a re-dispatch then creates a fresh campaign with
	// no indication that a half-created one may already exist upstream. The orphan must
	// be reconciled first. This mirrors the run-state toggle's refusal to act on an
	// unreconciled row (see CampaignStatusNeedsReconciliation).
	//
	// 'created_degraded' is deliberately NOT included: it means the campaign WAS fully
	// created upstream, so its row is a complete record and retiring it loses nothing.
	if model.CampaignStatusNeedsReconciliation(status) {
		return domain.ErrConflict
	}
	// Version is checked AFTER the state guards so a caller holding a stale ETag for
	// a mid-dispatch campaign gets the actionable "it is dispatching" conflict rather
	// than a 412 that suggests a simple reload would fix it.
	if version != expectedVersion {
		return domain.ErrPreconditionFailed
	}

	if _, uerr := tx.Exec(ctx, deleteCampaignQuery, id); uerr != nil {
		return fmt.Errorf("delete campaign: soft delete: %w", uerr)
	}
	// Enqueue the deletion to the index, just as every other write does. A nil
	// indexPayload means the caller does not want this delete indexed; the write
	// still commits. This ensures the search index stays in sync: a campaign that
	// was previously indexed must be removed when soft-deleted.
	if indexPayload != nil {
		deleted := &model.Campaign{
			ID:        id,
			BriefID:   briefID,
			ProjectID: projectID,
			Status:    model.CampaignStatusDeleted,
		}
		if ierr := enqueueCampaignIndex(ctx, tx, deleted, indexPayload); ierr != nil {
			return fmt.Errorf("delete campaign: %w", ierr)
		}
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return fmt.Errorf("delete campaign: commit: %w", cerr)
	}
	return nil
}

func scanCampaign(row pgx.Row) (*model.Campaign, error) {
	var (
		c          model.Campaign
		platform   string
		pcID       *string
		budgetType *string
	)
	if err := row.Scan(
		&c.ID, &c.ProjectID, &c.BriefID, &c.JobID, &platform, &pcID, &c.CampaignName,
		&c.Status, &c.BudgetAmount, &budgetType, &c.StartDate, &c.EndDate,
		&c.ConfigSnapshot, &c.Result, &c.Version, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	c.Platform = model.Provider(platform)
	if pcID != nil {
		c.PlatformCampaignID = *pcID
	}
	if budgetType != nil {
		bt := model.BudgetType(*budgetType)
		c.BudgetType = &bt
	}
	return &c, nil
}

func budgetTypeArg(bt *model.BudgetType) any {
	if bt == nil {
		return nil
	}
	return string(*bt)
}
