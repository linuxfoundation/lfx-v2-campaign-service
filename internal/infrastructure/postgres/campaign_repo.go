// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	// DO NOTHING, deliberately — the claim is NOT auto-reclaimed on a timer. See
	// stuckClaimReportAge: 'pending' marks both a claim in flight AND an ambiguous dispatch
	// outcome where a paid campaign may already exist upstream, and nothing distinguishes
	// them, so a time-based takeover could authorize a duplicate paid create.
	// StuckDispatchClaims surfaces stranded claims for a human instead.
	q := `INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status)
		VALUES ($1, $2, $3, $4, '', 'pending')
		ON CONFLICT (brief_id, platform) DO NOTHING`
	tag, err := r.db.Exec(ctx, q, projectID, briefID, jobID, string(platform))
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

// GetCampaign returns a single campaign under a brief.
func (r *CampaignRepo) GetCampaign(ctx context.Context, projectID, briefID, id string) (*model.Campaign, error) {
	q := `SELECT ` + campaignCols + ` FROM campaigns WHERE id=$1 AND brief_id=$2 AND project_id=$3`
	c, err := scanCampaign(r.db.QueryRow(ctx, q, id, briefID, projectID))
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
	q := `SELECT ` + campaignCols + ` FROM campaigns WHERE brief_id=$1 AND platform=$2 AND project_id=$3`
	c, err := scanCampaign(r.db.QueryRow(ctx, q, briefID, string(platform), projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get campaign by platform: %w", err)
	}
	return c, nil
}

// UpsertCampaign inserts or updates the (brief, platform) campaign row. On
// conflict it updates in place (a brief change after campaigns exist).
func (r *CampaignRepo) UpsertCampaign(ctx context.Context, c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	q := `INSERT INTO campaigns
		(project_id, brief_id, job_id, platform, platform_campaign_id, campaign_name, status,
		 budget_amount, budget_type, start_date, end_date, config_snapshot, result)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (brief_id, platform) DO UPDATE SET
			job_id=EXCLUDED.job_id, platform_campaign_id=EXCLUDED.platform_campaign_id,
			campaign_name=EXCLUDED.campaign_name, status=EXCLUDED.status,
			budget_amount=EXCLUDED.budget_amount, budget_type=EXCLUDED.budget_type,
			start_date=EXCLUDED.start_date, end_date=EXCLUDED.end_date,
			config_snapshot=EXCLUDED.config_snapshot, result=EXCLUDED.result,
			version=campaigns.version+1, updated_at=now()
		RETURNING ` + campaignCols
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert campaign: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, q,
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
	// If a caller holds the claim lock for this campaign, the write MUST run on that
	// SAME pooled connection rather than acquiring a second one from the pool: the lock
	// holder pins one connection for the whole claim window, so opening a second here
	// would need a second connection from the pool at the same time. Under a small or
	// saturated pool (pool_max_conns=1 makes it certain) that starves this write behind
	// its own lock holder, blocking every toggle until persistResultTimeout.
	var beginner interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	if val, ok := activeCampaignLocks.Load(c.ID); ok {
		beginner = val.(*campaignLock).conn
	} else {
		beginner = r.db
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

var activeCampaignLocks sync.Map // maps campaignID to *campaignLock

// campaignLock holds the session-scoped advisory lock for a campaign.
type campaignLock struct {
	conn    *pgxpool.Conn
	lockKey int64
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
		if closeErr := conn.Conn().Close(context.WithoutCancel(ctx)); closeErr != nil {
			slog.WarnContext(ctx, "failed to close campaign connection after advisory lock acquire error",
				"campaign_id", campaignID, "error", closeErr)
		}
		conn.Release()
		return nil, domain.CampaignLockToken{}, fmt.Errorf("claim advisory lock: %w", err)
	}

	// Read the campaign at the expected version. Do NOT bump the version yet; that
	// happens in ReplaceCampaign so it co-commits with the outbox message.
	q := `SELECT ` + campaignCols + ` FROM campaigns
		WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND version=$4`
	c, err := scanCampaign(conn.QueryRow(ctx, q, campaignID, briefID, projectID, expectedVersion))
	if err != nil {
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
			if closeErr := conn.Conn().Close(context.WithoutCancel(ctx)); closeErr != nil {
				slog.WarnContext(ctx, "failed to close campaign advisory lock connection after unlock failure",
					"campaign_id", campaignID, "error", closeErr)
			}
		}
		conn.Release()

		if errors.Is(err, pgx.ErrNoRows) {
			// Disambiguate: not-found vs. precondition-failed by checking if the row exists
			// at any version. If GetCampaign returns not-found, the row is gone; if it
			// returns a row, the version mismatch is the issue.
			_, gerr := r.GetCampaign(ctx, projectID, briefID, campaignID)
			switch {
			case errors.Is(gerr, domain.ErrNotFound):
				return nil, domain.CampaignLockToken{}, domain.ErrNotFound
			case gerr != nil:
				return nil, domain.CampaignLockToken{}, gerr
			default:
				return nil, domain.CampaignLockToken{}, domain.ErrPreconditionFailed
			}
		}
		return nil, domain.CampaignLockToken{}, fmt.Errorf("claim campaign version: %w", err)
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
// ClaimCampaignVersion. It is a no-op for the zero CampaignLockToken, or if the entry currently
// held for token.CampaignID is no longer the exact lock token identifies (see the
// CompareAndDelete below). Callers MUST call this after claiming, either directly or via defer,
// to allow other writers to proceed.
func (r *CampaignRepo) ReleaseCampaignLock(ctx context.Context, token domain.CampaignLockToken) error {
	lock, ok := token.Handle().(*campaignLock)
	if !ok || lock == nil {
		// No lock claimed (zero token), or a token from a different implementation.
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
	if !activeCampaignLocks.CompareAndDelete(campaignID, lock) {
		return nil
	}

	// Release on a bounded detached context to guarantee the unlock runs even
	// if the original context was cancelled (which may happen during platform
	// calls or other operations with independent lifecycle).
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := lock.conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", lock.lockKey); err != nil {
		// A session advisory lock is NOT released just because the connection goes back
		// to the pool — it stays held for the life of the underlying Postgres session, so
		// returning this connection to the pool after a failed unlock would strand the
		// lock on a connection future claims can be handed, permanently blocking them.
		// Destroy the connection instead of releasing it.
		slog.WarnContext(ctx, "failed to release campaign advisory lock; destroying connection",
			"campaign_id", campaignID, "error", err)
		if closeErr := lock.conn.Conn().Close(context.WithoutCancel(ctx)); closeErr != nil {
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
)

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
		_ = r.ReleaseCampaignLock(context.Background(), token)
		return
	}
	cooldownWG.Add(1)
	cooldownMu.Unlock()
	go func() {
		defer cooldownWG.Done()
		select {
		case <-time.After(cooldown):
		case <-cooldownShutdown:
		}
		_ = r.ReleaseCampaignLock(context.Background(), token)
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
