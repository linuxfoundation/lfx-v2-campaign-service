// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The age/gender report KIND of KeywordReportRepo (model.InsightReportAgeGender): the keyword
// kind's four statements over 000041's age_gender_-prefixed columns of the same row. Every
// statement names ONLY those columns, so a keyword report can never be read, completed, failed
// or served through these methods (and the keyword statements name none of them), and the mark's
// conflict guard checks this kind's pending id only, so one kind building never blocks the other.

var _ domain.AudienceReportRepository = (*KeywordReportRepo)(nil)

// $1..$4 = project_id, platform, account_id, report_window in every statement.
const (
	getAudienceReportQuery = `SELECT
		age_gender_ready_report_id, age_gender_ready_rows, age_gender_ready_partial,
		age_gender_ready_campaign_ids, age_gender_ready_window_start, age_gender_ready_window_end,
		age_gender_ready_as_of,
		age_gender_pending_report_id, age_gender_pending_campaign_ids,
		age_gender_pending_window_start, age_gender_pending_window_end,
		age_gender_pending_submitted_at,
		age_gender_last_failure, age_gender_last_failure_at
		FROM keyword_insight_reports
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4`

	markAudienceReportPendingQuery = `INSERT INTO keyword_insight_reports
		(project_id, platform, account_id, report_window,
		 age_gender_pending_report_id, age_gender_pending_campaign_ids,
		 age_gender_pending_window_start, age_gender_pending_window_end,
		 age_gender_pending_submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (project_id, platform, account_id, report_window) DO UPDATE SET
			age_gender_pending_report_id    = EXCLUDED.age_gender_pending_report_id,
			age_gender_pending_campaign_ids = EXCLUDED.age_gender_pending_campaign_ids,
			age_gender_pending_window_start = EXCLUDED.age_gender_pending_window_start,
			age_gender_pending_window_end   = EXCLUDED.age_gender_pending_window_end,
			age_gender_pending_submitted_at = EXCLUDED.age_gender_pending_submitted_at,
			updated_at                      = now()
		WHERE keyword_insight_reports.age_gender_pending_report_id IS NULL`

	completeAudienceReportQuery = `UPDATE keyword_insight_reports SET
		age_gender_ready_report_id      = $5,
		age_gender_ready_rows           = $6,
		age_gender_ready_partial        = $7,
		age_gender_ready_campaign_ids   = $8,
		age_gender_ready_window_start   = $9,
		age_gender_ready_window_end     = $10,
		age_gender_ready_as_of          = $11,
		age_gender_pending_report_id    = NULL,
		age_gender_pending_campaign_ids = NULL,
		age_gender_pending_window_start = NULL,
		age_gender_pending_window_end   = NULL,
		age_gender_pending_submitted_at = NULL,
		updated_at                      = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4
		  AND age_gender_pending_report_id = $5`

	failAudienceReportQuery = `UPDATE keyword_insight_reports SET
		age_gender_pending_report_id    = NULL,
		age_gender_pending_campaign_ids = NULL,
		age_gender_pending_window_start = NULL,
		age_gender_pending_window_end   = NULL,
		age_gender_pending_submitted_at = NULL,
		age_gender_last_failure         = $6,
		age_gender_last_failure_at      = $7,
		updated_at                      = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4
		  AND age_gender_pending_report_id = $5`
)

// audienceReportStatements is the age_gender kind's statement set.
var audienceReportStatements = insightReportStatements{
	kind:     model.InsightReportAgeGender,
	get:      getAudienceReportQuery,
	mark:     markAudienceReportPendingQuery,
	complete: completeAudienceReportQuery,
	fail:     failAudienceReportQuery,
}

// GetAudienceReport implements domain.AudienceReportRepository.
func (r *KeywordReportRepo) GetAudienceReport(ctx context.Context, key model.InsightReportKey) (*model.AudienceReportSnapshot, error) {
	return getInsightReport[model.AudienceReportRow](ctx, r.db, audienceReportStatements, key)
}

// MarkAudienceReportPending implements domain.AudienceReportRepository.
func (r *KeywordReportRepo) MarkAudienceReportPending(ctx context.Context, key model.InsightReportKey, p model.PendingInsightReport) (bool, error) {
	return markInsightReportPending(ctx, r.db, audienceReportStatements, key, p)
}

// CompleteAudienceReport implements domain.AudienceReportRepository.
func (r *KeywordReportRepo) CompleteAudienceReport(ctx context.Context, key model.InsightReportKey, rep model.ReadyAudienceReport) (bool, error) {
	return completeInsightReport(ctx, r.db, audienceReportStatements, key, rep)
}

// FailAudienceReport implements domain.AudienceReportRepository.
func (r *KeywordReportRepo) FailAudienceReport(ctx context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (bool, error) {
	return failInsightReport(ctx, r.db, audienceReportStatements, key, reportID, reason, at)
}
