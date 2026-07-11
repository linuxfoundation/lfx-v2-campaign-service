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

// advisoryUnlockTimeout bounds the explicit advisory-lock release so a slow
// unlock can't wedge connection return; the lock also frees when the session
// ends, so this is a backstop.
const advisoryUnlockTimeout = 5 * time.Second

// CampaignRepo is a pgx-backed implementation of domain.CampaignRepository.
type CampaignRepo struct {
	db *Pool
}

// NewCampaignRepo returns a CampaignRepo backed by pool.
func NewCampaignRepo(pool *Pool) *CampaignRepo { return &CampaignRepo{db: pool} }

var _ domain.CampaignRepository = (*CampaignRepo)(nil)

// WithDispatchLock runs fn while holding a SESSION-level Postgres advisory lock
// keyed on (briefID, platform), acquired on a dedicated pooled connection and
// released explicitly when fn returns. A session lock (not a transaction lock)
// is used deliberately: fn's own repository calls go through the shared pool
// (r.db) and commit independently, so the lock must live on a separate
// connection that stays held for fn's whole duration rather than being tied to a
// transaction fn isn't part of. Serialization is what matters — while one worker
// holds the lock, a second worker for the same pair blocks here, and once it
// proceeds its committed-read idempotency check sees the first worker's persisted
// row and reuses it. The lock is cross-replica (it lives in the database) and
// released even if fn panics (deferred Unlock + connection release). The two-key
// hashtext form makes distinct pairs rarely collide; a collision only serializes
// two unrelated pairs, which is harmless.
func (r *CampaignRepo) WithDispatchLock(ctx context.Context, briefID string, platform model.Provider, fn func(context.Context) error) error {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire dispatch-lock connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_lock(hashtext($1), hashtext($2))`,
		briefID, string(platform),
	); err != nil {
		return fmt.Errorf("acquire dispatch lock: %w", err)
	}
	// Release the session lock on the same connection before returning it to the
	// pool; use a background context so a cancelled ctx still frees the lock.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()
		if _, uerr := conn.Exec(unlockCtx,
			`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`,
			briefID, string(platform),
		); uerr != nil {
			// The lock also frees when the session (connection) ends, so a failed
			// explicit unlock is not fatal; surface it for diagnostics.
			slog.WarnContext(ctx, "failed to release dispatch advisory lock", "brief_id", briefID, "platform", platform, "error", uerr)
		}
	}()

	return fn(ctx)
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
// (brief_id, platform) pair is unique, so at most one row matches.
func (r *CampaignRepo) GetCampaignByPlatform(ctx context.Context, briefID string, platform model.Provider) (*model.Campaign, error) {
	q := `SELECT ` + campaignCols + ` FROM campaigns WHERE brief_id=$1 AND platform=$2`
	c, err := scanCampaign(r.db.QueryRow(ctx, q, briefID, string(platform)))
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
