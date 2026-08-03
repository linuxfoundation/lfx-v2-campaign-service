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

// StuckDispatchClaims returns 'pending' campaign rows older than stuckClaimReportAge, OLDEST first,
// capped at limit. It is READ-ONLY: nothing here reclaims, deletes, or redispatches.
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
	q := `SELECT ` + campaignCols + ` FROM campaigns
		WHERE status = 'pending' AND created_at < now() - $1::interval
		ORDER BY created_at ASC
		LIMIT $2`
	rows, err := r.db.Query(ctx, q, stuckClaimReportAge.String(), limit+1)
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
func (r *CampaignRepo) UpsertCampaign(ctx context.Context, c *model.Campaign) (*model.Campaign, error) {
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
	row := r.db.QueryRow(ctx, q,
		c.ProjectID, c.BriefID, c.JobID, string(c.Platform), nullStr(c.PlatformCampaignID),
		c.CampaignName, c.Status, c.BudgetAmount, budgetTypeArg(c.BudgetType),
		c.StartDate, c.EndDate, nullJSON(c.ConfigSnapshot), nullJSON(c.Result),
	)
	upserted, err := scanCampaign(row)
	if err != nil {
		return nil, fmt.Errorf("upsert campaign: %w", err)
	}
	return upserted, nil
}

// ReplaceCampaign replaces mutable fields, gating on expectedVersion.
func (r *CampaignRepo) ReplaceCampaign(ctx context.Context, c *model.Campaign, expectedVersion int64) (*model.Campaign, error) {
	q := `UPDATE campaigns SET
		campaign_name=$1, status=$2, budget_amount=$3, budget_type=$4, start_date=$5, end_date=$6,
		config_snapshot=$7, result=$8, version=version+1, updated_at=now()
		WHERE id=$9 AND brief_id=$10 AND project_id=$11 AND version=$12`
	tag, err := r.db.Exec(ctx, q,
		c.CampaignName, c.Status, c.BudgetAmount, budgetTypeArg(c.BudgetType), c.StartDate, c.EndDate,
		nullJSON(c.ConfigSnapshot), nullJSON(c.Result), c.ID, c.BriefID, c.ProjectID, expectedVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("replace campaign: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Surface a transient re-fetch error rather than masking it as a
		// precondition failure, consistent with ConnectionRepo.Update.
		_, gerr := r.GetCampaign(ctx, c.ProjectID, c.BriefID, c.ID)
		switch {
		case errors.Is(gerr, domain.ErrNotFound):
			return nil, domain.ErrNotFound
		case gerr != nil:
			return nil, gerr
		default:
			return nil, domain.ErrPreconditionFailed
		}
	}
	return r.GetCampaign(ctx, c.ProjectID, c.BriefID, c.ID)
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
