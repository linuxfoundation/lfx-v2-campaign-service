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

// AudienceRepo is a pgx-backed implementation of domain.AudienceRepository.
type AudienceRepo struct {
	db *Pool
}

// NewAudienceRepo returns an AudienceRepo backed by pool.
func NewAudienceRepo(pool *Pool) *AudienceRepo { return &AudienceRepo{db: pool} }

var _ domain.AudienceRepository = (*AudienceRepo)(nil)

// audienceCols is the column list every audience read scans, in scanAudience order.
const audienceCols = `id::text, project_id::text, brief_id::text, platform,
	platform_master_list_id, suppression_list_ids, inclusion_summary, status, version,
	created_by, created_at, updated_at`

// CreateAudience inserts a new audience row and returns it.
func (r *AudienceRepo) CreateAudience(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	// Gate the insert on an ACTIVE parent brief scoped by BOTH (project_id, brief_id).
	// A bare brief_id FK check would let a caller authorized for project A supply a
	// brief id from project B (tenant/parent disagree), and would accept an archived
	// brief. INSERT...SELECT...WHERE EXISTS inserts zero rows when the active,
	// same-project parent is absent, which we map to ErrNotFound.
	q := `INSERT INTO campaign_audiences
		(project_id, brief_id, platform, platform_master_list_id, suppression_list_ids,
		 inclusion_summary, status, created_by)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8
		WHERE EXISTS (
			SELECT 1 FROM campaign_briefs
			WHERE id=$2 AND project_id=$1 AND status <> 'archived'
		)
		RETURNING ` + audienceCols
	row := r.db.QueryRow(ctx, q,
		a.ProjectID, a.BriefID, string(a.Platform), nullStr(a.PlatformMasterListID),
		nullJSON(a.SuppressionListIDs), nullStr(a.InclusionSummary), string(a.StatusOrDefault()),
		nullJSON(a.CreatedBy),
	)
	created, err := scanAudience(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// No active parent brief for (project, brief) → the parent is missing,
		// archived, or belongs to another project.
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create audience: %w", err)
	}
	return created, nil
}

// GetAudience returns one audience by id, scoped to (project, brief), or ErrNotFound.
func (r *AudienceRepo) GetAudience(ctx context.Context, projectID, briefID, id string) (*model.CampaignAudience, error) {
	// Require an ACTIVE parent brief, consistent with ListAudiences and CreateAudience:
	// once a brief is archived its audiences are no longer part of the live lifecycle, so
	// get/update must 404 rather than list 404-ing while get/patch still succeed on the
	// same nested resource. The EXISTS keeps this a single round-trip. (Update loads via
	// this method, so guarding Get covers the patch path too.)
	q := `SELECT ` + audienceCols + ` FROM campaign_audiences ca
		WHERE ca.id=$1 AND ca.brief_id=$2 AND ca.project_id=$3
		AND EXISTS (
			SELECT 1 FROM campaign_briefs b
			WHERE b.id=ca.brief_id AND b.project_id=ca.project_id AND b.status <> 'archived'
		)`
	a, err := scanAudience(r.db.QueryRow(ctx, q, id, briefID, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get audience: %w", err)
	}
	return a, nil
}

// ListAudiences returns a brief's audiences (newest first), scoped to the project.
// maxAudiencesPerList bounds a single ListAudiences response. Audiences accumulate
// over time (per platform / per build), so an unbounded list would grow without
// limit; this caps the query cost and response size. A stable (created_at, id) order
// makes the cap deterministic (newest first).
const maxAudiencesPerList = 200

func (r *AudienceRepo) ListAudiences(ctx context.Context, projectID, briefID string) ([]*model.CampaignAudience, error) {
	// Verify the ACTIVE parent brief exists for (project, brief) first — otherwise a
	// missing / cross-project / archived brief would return 200 with an empty array
	// instead of the NotFound the endpoint declares (the child-only query can't
	// distinguish "no audiences yet" from "no such brief").
	var exists bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM campaign_briefs WHERE id=$1 AND project_id=$2 AND status <> 'archived')`,
		briefID, projectID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify parent brief: %w", err)
	}
	if !exists {
		return nil, domain.ErrNotFound
	}

	q := `SELECT ` + audienceCols + ` FROM campaign_audiences
		WHERE brief_id=$1 AND project_id=$2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`
	rows, err := r.db.Query(ctx, q, briefID, projectID, maxAudiencesPerList)
	if err != nil {
		return nil, fmt.Errorf("list audiences: %w", err)
	}
	defer rows.Close()

	var out []*model.CampaignAudience
	for rows.Next() {
		a, sErr := scanAudience(rows)
		if sErr != nil {
			return nil, fmt.Errorf("scan audience row: %w", sErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audience rows: %w", err)
	}
	return out, nil
}

// UpdateAudience replaces the mutable fields under an optimistic-concurrency guard on
// expectedVersion (ErrPreconditionFailed on mismatch, ErrNotFound when absent).
func (r *AudienceRepo) UpdateAudience(ctx context.Context, a *model.CampaignAudience, expectedVersion int64) (*model.CampaignAudience, error) {
	// UPDATE ... RETURNING returns the row THIS statement wrote, atomically — so the
	// caller always gets the state + ETag produced by its OWN write. A separate
	// post-update re-read would race: a concurrent version N+1 could land between the
	// UPDATE and the read, handing this caller the other writer's row and ETag.
	q := `UPDATE campaign_audiences SET
		platform_master_list_id=$1, suppression_list_ids=$2, inclusion_summary=$3,
		status=$4, version=version+1, updated_at=now()
		WHERE id=$5 AND brief_id=$6 AND project_id=$7 AND version=$8
		RETURNING ` + audienceCols
	updated, err := scanAudience(r.db.QueryRow(ctx, q,
		nullStr(a.PlatformMasterListID), nullJSON(a.SuppressionListIDs), nullStr(a.InclusionSummary),
		string(a.StatusOrDefault()), a.ID, a.BriefID, a.ProjectID, expectedVersion,
	))
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("update audience: %w", err)
	}
	// No row matched the (id, brief, project, version) predicate. Distinguish "absent"
	// from "version mismatch" by re-reading (this read is only for classifying the
	// no-op — it never becomes the returned row — so it can't race the success path),
	// and surface a transient re-fetch error rather than masking it as a precondition
	// failure (consistent with ReplaceCampaign / ConnectionRepo.Update).
	_, gerr := r.GetAudience(ctx, a.ProjectID, a.BriefID, a.ID)
	switch {
	case errors.Is(gerr, domain.ErrNotFound):
		return nil, domain.ErrNotFound
	case gerr != nil:
		return nil, gerr
	default:
		return nil, domain.ErrPreconditionFailed
	}
}

// scanAudience reads one campaign_audiences row in audienceCols order.
func scanAudience(row pgx.Row) (*model.CampaignAudience, error) {
	var (
		a         model.CampaignAudience
		platform  string
		masterID  *string
		suppress  []byte
		inclusion *string
		status    string
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &platform,
		&masterID, &suppress, &inclusion, &status, &a.Version,
		&createdBy, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.Platform = model.Provider(platform)
	if masterID != nil {
		a.PlatformMasterListID = *masterID
	}
	if inclusion != nil {
		a.InclusionSummary = *inclusion
	}
	a.SuppressionListIDs = suppress
	a.CreatedBy = createdBy
	a.Status = model.AudienceStatus(status)
	return &a, nil
}
