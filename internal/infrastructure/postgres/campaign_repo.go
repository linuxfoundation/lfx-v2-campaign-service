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

// NewCampaignRepo returns a CampaignRepo backed by pool.
func NewCampaignRepo(pool *Pool) *CampaignRepo { return &CampaignRepo{db: pool} }

var _ domain.CampaignRepository = (*CampaignRepo)(nil)

// ClaimCampaignDispatch atomically claims the right to dispatch (brief, platform) by
// inserting a placeholder pending campaign row. The (brief_id, platform) unique index
// makes the claim single-winner across all replicas without holding a connection or a
// blocking lock. The claim is one INSERT ... ON CONFLICT with two conflict actions
// (see the inline block): DO NOTHING (reclaimAfter<=0) or a stale-orphan-stealing DO
// UPDATE (reclaimAfter>0). We use `RETURNING (xmax = 0)` to detect the winner: a row
// comes back only when THIS caller inserted or stole (xmax=0 → fresh insert, else a
// steal); a conflict that did nothing (or a DO UPDATE whose WHERE didn't match) returns
// no row (pgx.ErrNoRows → claimed=false). NOTE the load-bearing invariants keyed on
// below: status == model.CampaignStatusPending ('pending') and the EMPTY-STRING (not
// NULL) platform_campaign_id mark an un-completed claim; a Go writer that drifts from
// either silently breaks the steal/skip logic (campaigns.status has no CHECK).
func (r *CampaignRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, platform model.Provider, jobID string, reclaimAfter time.Duration) (bool, *model.Campaign, error) {
	// The claim is one atomic INSERT ... ON CONFLICT. Two conflict actions:
	//   - reclaimAfter <= 0: DO NOTHING — a conflicting row is never touched (today's
	//     behavior; used for platforms whose client can't resume without duplicating).
	//   - reclaimAfter  > 0: DO UPDATE ... WHERE <stale-orphan> — a conflicting row that
	//     is still an ORPHAN (status 'pending', EMPTY platform_campaign_id) whose lease
	//     (claimed_at) is stale (NULL or older than reclaimAfter) is STOLEN by this
	//     caller so a later job can resume the partial create. The empty-id guard means
	//     a COMPLETED campaign is never stolen; the lease guard means an actively-owned
	//     claim (recent claimed_at) is never stolen mid-flight.
	// Either way, RETURNING yields exactly one row when THIS caller won (a fresh insert
	// or a steal) and zero rows when it did not (no conflict-free insert AND the update
	// WHERE didn't match) — so `claimed` is "a row came back", and `xmax = 0` distinguishes
	// a fresh insert (xmax 0) from a steal (xmax set), purely for logging.
	var conflict string
	args := []any{projectID, briefID, jobID, string(platform)}
	if reclaimAfter > 0 {
		// $5 is the lease window in SECONDS (a float): make_interval(secs => $5) builds
		// the interval server-side. Passing a Go time.Duration (int64 nanoseconds)
		// directly to `$5::interval` would fail — a bigint has no cast to interval.
		conflict = `ON CONFLICT (brief_id, platform) DO UPDATE
			SET job_id = EXCLUDED.job_id, claimed_at = now(), version = campaigns.version + 1
			WHERE campaigns.platform_campaign_id = ''
			  AND campaigns.status = 'pending'
			  AND (campaigns.claimed_at IS NULL
			       OR campaigns.claimed_at < now() - make_interval(secs => $5))`
		args = append(args, reclaimAfter.Seconds())
	} else {
		conflict = `ON CONFLICT (brief_id, platform) DO NOTHING`
	}
	q := `INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status, claimed_at)
		VALUES ($1, $2, $3, $4, '', 'pending', now())
		` + conflict + `
		RETURNING (xmax = 0) AS inserted`

	var inserted bool
	claimed := true
	if err := r.db.QueryRow(ctx, q, args...).Scan(&inserted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row returned: we neither inserted (conflict) nor stole (the DO UPDATE
			// WHERE didn't match, or DO NOTHING). Not this caller's claim.
			claimed = false
		} else {
			return false, nil, fmt.Errorf("claim campaign dispatch: %w", err)
		}
	}
	if claimed && !inserted {
		// A steal of a stale orphan (vs a fresh insert). Surface it so a resumed
		// dispatch is traceable in logs.
		slog.InfoContext(ctx, "resumed a stale pending dispatch claim (orphan re-claimed for redispatch)",
			"project_id", projectID, "brief_id", briefID, "platform", string(platform), "job_id", jobID)
	}

	row, gerr := r.GetCampaignByPlatform(ctx, projectID, briefID, platform)
	if gerr != nil {
		// The row must exist now (we or someone else just wrote it); a read failure
		// here is a genuine error. If WE just INSERTED the pending row (a fresh claim,
		// not a steal), roll it back (best effort) so a failed claim doesn't leave a
		// pending row that blocks the pair forever; report claimed=false so the caller
		// treats it as a clean failure with nothing to release. A STOLEN row is NOT
		// rolled back — it is a pre-existing orphan we only re-leased, and deleting it
		// would destroy the recorded partial we meant to resume.
		if claimed && inserted {
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
