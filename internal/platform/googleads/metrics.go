// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// metricsDateLayout is GAQL's date literal format for segments.date.
const metricsDateLayout = "2006-01-02"

// numericGAIDRE matches a Google Ads numeric resource id (campaign id, ad group id):
// digits only. It is the same pattern as customerIDRE but is named for the general
// case, because it guards a DIFFERENT risk here.
//
// customerIDRE protects a URL path segment. This guards a value interpolated into
// GAQL QUERY TEXT — and GAQL has no bind parameters, so a campaign id cannot be
// passed out-of-band and must be proven safe before it is concatenated. Restricting
// it to digits makes query injection structurally impossible rather than relying on
// escaping. Aliasing (rather than reusing the customer-id name) keeps that intent
// legible at the call site: a future edit to either rule cannot silently weaken the
// other.
var numericGAIDRE = customerIDRE

// CampaignMetricRow is ONE day's reported performance for one campaign, exactly as
// Google returned it. Money and conversion values are decimal STRINGS, not float64:
// they are summed downstream across days and campaigns, and float64 cannot represent
// most decimal fractions, so the error compounds with every addition (see
// model.addDecimal for the measured failure). Keeping them as strings means the value
// the caller stores is bit-for-bit the value Google reported.
type CampaignMetricRow struct {
	// Date is the reporting day in the AD ACCOUNT's timezone — Google's own day
	// boundary, and the only one it will report against.
	Date        time.Time
	Impressions int64
	Clicks      int64
	// Spend is in whole units of the account currency, converted EXACTLY from the
	// cost_micros integer Google returns (see microsToDecimal).
	Spend string
	// Conversions may be FRACTIONAL: under data-driven attribution Google splits
	// credit across touchpoints, so this is a decimal, not a count.
	Conversions string
	// Currency is the account's ISO 4217 code, selected in the same query so the
	// spend figure is never stored without the unit it is denominated in.
	Currency string
	// Raw is Google's own result row, verbatim, for auditability.
	Raw json.RawMessage
}

// gaqlMetricsRow mirrors the SELECT clause in FetchCampaignMetrics. Google returns
// int64-valued metrics as JSON STRINGS (protobuf int64 encoding) and doubles as JSON
// numbers, so the numeric fields are decoded as json.Number / string and converted
// explicitly rather than being trusted to unmarshal into a Go numeric type.
type gaqlMetricsRow struct {
	Segments struct {
		Date string `json:"date"`
	} `json:"segments"`
	Metrics struct {
		// Impressions/Clicks/CostMicros arrive as quoted strings (int64 over JSON).
		Impressions string `json:"impressions"`
		Clicks      string `json:"clicks"`
		CostMicros  string `json:"costMicros"`
		// Conversions is a double and arrives unquoted. json.Number preserves the
		// literal text so a fractional value never round-trips through float64.
		Conversions json.Number `json:"conversions"`
	} `json:"metrics"`
	Customer struct {
		CurrencyCode string `json:"currencyCode"`
	} `json:"customer"`
}

// FetchCampaignMetrics returns one campaign's daily performance rows for the
// inclusive date range [from, to].
//
// It runs a single GAQL query over the client's existing search transport, which
// already handles auth, cursor pagination to exhaustion, 429 retry (the read is
// idempotent), page-token loop detection, and response-size caps.
//
// Selecting segments.date makes each returned row exactly ONE day, which is the
// storage grain — so there is no client-side bucketing and therefore no timezone
// re-bucketing bug. The day boundary is the ad account's own.
//
// NOTE: campaign.start_date / campaign.end_date must NOT appear in a v23 query (they
// were replaced by *_date_time and are rejected as unrecognized). The reporting window
// here is segments.date, which is a separate concern and remains valid.
func (c *Client) FetchCampaignMetrics(ctx context.Context, campaignID string, from, to time.Time) ([]CampaignMetricRow, error) {
	id := strings.TrimSpace(campaignID)
	// The campaign id is interpolated into the query text (GAQL has no bind
	// parameters), so it MUST be validated as digits-only rather than escaped. An
	// unvalidated value here would let a crafted id alter the query's WHERE clause
	// and read rows for a campaign outside the caller's scope.
	if !numericGAIDRE.MatchString(id) {
		return nil, fmt.Errorf("google-ads: campaign id %q must be digits only", campaignID)
	}
	if from.After(to) {
		return nil, fmt.Errorf("google-ads: metrics range start %s is after end %s",
			from.Format(metricsDateLayout), to.Format(metricsDateLayout))
	}

	// Dates are formatted, not interpolated from caller text, so they cannot carry
	// query syntax.
	query := fmt.Sprintf(
		"SELECT segments.date, metrics.impressions, metrics.clicks, "+
			"metrics.cost_micros, metrics.conversions, customer.currency_code "+
			"FROM campaign "+
			"WHERE campaign.id = %s AND segments.date BETWEEN '%s' AND '%s'",
		id, from.Format(metricsDateLayout), to.Format(metricsDateLayout))

	rawRows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("google-ads: fetch campaign %s metrics: %w", id, err)
	}

	out := make([]CampaignMetricRow, 0, len(rawRows))
	for _, raw := range rawRows {
		var r gaqlMetricsRow
		if uerr := json.Unmarshal(raw, &r); uerr != nil {
			// Do NOT skip a row we cannot decode. Silently dropping it would under-report
			// spend — the campaign looks cheaper than it was, which is exactly the class
			// of plausible-looking wrong number this feature exists to avoid.
			return nil, fmt.Errorf("google-ads: decode metrics row for campaign %s: %w", id, uerr)
		}
		day, perr := time.Parse(metricsDateLayout, r.Segments.Date)
		if perr != nil {
			return nil, fmt.Errorf("google-ads: campaign %s returned an unparseable segments.date %q: %w", id, r.Segments.Date, perr)
		}
		out = append(out, CampaignMetricRow{
			Date:        day,
			Impressions: parseInt64(r.Metrics.Impressions),
			Clicks:      parseInt64(r.Metrics.Clicks),
			Spend:       microsToDecimal(r.Metrics.CostMicros),
			Conversions: normalizeDecimal(r.Metrics.Conversions.String()),
			Currency:    strings.TrimSpace(r.Customer.CurrencyCode),
			Raw:         raw,
		})
	}
	return out, nil
}

// parseInt64 converts Google's quoted int64 to a Go int64, yielding 0 for an absent
// or unparseable value (an omitted metric means zero for that day).
func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// microsToDecimal converts Google's cost_micros (1/1,000,000 of the account currency)
// to a whole-currency decimal string, EXACTLY.
//
// The division is done with big.Rat, not float64. 1234567 micros must become exactly
// "1.234567"; a float64 division introduces a representation error that is invisible
// on one row and compounds once rows are summed. Because the scale is fixed at 6
// decimal places and micros are integers, this conversion is always exact.
func microsToDecimal(micros string) string {
	micros = strings.TrimSpace(micros)
	if micros == "" {
		return "0.000000"
	}
	n, ok := new(big.Int).SetString(micros, 10)
	if !ok {
		return "0.000000"
	}
	return new(big.Rat).SetFrac(n, big.NewInt(1_000_000)).FloatString(6)
}

// normalizeDecimal renders a decimal literal at the fixed 6-place scale used
// throughout the metrics path, so every value has the same shape as the
// NUMERIC(18,6) column that stores it. Parsing goes through big.Rat so a value like
// "1e-3" (valid JSON number notation) is expanded exactly rather than mis-read.
func normalizeDecimal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0.000000"
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "0.000000"
	}
	return r.FloatString(6)
}
