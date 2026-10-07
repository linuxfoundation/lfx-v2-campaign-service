// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// Keyword reports are the saved, ASYNCHRONOUS form of the project-scoped keyword performance
// read, for a platform that serves keyword performance only through a report that takes minutes
// to build (Microsoft Advertising's KeywordPerformanceReportRequest). They follow the account
// report's shape (account_report.go) — a READY half served while a PENDING half builds, the same
// compare-and-set on the pending id — and differ in what keys and scopes them:
//
//   - The key carries the reporting WINDOW (the platform-agnostic vocabulary), not a day count:
//     the keyword read takes model.MetricsWindow, and "this_month" is not a number of days.
//   - Each half records the campaign SCOPE it was built for. The read is confined to the
//     project's own campaigns, and that set changes as campaigns are dispatched or deleted, so a
//     saved report is only served while it still covers every campaign the project now owns.

// KeywordReportKey identifies one saved keyword report. It is InsightReportKey: the key is
// shared by every report kind, which is told apart by the store method, not by the key.
type KeywordReportKey = InsightReportKey

// KeywordReportRow is one keyword's totals over a report's window, as persisted (JSONB in
// keyword_insight_reports.ready_rows). Spend is in the ACCOUNT's currency, unconverted.
// Conversions and QualityScore are pointers for the reasons given on the platform row: nil is
// "not reported", which is a different claim from a measured 0 or a score of 0.
type KeywordReportRow struct {
	CampaignID   string   `json:"campaign_id"`
	CampaignName string   `json:"campaign_name"`
	AdGroupID    string   `json:"ad_group_id"`
	AdGroupName  string   `json:"ad_group_name"`
	KeywordID    string   `json:"keyword_id"`
	Text         string   `json:"text"`
	MatchType    string   `json:"match_type"`
	Status       string   `json:"status"`
	QualityScore *int64   `json:"quality_score,omitempty"`
	Impressions  int64    `json:"impressions"`
	Clicks       int64    `json:"clicks"`
	Spend        float64  `json:"spend"`
	Conversions  *float64 `json:"conversions,omitempty"`
}

// KeywordReportSubmission is what submitting a keyword report returns (InsightReportSubmission).
type KeywordReportSubmission = InsightReportSubmission

// KeywordReportCheck is the outcome of checking a submitted keyword report once.
type KeywordReportCheck = InsightReportCheck[KeywordReportRow]

// ReadyKeywordReport is the last keyword report that finished.
type ReadyKeywordReport = ReadyInsightReport[KeywordReportRow]

// PendingKeywordReport is a keyword report submitted and not yet collected.
type PendingKeywordReport = PendingInsightReport

// KeywordReportSnapshot is everything saved for one key's keyword report.
type KeywordReportSnapshot = InsightReportSnapshot[KeywordReportRow]

// ReportedKeywordRead is the keyword read for a report-backed platform: the rows the API
// publishes, plus the facts a reader needs to judge them.
type ReportedKeywordRead struct {
	KeywordPerformance
	// ConversionsComplete is false when at least one published row's conversion count was not
	// reported by the platform (published as 0). A consumer must then not read those zeros as
	// measured, nor total conversions into a CPA.
	ConversionsComplete bool
	// MetricsAsOf is when the served report was requested from the platform; nil when no report
	// covering the project's current campaigns has finished yet (Rows is then empty).
	MetricsAsOf *time.Time
	// MetricsPending is true while a newer report is building.
	MetricsPending bool
	// DataIncomplete is the served report's Partial flag: the platform said its last day may
	// still be aggregating. False when no report is served.
	DataIncomplete bool
}
