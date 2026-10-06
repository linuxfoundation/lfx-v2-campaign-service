// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// KeywordReportRepo is a pgx-backed implementation of domain.KeywordReportRepository over
// keyword_insight_reports (migration 000038). Its statements are account_report_repo.go's, one
// for one, with the window in place of days and each half's campaign scope added; the reasoning
// on each of those statements (first mark wins, compare-and-set on pending_report_id, fail never
// touches the ready half) applies unchanged and is not repeated here.
type KeywordReportRepo struct {
	db *Pool
}

// NewKeywordReportRepo returns a KeywordReportRepo backed by pool.
func NewKeywordReportRepo(pool *Pool) *KeywordReportRepo { return &KeywordReportRepo{db: pool} }

var _ domain.KeywordReportRepository = (*KeywordReportRepo)(nil)

// $1..$4 = project_id, platform, account_id, report_window in every statement.
const (
	getKeywordReportQuery = `SELECT
		ready_report_id, ready_rows, ready_partial, ready_campaign_ids, ready_window_start,
		ready_window_end, ready_as_of,
		pending_report_id, pending_campaign_ids, pending_window_start, pending_window_end,
		pending_submitted_at,
		last_failure, last_failure_at
		FROM keyword_insight_reports
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4`

	markKeywordReportPendingQuery = `INSERT INTO keyword_insight_reports
		(project_id, platform, account_id, report_window,
		 pending_report_id, pending_campaign_ids, pending_window_start, pending_window_end,
		 pending_submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (project_id, platform, account_id, report_window) DO UPDATE SET
			pending_report_id    = EXCLUDED.pending_report_id,
			pending_campaign_ids = EXCLUDED.pending_campaign_ids,
			pending_window_start = EXCLUDED.pending_window_start,
			pending_window_end   = EXCLUDED.pending_window_end,
			pending_submitted_at = EXCLUDED.pending_submitted_at,
			updated_at           = now()
		WHERE keyword_insight_reports.pending_report_id IS NULL`

	completeKeywordReportQuery = `UPDATE keyword_insight_reports SET
		ready_report_id      = $5,
		ready_rows           = $6,
		ready_partial        = $7,
		ready_campaign_ids   = $8,
		ready_window_start   = $9,
		ready_window_end     = $10,
		ready_as_of          = $11,
		pending_report_id    = NULL,
		pending_campaign_ids = NULL,
		pending_window_start = NULL,
		pending_window_end   = NULL,
		pending_submitted_at = NULL,
		updated_at           = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4
		  AND pending_report_id = $5`

	failKeywordReportQuery = `UPDATE keyword_insight_reports SET
		pending_report_id    = NULL,
		pending_campaign_ids = NULL,
		pending_window_start = NULL,
		pending_window_end   = NULL,
		pending_submitted_at = NULL,
		last_failure         = $6,
		last_failure_at      = $7,
		updated_at           = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4
		  AND pending_report_id = $5`
)

// GetKeywordReport implements domain.KeywordReportRepository.
func (r *KeywordReportRepo) GetKeywordReport(ctx context.Context, key model.KeywordReportKey) (*model.KeywordReportSnapshot, error) {
	if err := validateKeywordReportKey(key); err != nil {
		return nil, fmt.Errorf("get keyword report: %w", err)
	}
	snap, err := scanKeywordReport(key, r.db.QueryRow(ctx, getKeywordReportQuery, keywordReportKeyArgs(key)...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get keyword report: %w", err)
	}
	return snap, nil
}

// MarkKeywordReportPending implements domain.KeywordReportRepository.
func (r *KeywordReportRepo) MarkKeywordReportPending(ctx context.Context, key model.KeywordReportKey, p model.PendingKeywordReport) (bool, error) {
	if err := validateKeywordReportKey(key); err != nil {
		return false, fmt.Errorf("mark keyword report pending: %w", err)
	}
	if p.ReportID == "" {
		return false, errors.New("mark keyword report pending: empty report id")
	}
	if len(p.CampaignIDs) == 0 {
		// A report with no recorded scope could never be proven to cover the project, so it
		// would never be served; refusing here keeps the column's meaning exact.
		return false, errors.New("mark keyword report pending: empty campaign scope")
	}
	if err := validateReportWindow(p.WindowStart, p.WindowEnd); err != nil {
		return false, fmt.Errorf("mark keyword report pending: %w", err)
	}
	if p.SubmittedAt.IsZero() {
		return false, errors.New("mark keyword report pending: zero submitted-at")
	}
	args := append(keywordReportKeyArgs(key), p.ReportID, p.CampaignIDs,
		reportDate(p.WindowStart), reportDate(p.WindowEnd), p.SubmittedAt)
	tag, err := r.db.Exec(ctx, markKeywordReportPendingQuery, args...)
	if err != nil {
		return false, fmt.Errorf("mark keyword report pending: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteKeywordReport implements domain.KeywordReportRepository.
func (r *KeywordReportRepo) CompleteKeywordReport(ctx context.Context, key model.KeywordReportKey, rep model.ReadyKeywordReport) (bool, error) {
	if err := validateKeywordReportKey(key); err != nil {
		return false, fmt.Errorf("complete keyword report: %w", err)
	}
	if rep.ReportID == "" {
		return false, errors.New("complete keyword report: empty report id")
	}
	if len(rep.CampaignIDs) == 0 {
		return false, errors.New("complete keyword report: empty campaign scope")
	}
	if err := validateReportWindow(rep.WindowStart, rep.WindowEnd); err != nil {
		return false, fmt.Errorf("complete keyword report: %w", err)
	}
	if rep.AsOf.IsZero() {
		return false, errors.New("complete keyword report: zero as-of")
	}
	// '[]', never JSON null, for AccountReportRepo.CompleteAccountReport's reason.
	rows := rep.Rows
	if rows == nil {
		rows = []model.KeywordReportRow{}
	}
	rowsJSON, err := json.Marshal(rows)
	if err != nil {
		return false, fmt.Errorf("complete keyword report: marshal rows: %w", err)
	}
	args := append(keywordReportKeyArgs(key), rep.ReportID, rowsJSON, rep.Partial, rep.CampaignIDs,
		reportDate(rep.WindowStart), reportDate(rep.WindowEnd), rep.AsOf)
	tag, err := r.db.Exec(ctx, completeKeywordReportQuery, args...)
	if err != nil {
		return false, fmt.Errorf("complete keyword report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailKeywordReport implements domain.KeywordReportRepository.
func (r *KeywordReportRepo) FailKeywordReport(ctx context.Context, key model.KeywordReportKey, reportID, reason string, at time.Time) (bool, error) {
	if err := validateKeywordReportKey(key); err != nil {
		return false, fmt.Errorf("fail keyword report: %w", err)
	}
	if reportID == "" {
		return false, errors.New("fail keyword report: empty report id")
	}
	if at.IsZero() {
		return false, errors.New("fail keyword report: zero failed-at")
	}
	args := append(keywordReportKeyArgs(key), reportID, clipFailureReason(reason), at)
	tag, err := r.db.Exec(ctx, failKeywordReportQuery, args...)
	if err != nil {
		return false, fmt.Errorf("fail keyword report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func keywordReportKeyArgs(key model.KeywordReportKey) []any {
	return []any{key.ProjectID, string(key.Platform), key.AccountID, string(key.Window)}
}

func validateKeywordReportKey(key model.KeywordReportKey) error {
	switch {
	case key.ProjectID == "":
		return errors.New("empty project id")
	case key.Platform == "":
		return errors.New("empty platform")
	case key.AccountID == "":
		return errors.New("empty account id")
	case !model.IsValidMetricsWindow(key.Window):
		return fmt.Errorf("invalid report window %q", key.Window)
	}
	return nil
}

func scanKeywordReport(key model.KeywordReportKey, row pgx.Row) (*model.KeywordReportSnapshot, error) {
	var (
		readyID, pendingID, lastFailure    *string
		readyRows                          []byte
		readyPartial                       *bool
		readyScope, pendingScope           []string
		readyStart, readyEnd, readyAsOf    *time.Time
		pendingStart, pendingEnd, pendSubm *time.Time
		lastFailureAt                      *time.Time
	)
	if err := row.Scan(
		&readyID, &readyRows, &readyPartial, &readyScope, &readyStart, &readyEnd, &readyAsOf,
		&pendingID, &pendingScope, &pendingStart, &pendingEnd, &pendSubm,
		&lastFailure, &lastFailureAt,
	); err != nil {
		return nil, err
	}
	snap := &model.KeywordReportSnapshot{Key: key, LastFailure: derefStr(lastFailure)}
	if lastFailureAt != nil {
		t := *lastFailureAt
		snap.LastFailureAt = &t
	}
	if readyID != nil {
		// Undecodable rows are an error, not an empty report: an empty slice would claim "none
		// of the project's keywords served".
		var rows []model.KeywordReportRow
		if err := json.Unmarshal(readyRows, &rows); err != nil {
			return nil, fmt.Errorf("decode ready_rows: %w", err)
		}
		snap.Ready = &model.ReadyKeywordReport{
			ReportID:    *readyID,
			Rows:        rows,
			Partial:     derefAccountReportBool(readyPartial),
			CampaignIDs: readyScope,
			WindowStart: reportDate(derefAccountReportTime(readyStart)),
			WindowEnd:   reportDate(derefAccountReportTime(readyEnd)),
			AsOf:        derefAccountReportTime(readyAsOf),
		}
	}
	if pendingID != nil {
		snap.Pending = &model.PendingKeywordReport{
			ReportID:    *pendingID,
			CampaignIDs: pendingScope,
			WindowStart: reportDate(derefAccountReportTime(pendingStart)),
			WindowEnd:   reportDate(derefAccountReportTime(pendingEnd)),
			SubmittedAt: derefAccountReportTime(pendSubm),
		}
	}
	return snap, nil
}
