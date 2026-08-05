// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
// — LAST_30_DAYS, THIS_MONTH, LAST_MONTH — will return ErrUnsupportedWindow
// explaining the 7-day API limitation. Do NOT extend this allow-list without
// also lifting X's API ceiling; extrapolation or averaging is never acceptable.
const (
	WindowToday     MetricsWindow = "TODAY"
	WindowLast7Days MetricsWindow = "LAST_7_DAYS"

	// defaultMetricsWindow is used when the caller passes an empty MetricsWindow.
	defaultMetricsWindow = WindowLast7Days
)

// ErrInvalidCampaignID and ErrUnsupportedWindow are typed sentinels so callers
// can discriminate an input-validation failure (errors.Is) from an upstream/
// transport failure, rather than parsing the error message.
var (
	ErrInvalidCampaignID = errors.New("campaign id must be alphanumeric")
	ErrUnsupportedWindow = errors.New("unsupported metrics window: X Ads API stats endpoint caps queryable date ranges at 7 days per request")
)

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

// statsMetrics is the "metrics" object nested inside each id_data entry of a
// stats/accounts response. Every field is an array indexed by time bucket;
// with granularity=TOTAL the array always has exactly one element. X omits a
// metric entirely (rather than returning a zero) when there was no activity,
// so each field is a pointer slice and a nil/missing metric is treated as 0 —
// this is a real "no data", not a decode failure, unlike a malformed envelope.
type statsMetrics struct {
	Impressions            []int64 `json:"impressions"`
	Clicks                 []int64 `json:"clicks"`
	BilledChargeLocalMicro []int64 `json:"billed_charge_local_micro"`
}

// statsIDData is one segment's worth of metrics for an entity. segment is
// always null/omitted for our request (we never pass a segmentation_type
// param), so exactly one element is expected per entity.
type statsIDData struct {
	Metrics statsMetrics `json:"metrics"`
}

// statsEntity is one entity's (campaign's) stats row.
type statsEntity struct {
	ID     string        `json:"id"`
	IDData []statsIDData `json:"id_data"`
}

// firstOrZero returns metrics[0], or 0 when metrics is empty (X omits a metric
// entirely when there was no activity for it in the window).
func firstOrZero(metrics []int64) int64 {
	if len(metrics) == 0 {
		return 0
	}
	return metrics[0]
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
// (defaulting to WindowLast7Days when empty) via the X Ads synchronous analytics
// (stats) endpoint.
//
// The X Ads stats endpoint caps queryable date ranges at 7 days per request. If
// the caller requests a window longer than 7 days, this method returns
// ErrUnsupportedWindow — NOT a silently-truncated result, averaged data, or
// extrapolation. This is a permanent API constraint, not a TODO.
//
// A campaign with no impressions in the window is not an error: the stats
// endpoint omits it (or its metrics) from results entirely, and this method
// returns a zero-value CampaignMetrics rather than surfacing "not found".
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window MetricsWindow) (*CampaignMetrics, error) {
	id := strings.TrimSpace(campaignID)
	if id == "" || !campaignIDRe.MatchString(id) {
		return nil, fmt.Errorf("get campaign metrics: campaign id %q: %w", campaignID, ErrInvalidCampaignID)
	}

	w := window
	if w == "" {
		w = defaultMetricsWindow
	}

	// Validate window against the allow-list. Windows longer than 7 days return
	// ErrUnsupportedWindow so callers can discriminate this from an upstream
	// failure via errors.Is.
	switch w {
	case WindowToday, WindowLast7Days:
		// Valid; proceed
	default:
		return nil, fmt.Errorf("get campaign metrics: window %q: %w", w, ErrUnsupportedWindow)
	}

	// Compute the date range for the window
	startDate, endDate := dateRangeForWindow(w, c.timeFn())

	// Query the X Ads stats endpoint. Per the X Ads v12 analytics contract:
	//   - metric_groups is required and selects which metric families are
	//     returned: ENGAGEMENT (impressions, clicks) and BILLING
	//     (billed_charge_local_micro, already in micro-currency — no USD/micro
	//     conversion needed or wanted here).
	//   - placement is required; ALL_ON_TWITTER covers all delivery surfaces.
	//   - granularity=TOTAL (not ALL) requests one aggregated bucket for the
	//     whole date range rather than a time series.
	// UNVERIFIED ASSUMPTION: this matches the documented X Ads v12
	// stats/accounts/:account_id contract, but has not been verified against a
	// live X Ads account. Mirrors the same disclosed-assumption convention used
	// in internal/platform/googleads/metrics.go and internal/platform/linkedin/metrics.go.
	params := map[string]string{
		"start_time":    startDate + "T00:00:00Z",
		"end_time":      endDate + "T23:59:59Z",
		"granularity":   "TOTAL",
		"entity":        "CAMPAIGN",
		"entity_ids":    id,
		"metric_groups": "ENGAGEMENT,BILLING",
		"placement":     "ALL_ON_TWITTER",
	}

	// The stats endpoint is NOT nested under /accounts/{id} the way every other
	// endpoint this client calls is (it's /stats/accounts/{id}), so this uses
	// doRequestAbs directly against statsURL() rather than doRequest (which
	// always prefixes accountURL()).
	resp, err := c.doRequestAbs(ctx, http.MethodGet, c.statsURL(), "stats", params)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	var entities []statsEntity
	if err := json.Unmarshal(resp.Data, &entities); err != nil {
		return nil, &transportError{
			Method: http.MethodGet,
			Path:   "stats",
			Err:    fmt.Errorf("decode campaign metrics response: %w", err),
		}
	}

	// If no data is returned, return a zero-value metrics struct (campaign had no activity)
	if len(entities) == 0 || len(entities[0].IDData) == 0 {
		return &CampaignMetrics{CampaignID: id, Window: w}, nil
	}

	metrics := entities[0].IDData[0].Metrics
	impressions := firstOrZero(metrics.Impressions)
	clicks := firstOrZero(metrics.Clicks)
	// billed_charge_local_micro is already in micro-currency units (X's own
	// billing scale) — no USD parse/round conversion, unlike platforms that
	// report spend as a decimal-USD string.
	costMicros := firstOrZero(metrics.BilledChargeLocalMicro)

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
