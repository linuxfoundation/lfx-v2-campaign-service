// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Age/gender audience primitives — the second campaign-scoped saved report, built exactly like
// keyword_report.go's: STATELESS submit/check steps, because Microsoft serves audience
// demographics only through the asynchronous Reporting service (AgeGenderAudienceReportRequest),
// whose reports take minutes, while the audience read runs inside a 20s request budget.
//
//	SubmitAgeGenderReport -> a ReportRequestId the CALLER persists
//	CheckAgeGenderReport  -> exactly ONE Poll (no loop, no sleep); on Success, the rows
//
// NO DEVICE DIMENSION. AgeGenderAudienceReportColumn has AgeGroup and Gender but no device
// column, so this report cannot answer the Google audience read's age/gender/device shape; a
// device breakdown would need a SECOND report (and a second pending half per key) and is out of
// scope. The read publishes (age, gender) buckets only.
//
// Contract, verified against learn.microsoft.com on 2026-10-07 (NOT against a live account —
// the same UNVERIFIED CONTRACT caveat as metrics.go applies, and the same
// MICROSOFT_METRICS_ENABLED gate in the dispatcher):
//
//   - AgeGenderAudienceReportRequest — Aggregation, Columns, Filter (optional), Scope
//     (AccountThroughAdGroupReportScope), Time; Summary is the default aggregation, and without
//     a TimePeriod column Summary is used whatever Aggregation says.
//     https://learn.microsoft.com/en-us/advertising/reporting-service/agegenderaudiencereportrequest?view=bingads-13
//   - AgeGenderAudienceReportColumn — CampaignId, AgeGroup ("13-17, 18-24, 25-34, 35-49, 50-64,
//     and 65+"), Gender ("male or female"), Impressions, Clicks, Spend; required columns AgeGroup,
//     Gender and TimePeriod, TimePeriod being "expected for all aggregation types except
//     Summary" (and not allowed with it). No Ctr, no currency and no device column.
//     https://learn.microsoft.com/en-us/advertising/reporting-service/agegenderaudiencereportcolumn?view=bingads-13
//   - AccountThroughAdGroupReportScope — the same UNION-of-elements scope and 300-campaign
//     ceiling the keyword report documents (keyword_report.go).

// ErrAudienceReportScope marks an age/gender report scope this client will not send: empty, too
// large, or holding an id that is not a Microsoft campaign id — ErrKeywordReportScope's rule.
var ErrAudienceReportScope = errors.New("microsoft-ads: invalid age/gender report scope")

// ErrAudienceReportScopeRejected marks Microsoft refusing the campaign-only age/gender report
// scope itself (error 2027) — PERMANENT, for ErrKeywordReportScopeRejected's reason.
var ErrAudienceReportScopeRejected = errors.New("microsoft-ads: the campaign-only age/gender report scope was rejected")

// MaxAgeGenderBuckets bounds the distinct (age group, gender) pairs one report may carry.
// Microsoft documents six age groups and two genders (plus, in practice, an unknown value of
// each); a report past this bound is not the documented report, and is refused rather than
// published as an unbounded list of verbatim labels.
const MaxAgeGenderBuckets = 64

// maxAudienceLabelBytes bounds one AgeGroup/Gender cell.
const maxAudienceLabelBytes = 64

// ageGenderReportColumns is the column list SubmitAgeGenderReport requests. CampaignId so a saved
// report can be confined to the campaigns the project owns at serve time; AgeGroup and Gender
// (two of the three documented required columns — TimePeriod is not allowed with Summary); and
// the three counters. No ad-group columns: they would only split rows the read sums anyway.
var ageGenderReportColumns = []string{"CampaignId", "AgeGroup", "Gender", "Impressions", "Clicks", "Spend"}

// ValidateAudienceReportCampaignID reports whether id is a canonical Microsoft campaign id, the
// shape SubmitAgeGenderReport sends. Exported so the dispatcher can refuse a malformed stored id
// BEFORE any upstream call.
func ValidateAudienceReportCampaignID(id string) error {
	return validateCampaignReportScope([]string{id}, ErrAudienceReportScope)
}

// SubmitAgeGenderReport submits an age/gender audience report over window, scoped to
// campaignIDs on the client's account, and returns its ReportRequestId with the calendar window
// it covers. It does not wait for the report. The scope, time and completeness settings are
// SubmitKeywordReport's exactly (submitCampaignScopedReport): Campaigns ONLY, never AccountIds.
func (c *Client) SubmitAgeGenderReport(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (reportID string, windowStart, windowEnd time.Time, err error) {
	if err := validateCampaignReportScope(campaignIDs, ErrAudienceReportScope); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return c.submitCampaignScopedReport(ctx, "AgeGenderAudienceReportRequest", "age/gender", ageGenderReportColumns, window, campaignIDs, ErrAudienceReportScopeRejected)
}

// AgeGenderReportRow is one (campaign, age group, gender) segment's totals over the window.
type AgeGenderReportRow struct {
	CampaignID string
	// AgeGroup and Gender are Microsoft's values verbatim (trimmed).
	AgeGroup    string
	Gender      string
	Impressions int64
	Clicks      int64
	// Spend is a decimal in the ACCOUNT's currency, unconverted.
	Spend float64
}

// AgeGenderReportResult is the outcome of one CheckAgeGenderReport.
type AgeGenderReportResult struct {
	Status AccountReportStatus
	// Rows is set only for Success: one row per (campaign, age group, gender), first-appearance
	// order. Empty (non-nil) when none of the scoped campaigns served.
	Rows []AgeGenderReportRow
	// Partial is true when Microsoft flagged the data as potentially incomplete.
	Partial bool
}

// CheckAgeGenderReport polls a report submitted by SubmitAgeGenderReport EXACTLY ONCE and, if it
// is built, downloads and folds it — CheckKeywordReport's semantics, including an empty Success
// for a missing download URL or a header-only CSV.
func (c *Client) CheckAgeGenderReport(ctx context.Context, reportID string) (*AgeGenderReportResult, error) {
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
		return &AgeGenderReportResult{Status: AccountReportStatusPending}, nil
	case AccountReportStatusError:
		return &AgeGenderReportResult{Status: AccountReportStatusError}, nil
	case AccountReportStatusSuccess:
	default:
		return nil, fmt.Errorf("microsoft report %s returned unrecognized status %q", clipID(reportID), status)
	}
	if downloadURL == "" {
		return &AgeGenderReportResult{Status: AccountReportStatusSuccess, Rows: []AgeGenderReportRow{}}, nil
	}
	records, err := c.downloadReportRecords(ctx, downloadURL)
	if err != nil {
		return nil, err
	}
	return foldAgeGenderReportRows(records)
}

// foldAgeGenderReportRows folds a downloaded age/gender report into one row per (campaign, age
// group, gender), with foldKeywordReportRows' discipline: columns by header name, all six
// required; negative or non-finite values and int64 overflow refused; an unattributable row (a
// non-id campaign, a blank or untrustworthy age group or gender) fails the WHOLE read, because
// dropping it would under-report a segment and present the rest as the whole.
//
// A repeated (campaign, age group, gender) is SUMMED, as the keyword fold sums a repeated
// keyword: with Summary aggregation Microsoft groups rows by the requested attribute columns, so a
// repeat can only be a split along a column not requested, and its counters belong to the same
// segment.
//
// Label cells are the identity of a published bucket, so they get identityjson's raw-bytes
// discipline in CSV form: malformed UTF-8 (which the JSON encoder would silently replace with
// U+FFFD, merging distinct labels), control and format (Cf) characters and over-long cells are
// refused rather than published.
func foldAgeGenderReportRows(records [][]string) (*AgeGenderReportResult, error) {
	header, rows, preamble, err := reportHeaderAndRows(records)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		key := strings.ToLower(strings.TrimSpace(name))
		if _, dup := idx[key]; dup {
			// Two columns of one name would make "the" AgeGroup or Spend a guess.
			return nil, fmt.Errorf("microsoft age/gender report csv repeats column %s", strings.TrimSpace(name))
		}
		idx[key] = i
	}
	cols := make(map[string]int, len(ageGenderReportColumns))
	for _, name := range ageGenderReportColumns {
		i, ok := idx[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("microsoft age/gender report csv missing required column %s (have %v)", name, header)
		}
		cols[name] = i
	}

	order := make([]string, 0)
	byKey := map[string]*AgeGenderReportRow{}
	pairs := map[string]bool{}
	for n, row := range rows {
		if cols["CampaignId"] >= len(row) {
			return nil, fmt.Errorf("age/gender report row %d: row has %d columns, wanted column %d", n, len(row), cols["CampaignId"])
		}
		raw := strings.TrimSpace(row[cols["CampaignId"]])
		num := json.Number(raw)
		campID := numberID(&num)
		if campID == "" {
			return nil, fmt.Errorf("age/gender report row %d: CampaignId %q is not an id; the row cannot be attributed", n, clipID(raw))
		}
		age, err := audienceLabel(row, cols["AgeGroup"], "AgeGroup", n)
		if err != nil {
			return nil, err
		}
		gender, err := audienceLabel(row, cols["Gender"], "Gender", n)
		if err != nil {
			return nil, err
		}
		imp, err := parseReportInt(row, cols["Impressions"])
		if err != nil {
			return nil, fmt.Errorf("age/gender report row %d impressions: %w", n, err)
		}
		clk, err := parseReportInt(row, cols["Clicks"])
		if err != nil {
			return nil, fmt.Errorf("age/gender report row %d clicks: %w", n, err)
		}
		spend, err := parseReportFloat(row, cols["Spend"])
		if err != nil {
			return nil, fmt.Errorf("age/gender report row %d spend: %w", n, err)
		}
		if imp < 0 || clk < 0 || math.IsNaN(spend) || math.IsInf(spend, 0) || spend < 0 {
			return nil, fmt.Errorf("age/gender report row %d: negative or non-finite counter (impressions %d, clicks %d, spend %v)", n, imp, clk, spend)
		}

		// \x00 cannot occur in a label (control characters are refused), so the key is exact.
		key := campID + "\x00" + age + "\x00" + gender
		acc, seen := byKey[key]
		if !seen {
			pairs[age+"\x00"+gender] = true
			if len(pairs) > MaxAgeGenderBuckets {
				return nil, fmt.Errorf("microsoft age/gender report carries more than %d distinct (age group, gender) pairs", MaxAgeGenderBuckets)
			}
			acc = &AgeGenderReportRow{CampaignID: campID, AgeGroup: age, Gender: gender}
			byKey[key] = acc
			order = append(order, key)
		}
		if imp > 0 && acc.Impressions > math.MaxInt64-imp {
			return nil, fmt.Errorf("age/gender report row %d impressions: total would overflow", n)
		}
		acc.Impressions += imp
		if clk > 0 && acc.Clicks > math.MaxInt64-clk {
			return nil, fmt.Errorf("age/gender report row %d clicks: total would overflow", n)
		}
		acc.Clicks += clk
		total := acc.Spend + spend
		if math.IsInf(total, 0) {
			return nil, fmt.Errorf("age/gender report row %d spend: total would overflow", n)
		}
		acc.Spend = total
	}

	out := &AgeGenderReportResult{
		Status:  AccountReportStatusSuccess,
		Rows:    make([]AgeGenderReportRow, 0, len(order)),
		Partial: reportDataIsIncomplete(preamble),
	}
	for _, k := range order {
		out.Rows = append(out.Rows, *byKey[k])
	}
	return out, nil
}

// isUnsafeLabelRune reports a control character (Cc) or a FORMAT character (Cf — e.g. the
// bidirectional override U+202E or a zero-width joiner): neither is part of a demographic label,
// and a format character can make two distinct published labels render identically or reorder
// the text around them.
func isUnsafeLabelRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// audienceLabel reads one AgeGroup/Gender cell: required, valid UTF-8, no control or
// format characters, at most maxAudienceLabelBytes. The offending bytes never reach the error text.
func audienceLabel(row []string, col int, what string, n int) (string, error) {
	if col >= len(row) {
		return "", fmt.Errorf("age/gender report row %d: row has %d columns, wanted column %d", n, len(row), col)
	}
	raw := row[col]
	// The UNSAFE-rune and UTF-8 checks run on the cell AS RECEIVED, before any trimming:
	// strings.TrimSpace also strips \t, \n, \r, \v, \f and U+0085, so checking only the trimmed
	// value would accept "\tMale" as "Male". A label carrying a control or format character
	// anywhere — leading and trailing included — is malformed. Only then are ordinary surrounding
	// spaces trimmed, as the keyword fold trims its cells.
	switch {
	case !utf8.ValidString(raw):
		return "", fmt.Errorf("age/gender report row %d: %s is not valid UTF-8", n, what)
	case strings.IndexFunc(raw, isUnsafeLabelRune) >= 0:
		return "", fmt.Errorf("age/gender report row %d: %s carries a control or format character", n, what)
	}
	v := strings.TrimSpace(raw)
	switch {
	case v == "":
		return "", fmt.Errorf("age/gender report row %d: blank %s; the row cannot be attributed", n, what)
	case len(v) > maxAudienceLabelBytes:
		return "", fmt.Errorf("age/gender report row %d: %s is longer than %d bytes", n, what, maxAudienceLabelBytes)
	}
	return v, nil
}
