// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// Insight reports generalise the saved keyword report (keyword_report.go) over a report KIND:
// the same READY/PENDING halves, the same compare-and-set on the pending id and the same
// recorded campaign scope, with only the row type differing per kind. Two kinds exist, both
// Microsoft Advertising Reporting v13 reports scoped to the project's own campaigns:
//
//   - InsightReportKeywords  — KeywordPerformanceReportRequest, rows KeywordReportRow;
//   - InsightReportAgeGender — AgeGenderAudienceReportRequest, rows AudienceReportRow.
//
// The kind is carried by the TYPE (the row parameter) and by the store method, never by a value
// a caller could get wrong: a keyword snapshot cannot hold audience rows, and the store reads and
// writes each kind's own columns only (migration 000041).

// InsightReportKind names a saved insight report's kind.
type InsightReportKind string

const (
	// InsightReportKeywords is the keyword performance report behind get-microsoft-ads-keywords.
	InsightReportKeywords InsightReportKind = "keywords"
	// InsightReportAgeGender is the age/gender audience report behind get-microsoft-ads-audience.
	InsightReportAgeGender InsightReportKind = "age_gender"
)

// InsightReportKey identifies one saved insight report of a given kind.
//
//   - The key carries the reporting WINDOW (the platform-agnostic vocabulary), not a day count:
//     the reads take model.MetricsWindow, and "this_month" is not a number of days.
//   - AccountID is the ad account the project's own connection was bound to at submission, so a
//     re-pointed connection never serves the previous account's report.
type InsightReportKey struct {
	ProjectID string
	Platform  Provider
	AccountID string
	Window    MetricsWindow
}

// InsightReportSubmission is what submitting an insight report returns. CampaignIDs is the scope
// the platform was actually asked for, recorded with the pending half.
type InsightReportSubmission struct {
	ReportID    string
	WindowStart time.Time
	WindowEnd   time.Time
	CampaignIDs []string
}

// InsightReportCheck is the outcome of checking a submitted insight report once. Rows is set
// only when Status is AccountReportReady.
type InsightReportCheck[R any] struct {
	Status  AccountReportStatus
	Rows    []R
	Partial bool
}

// ReadyInsightReport is the last insight report of one kind that finished. AsOf is its
// SUBMISSION time, for ReadyAccountReport.AsOf's reason.
type ReadyInsightReport[R any] struct {
	ReportID    string
	Rows        []R
	Partial     bool
	CampaignIDs []string
	WindowStart time.Time
	WindowEnd   time.Time
	AsOf        time.Time
}

// PendingInsightReport is an insight report submitted and not yet collected.
type PendingInsightReport struct {
	ReportID    string
	CampaignIDs []string
	WindowStart time.Time
	WindowEnd   time.Time
	SubmittedAt time.Time
}

// InsightReportSnapshot is everything saved for one key's report of one kind.
type InsightReportSnapshot[R any] struct {
	Key           InsightReportKey
	Ready         *ReadyInsightReport[R]
	Pending       *PendingInsightReport
	LastFailure   string
	LastFailureAt *time.Time
}

// AudienceReportRow is one (campaign, age group, gender) segment's totals over a report's
// window, as persisted (JSONB in keyword_insight_reports.age_gender_ready_rows). AgeGroup and
// Gender are the platform's own values, verbatim. Spend is in the ACCOUNT's currency,
// unconverted. The campaign travels with the row so a saved report can be confined to the
// campaigns the project owns NOW.
type AudienceReportRow struct {
	CampaignID  string  `json:"campaign_id"`
	AgeGroup    string  `json:"age_group"`
	Gender      string  `json:"gender"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	Spend       float64 `json:"spend"`
}

// AudienceReportCheck is the outcome of checking a submitted age/gender report once.
type AudienceReportCheck = InsightReportCheck[AudienceReportRow]

// ReadyAudienceReport is the last age/gender report that finished.
type ReadyAudienceReport = ReadyInsightReport[AudienceReportRow]

// AudienceReportSnapshot is everything saved for one key's age/gender report.
type AudienceReportSnapshot = InsightReportSnapshot[AudienceReportRow]

// ReportedAudienceBucket is one (age group, gender) bucket of the report-backed audience read,
// summed over the project's current campaigns. CostMicros is the account currency × 10⁶ (no
// FX); Ctr is clicks/impressions as a fraction, 0 when impressions is 0.
type ReportedAudienceBucket struct {
	AgeGroup    string
	Gender      string
	Impressions int64
	Clicks      int64
	CostMicros  int64
	Ctr         float64
}

// ReportedAudienceRead is the audience read for a report-backed platform: the buckets the API
// publishes plus the facts a reader needs to judge them (ReportedKeywordRead's, minus the
// keyword-only conversions caveat).
type ReportedAudienceRead struct {
	Window  MetricsWindow
	Buckets []ReportedAudienceBucket
	// MetricsAsOf is when the served report was requested from the platform; nil when no report
	// covering the project's current campaigns has finished yet (Buckets is then empty).
	MetricsAsOf *time.Time
	// MetricsPending is true while a newer report is building.
	MetricsPending bool
	// DataIncomplete is the served report's Partial flag. False when no report is served.
	DataIncomplete bool
}
