// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// campaignIDRe validates X campaign IDs (alphanumeric, matching X's real id format)
// to prevent path injection. Mirrors the accountIDRe pattern used elsewhere in the
// Twitter client.
var campaignIDRe = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// MetricsWindow is an X Ads API predefined date-range literal. The X Ads API
// stats endpoint caps queryable date ranges at 7 days per request; this allow-list
// enforces that limitation by excluding windows longer than 7 days. The caller
// will receive an explicit error if they request an unsupported longer range
// (not a silently-truncated result).
type MetricsWindow string

// Supported metrics windows. X Ads limits the date range to 7 days per request,
// so only LAST_7_DAYS (7 days) and TODAY (1 day) are supported. Any other window
// — LAST_30_DAYS, THIS_MONTH, LAST_MONTH — will return an error explaining the
// 7-day API limitation. Do NOT extend this allow-list without also lifting X's
// API ceiling; extrapolation or averaging is never acceptable.
const (
	WindowToday     MetricsWindow = "TODAY"
	WindowLast7Days MetricsWindow = "LAST_7_DAYS"

	// defaultMetricsWindow is used when the caller passes an empty MetricsWindow.
	defaultMetricsWindow = WindowLast7Days
)

var validMetricsWindows = map[MetricsWindow]struct{}{
	WindowToday:     {},
	WindowLast7Days: {},
}

// CampaignMetrics is the aggregated performance snapshot for one campaign over
// one MetricsWindow. It is a live read-through of X Ads — this client never
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

// xAdsStat is a single stat row from the X Ads API stats endpoint.
type xAdsStat struct {
	CampaignID  string `json:"campaign_id"`
	Impressions string `json:"impressions"`
	Clicks      string `json:"clicks"`
	Spend       string `json:"spend"`
}

// parseMetricInt parses a metric string value, treating empty string as zero.
// X Ads omits zero-valued metrics from JSON, leaving empty strings on unmarshaling;
// this func treats empty as 0 rather than a parse error.
func (c *Client) parseMetricInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseMetricFloat parses a metric string value to float64, treating empty string as zero.
// Used for spend values which are typically returned as decimal strings.
func (c *Client) parseMetricFloat(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// dateRangeForWindow computes the start and end dates for a metrics window,
// returning them as YYYY-MM-DD strings suitable for X Ads API parameters.
// X Ads uses the convention of date ranges that are inclusive on both ends.
func dateRangeForWindow(window MetricsWindow, now time.Time) (startDate, endDate string) {
	// Normalize to UTC for consistent date computation
	now = now.UTC()
	endDate = now.Format("2006-01-02")

	switch window {
	case WindowToday:
		startDate = endDate
	case WindowLast7Days:
		// 7 days = today + 6 days before = 7 days total
		startDate = now.AddDate(0, 0, -6).Format("2006-01-02")
	default:
		// Shouldn't reach here due to validation, but default to today
		startDate = endDate
	}
	return startDate, endDate
}

// GetCampaignMetrics reads impressions/clicks/spend for one campaign over window
// (defaulting to WindowLast7Days when empty) via the X Ads stats endpoint.
//
// The X Ads stats endpoint caps queryable date ranges at 7 days per request. If
// the caller requests a window longer than 7 days, this method returns a clear,
// typed error explaining the limitation — NOT a silently-truncated result,
// averaged data, or extrapolation. This is a permanent API constraint, not a TODO.
//
// A campaign with no impressions in the window is not an error: the stats endpoint
// omits it from results entirely, and this method returns a zero-value
// CampaignMetrics rather than surfacing "not found".
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window MetricsWindow) (*CampaignMetrics, error) {
	id := strings.TrimSpace(campaignID)
	if id == "" || !campaignIDRe.MatchString(id) {
		return nil, fmt.Errorf("get campaign metrics: campaign id %q must be alphanumeric", campaignID)
	}

	w := window
	if w == "" {
		w = defaultMetricsWindow
	}

	// Validate window against the allow-list. Windows longer than 7 days return an
	// error that explicitly mentions the API limitation, not a generic "unsupported".
	switch w {
	case WindowToday, WindowLast7Days:
		// Valid; proceed
	default:
		// Return a clear error explaining WHY the window is unsupported:
		// "X Ads API limit, not a TODO". Include the window in the message so the caller
		// sees what they requested.
		return nil, fmt.Errorf("get campaign metrics: unsupported window %q — X Ads API stats endpoint caps queryable date ranges at 7 days per request", w)
	}

	// Compute the date range for the window
	startDate, endDate := dateRangeForWindow(w, c.timeFn())

	// Query the X Ads stats endpoint. Parameters are passed as query strings.
	// doRequest handles the URL building and parameter encoding uniformly.
	params := map[string]string{
		"start_time":  startDate + "T00:00:00Z",
		"end_time":    endDate + "T23:59:59Z",
		"granularity": "ALL",
		"entity":      "CAMPAIGN",
		"entity_ids":  id,
	}

	// Use doRequest() for a GET with query parameters; unlike campaign creates
	// (which use createRequest/POST), stats reads use GET with query params.
	resp, err := c.doRequest(ctx, http.MethodGet, "stats", params)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	// Unmarshal the response. resp.Data contains the "data" field from the JSON
	// response, which is an array of stat objects.
	var stats []xAdsStat
	if err := json.Unmarshal(resp.Data, &stats); err != nil {
		return nil, &transportError{
			Method: http.MethodGet,
			Path:   "stats",
			Err:    fmt.Errorf("decode campaign metrics response: %w", err),
		}
	}

	// If no data is returned, return a zero-value metrics struct (campaign had no activity)
	if len(stats) == 0 {
		return &CampaignMetrics{CampaignID: id, Window: w}, nil
	}

	// Extract the first (and should be only) result
	stat := stats[0]

	// Parse metric values, treating empty strings as zero
	impressions, errImpressions := c.parseMetricInt(stat.Impressions)
	clicks, errClicks := c.parseMetricInt(stat.Clicks)

	// Spend is returned as a decimal string (USD), convert to micro-currency (x1e6)
	spendUSD, errSpend := c.parseMetricFloat(stat.Spend)
	costMicros := int64(spendUSD * 1_000_000) // Convert USD to micros

	if errImpressions != nil || errClicks != nil || errSpend != nil {
		return nil, &transportError{
			Method: http.MethodGet,
			Path:   "stats",
			Err: fmt.Errorf("decode campaign metrics row: impressions %q (%v), clicks %q (%v), spend %q (%v)",
				stat.Impressions, errImpressions, stat.Clicks, errClicks, stat.Spend, errSpend),
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
