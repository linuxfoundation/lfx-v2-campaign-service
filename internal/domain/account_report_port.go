// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// AccountReportRepository persists the saved account reports behind the asynchronous half of
// the account monitor (see model.AccountReportSnapshot).
//
// The pending and ready halves are written by separate, single-statement operations, and
// completing or failing a report is a compare-and-set on its report id. Two requests for the
// same account can race — both may submit, and the later submission wins the pending slot — and
// the compare-and-set is what stops a request that collected an OLDER report from clearing the
// newer one's pending marker. Nothing here holds a lock across a platform call.
type AccountReportRepository interface {
	// GetAccountReport returns the snapshot for key, or ErrNotFound when nothing was ever
	// saved for it.
	GetAccountReport(ctx context.Context, key model.AccountReportKey) (*model.AccountReportSnapshot, error)
	// MarkAccountReportPending records a newly submitted report as the key's pending one,
	// creating the row if needed. It replaces any earlier pending report and leaves the ready
	// half untouched.
	MarkAccountReportPending(ctx context.Context, key model.AccountReportKey, p model.PendingAccountReport) error
	// CompleteAccountReport stores r as the key's ready report and clears the pending half,
	// but only if the pending report is still r.ReportID. applied=false means a newer
	// submission replaced it meanwhile (or the row is gone), and nothing was written.
	CompleteAccountReport(ctx context.Context, key model.AccountReportKey, r model.ReadyAccountReport) (applied bool, err error)
	// FailAccountReport clears the pending half and records reason, under the same
	// compare-and-set as CompleteAccountReport. The ready half is untouched.
	FailAccountReport(ctx context.Context, key model.AccountReportKey, reportID, reason string, at time.Time) (applied bool, err error)
}
