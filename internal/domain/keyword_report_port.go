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
