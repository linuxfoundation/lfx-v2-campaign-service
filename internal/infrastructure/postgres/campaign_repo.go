// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// CampaignRepo is a pgx-backed implementation of domain.CampaignRepository.
type CampaignRepo struct {
	db *Pool
}

// NewCampaignRepo returns a CampaignRepo backed by pool.
func NewCampaignRepo(pool *Pool) *CampaignRepo { return &CampaignRepo{db: pool} }

var _ domain.CampaignRepository = (*CampaignRepo)(nil)

// ClaimCampaignDispatch atomically claims the right to dispatch (brief, platform)
// by inserting a placeholder 'pending' campaign row. The (brief_id, platform)
// unique index makes the claim single-winner across all replicas without holding
// a connection or a blocking lock: INSERT ... ON CONFLICT DO NOTHING inserts a
// row (claimed) or does nothing (already claimed/completed). No RETURNING is used
// because ON CONFLICT DO NOTHING returns no row on conflict, so we detect the
// winner via RowsAffected and then read the current row to return it.
func (r *CampaignRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, platform model.Provider, jobID string) (bool, *model.Campaign, error) {
	q := `INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status)
		VALUES ($1, $2, $3, $4, '', 'pending')
		ON CONFLICT (brief_id, platform) DO NOTHING`
	tag, err := r.db.Exec(ctx, q, projectID, briefID, jobID, string(platform))
	if err != nil {
		return false, nil, fmt.Errorf("claim campaign dispatch: %w", err)
	}
	claimed := tag.RowsAffected() == 1

	row, gerr := r.GetCampaignByPlatform(ctx, briefID, platform)
	if gerr != nil {
		// The row must exist now (we or someone else just wrote it); a not-found
		// here is a genuine error.
		return claimed, nil, fmt.Errorf("read campaign after claim: %w", gerr)
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
