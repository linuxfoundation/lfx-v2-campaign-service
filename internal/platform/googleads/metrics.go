// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetricsWindow is a Google Ads GAQL predefined date-range literal accepted after
// a GAQL query's DURING keyword. Only a fixed allow-list is supported — the value
// is concatenated directly into the GAQL query string (GAQL has no query
// parameters), so an unvalidated caller-supplied window would be a query-injection
// vector the same way an unvalidated campaign id would be.
type MetricsWindow string

// Supported metrics windows. This is a deliberately small subset of GAQL's full
// predefined-date-range literal set (TODAY, YESTERDAY, LAST_7_DAYS, LAST_14_DAYS,
// LAST_30_DAYS, THIS_MONTH, LAST_MONTH, plus several week-aligned variants this
// broker has no current caller for) — extend the allow-list, don't bypass it.
const (
	WindowToday      MetricsWindow = "TODAY"
	WindowYesterday  MetricsWindow = "YESTERDAY"
	WindowLast7Days  MetricsWindow = "LAST_7_DAYS"
	WindowLast14Days MetricsWindow = "LAST_14_DAYS"
	WindowLast30Days MetricsWindow = "LAST_30_DAYS"
	WindowThisMonth  MetricsWindow = "THIS_MONTH"
	WindowLastMonth  MetricsWindow = "LAST_MONTH"

	// defaultMetricsWindow is used when the caller passes an empty MetricsWindow.
	defaultMetricsWindow = WindowLast30Days
)

var validMetricsWindows = map[MetricsWindow]struct{}{
	WindowToday:      {},
	WindowYesterday:  {},
	WindowLast7Days:  {},
	WindowLast14Days: {},
	WindowLast30Days: {},
	WindowThisMonth:  {},
	WindowLastMonth:  {},
}

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map
// to a GAQL literal. Google Ads happens to cover the whole platform-agnostic vocabulary
// today, so this is only reachable for a value outside that closed set — but the mapping
// is still fallible by construction, since a window added to model must be mapped here
// deliberately rather than silently falling through to the default range.
var ErrUnsupportedWindow = errors.New("google ads does not support this metrics window")

// WindowFor translates the platform-agnostic model.MetricsWindow into this client's GAQL
// literal. The mapping lives in the platform package, not the dispatcher, so Google's
// dialect never leaks past this boundary — the same split every other platform client uses.
// The result still goes through validMetricsWindows in GetCampaignMetrics: this function is
// a translation, not a substitute for the injection guard.
func WindowFor(w model.MetricsWindow) (MetricsWindow, error) {
	switch w {
	case model.MetricsWindowToday:
		return WindowToday, nil
	case model.MetricsWindowYesterday:
		return WindowYesterday, nil
	case model.MetricsWindowLast7Days:
		return WindowLast7Days, nil
	case model.MetricsWindowLast14Days:
		return WindowLast14Days, nil
	case model.MetricsWindowLast30Days:
		return WindowLast30Days, nil
	case model.MetricsWindowThisMonth:
		return WindowThisMonth, nil
	case model.MetricsWindowLastMonth:
		return WindowLastMonth, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedWindow, string(w))
	}
}

// CampaignMetrics is the aggregated performance snapshot for one campaign over
// one MetricsWindow. It is a live read-through of Google Ads — this client never
// persists it.
type CampaignMetrics struct {
	CampaignID  string        `json:"campaignId"`
	Window      MetricsWindow `json:"window"`
	Impressions int64         `json:"impressions"`
	Clicks      int64         `json:"clicks"`
	CostMicros  int64         `json:"costMicros"`
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64 `json:"ctr"`
}

// gaqlMetricsRow is the shape of one googleAds:search result row for the
// campaign-metrics query below. Google Ads REST returns int64-valued fields
// (resource ids and metrics) as JSON strings to avoid float64 precision loss, so
// every field here is a string despite representing a number.
type gaqlMetricsRow struct {
	Campaign struct {
		ID string `json:"id"`
	} `json:"campaign"`
	Metrics struct {
		Impressions string `json:"impressions"`
		Clicks      string `json:"clicks"`
		CostMicros  string `json:"costMicros"`
	} `json:"metrics"`
}

// parseMetricInt parses a metric string value, treating empty string as zero.
// Google Ads omits zero-valued metrics from REST JSON, leaving empty strings
// on unmarshaling; this func treats empty as 0 rather than a parse error.
func parseMetricInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// GetCampaignMetrics reads impressions/clicks/cost for one campaign over window
// (defaulting to WindowLast30Days when empty) via a single GAQL search.
//
// UNVERIFIED ASSUMPTION (flag before relying on this in production, same
// convention as the GA-3 AdGroupAd composite-resourceName and GA-4
// observation-targeting flags): a segments.date WHERE-clause filter without
// segments.date in the SELECT list is documented by Google to filter rows
// without segmenting them, so this query returns at most one row — the metrics
// aggregated across the whole window, not one row per day. Verify against a live
// account before trusting the aggregate.
//
// A campaign with no impressions in the window is not an error: Google Ads omits
// it from results entirely, and this method returns a zero-value CampaignMetrics
// rather than surfacing "not found".
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window MetricsWindow) (*CampaignMetrics, error) {
	// customerIDRE (defined in client.go) enforces digits-only IDs to prevent GAQL
	// injection, since the ID is concatenated directly into the query string below.
	id := strings.TrimSpace(campaignID)
	if !customerIDRE.MatchString(id) {
		return nil, fmt.Errorf("get campaign metrics: campaign id %q must be digits only", campaignID)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	if _, ok := validMetricsWindows[w]; !ok {
		return nil, fmt.Errorf("get campaign metrics: unsupported window %q", window)
	}

	query := fmt.Sprintf(
		"SELECT campaign.id, metrics.impressions, metrics.clicks, metrics.cost_micros "+
			"FROM campaign WHERE campaign.id = %s AND segments.date DURING %s",
		id, w,
	)
	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}
	if len(rows) == 0 {
		return &CampaignMetrics{CampaignID: id, Window: w}, nil
	}
	// Enforce the single-row assumption documented above rather than trusting it.
	// Reading rows[0] alone is only correct while the query segments by nothing; a
	// future SELECT that reintroduces a segmenting field would silently UNDERREPORT
	// every metric, and an underreport is indistinguishable from a genuinely quiet
	// campaign. Failing loudly is the only outcome a caller can act on: silently
	// summing here would instead paper over a query change that also invalidates
	// CampaignMetrics.Window, since each row would then cover a sub-window.
	if len(rows) > 1 {
		return nil, fmt.Errorf(
			"get campaign metrics: expected at most 1 aggregated row for campaign %s, got %d "+
				"(the GAQL query is segmenting; metrics would be underreported)", id, len(rows))
	}

	var row gaqlMetricsRow
	if err := json.Unmarshal(rows[0], &row); err != nil {
		return nil, &transportError{
			Method: http.MethodPost,
			Path:   c.customerPath("googleAds:search"),
			Err:    fmt.Errorf("decode campaign metrics row: %w", err),
		}
	}

	// Google Ads marks metric fields optional and omits zero-valued metrics from REST JSON,
	// so unmarshaling leaves empty strings. Treat empty as 0, parse non-empty strictly.
	impressions, errImpressions := parseMetricInt(row.Metrics.Impressions)
	clicks, errClicks := parseMetricInt(row.Metrics.Clicks)
	costMicros, errCost := parseMetricInt(row.Metrics.CostMicros)
	if errImpressions != nil || errClicks != nil || errCost != nil {
		// Name the fields that failed, never their values. These strings come straight from
		// the upstream response body, and the service's default failure branch renders this
		// error into a warning log — echoing the raw value would let a malformed metric
		// inject arbitrary attacker-influenced text (including newlines) into the log stream.
		// Which fields failed is what a responder actually needs; the value itself is
		// recoverable from the upstream request if it is ever wanted.
		var bad []string
		if errImpressions != nil {
			bad = append(bad, "impressions")
		}
		if errClicks != nil {
			bad = append(bad, "clicks")
		}
		if errCost != nil {
			bad = append(bad, "costMicros")
		}
		return nil, &transportError{
			Method: http.MethodPost,
			Path:   c.customerPath("googleAds:search"),
			Err:    fmt.Errorf("decode campaign metrics row: non-numeric metric field(s): %s", strings.Join(bad, ", ")),
		}
	}

	m := &CampaignMetrics{
		CampaignID:  id,
		Window:      w,
		Impressions: impressions,
		Clicks:      clicks,
		CostMicros:  costMicros,
	}
	if impressions > 0 {
		m.Ctr = float64(clicks) / float64(impressions)
	}
	return m, nil
}
