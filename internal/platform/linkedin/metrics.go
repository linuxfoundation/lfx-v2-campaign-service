// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// AdAnalyticsElement is one analytics record returned by the Ad Analytics API.
// The API aggregates metrics for the requested campaigns over the given date range.
type AdAnalyticsElement struct {
	// Impressions is the number of times the ad was displayed.
	Impressions int64 `json:"impressions"`
	// Clicks is the number of times the ad was clicked.
	Clicks int64 `json:"clicks"`
	// CostInUsd is the spend in decimal USD (e.g. 25.50 for $25.50).
	// The API returns this as a JSON number.
	CostInUsd *float64 `json:"costInUsd"`
}

// AdAnalyticsResponse is the JSON response from LinkedIn's Ad Analytics endpoint.
// It contains a slice of analytics elements aggregated over the requested date range.
type AdAnalyticsResponse struct {
	Elements []AdAnalyticsElement `json:"elements"`
}

// GetCampaignMetrics reads live campaign metrics from LinkedIn's Ad Analytics API
// for the given campaign ID during the specified time window. The campaign ID must
// be the LinkedIn campaign URN (e.g., "urn:li:sponsoredCampaign:123456").
func (c *Client) GetCampaignMetrics(ctx context.Context, accountID, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if campaignID == "" {
		return nil, fmt.Errorf("campaign ID is required")
	}
	if accountID == "" {
		return nil, fmt.Errorf("account ID is required")
	}

	startDate, endDate, err := c.dateRangeForWindow(window)
	if err != nil {
		return nil, err
	}

	// Build the ad analytics request. The LinkedIn API expects ISO 8601 timestamps
	// (e.g., "2025-01-15T00:00:00Z") for the dateRange query parameters.
	params := map[string]string{
		"q":               "analytics",
		"adAccountId":     accountID,
		"dateRange.start": startDate.Format(time.RFC3339),
		"dateRange.end":   endDate.Format(time.RFC3339),
		"campaigns[0]":    campaignID,
		"fields":          "impressions,clicks,costInUsd",
	}

	// Make the raw request manually instead of using doRequest to handle the
	// custom analytics response format (which doesn't match responseElement).
	resp, err := c.makeAdAnalyticsRequest(ctx, accountID, campaignID, params)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	// Parse the analytics response.
	// If no elements, we return zero metrics (no activity).
	if resp == nil || len(resp.Elements) == 0 {
		return &model.CampaignMetrics{}, nil
	}

	// Aggregate metrics from all elements. In practice there should be one element
	// for a single campaign.
	var metrics model.CampaignMetrics
	for _, elem := range resp.Elements {
		metrics.Impressions += elem.Impressions
		metrics.Clicks += elem.Clicks
		if elem.CostInUsd != nil {
			// Convert USD to micros (multiply by 1,000,000).
			costMicros := int64(math.Round(*elem.CostInUsd * 1_000_000))
			metrics.CostMicros += costMicros
		}
	}

	// Calculate CTR: clicks / impressions. If impressions is 0, CTR stays 0.
	if metrics.Impressions > 0 {
		metrics.CTR = float64(metrics.Clicks) / float64(metrics.Impressions)
	}

	return &metrics, nil
}

// makeAdAnalyticsRequest makes a raw HTTP request to LinkedIn's Ad Analytics endpoint
// and parses the response into AdAnalyticsResponse. This is separate from doRequest
// because ad analytics responses have a different JSON structure than other LinkedIn
// API responses (they use analytics-specific fields like impressions/clicks).
func (c *Client) makeAdAnalyticsRequest(ctx context.Context, accountID, campaignID string, params map[string]string) (*AdAnalyticsResponse, error) {
	// Build the URL manually since doRequest would unmarshal into the wrong type.
	u, err := url.Parse(c.baseURL + "/" + "adAnalytics")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	// Bound attempt with a per-attempt context deadline.
	attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Check if this is a pre-send error.
		if isPreSendDialError(err) {
			return nil, fmt.Errorf("linkedin GET adAnalytics: %w", err)
		}
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the response body.
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("read response body: %w", err)}
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: fmt.Sprintf("read response body: %v", err)}
	}

	if int64(buf.Len()) > maxResponseBytes {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := buf.String()
		if len(text) > 400 {
			text = text[:400]
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: text}
	}

	// Decode the analytics response.
	var analytics AdAnalyticsResponse
	if buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), &analytics); err != nil {
			return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode response: %w", err)}
		}
	}

	return &analytics, nil
}

// dateRangeForWindow computes the start and end times for the given metrics window,
// relative to the current time (c.now()). Each window is computed as:
//   - today: 00:00:00 to 23:59:59 of today (local time)
//   - last_7_days: 00:00:00 7 days ago to 23:59:59 today
//   - last_30_days: 00:00:00 30 days ago to 23:59:59 today
//   - this_month: 00:00:00 of the 1st of this month to 23:59:59 today
//   - last_month: 00:00:00 of the 1st of last month to 23:59:59 of the last day of last month
//
// All times are converted to UTC for the LinkedIn API.
func (c *Client) dateRangeForWindow(window model.MetricsWindow) (start, end time.Time, err error) {
	now := c.now()
	// Normalize to start of today in the local timezone.
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	todayEndOfDay := time.Date(year, month, day, 23, 59, 59, 999999999, now.Location())

	switch window {
	case model.MetricsWindowToday:
		start, end = today, todayEndOfDay

	case model.MetricsWindowLast7Days:
		start = today.AddDate(0, 0, -6) // 6 days before today = 7 days inclusive
		end = todayEndOfDay

	case model.MetricsWindowLast30Days:
		start = today.AddDate(0, 0, -29) // 29 days before today = 30 days inclusive
		end = todayEndOfDay

	case model.MetricsWindowThisMonth:
		start = time.Date(year, month, 1, 0, 0, 0, 0, now.Location())
		end = todayEndOfDay

	case model.MetricsWindowLastMonth:
		// First day of last month.
		prevMonth := today.AddDate(0, -1, 0)
		prevYear, prevMonthNum, _ := prevMonth.Date()
		start = time.Date(prevYear, prevMonthNum, 1, 0, 0, 0, 0, now.Location())
		// Last day of last month, end of day.
		lastDayOfLastMonth := today.AddDate(0, 0, -day) // day-1 days ago is the last day of last month
		end = time.Date(prevYear, prevMonthNum, lastDayOfLastMonth.Day(), 23, 59, 59, 999999999, now.Location())

	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported metrics window: %s", window)
	}

	// Convert to UTC for the API.
	return start.UTC(), end.UTC(), nil
}
