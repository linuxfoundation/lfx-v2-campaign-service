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

// JobRepo is a pgx-backed implementation of domain.JobRepository.
type JobRepo struct {
	db *Pool
}

// NewJobRepo returns a JobRepo backed by pool.
func NewJobRepo(pool *Pool) *JobRepo { return &JobRepo{db: pool} }

var _ domain.JobRepository = (*JobRepo)(nil)

const jobCols = `id::text, brief_id::text, status, result, error, created_at, updated_at, expires_at`

// CreateJob inserts a queued job for a brief.
func (r *JobRepo) CreateJob(ctx context.Context, briefID string) (*model.CampaignJob, error) {
	q := `INSERT INTO campaign_jobs (brief_id) VALUES ($1) RETURNING ` + jobCols
	j, err := scanJob(r.db.QueryRow(ctx, q, briefID))
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

// GetJob returns a job by id.
func (r *JobRepo) GetJob(ctx context.Context, id string) (*model.CampaignJob, error) {
	q := `SELECT ` + jobCols + ` FROM campaign_jobs WHERE id=$1`
	j, err := scanJob(r.db.QueryRow(ctx, q, id))
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
