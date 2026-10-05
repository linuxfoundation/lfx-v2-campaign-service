// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// Account reports are the ASYNCHRONOUS half of the account monitor, for a platform whose
// performance data cannot be read inside one request.
//
// Microsoft Advertising is the case that forces it. Its delivery metrics come only from the
// Reporting service — submit, poll, download — and Microsoft documents that reports "complete
// within minutes" and should be polled at 2–15 minute intervals, while the monitor read runs
// inside a 20-second call budget. A synchronous read would therefore essentially never see a
// finished report. Instead the service keeps one saved report per (project, platform, account,
// days): a request reads the campaign list live, serves metrics from the last report that
// finished, and checks on (or submits) the next one without waiting for it.
//
// The types are platform-neutral on purpose. Nothing below is Microsoft-specific, so a second
// platform with an asynchronous reporting surface (X is the likely one) reuses the same store
// and the same orchestration rather than growing a parallel copy.

// AccountReportStatus is where a submitted account report stands on the platform.
type AccountReportStatus string

// Account report statuses.
const (
	// AccountReportPending: the platform is still building the report.
	AccountReportPending AccountReportStatus = "pending"
	// AccountReportReady: the report finished and its rows were read.
	AccountReportReady AccountReportStatus = "ready"
	// AccountReportFailed: the platform reported the report itself as failed. Terminal; a
	// fresh report must be submitted.
	AccountReportFailed AccountReportStatus = "failed"
)

// AccountReportRow is one campaign's delivery totals over a report's window.
//
// JSON tags because rows are persisted as a JSONB array (account_monitor_reports.ready_rows).
// Conversions keeps the pointer discipline of AccountCampaignMetrics.Conversions: nil means the
// platform did not report a count, which is not the same claim as a measured 0.
type AccountReportRow struct {
	PlatformCampaignID string   `json:"platform_campaign_id"`
	Spend              float64  `json:"spend"`
	Impressions        int64    `json:"impressions"`
	Clicks             int64    `json:"clicks"`
	Conversions        *float64 `json:"conversions,omitempty"`
}

// AccountReportSubmission is what submitting a report returns: the platform's id for it and
// the calendar window it covers (UTC dates; the time-of-day part is not meaningful).
type AccountReportSubmission struct {
	ReportID    string
	WindowStart time.Time
	WindowEnd   time.Time
}

// AccountReportCheck is the outcome of checking a submitted report once.
//
// Rows is set only when Status is AccountReportReady, and is then authoritative for the whole
// account: a report scoped to the account covers every campaign on it, so a campaign absent
// from Rows served nothing in the window. Partial means the platform flagged the window's last
// day as still aggregating — expected whenever the window includes today, which the monitor's
// window always does.
type AccountReportCheck struct {
	Status  AccountReportStatus
	Rows    []AccountReportRow
	Partial bool
}

// AccountReportKey identifies one saved report. days is part of the key because a 7-day and a
// 30-day view are different reports, not one report read two ways.
type AccountReportKey struct {
	ProjectID string
	Platform  Provider
	AccountID string
	Days      int
}

// ReadyAccountReport is the last report that finished, kept until a newer one replaces it so a
// request can serve slightly stale numbers rather than none while the next report builds.
type ReadyAccountReport struct {
	ReportID    string
	Rows        []AccountReportRow
	Partial     bool
	WindowStart time.Time
	WindowEnd   time.Time
	CompletedAt time.Time
}

// PendingAccountReport is a report submitted to the platform and not yet collected.
type PendingAccountReport struct {
	ReportID    string
	WindowStart time.Time
	WindowEnd   time.Time
	SubmittedAt time.Time
}

// AccountReportSnapshot is everything saved for one AccountReportKey. Ready and Pending are
// independent: a newer report can be building while an older one is still being served.
type AccountReportSnapshot struct {
	Key     AccountReportKey
	Ready   *ReadyAccountReport
	Pending *PendingAccountReport
	// LastFailure / LastFailureAt record the most recent report the platform failed or this
	// service abandoned, for operators. They never block a fresh submission.
	LastFailure   string
	LastFailureAt *time.Time
}

// ReportedAccountRead is what the orchestrator hands the monitor for a report-backed platform:
// the live campaign rows, with metrics filled from the saved report where one exists, and the
// two facts a reader needs to judge those metrics.
type ReportedAccountRead struct {
	// Rows always carries every campaign the live list returned. When no finished report
	// exists, each row has FetchFailed set: the rule engines skip it rather than reading its
	// zero metrics as a measurement.
	Rows []AccountCampaignMetrics
	// MetricsAsOf is when the report the metrics came from finished; nil when no report has
	// finished yet.
	MetricsAsOf *time.Time
	// MetricsPending is true while a report is building on the platform, so a later request
	// will see newer (or first) metrics.
	MetricsPending bool
}
