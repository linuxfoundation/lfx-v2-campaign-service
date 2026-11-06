// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"
)

// RedditReportLimits sizes one ad account's reporting budget: at most Total attempts at Reddit's
// reporting operation in any Window, of which at most Insights are the project-scoped insight
// reads'.
type RedditReportLimits struct {
	Window          time.Duration
	Total, Insights int
}

// RedditReportTake is the outcome of one RedditReportLedger.Take. Taken reports a slot was
// recorded, under SlotID (the handle Release takes). Otherwise Wait is how long until the earliest
// blocking slot leaves the window, measured on the ledger's own clock.
type RedditReportTake struct {
	SlotID int64
	Taken  bool
	Wait   time.Duration
}

// RedditReportLedger is the account-wide ledger of Reddit reporting attempts, SHARED by every
// replica (issue #380): Reddit's ads-reporting limit is per ad account, so a ledger each process
// keeps for itself lets N replicas spend N times the budget. The Postgres implementation is
// postgres.RedditReportLedger; reddit.ReportBudgets drives it (the wait loop, the deadline and
// the release-on-pre-send-failure contract stay there).
type RedditReportLedger interface {
	// Take records one attempt for accountID — counted against limits.Total and, when insights
	// is set, also against limits.Insights — if both have room, atomically with every other
	// Take on the account from any replica. A full budget is not an error: it is Taken false
	// with the Wait. An error means the ledger could not be asked; the caller must then send
	// nothing (fail closed).
	Take(ctx context.Context, accountID string, insights bool, limits RedditReportLimits) (RedditReportTake, error)
	// Release hands back a slot Take recorded, for an attempt proven NOT sent. Idempotent; a
	// slot already out of the window is a no-op.
	Release(ctx context.Context, slotID int64) error
}
