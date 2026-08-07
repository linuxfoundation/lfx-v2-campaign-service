// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// MetricsWindow is one of the campaign-service API's platform-agnostic window
// literals (design/brief.go's get-campaign-metrics Enum). This client maps each
// one to a Meta Graph API Insights "date_preset" value — Meta has no query
// parameters to inject into (unlike GAQL), but the mapping is still a fixed
// allow-list so an unsupported literal fails here rather than reaching Meta as
// an unrecognized date_preset.
type MetricsWindow string

// Supported metrics windows, mirroring googleads.MetricsWindow's literal set
// exactly so a caller can pass the same "window" value regardless of platform.
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

// datePresetFor maps the shared window literal to Meta's date_preset value.
// See https://developers.facebook.com/docs/marketing-api/insights/parameters#date_presets.
var datePresetFor = map[MetricsWindow]string{
	WindowToday:      "today",
	WindowYesterday:  "yesterday",
	WindowLast7Days:  "last_7d",
	WindowLast14Days: "last_14d",
	WindowLast30Days: "last_30d",
	WindowThisMonth:  "this_month",
	WindowLastMonth:  "last_month",
}

// CampaignMetrics is the aggregated performance snapshot for one campaign over
// one MetricsWindow. It is a live read-through of Meta — this client never
// persists it.
type CampaignMetrics struct {
	CampaignID  string        `json:"campaignId"`
	Window      MetricsWindow `json:"window"`
	Impressions int64         `json:"impressions"`
	Clicks      int64         `json:"clicks"`
	// CostMicros is the campaign's spend in the window, expressed in micros (1
	// whole currency unit = 1,000,000 micros) of the ad account's currency — the
	// same unit googleads.CampaignMetrics uses, so the platform-agnostic
	// model.CampaignMetrics.CostMicros field means the same thing for every
	// platform rather than silently switching scale per adapter.
	CostMicros int64 `json:"costMicros"`
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64 `json:"ctr"`
}

// insightsResponse is the shape of a Graph API .../insights read. Meta returns
// numeric fields as JSON strings (impressions/clicks/spend), same convention as
// the Google Ads REST client's metrics rows.
type insightsResponse struct {
	Data []struct {
		Impressions string `json:"impressions"`
		Clicks      string `json:"clicks"`
		Spend       string `json:"spend"`
	} `json:"data"`
}

// parseMetricInt parses a metric string value, treating empty string as zero.
// Meta omits zero-valued numeric fields from some responses, same as Google
// Ads REST; empty is treated as 0 rather than a parse error.
func parseMetricInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// GetCampaignMetrics reads impressions/clicks/spend for one campaign over window
// (defaulting to WindowLast30Days when empty) via a single Insights GET.
//
// A campaign with no delivery in the window is not an error: Meta returns an
// empty data array, and this method returns a zero-value CampaignMetrics rather
// than surfacing "not found" — mirrors googleads.GetCampaignMetrics.
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window MetricsWindow) (*CampaignMetrics, error) {
	id := strings.TrimSpace(campaignID)
	if id == "" || !numericIDRE.MatchString(id) {
		return nil, fmt.Errorf("get campaign metrics: campaign id %q must be numeric", campaignID)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	preset, ok := datePresetFor[w]
	if !ok {
		return nil, fmt.Errorf("get campaign metrics: unsupported window %q", window)
	}

	path := "/" + id + "/insights?fields=impressions,clicks,spend&date_preset=" + preset
	var resp insightsResponse
	if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}
	if len(resp.Data) == 0 {
		return &CampaignMetrics{CampaignID: id, Window: w}, nil
	}

	row := resp.Data[0]
	impressions, errImpressions := parseMetricInt(row.Impressions)
	clicks, errClicks := parseMetricInt(row.Clicks)
	if errImpressions != nil || errClicks != nil {
		return nil, &transportError{
			Method: http.MethodGet,
			Path:   path,
			Err:    fmt.Errorf("decode campaign metrics row: impressions %q (%v), clicks %q (%v)", row.Impressions, errImpressions, row.Clicks, errClicks),
		}
	}
	// Spend is decimal (e.g. "12.34"), unlike Google Ads' integer cost_micros —
	// Meta reports it in whole currency units, not minor units. Parsed as a float
	// and scaled to micros (×1e6) rather than reusing parseMetricInt.
	var spend float64
	if row.Spend != "" {
		var err error
		spend, err = strconv.ParseFloat(row.Spend, 64)
		if err != nil {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("decode campaign metrics row: spend %q: %w", row.Spend, err),
			}
		}
		if math.IsNaN(spend) || math.IsInf(spend, 0) {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("decode campaign metrics row: spend %q is not a finite number", row.Spend),
			}
		}
		// Scale and check for overflow: a finite value like 1e307 can become +Inf
		// when multiplied by 1_000_000, and out-of-range values must be rejected
		// before int64 conversion to prevent underflow/corruption.
		//
		// The comparison is '>=', not '>': math.MaxInt64 is not exactly representable
		// as a float64, so float64(math.MaxInt64) is 2^63 — one MORE than MaxInt64.
		// A product of exactly 2^63 therefore passes a '>' guard and then wraps to
		// MinInt64 on int64 conversion, corrupting the cost. (Float spacing at this
		// magnitude is 2048, so 2^63 is reachable while 2^63-1 is not; rounding does
		// not create this case and cannot avoid it — only '>=' rejects it.) Round
		// first so sub-boundary spends are rounded rather than truncated, mirroring
		// the budget-scaling guard in client.go's applyBudget.
		scaled := math.Round(spend * 1_000_000)
		if math.IsInf(scaled, 0) || scaled >= float64(math.MaxInt64) || scaled <= float64(math.MinInt64) {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("decode campaign metrics row: spend %q overflows int64 when scaled to micros", row.Spend),
			}
		}
		spend = scaled
	} else {
		spend = 0
	}

	m := &CampaignMetrics{
		CampaignID:  id,
		Window:      w,
		Impressions: impressions,
		Clicks:      clicks,
		CostMicros:  int64(math.Round(spend)),
	}
	if impressions > 0 {
		m.Ctr = float64(clicks) / float64(impressions)
	}
	return m, nil
}
