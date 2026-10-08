// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Keyword performance primitives — the Microsoft counterpart of googleads/keywords.go's
// GetKeywordPerformance, split into STATELESS submit/check steps for the same reason monitor.go
// splits the account report: Microsoft serves keyword performance only through the asynchronous
// Reporting service (KeywordPerformanceReportRequest), whose reports take minutes, while the
// keyword read runs inside a 20s request budget.
//
//	SubmitKeywordReport -> a ReportRequestId the CALLER persists
//	CheckKeywordReport  -> exactly ONE Poll (no loop, no sleep); on Success, the rows
//
// This layer holds no state; the orchestrator keeps the saved report between requests.
//
// Contract, verified against learn.microsoft.com on 2026-10-05 (NOT against a live account —
// the same UNVERIFIED CONTRACT caveat as metrics.go applies, and the same
// MICROSOFT_METRICS_ENABLED gate in the dispatcher):
//
//   - KeywordPerformanceReportRequest — Aggregation, Columns, Filter, MaxRows, Scope
//     (AccountThroughAdGroupReportScope), Time; Summary is the default aggregation and
//     TimePeriod must be OMITTED with it.
//     https://learn.microsoft.com/en-us/advertising/reporting-service/keywordperformancereportrequest?view=bingads-13
//   - KeywordPerformanceReportColumn — CampaignId, CampaignName, AdGroupId, AdGroupName,
//     KeywordId, Keyword, BidMatchType (Broad/Exact/Phrase/Unknown), KeywordStatus, QualityScore
//     (1-10, "--" when not computed), Impressions, Clicks, Spend, ConversionsQualified (double;
//     the plain Conversions column is deprecated and returns 0).
//     https://learn.microsoft.com/en-us/advertising/reporting-service/keywordperformancereportcolumn?view=bingads-13
//   - AccountThroughAdGroupReportScope — "The report scope includes a union of the AccountIds,
//     AdGroups, and Campaigns elements"; Campaigns holds "up to 300 campaigns".
//     https://learn.microsoft.com/en-us/advertising/reporting-service/accountthroughadgroupreportscope?view=bingads-13
//   - KeywordStatusReportFilter — Active, Paused, Deleted; "also used as column values".
//     https://learn.microsoft.com/en-us/advertising/reporting-service/keywordstatusreportfilter?view=bingads-13

// MaxKeywordReportCampaigns is the documented ceiling on AccountThroughAdGroupReportScope's
// Campaigns element ("A list of up to 300 campaigns"). A larger scope cannot be sent as one
// report, and splitting it would need several pending reports per key; the dispatcher refuses
// it instead, before any upstream call.
const MaxKeywordReportCampaigns = 300

// ErrKeywordReportScope marks a keyword-report scope this client will not send: empty, too
// large, or holding an id that is not a Microsoft campaign id. An EMPTY scope is refused rather
// than sent without a Campaigns element, because the report scope is the UNION of its elements
// — with none of them Microsoft would refuse the request at best, and nothing here may ever
// build a report wider than the caller's own campaigns.
var ErrKeywordReportScope = errors.New("microsoft-ads: invalid keyword report scope")

// ErrKeywordReportScopeRejected marks Microsoft refusing the campaign-only report scope itself
// (error 2027 / InvalidAccountThruCampaignReportScope). The request shape is fixed by this client,
// so the same request is refused every time: the caller must treat it as permanent, never as a
// transient failure to retry on the next read.
var ErrKeywordReportScopeRejected = errors.New("microsoft-ads: the campaign-only keyword report scope was rejected")

// ValidateKeywordReportCampaignID reports whether id is a canonical Microsoft campaign id — a
// positive integer, no leading zero, at most 18 digits — the shape SubmitKeywordReport sends.
// Exported so the dispatcher can refuse a malformed stored id BEFORE any upstream call.
func ValidateKeywordReportCampaignID(id string) error {
	if !monitorAccountIDRE.MatchString(id) {
		return fmt.Errorf("%w: campaign id %q is not a Microsoft campaign id", ErrKeywordReportScope, clipID(id))
	}
	return nil
}

// ValidateKeywordReportWindow reports whether window has a Microsoft date-range mapping, without
// a clock. The dispatcher calls it BEFORE resolving a connection so an unsupported window is the
// same 400 whatever the connection's state.
func ValidateKeywordReportWindow(window model.MetricsWindow) error {
	_, _, err := reportDateRange(window, time.Unix(0, 0))
	return err
}

// ReportWindowDates returns the calendar dates a campaign-scoped report submitted at now covers
// for window — reportDateRange, the rule SubmitKeywordReport and SubmitAgeGenderReport send — so
// a caller can tell whether a saved report describes the period a window means NOW.
func ReportWindowDates(window model.MetricsWindow, now time.Time) (start, end time.Time, err error) {
	return reportDateRange(window, now)
}

// keywordReportColumns is the column list SubmitKeywordReport requests, in the order the
// reasoning on SubmitKeywordReport gives. foldKeywordReportRows resolves every column BY NAME,
// so the order is presentational only.
var keywordReportColumns = []string{
	"CampaignId", "CampaignName", "AdGroupId", "AdGroupName", "KeywordId", "Keyword",
	"BidMatchType", "KeywordStatus", "QualityScore",
	"Impressions", "Clicks", "Spend", "ConversionsQualified",
}

// SubmitKeywordReport submits a keyword performance report over window, scoped to campaignIDs
// on the client's account, and returns its ReportRequestId with the calendar window it covers.
// It does not wait for the report.
//
// SCOPE IS Campaigns ONLY — never AccountIds. The scope is a UNION of its elements, so adding
// AccountIds would widen the read to every campaign on the account, which on an LF-shared account
// is every other foundation's keywords and spend. Each Campaigns entry carries the account id
// nested, as CampaignReportScope requires. The open 2027 (InvalidAccountThruCampaignReportScope)
// question recorded on submitReport applies here unchanged; it is surfaced verbatim in the error
// rather than worked around by widening the scope.
//
// Columns: ids for every level (CampaignId, AdGroupId, KeywordId) so a row is addressable by the
// same (ad group id, keyword id) pair the keyword mutation takes; the display names; the
// keyword's own attributes (Keyword, BidMatchType — the match type the advertiser BID, not the
// per-query DeliveredMatchType, which would split one keyword into several rows — KeywordStatus,
// QualityScore); and the counters. ConversionsQualified, never the deprecated Conversions column,
// for the reason recorded on submitReportDefinition. Ctr and AverageCpc are NOT requested: both
// are ratios Microsoft computes per row, and the rows are summed here, so they are recomputed
// from the summed counters instead of averaged.
//
// No Filter and no MaxRows. Deleted keywords are dropped while folding (KeywordStatusReportFilter
// is an xs:list whose JSON rendering is unverified, and a wrong filter would fail every report),
// and MaxRows would truncate BEFORE that drop and before the per-keyword sum, so the cap and the
// truncated flag are applied by the caller over the folded rows instead.
//
// Aggregation Summary with no TimePeriod column, ReturnOnlyCompleteData=false and the shared
// reportTime are the account monitor's settings, for the same reasons (submitReportDefinition).
func (c *Client) SubmitKeywordReport(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (reportID string, windowStart, windowEnd time.Time, err error) {
	if err := validateKeywordReportScope(campaignIDs); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return c.submitCampaignScopedReport(ctx, "KeywordPerformanceReportRequest", "keyword", keywordReportColumns, window, campaignIDs, ErrKeywordReportScopeRejected)
}

// submitCampaignScopedReport submits a Summary-aggregated CSV report of reportType over window,
// scoped to campaignIDs (already validated by the caller) on the client's account, with the
// settings SubmitKeywordReport documents: Campaigns-only scope, each entry carrying the account
// id nested, ReturnOnlyCompleteData=false, no Filter, no MaxRows, the shared reportTime. A
// 2027/InvalidAccountThruCampaignReportScope rejection is returned wrapping rejected, never
// retried with a wider scope. Shared by the keyword and age/gender reports so neither can drift
// onto a wider scope or another day boundary.
func (c *Client) submitCampaignScopedReport(ctx context.Context, reportType, what string, columns []string, window model.MetricsWindow, campaignIDs []string, rejected error) (reportID string, windowStart, windowEnd time.Time, err error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	start, end, err := reportDateRange(window, c.now())
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	campaigns := make([]map[string]any, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		// Quoted strings for `long`, as submitReport documents.
		campaigns = append(campaigns, map[string]any{"AccountId": c.account.AccountID, "CampaignId": id})
	}
	body := map[string]any{
		"ReportRequest": map[string]any{
			"Type":                   reportType,
			"Format":                 "Csv",
			"ReturnOnlyCompleteData": false,
			"Aggregation":            "Summary",
			"Columns":                columns,
			"Scope":                  map[string]any{"Campaigns": campaigns},
			"Time":                   reportTime(start, end),
		},
	}
	id, err := c.submitReportRequest(ctx, body)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.hasErrorCode(msErrCodeInvalidScope) || ae.hasErrorCode(msErrNameInvalidScope)) {
			return "", time.Time{}, time.Time{}, fmt.Errorf("submit microsoft %s report: %w "+
				"(error %s/%s); it is NOT widened to AccountIds here, because that would read every campaign on the account — see "+
				"docs/knowledge/log/2026-08-18-LFXV2-3260-scope-union-tradeoff.md: %w",
				what, rejected, msErrCodeInvalidScope, msErrNameInvalidScope, errors.Unwrap(err))
		}
		return "", time.Time{}, time.Time{}, err
	}
	return id, start, end, nil
}

// validateKeywordReportScope refuses an empty scope, one past the documented ceiling, and any
// id that is not a canonical positive Microsoft id (the same shape as an account id).
func validateKeywordReportScope(ids []string) error {
	return validateCampaignReportScope(ids, ErrKeywordReportScope)
}

// validateCampaignReportScope is validateKeywordReportScope's rule with the kind's own
// sentinel: the scope ceiling and id shape are AccountThroughAdGroupReportScope's, which both
// the keyword and the age/gender report requests take.
func validateCampaignReportScope(ids []string, sentinel error) error {
	if len(ids) == 0 {
		return fmt.Errorf("%w: no campaigns; an empty scope is never sent", sentinel)
	}
	if len(ids) > MaxKeywordReportCampaigns {
		return fmt.Errorf("%w: %d campaigns exceeds the documented limit of %d", sentinel, len(ids), MaxKeywordReportCampaigns)
	}
	for _, id := range ids {
		if !monitorAccountIDRE.MatchString(id) {
			return fmt.Errorf("%w: campaign id %q is not a Microsoft campaign id", sentinel, clipID(id))
		}
	}
	return nil
}

// KeywordReportRow is one keyword's totals over the report window.
type KeywordReportRow struct {
	CampaignID   string
	CampaignName string
	AdGroupID    string
	AdGroupName  string
	KeywordID    string
	Keyword      string
	// MatchType is BidMatchType verbatim (Broad, Exact, Phrase, Unknown).
	MatchType string
	// Status is KeywordStatus verbatim (Active, Paused). Deleted rows are dropped while folding.
	Status string
	// QualityScore is nil when Microsoft reported "--" (not computed) or a blank cell.
	QualityScore *int64
	Impressions  int64
	Clicks       int64
	// Spend is a decimal in the ACCOUNT's currency, unconverted.
	Spend float64
	// Conversions is ConversionsQualified summed, or nil when the column was absent or ANY of
	// this keyword's cells was blank. nil is "unknown", never zero.
	Conversions *float64
}

// KeywordReportResult is the outcome of one CheckKeywordReport. Status reuses the account
// report's vocabulary: it is the same Poll.
type KeywordReportResult struct {
	Status AccountReportStatus
	// Rows is set only for Success: one row per (ad group, keyword), first-appearance order.
	// Empty (non-nil) when none of the scoped campaigns' keywords served.
	Rows []KeywordReportRow
	// Partial is true when Microsoft flagged the data as potentially incomplete.
	Partial bool
}

// CheckKeywordReport polls a report submitted by SubmitKeywordReport EXACTLY ONCE and, if it is
// built, downloads and folds it per keyword.
//
// Success with no download URL, and a header-only CSV, are an EMPTY Success — not an error, for
// the account report's reason (CheckAccountCampaignReport): the scope is a set of campaigns this
// service already proved the project owns on this account, so an empty report can only mean
// none of their keywords served in the window. Partial data is reported, not refused, likewise.
func (c *Client) CheckKeywordReport(ctx context.Context, reportID string) (*KeywordReportResult, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, fmt.Errorf("microsoft report id is required")
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	status, downloadURL, err := c.pollOnce(ctx, reportID)
	if err != nil {
		return nil, err
	}
	switch AccountReportStatus(status) {
	case AccountReportStatusPending:
		return &KeywordReportResult{Status: AccountReportStatusPending}, nil
	case AccountReportStatusError:
		return &KeywordReportResult{Status: AccountReportStatusError}, nil
	case AccountReportStatusSuccess:
	default:
		return nil, fmt.Errorf("microsoft report %s returned unrecognized status %q", clipID(reportID), status)
	}
	if downloadURL == "" {
		return &KeywordReportResult{Status: AccountReportStatusSuccess, Rows: []KeywordReportRow{}}, nil
	}
	records, err := c.downloadReportRecords(ctx, downloadURL)
	if err != nil {
		return nil, err
	}
	return foldKeywordReportRows(records)
}

// keywordStatusDeleted is the one KeywordStatus the read drops: a deleted keyword cannot be
// acted on, and offering it would hand the UI a handle whose only use is guaranteed to fail.
const keywordStatusDeleted = "Deleted"

type keywordReportAcc struct {
	row            KeywordReportRow
	convTotal      *float64
	convIncomplete bool
}

// foldKeywordReportRows folds a downloaded keyword report into one row per (ad group, keyword),
// with foldAccountReportRows' discipline: columns by header name; the id and counter columns
// required; negative or non-finite values and int64 overflow refused; an unattributable id
// fails the WHOLE read (dropping the row would under-report its keyword as idle); a blank
// conversion cell withdraws conversions for that keyword only.
func foldKeywordReportRows(records [][]string) (*KeywordReportResult, error) {
	header, rows, preamble, err := reportHeaderAndRows(records)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.ToLower(strings.TrimSpace(name))] = i
	}
	col := func(name string) (int, bool) { i, ok := idx[strings.ToLower(name)]; return i, ok }
	required := []string{"CampaignId", "AdGroupId", "KeywordId", "Keyword", "Impressions", "Clicks", "Spend"}
	for _, name := range required {
		if _, ok := col(name); !ok {
			return nil, fmt.Errorf("microsoft keyword report csv missing required column %s (have %v)", name, header)
		}
	}
	campCol, _ := col("CampaignId")
	agCol, _ := col("AdGroupId")
	kwCol, _ := col("KeywordId")
	textCol, _ := col("Keyword")
	impCol, _ := col("Impressions")
	clkCol, _ := col("Clicks")
	spendCol, _ := col("Spend")
	campNameCol, campNameOK := col("CampaignName")
	agNameCol, agNameOK := col("AdGroupName")
	matchCol, matchOK := col("BidMatchType")
	statusCol, statusOK := col("KeywordStatus")
	qsCol, qsOK := col("QualityScore")
	convCol, convOK := col("ConversionsQualified")

	cell := func(row []string, i int, ok bool) string {
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	reportID := func(row []string, i int, what string, n int) (string, error) {
		if i >= len(row) {
			return "", fmt.Errorf("keyword report row %d: row has %d columns, wanted column %d", n, len(row), i)
		}
		raw := strings.TrimSpace(row[i])
		num := json.Number(raw)
		id := numberID(&num)
		if id == "" {
			return "", fmt.Errorf("keyword report row %d: %s %q is not an id; the row cannot be attributed", n, what, clipID(raw))
		}
		return id, nil
	}

	order := make([]string, 0)
	byKey := map[string]*keywordReportAcc{}
	for i, row := range rows {
		campID, err := reportID(row, campCol, "CampaignId", i)
		if err != nil {
			return nil, err
		}
		agID, err := reportID(row, agCol, "AdGroupId", i)
		if err != nil {
			return nil, err
		}
		kwID, err := reportID(row, kwCol, "KeywordId", i)
		if err != nil {
			return nil, err
		}
		status := cell(row, statusCol, statusOK)
		if strings.EqualFold(status, keywordStatusDeleted) {
			continue
		}
		imp, err := parseReportInt(row, impCol)
		if err != nil {
			return nil, fmt.Errorf("keyword %s impressions: %w", kwID, err)
		}
		clk, err := parseReportInt(row, clkCol)
		if err != nil {
			return nil, fmt.Errorf("keyword %s clicks: %w", kwID, err)
		}
		spend, err := parseReportFloat(row, spendCol)
		if err != nil {
			return nil, fmt.Errorf("keyword %s spend: %w", kwID, err)
		}
		if imp < 0 || clk < 0 || math.IsNaN(spend) || math.IsInf(spend, 0) || spend < 0 {
			return nil, fmt.Errorf("keyword %s: negative or non-finite counter (impressions %d, clicks %d, spend %v)", kwID, imp, clk, spend)
		}

		// Keyword ids are unique within the account, but the ad group travels with the key so
		// the pair the mutation addresses is the pair rows are grouped by.
		key := agID + "/" + kwID
		acc, seen := byKey[key]
		if !seen {
			acc = &keywordReportAcc{row: KeywordReportRow{
				CampaignID: campID, AdGroupID: agID, KeywordID: kwID,
				CampaignName: cell(row, campNameCol, campNameOK),
				AdGroupName:  cell(row, agNameCol, agNameOK),
				Keyword:      cell(row, textCol, true),
				MatchType:    cell(row, matchCol, matchOK),
				Status:       status,
			}}
			byKey[key] = acc
			order = append(order, key)
		} else if acc.row.CampaignID != campID {
			return nil, fmt.Errorf("keyword report row %d: keyword %s in ad group %s is reported under two campaigns (%s, %s)", i, kwID, agID, acc.row.CampaignID, campID)
		}
		if acc.row.QualityScore == nil {
			acc.row.QualityScore = parseQualityScore(cell(row, qsCol, qsOK))
		}
		if imp > 0 && acc.row.Impressions > math.MaxInt64-imp {
			return nil, fmt.Errorf("keyword %s impressions: total would overflow", kwID)
		}
		acc.row.Impressions += imp
		if clk > 0 && acc.row.Clicks > math.MaxInt64-clk {
			return nil, fmt.Errorf("keyword %s clicks: total would overflow", kwID)
		}
		acc.row.Clicks += clk
		total := acc.row.Spend + spend
		if math.IsInf(total, 0) {
			return nil, fmt.Errorf("keyword %s spend: total would overflow", kwID)
		}
		acc.row.Spend = total

		if !convOK {
			continue
		}
		conv, present, cerr := parseConversionCell(row, convCol)
		if cerr != nil {
			return nil, fmt.Errorf("keyword %s conversionsQualified: %w", kwID, cerr)
		}
		if !present {
			acc.convIncomplete = true
			continue
		}
		if math.IsNaN(conv) || math.IsInf(conv, 0) || conv < 0 {
			return nil, fmt.Errorf("keyword %s conversionsQualified: non-finite or negative value %v", kwID, conv)
		}
		var running float64
		if acc.convTotal != nil {
			running = *acc.convTotal
		}
		sum := running + conv
		if math.IsInf(sum, 0) {
			return nil, fmt.Errorf("keyword %s conversionsQualified: total would overflow", kwID)
		}
		acc.convTotal = &sum
	}

	out := &KeywordReportResult{
		Status:  AccountReportStatusSuccess,
		Rows:    make([]KeywordReportRow, 0, len(order)),
		Partial: reportDataIsIncomplete(preamble),
	}
	for _, k := range order {
		acc := byKey[k]
		row := acc.row
		if !acc.convIncomplete {
			row.Conversions = acc.convTotal
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// parseQualityScore reads a QualityScore cell: an integer 1-10, or nil for "--" (Microsoft's
// documented "not computed"), a blank, or anything off the scale. Off-scale is nil rather than
// an error: the score is advisory, and one unreadable score must not hide the keyword's
// counters — the same choice googleads/keywords.go makes for an out-of-range score.
func parseQualityScore(s string) *int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 1 || v > 10 {
		return nil
	}
	return &v
}
