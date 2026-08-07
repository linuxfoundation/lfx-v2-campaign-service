// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
// claimCampaignVersionQuery reads the campaign at an expected version under the
// advisory lock, without bumping it (ReplaceCampaign does that, co-committed with the
// outbox message).
//
// `status <> 'deleted'` is not decoration here, it is what keeps the claim honest. A
// soft-deleted campaign reads as ABSENT everywhere else — getCampaignQuery,
// getCampaignByPlatformQuery, and replaceCampaignQuery all carry this predicate.
// Without it, a claim on a deleted row whose version happens to match SUCCEEDS, the
// caller goes on to make its paid platform call, and only then does ReplaceCampaign
// (which does exclude deleted rows) fail — leaving the campaign mutated upstream with
// no local record of it. That is the same failure the advisory-lock protocol exists to
// prevent, reached from the other direction.
//
// Declared as a named constant rather than inlined so TestCampaignRepo_ReadsExcludeSoftDeleted
// can hold it to the predicate along with every other campaigns read.
const claimCampaignVersionQuery = `SELECT ` + campaignCols + ` FROM campaigns
	WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND version=$4 AND status <> 'deleted'`

// claimCampaignExistsQuery classifies a no-rows claim as 404 vs 412, and MUST use the
// same liveness predicate as claimCampaignVersionQuery. If the two disagree a
// soft-deleted row counts as "exists" and a correct 404 becomes a 412 — telling the
// caller to reload and retry a campaign that is gone for good.
const claimCampaignExistsQuery = `SELECT EXISTS (
	SELECT 1 FROM campaigns WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND status <> 'deleted')`

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

// replaceCampaignExistsQuery classifies a guarded ReplaceCampaign UPDATE that matched no row.
// status <> 'deleted' mirrors replaceCampaignQuery's own guard (and GetCampaign/DeleteCampaign):
// a soft-deleted row is treated as absent, so a replace against it reports 404 rather than 412.
// Pinned by TestCampaignRepo_ReadsExcludeSoftDeleted.
const replaceCampaignExistsQuery = `SELECT EXISTS (SELECT 1 FROM campaigns WHERE id = $1 AND brief_id = $2 AND project_id = $3 AND status <> 'deleted')`

// ReplaceCampaign replaces mutable fields, gating on expectedVersion. lockToken is the
// CampaignLockToken returned by ClaimCampaignVersion when the caller holds the claim lock for
// this campaign (the zero token otherwise); see the connection-reuse comment below.
func (r *CampaignRepo) ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64, lockToken domain.CampaignLockToken, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	// RETURNING keeps the write and the snapshot that gets indexed in ONE statement, so the
	// outbox payload is exactly what committed. The prior implementation re-read via
	// GetCampaign after the UPDATE, which could observe a LATER concurrent write and index a
	// snapshot this call never produced.
	//
	// This reuses replaceCampaignQuery (rather than a second, independently-maintained copy)
	// so the soft-delete guard it documents is the guard actually executed here, not just the
	// one a test happens to inspect.
	q := replaceCampaignQuery + `
		RETURNING ` + campaignCols
	// If the caller holds the claim lock for this campaign, the write MUST run on that
	// SAME pooled connection rather than acquiring a second one from the pool: the lock
	// holder pins one connection for the whole claim window, so opening a second here
	// would need a second connection from the pool at the same time. Under a small or
	// saturated pool (pool_max_conns=1 makes it certain) that starves this write behind
	// its own lock holder, blocking every toggle until persistResultTimeout.
	//
	// Use the caller's OWN claimed connection (via lockToken), never activeCampaignLocks.Load
	// by campaign ID: if this caller's session died and a successor's ClaimCampaignVersion
	// already overwrote the map entry, a lookup-by-ID would silently attach this write to the
	// SUCCESSOR's connection instead of this caller's — corrupting the mutual exclusion the
	// lock exists to provide (both writers' effects could land, or one write silently uses the
	// wrong session). lockToken pins the exact connection this caller owns.
	var beginner interface {
		Begin(context.Context) (pgx.Tx, error)
	} = r.db
	if lock, ok := lockToken.Handle().(*campaignLock); ok && lock != nil {
		beginner = lock.conn
	}
	tx, err := beginner.Begin(ctx)
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
		if serr := tx.QueryRow(ctx, replaceCampaignExistsQuery, c.ID, c.BriefID, c.ProjectID).Scan(&exists); serr != nil {
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

var activeCampaignLocks sync.Map // maps campaignID to *campaignLock

// campaignLock holds the session-scoped advisory lock for a campaign.
type campaignLock struct {
	conn    *pgxpool.Conn
	lockKey int64
	// released guards conn/lockKey disposal so a second ReleaseCampaignLock call with
	// this SAME token (e.g. a caller's explicit release racing the UNCONFIRMED cooldown
	// path, or any other duplicate release) is a true no-op instead of re-unlocking and
	// re-releasing a connection the pool may have already handed to a different caller.
	released atomic.Bool
}

// ClaimCampaignVersion gates a writer's exclusive access to a campaign version,
// enforcing that only one writer can win a given expectedVersion. It acquires an
// advisory lock on a dedicated connection and returns the campaign row at the
// current version; the caller is responsible for gating all mutations on this
// version number until the lock is released.
//
// Serialization is enforced via Postgres advisory locks held on a dedicated
// connection, which prevents concurrent writers from claiming and executing
// side-effecting calls (e.g., platform toggling) on the same campaign row
// simultaneously. Only the lock holder can proceed to call external services or
// perform ReplaceCampaign with the claimed version; any other caller attempting
// to claim while a lock is held will wait for it to be released.
//
// The lock is stored in a package-level map and MUST be released via
// ReleaseCampaignLock to unblock other writers. Failure to release will strand
// the session lock.
//
// CRITICAL: the version bump itself happens in ReplaceCampaign, not in this claim.
// This keeps the version increment inside the outbox transaction, preserving the
// invariant that EVERY campaign write co-commits its index event (see
// campaign_repo.go:273-278).
//
// DURABILITY BOUNDARY — read this before treating the lock as ownership. A session
// advisory lock lives only as long as its connection: a failover, a pool eviction, or a
// severed TCP connection releases it server-side while the holder is still inside its
// external platform call. A successor can then claim the SAME version and issue a second
// platform call. The lock is therefore a CONTENTION guard (it makes the common case one
// writer at a time), not durable ownership.
//
// What is durable is the compare-and-swap in replaceCampaignQuery: `WHERE ... version=$12`
// with `version=version+1`. Whichever writer commits first bumps the version; the other's
// ReplaceCampaign matches zero rows and surfaces ErrPreconditionFailed, so a lost lock can
// never produce two persisted writes at the same version, two outbox rows, or a stale
// overwrite. TestClaimVersionIsBackedByACompareAndSwap pins that predicate.
//
// In the specific case where the lock was lost because the CONNECTION died, the CAS is not
// even reached by the first writer: ReplaceCampaign begins its transaction on the claimant's
// own connection (taken from the lock token — see the reasoning at the `beginner` selection
// above), which is the dead one, so that writer's persist fails outright and only the
// successor reaches the row. The CAS covers every other route to two writers at one version.
//
// The residual exposure is exactly one duplicated platform call, and every guarded
// mutation today is DECLARATIVE (set status to active/paused) rather than incremental, so
// a repeat converges on the same upstream state instead of compounding. That is a
// deliberate trade, not an oversight: making ownership itself durable needs a lease row
// with expiry and reconciliation semantics — a design change, tracked separately, not
// something to graft onto this claim.
//
// POOL COST — the second thing to know before adding callers. The claim checks a connection
// out of the SERVICE-WIDE pool and holds it for the whole guarded operation: the platform
// call (up to 45s) plus, on an UNCONFIRMED result, unconfirmedLockCooldown after that. It is
// therefore not free to spread this claim over more code paths. Concurrent guarded writes —
// on different campaigns, or waiters queued behind the same advisory key — consume one pool
// slot each, and enough of them starve ordinary reads, readiness probes, and every other
// write in the process.
//
// The two callers today cost very different amounts, and the cheap one is why the hold time
// above is stated as an upper bound rather than a typical one. service.ToggleCampaignStatus
// is the expensive one: it holds the claim ACROSS the ad-platform call, which is where the
// 45s and the UNCONFIRMED cooldown come from. service.UpdateCampaign claims too — it must,
// or a name/config edit could take the version a toggle already reserved — but it performs
// no I/O between the claim and ReplaceCampaign, so its hold is a single local round-trip.
// What bounds the concurrent claim count is that both are one operation per campaign per
// request; it is not bounded by request volume, and it is not bounded by anything
// structural. Making that bound structural instead of incidental
// (a reserved sub-pool, or a durable lease that holds no connection across external I/O)
// belongs to the same tracked design change as durable ownership above; a new caller added
// before then must be weighed against pool capacity, not just against correctness.
func (r *CampaignRepo) ClaimCampaignVersion(ctx context.Context, projectID, briefID, campaignID string, expectedVersion int64) (*model.Campaign, domain.CampaignLockToken, error) {
	// Acquire a dedicated connection from the pool. This connection is held for the
	// duration of the claim and must be released by ReleaseCampaignLock on the same
	// session (advisory locks are connection-scoped).
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return nil, domain.CampaignLockToken{}, fmt.Errorf("acquire connection for claim: %w", err)
	}

	// Acquire an advisory lock on the dedicated connection. Use a deterministic
	// hash of the campaign ID so the same campaign always gets the same lock key.
	lockKey := int64(hashCampaignID(campaignID))
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		// pg_advisory_lock itself can be reported as failed to the client (e.g. the request
		// context is cancelled mid-call) while Postgres actually granted the lock server-side.
		// A plain conn.Release() would return that connection to the pool with the lock still
		// held on its session — permanently stranding it, since a session advisory lock is NOT
		// released just by returning the connection. Destroy the connection instead, mirroring
		// the pattern used below when the guarded read fails after a successful lock acquire.
		closeLockConn(ctx, conn, campaignID, "failed to close campaign connection after advisory lock acquire error")
		conn.Release()
		return nil, domain.CampaignLockToken{}, fmt.Errorf("claim advisory lock: %w", err)
	}

	// Read the campaign at the expected version. Do NOT bump the version yet; that
	// happens in ReplaceCampaign so it co-commits with the outbox message.
	c, err := scanCampaign(conn.QueryRow(ctx, claimCampaignVersionQuery, campaignID, briefID, projectID, expectedVersion))
	if err != nil {
		// Classify no-rows as 404 vs 412 BEFORE unlocking, on the SAME locked connection.
		// The advisory lock is what makes the classification trustworthy: once it is
		// released a waiting delete can run, and a row that was merely at the wrong
		// version at claim time would then be reported ErrNotFound (404) instead of the
		// contract's ErrPreconditionFailed (412). Probing under the lock pins the row's
		// existence to the same instant the version comparison was made.
		claimErr := fmt.Errorf("claim campaign version: %w", err)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			probeErr := conn.QueryRow(ctx, claimCampaignExistsQuery,
				campaignID, briefID, projectID).Scan(&exists)
			switch {
			case probeErr != nil:
				// Cannot tell the two apart; surface the probe failure rather than
				// guessing a status the caller would act on.
				claimErr = fmt.Errorf("classify campaign claim failure: %w", probeErr)
			case exists:
				claimErr = domain.ErrPreconditionFailed
			default:
				claimErr = domain.ErrNotFound
			}
		}

		// Release the lock immediately on error, on a detached bounded context: the
		// caller's ctx may already be cancelled (e.g. the read above failed because the
		// request context expired), and an unlock issued with a dead context fails
		// immediately. A session advisory lock is NOT released just because the
		// connection is returned to the pool, so a failed unlock here would strand the
		// lock on a now-pooled connection and block every future claim for this
		// campaign. If the unlock itself errors, destroy the connection instead of
		// releasing it, so the lock-bearing session can never be reused.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimRollbackTimeout)
		_, unlockErr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
		cancel()
		if unlockErr != nil {
			slog.WarnContext(ctx, "failed to release campaign advisory lock after claim error; destroying connection",
				"campaign_id", campaignID, "error", unlockErr)
			closeLockConn(ctx, conn, campaignID, "failed to close campaign advisory lock connection after unlock failure")
		}
		conn.Release()

		return nil, domain.CampaignLockToken{}, claimErr
	}

	// Store the lock in the package-level map. The advisory lock ensures only
	// one writer can hold a lock for a given campaign at a time, so this map
	// is safe from concurrent modification for the same key.
	lock := &campaignLock{
		conn:    conn,
		lockKey: lockKey,
	}
	activeCampaignLocks.Store(campaignID, lock)

	return c, domain.NewCampaignLockToken(campaignID, lock), nil
}

// ReleaseCampaignLock releases the advisory lock identified by token, as returned by
// ClaimCampaignVersion. Callers MUST call this after claiming, either directly or via defer,
// to allow other writers to proceed.
//
// It is a complete no-op only for the zero CampaignLockToken, and for a token already
// released by an earlier call. When the entry currently held for token.CampaignID is a
// SUCCESSOR's lock rather than this one, the call is a no-op with respect to the MAP and the
// successor — the CompareAndDelete below deliberately fails and the successor's lock and
// connection are left untouched — but it still unlocks and disposes THIS token's own
// connection. That connection is checked out of the pool and referenced by nothing else, so
// returning early without disposing it would leak the slot permanently. Do not "simplify"
// this into an early return.
func (r *CampaignRepo) ReleaseCampaignLock(ctx context.Context, token domain.CampaignLockToken) error {
	return r.releaseCampaignLock(ctx, token, lockReleaseTimeout)
}

// releaseBoundObserver, when set, receives the unlock budget every release runs under. It is a
// test seam, and it exists because the property it exposes cannot be observed any other way:
// which of the two budgets a release path picks is a pure timing decision, invisible in the
// return value, and a release with no live Postgres connection returns before the budget is
// ever spent. Nothing sets it in production, so the load is one atomic read per release.
var releaseBoundObserver atomic.Pointer[func(time.Duration)]

// lockReleaseTimeout bounds the pg_advisory_unlock round-trip on the ordinary release path.
// Generous, because the alternative to a slow-but-successful unlock is destroying the
// connection.
const lockReleaseTimeout = 5 * time.Second

// releaseCampaignLock is ReleaseCampaignLock with the unlock round-trip's bound supplied by
// the caller. Exposed internally only so the shutdown path can pass Close's own (much
// smaller) budget: if the unlock cannot finish inside it, the Exec fails and the branch
// below DESTROYS the connection — which is what actually returns the pool slot pgxpool.Close
// blocks on. A destroyed connection at shutdown is strictly better than a Close that
// overruns its budget and gets SIGKILLed mid-drain (Postgres drops a dead session's advisory
// locks on its own, so nothing is stranded upstream either).
func (r *CampaignRepo) releaseCampaignLock(ctx context.Context, token domain.CampaignLockToken, timeout time.Duration) error {
	if obs := releaseBoundObserver.Load(); obs != nil {
		(*obs)(timeout)
	}
	lock, ok := token.Handle().(*campaignLock)
	if !ok || lock == nil {
		// No lock claimed (zero token), or a token from a different implementation.
		return nil
	}
	if !lock.released.CompareAndSwap(false, true) {
		// This exact token was already released (e.g. by a prior call, or a concurrent
		// caller racing this one) — conn was already unlocked and returned to the pool,
		// so touching it again here would reuse a connection the pool may have since
		// handed to a different caller.
		return nil
	}
	campaignID := token.CampaignID

	// CompareAndDelete, not LoadAndDelete: a delayed release (the UNCONFIRMED cooldown path
	// in service.ToggleCampaignStatus schedules one up to unconfirmedLockCooldown after this
	// lock's own session may have ended) must not blindly delete whatever is in the map now.
	// If this session's connection died during the cooldown, Postgres drops its advisory lock
	// on its own, and a NEW claimant can then Store its own *campaignLock under the same
	// campaignID key. token pins the EXACT *campaignLock this caller claimed (not whatever is
	// freshly loaded from the map), so CompareAndDelete only succeeds if that same lock is
	// still the live entry — an unconditional delete-by-key here would otherwise remove and
	// release a successor's lock and connection out from under it, re-opening the exact
	// concurrent-write window the lock exists to prevent.
	//
	// A failed CompareAndDelete only means SOMEONE ELSE'S lock now occupies the map slot — it
	// says nothing about this token's own connection, which this call still exclusively owns
	// and must dispose of below regardless. Returning early here without disposing lock.conn
	// would leak that pool slot forever, since nothing else references it.
	activeCampaignLocks.CompareAndDelete(campaignID, lock)

	// Release on a bounded detached context to guarantee the unlock runs even
	// if the original context was cancelled (which may happen during platform
	// calls or other operations with independent lifecycle). The bound is
	// explicit rather than fixed at lockReleaseTimeout so the shutdown path can
	// hold the round-trip inside Close's much smaller budget — see
	// releaseTimeoutForShutdown.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	if _, err := lock.conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", lock.lockKey); err != nil {
		// A session advisory lock is NOT released just because the connection goes back
		// to the pool — it stays held for the life of the underlying Postgres session, so
		// returning this connection to the pool after a failed unlock would strand the
		// lock on a connection future claims can be handed, permanently blocking them.
		// Destroy the connection instead of releasing it.
		slog.WarnContext(ctx, "failed to release campaign advisory lock; destroying connection",
			"campaign_id", campaignID, "error", err)
		// releaseCtx, NOT context.WithoutCancel(ctx). pgx uses Close's context to bound how
		// long it waits for a graceful shutdown of the session, and on the cooldown paths ctx
		// is context.Background() — so WithoutCancel would hand Close no deadline at all. The
		// pool slot is not returned until Close returns, so an unbounded Close lets
		// pgxpool.Close block past ContainerCloseTimeout even after StopCooldownsForShutdown
		// finished its wait. releaseCtx is already bounded by the caller's timeout, which on
		// the shutdown path IS Close's own budget; an expired context still closes the
		// underlying socket, which is what actually frees the slot.
		if closeErr := lock.conn.Conn().Close(releaseCtx); closeErr != nil {
			slog.WarnContext(ctx, "failed to close campaign advisory lock connection after unlock failure",
				"campaign_id", campaignID, "error", closeErr)
		}
	}
	lock.conn.Release()
	return nil
}

// cooldownWG tracks in-flight ReleaseCampaignLockAfterCooldown goroutines so
// StopCooldownsForShutdown can wait for them to finish releasing their held connection.
// cooldownShutdown is closed exactly once, by StopCooldownsForShutdown, to make every pending
// cooldown release immediately instead of waiting out the rest of its cooldown.
var (
	cooldownWG       sync.WaitGroup
	cooldownShutdown = make(chan struct{})
	cooldownOnce     sync.Once
	// cooldownMu guards cooldownStopped, and serializes it against every cooldownWG.Add(1) in
	// ReleaseCampaignLockAfterCooldown. sync.WaitGroup requires that any Add(1) which starts
	// when the counter is zero happen before the matching Wait — but Wait can already have
	// observed a zero counter and returned by the time a straggler ReleaseCampaignLockAfterCooldown
	// call (racing shutdown) reaches its Add(1), violating that contract. Gating every Add(1) on
	// cooldownStopped, set under the same mutex before StopCooldownsForShutdown's Wait, makes the
	// two mutually exclusive in real time: a straggler either completes its Add() before stopped
	// is set (so Wait — locked out until the Add's critical section ends — is guaranteed to
	// observe it), or observes stopped already set and skips the WaitGroup entirely.
	cooldownMu      sync.Mutex
	cooldownStopped bool
	// cooldownReleaseDeadlineNanos carries the ABSOLUTE instant StopCooldownsForShutdown's
	// wait expires, so a cooldown goroutine woken by shutdown bounds its unlock by the budget
	// Close actually has LEFT rather than by lockReleaseTimeout. Read via shutdownReleaseBound.
	//
	// Absolute, not the relative duration: goroutines do not all wake at the same instant, and
	// each one creates its unlock timeout only when it is scheduled. Handing every straggler a
	// fresh full-length budget means one that wakes 200ms in can keep its connection for the
	// whole timeout AGAIN, past the point StopCooldownsForShutdown returned — and pgxpool.Close
	// then blocks on that connection outside ContainerCloseTimeout, which is the exact overrun
	// this budget exists to prevent. A shared deadline is the only form that composes across an
	// unknown number of wake-ups.
	cooldownReleaseDeadlineNanos atomic.Int64
)

// shutdownReleaseBound returns the unlock budget REMAINING for a cooldown release cut short by
// shutdown, or lockReleaseTimeout if a release somehow runs before the deadline is published
// (belt-and-braces; the store happens before the channel close that wakes these goroutines).
//
// A non-positive remainder is returned as-is rather than floored. The resulting context is
// already expired, so the unlock fails immediately and releaseCampaignLock destroys the
// connection — which is precisely what frees the pool slot. Waiting politely for a graceful
// unlock past the point Close stopped waiting would hold the slot pgxpool.Close is blocked on,
// so out-of-budget is the one case where destroying the connection is the FASTER answer.
func shutdownReleaseBound() time.Duration {
	if d := cooldownReleaseDeadlineNanos.Load(); d > 0 {
		return time.Until(time.Unix(0, d))
	}
	return lockReleaseTimeout
}

// ReleaseCampaignLockAfterCooldown releases the advisory lock identified by token after
// cooldown elapses, or immediately if StopCooldownsForShutdown is called first — whichever
// comes first. Used by the UNCONFIRMED toggle path (see service.ToggleCampaignStatus) to hold
// the claim lock past the request's lifetime without leaking it past process shutdown:
// pgxpool.Close blocks until every checked-out connection is returned, so an unbounded-looking
// time.Sleep here would otherwise make Container.Close overrun its shutdown budget waiting for
// this one connection.
func (r *CampaignRepo) ReleaseCampaignLockAfterCooldown(token domain.CampaignLockToken, cooldown time.Duration) {
	cooldownMu.Lock()
	if cooldownStopped {
		cooldownMu.Unlock()
		// Shutdown has already been signaled, and StopCooldownsForShutdown's Wait may already
		// have returned — joining cooldownWG now could race a Wait that already saw zero.
		// Release synchronously instead of spawning a tracked goroutine.
		//
		// shutdownReleaseBound(), not the ordinary 5s: this branch runs only AFTER shutdown
		// published its budget, and nothing waits for this release, so a 5s unlock here keeps
		// a connection checked out well past the point pgxpool.Close is blocking on it.
		// Before shutdown the bound falls back to lockReleaseTimeout, so this is never
		// tighter than the ordinary path.
		_ = r.releaseCampaignLock(context.Background(), token, shutdownReleaseBound())
		return
	}
	cooldownWG.Add(1)
	cooldownMu.Unlock()
	go func() {
		defer cooldownWG.Done()
		select {
		case <-time.After(cooldown):
			// shutdownReleaseBound() here too, and not because this is the shutdown case.
			// If the cooldown timer fires at the same instant shutdown closes
			// cooldownShutdown, BOTH select cases are ready and Go picks one at random — so
			// this branch can be the one that runs during shutdown, and using the ordinary
			// 5s bound there would overrun Close's budget exactly as the other branch
			// would. Before shutdown the bound falls back to lockReleaseTimeout, so the
			// normal cooldown release is unchanged.
			_ = r.releaseCampaignLock(context.Background(), token, shutdownReleaseBound())
		case <-cooldownShutdown:
			// Woken by shutdown: StopCooldownsForShutdown only waits so long for this
			// goroutine, so the unlock must be bounded by that same budget rather than by
			// lockReleaseTimeout. Otherwise Close returns after its wait elapses while this
			// connection stays checked out for up to lockReleaseTimeout, and pool.Close
			// blocks on it — pushing shutdown past ContainerCloseTimeout by the difference.
			_ = r.releaseCampaignLock(context.Background(), token, shutdownReleaseBound())
		}
	}()
}

// StopCooldownsForShutdown signals every in-flight UNCONFIRMED lock cooldown (see
// ReleaseCampaignLockAfterCooldown) to release its held connection immediately instead of
// waiting out the rest of its cooldown, then waits up to timeout for them to finish. MUST be
// called, and awaited, before the pool is closed: pgxpool.Close blocks until every checked-out
// connection is returned, and a cooldown goroutine otherwise holds one for up to
// unconfirmedLockCooldown. Safe to call more than once; only the first call's close takes
// effect, and every call still waits for outstanding releases.
func StopCooldownsForShutdown(timeout time.Duration) {
	// Publish the deadline BEFORE closing the channel: the woken goroutines read it to bound
	// their own unlock, and a goroutine that woke first and read a zero would fall back to
	// the default rather than to Close's actual budget. It is stamped here, where the wait
	// starts, so every straggler shares one expiry no matter when it is scheduled.
	cooldownReleaseDeadlineNanos.Store(time.Now().Add(timeout).UnixNano())
	cooldownMu.Lock()
	cooldownStopped = true
	cooldownMu.Unlock()
	cooldownOnce.Do(func() { close(cooldownShutdown) })
	done := make(chan struct{})
	go func() {
		cooldownWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("pending UNCONFIRMED lock cooldowns did not release promptly; pool close may block",
			"timeout", timeout)
	}
}

// closeLockConn destroys a connection that may still hold a session advisory lock, on a
// bounded context of its own.
//
// Close's context is not decoration: pgx uses it to bound how long Close waits for a
// graceful shutdown of the session, and the pool slot is NOT returned until Close returns.
// Every call site below is reached precisely BECAUSE something already failed or was
// cancelled, so the caller's ctx is routinely dead — and context.WithoutCancel strips the
// deadline along with the cancellation, handing Close no bound at all. That is backwards:
// it turns a failure path into one that can hold a request goroutine and a pool slot
// indefinitely, and can push pgxpool.Close past ContainerCloseTimeout during shutdown. An
// expired context still closes the underlying socket, which is what actually frees the slot.
func closeLockConn(ctx context.Context, conn *pgxpool.Conn, campaignID, msg string) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimRollbackTimeout)
	defer cancel()
	if closeErr := conn.Conn().Close(closeCtx); closeErr != nil {
		slog.WarnContext(ctx, msg, "campaign_id", campaignID, "error", closeErr)
	}
}

// hashCampaignID returns a stable hash of a campaign ID suitable for use as a
// Postgres advisory lock key. Uses a simple hash to produce an int64.
func hashCampaignID(id string) int64 {
	// Use Go's built-in hash for strings, then convert to a positive int64
	// suitable for pg_advisory_lock. The UUID format ensures good distribution.
	h := int64(0)
	for i := 0; i < len(id); i++ {
		h = h*31 + int64(id[i])
	}
	// Ensure non-negative
	if h < 0 {
		h = -h
	}
	return h
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
	// Participate in the SAME advisory-lock protocol as ClaimCampaignVersion before
	// taking the row lock. FOR UPDATE alone serializes this against the dispatch path
	// (which UPDATEs the row) but NOT against an in-flight run-state toggle: a toggle
	// holds its claim across the platform call, and between ClaimCampaignVersion and
	// ReplaceCampaign it holds no row lock at all. Deleting inside that window bumps
	// version, so the toggle's ReplaceCampaign(expectedVersion) then fails AFTER the
	// paid side effect already landed upstream — the campaign is changed on the
	// platform with no local record of it. Blocking here until the toggle releases
	// means we observe its bumped version and return a 412 the caller can act on.
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("delete campaign: acquire connection: %w", err)
	}
	lockKey := hashCampaignID(id)
	if _, lerr := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); lerr != nil {
		// Postgres may have granted the lock server-side even though the client saw a
		// failure (e.g. the request context was cancelled mid-call). Returning this
		// connection to the pool would strand a session-scoped lock on it forever, so
		// destroy it instead — same reasoning as ClaimCampaignVersion.
		closeLockConn(ctx, conn, id, "failed to close campaign connection after delete advisory lock acquire error")
		conn.Release()
		return fmt.Errorf("delete campaign: claim advisory lock: %w", lerr)
	}
	// Unlock on a context detached from the request: the caller's ctx may already be
	// cancelled by the time we get here, and an unlock issued on a dead context fails
	// immediately, stranding the lock on a pooled connection and blocking every future
	// claim AND delete for this campaign. If the unlock itself errors, destroy the
	// connection so the lock-bearing session can never be reused.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimRollbackTimeout)
		_, unlockErr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
		cancel()
		if unlockErr != nil {
			slog.WarnContext(ctx, "failed to release campaign advisory lock after delete; destroying connection",
				"campaign_id", id, "error", unlockErr)
			closeLockConn(ctx, conn, id, "failed to close campaign connection after delete unlock failure")
		}
		conn.Release()
	}()

	// Begin the transaction on the SAME connection that holds the advisory lock.
	// r.db.Begin would take a SECOND connection from the pool while this one is held,
	// which self-deadlocks whenever the pool is saturated (pool_max_conns=1 guarantees
	// it): the delete would wait for a connection that only it could free.
	tx, err := conn.Begin(ctx)
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
	// Only retire a row whose status is a settled, complete record — CampaignStatusDeletable
	// is a WHITELIST, not the complement of "needs reconciliation": campaigns.status is
	// unconstrained TEXT, so an unrecognized status (typo, future addition, upstream drift)
	// must fail closed here rather than pass through as deletable. Soft-deleting overwrites
	// status with 'deleted', which both erases the only local record of WHAT went wrong and
	// frees the (brief, platform) slot — so a re-dispatch then creates a fresh campaign with
	// no indication that a half-created one may already exist upstream. An unresolved row
	// (a live/died dispatch claim, or a partial orphan) must be reconciled first. This
	// mirrors the run-state toggle's refusal to act on an unreconciled row.
	if !model.CampaignStatusDeletable(status) {
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
