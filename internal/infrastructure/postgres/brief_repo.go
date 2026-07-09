// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// BriefRepo is a pgx-backed implementation of domain.BriefRepository.
type BriefRepo struct {
	db *Pool
}

// NewBriefRepo returns a BriefRepo backed by pool.
func NewBriefRepo(pool *Pool) *BriefRepo { return &BriefRepo{db: pool} }

var _ domain.BriefRepository = (*BriefRepo)(nil)

const briefCols = `id::text, project_id::text, program_type, event_slug, url, platforms, event_details,
	copy, keywords, targeting, status, version, approved_by, approved_at, created_at, updated_at`

// GetBrief returns a non-archived brief by id scoped to the project.
func (r *BriefRepo) GetBrief(ctx context.Context, projectID, id string) (*model.CampaignBrief, error) {
	q := `SELECT ` + briefCols + ` FROM campaign_briefs WHERE id = $1 AND project_id = $2 AND status <> 'archived'`
	b, err := scanBrief(r.db.QueryRow(ctx, q, id, projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get brief: %w", err)
	}
	return b, nil
}

// CreateBrief inserts a brief. Returns ErrConflict on UNIQUE(project_id, event_slug).
func (r *BriefRepo) CreateBrief(ctx context.Context, b *model.CampaignBrief) (*model.CampaignBrief, error) {
	approvedBy, err := marshalActor(b.ApprovedBy)
	if err != nil {
		return nil, err
	}
	q := `INSERT INTO campaign_briefs
		(project_id, program_type, event_slug, url, platforms, event_details, copy, keywords, targeting, approved_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING ` + briefCols
	row := r.db.QueryRow(ctx, q,
		b.ProjectID, string(b.ProgramType), b.EventSlug, nullStr(b.URL),
		nullJSON(b.Platforms), nullJSON(b.EventDetails), nullJSON(b.Copy),
		nullJSON(b.Keywords), nullJSON(b.Targeting), approvedBy,
	)
	created, err := scanBrief(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, domain.ErrConflict
		}
		return nil, fmt.Errorf("create brief: %w", err)
	}
	return created, nil
}

// ReplaceBrief replaces mutable fields, gating on expectedVersion.
func (r *BriefRepo) ReplaceBrief(ctx context.Context, b *model.CampaignBrief, expectedVersion int64) (*model.CampaignBrief, error) {
	q := `UPDATE campaign_briefs SET
		program_type=$1, url=$2, platforms=$3, event_details=$4, copy=$5, keywords=$6, targeting=$7,
		version=version+1, updated_at=now()
		WHERE id=$8 AND project_id=$9 AND version=$10 AND status <> 'archived'`
	tag, err := r.db.Exec(ctx, q,
		string(b.ProgramType), nullStr(b.URL), nullJSON(b.Platforms), nullJSON(b.EventDetails),
		nullJSON(b.Copy), nullJSON(b.Keywords), nullJSON(b.Targeting),
		b.ID, b.ProjectID, expectedVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("replace brief: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish missing from stale version. Surface a transient re-fetch
		// error rather than masking it as a precondition failure (which would make
		// the caller retry with a fresh ETag instead of backing off on a server
		// error), consistent with ConnectionRepo.Update.
		_, gerr := r.GetBrief(ctx, b.ProjectID, b.ID)
		switch {
		case errors.Is(gerr, domain.ErrNotFound):
			return nil, domain.ErrNotFound
		case gerr != nil:
			return nil, gerr
		default:
			return nil, domain.ErrPreconditionFailed
		}
	}
	return r.GetBrief(ctx, b.ProjectID, b.ID)
}

// Approve marks a brief approved, recording the actor.
func (r *BriefRepo) Approve(ctx context.Context, projectID, id string, by *model.Actor) (*model.CampaignBrief, error) {
	approvedBy, err := marshalActor(by)
	if err != nil {
		return nil, err
	}
	q := `UPDATE campaign_briefs SET status='approved', approved_by=$1, approved_at=now(),
		version=version+1, updated_at=now()
		WHERE id=$2 AND project_id=$3 AND status <> 'archived'`
	tag, err := r.db.Exec(ctx, q, approvedBy, id, projectID)
	if err != nil {
		return nil, fmt.Errorf("approve brief: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.ErrNotFound
	}
	return r.GetBrief(ctx, projectID, id)
}

// ArchiveBrief soft-archives a brief.
func (r *BriefRepo) ArchiveBrief(ctx context.Context, projectID, id string) error {
	q := `UPDATE campaign_briefs SET status='archived', version=version+1, updated_at=now()
		WHERE id=$1 AND project_id=$2 AND status <> 'archived'`
	tag, err := r.db.Exec(ctx, q, id, projectID)
	if err != nil {
		return fmt.Errorf("archive brief: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func scanBrief(row pgx.Row) (*model.CampaignBrief, error) {
	var (
		b                   model.CampaignBrief
		programType, status string
		url                 *string
		approvedBy          []byte
	)
	if err := row.Scan(
		&b.ID, &b.ProjectID, &programType, &b.EventSlug, &url,
		&b.Platforms, &b.EventDetails, &b.Copy, &b.Keywords, &b.Targeting,
		&status, &b.Version, &approvedBy, &b.ApprovedAt, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	b.ProgramType = model.ProgramType(programType)
	b.Status = model.BriefStatus(status)
	if url != nil {
		b.URL = *url
	}
	// Surface corrupt actor JSON rather than silently returning a nil audit
	// trail (which would hide data corruption until a downstream nil deref).
	ab, err := unmarshalActor(approvedBy)
	if err != nil {
		return nil, fmt.Errorf("scan brief: unmarshal approved_by: %w", err)
	}
	b.ApprovedBy = ab
	return &b, nil
}

// nullJSON returns nil for empty raw JSON so the column stores SQL NULL.
func nullJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return []byte(j)
}
