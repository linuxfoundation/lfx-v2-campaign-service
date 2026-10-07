// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// KeywordReportRepository persists the saved keyword reports behind the report-backed keyword
// read (see model.KeywordReportSnapshot). Same contract as AccountReportRepository, method for
// method: the first pending mark wins, and complete/fail are a compare-and-set on the pending
// report id so a request holding an OLDER report can never clear a newer one's marker.
type KeywordReportRepository interface {
	// GetKeywordReport returns the snapshot for key, or ErrNotFound when nothing was saved.
	GetKeywordReport(ctx context.Context, key model.KeywordReportKey) (*model.KeywordReportSnapshot, error)
	// MarkKeywordReportPending records p as the key's pending report only when none is pending;
	// applied=false means a concurrent request marked its own first. The ready half is untouched.
	MarkKeywordReportPending(ctx context.Context, key model.KeywordReportKey, p model.PendingKeywordReport) (applied bool, err error)
	// CompleteKeywordReport stores r as the ready report and clears the pending half, only if
	// the pending report is still r.ReportID.
	CompleteKeywordReport(ctx context.Context, key model.KeywordReportKey, r model.ReadyKeywordReport) (applied bool, err error)
	// FailKeywordReport clears the pending half and records reason, under the same
	// compare-and-set. The ready half is untouched.
	FailKeywordReport(ctx context.Context, key model.KeywordReportKey, reportID, reason string, at time.Time) (applied bool, err error)
}

// AudienceReportRepository persists the saved age/gender reports behind the report-backed
// audience read — the second report KIND of the same store (model.InsightReportAgeGender,
// migration 000041). The contract is KeywordReportRepository's, method for method; a store
// implementing both keeps the kinds apart structurally, so a keyword report is never returned,
// completed or failed through these methods, nor an audience report through those.
type AudienceReportRepository interface {
	// GetAudienceReport returns the age/gender snapshot for key, or ErrNotFound when nothing at
	// all was saved for key. A key that has only a keyword report saved returns an empty
	// snapshot (no Ready, no Pending).
	GetAudienceReport(ctx context.Context, key model.InsightReportKey) (*model.AudienceReportSnapshot, error)
	// MarkAudienceReportPending records p as the key's pending age/gender report only when none
	// is pending; applied=false means a concurrent request marked its own first.
	MarkAudienceReportPending(ctx context.Context, key model.InsightReportKey, p model.PendingInsightReport) (applied bool, err error)
	// CompleteAudienceReport stores r as the ready age/gender report and clears the pending
	// half, only if the pending report is still r.ReportID.
	CompleteAudienceReport(ctx context.Context, key model.InsightReportKey, r model.ReadyAudienceReport) (applied bool, err error)
	// FailAudienceReport clears the pending half and records reason, under the same
	// compare-and-set. The ready half is untouched.
	FailAudienceReport(ctx context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (applied bool, err error)
}
