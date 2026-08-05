// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map to a
// LinkedIn Ad Analytics date range (currently: yesterday, last_14_days).
var ErrUnsupportedWindow = errors.New("unsupported metrics window")

// AdAnalyticsElement is one analytics record returned by the Ad Analytics API.
// The API aggregates metrics for the requested campaign over the given date range.
type AdAnalyticsElement struct {
	// Impressions is the number of times the ad was displayed.
	Impressions int64 `json:"impressions"`
	// Clicks is the number of times the ad was clicked.
	Clicks int64 `json:"clicks"`
	// CostInUsd is the spend in decimal USD (e.g. 25.50 for $25.50). LinkedIn's
	// Ad Analytics API returns this as a JSON string representing a BigDecimal,
	// so a custom unmarshaler is required to parse it. This is always USD,
	// regardless of the ad account's billing currency configuration.
	CostInUsd *string `json:"costInUsd"`
}

// AdAnalyticsResponse is the JSON response from LinkedIn's Ad Analytics endpoint.
// Elements is a pointer so a missing/null "elements" field (malformed response,
// e.g. an empty body, "{}", or "null") is distinguishable from an explicit empty
// array (genuinely zero activity in the window) — nil means "we couldn't confirm
// this is real zero-activity data" and is treated as a decode error, not silently
// reported as zero metrics.
type AdAnalyticsResponse struct {
	Elements *[]AdAnalyticsElement `json:"elements"`
}

// GetCampaignMetrics reads live campaign metrics from LinkedIn's Ad Analytics API
// for the given campaign during the specified time window. This method implements
// the service.MetricsReader interface (the optional capability the orchestrator
// discovers per dispatcher).
//
// campaignID is the bare numeric LinkedIn campaign id as persisted by
// campaignFromLinkedIn (trailingID of the campaign URN returned on creation)
// — NOT a URN. This method builds the sponsoredCampaign/sponsoredAccount URNs the
// Ad Analytics finder requires.
//
// The returned CampaignMetrics contains:
//   - Impressions: number of times the ad was displayed
//   - Clicks: number of times the ad was clicked
//   - CostMicros: spend in micros of USD (the Ad Analytics API returns costInUsd
//     which is always in USD regardless of the ad account's billing currency
//     configuration)
//   - Ctr: clicks/impressions (0 when impressions is 0)
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

	resp, err := c.makeAdAnalyticsRequest(ctx, accountID, campaignID, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	// No elements is not an error: the finder returns an empty array (never a
	// null/missing "elements" field on a well-formed 2xx — that case is rejected
	// as a decode error inside makeAdAnalyticsRequest) when the campaign had no
	// activity in the window.
	if len(*resp.Elements) == 0 {
		return &model.CampaignMetrics{CampaignID: campaignID, Window: window}, nil
	}

	// Aggregate metrics from all elements. In practice there should be one element
	// for a single campaign over a single (non-daily) date range.
	var metrics model.CampaignMetrics
	for _, elem := range *resp.Elements {
		metrics.Impressions += elem.Impressions
		metrics.Clicks += elem.Clicks
		if elem.CostInUsd != nil {
			// LinkedIn's Ad Analytics API returns costInUsd as a BigDecimal serialized as a
			// JSON string. A present but malformed/non-finite/negative/overflowing value
			// must fail the read, not be silently dropped: dropping it returns 200 with
			// understated spend while impressions/clicks still report normally, which looks
			// like a real (if low) cost rather than a decode failure.
			micros, err := costInUsdToMicros(*elem.CostInUsd)
			if err != nil {
				return nil, fmt.Errorf("get campaign metrics: parse costInUsd %q: %w", *elem.CostInUsd, err)
			}
			metrics.CostMicros += micros
		}
	}

	// Calculate Ctr: clicks / impressions. If impressions is 0, Ctr stays 0.
	if metrics.Impressions > 0 {
		metrics.Ctr = float64(metrics.Clicks) / float64(metrics.Impressions)
	}

	metrics.CampaignID = campaignID
	metrics.Window = window

	return &metrics, nil
}

// costInUsdToMicros parses a LinkedIn Ad Analytics costInUsd decimal string (e.g.
// "25.50") into micros of USD, rounding rather than truncating so a value like
// 25.505 doesn't silently lose a micro. It rejects anything that isn't a clean,
// finite, non-negative decimal, or that would overflow int64 once converted to
// micros, so a malformed value surfaces as a decode error instead of being
// silently coerced into an understated cost.
func costInUsdToMicros(s string) (int64, error) {
	costUsd, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a valid decimal: %w", err)
	}
	if math.IsNaN(costUsd) || math.IsInf(costUsd, 0) {
		return 0, fmt.Errorf("non-finite value: %v", costUsd)
	}
	if costUsd < 0 {
		return 0, fmt.Errorf("negative value: %v", costUsd)
	}
	micros := costUsd * 1_000_000
	if micros > math.MaxInt64 {
		return 0, fmt.Errorf("value overflows int64 micros: %v", costUsd)
	}
	return int64(math.Round(micros)), nil
}

// restLiDate renders a time.Time as a Rest.li 2.0 nested date object literal,
// e.g. "(day:15,month:1,year:2025)".
func restLiDate(t time.Time) string {
	return fmt.Sprintf("(day:%d,month:%d,year:%d)", t.Day(), int(t.Month()), t.Year())
}

// makeAdAnalyticsRequest makes a raw HTTP request to LinkedIn's Ad Analytics
// finder and parses the response into AdAnalyticsResponse.
//
// This bypasses doRequest (which unmarshals into the campaign/creative-shaped
// linkedInResponse) because Ad Analytics responses have a different JSON shape,
// AND because the Ad Analytics finder uses Rest.li 2.0 array/nested-object query
// parameter syntax (List(...), nested dateRange) that doRequest's flat
// map[string]string params can't express. It reuses the client's 429 retry
// policy (parseRetryAfter/retryBaseDelay/maxRetryWait) so an analytics read is
// retried the same way doRequest retries idempotent GETs — see
// docs/knowledge/code/internal-platform-linkedin.md.
//
// UNVERIFIED ASSUMPTION: the finder name (q=analytics), pivot=CAMPAIGN, and
// timeGranularity=ALL are LinkedIn's documented Ad Analytics contract, but this
// has not yet been verified against a live LinkedIn Marketing API account.
func (c *Client) makeAdAnalyticsRequest(ctx context.Context, accountID, campaignID string, startDate, endDate time.Time) (*AdAnalyticsResponse, error) {
	campaignURN := "urn:li:sponsoredCampaign:" + campaignID
	accountURN := "urn:li:sponsoredAccount:" + accountID

	u, err := url.Parse(c.baseURL + "/" + "adAnalytics")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	// Rest.li 2.0 List()/nested-object literals are NOT standard query-string
	// values, so they are appended to RawQuery directly rather than through
	// url.Values.Encode() (which would percent-encode the structural
	// parentheses/colons LinkedIn's finder requires literally).
	rawQuery := "q=analytics" +
		"&pivot=CAMPAIGN" +
		"&timeGranularity=ALL" +
		"&dateRange=(start:" + restLiDate(startDate) + ",end:" + restLiDate(endDate) + ")" +
		"&campaigns=List(" + url.QueryEscape(campaignURN) + ")" +
		"&accounts=List(" + url.QueryEscape(accountURN) + ")" +
		"&fields=impressions,clicks,costInUsd"
	u.RawQuery = rawQuery

	idempotent := true // GET is always retried on 429, same as doRequest's SAFE-method rule.
	for attempt := 0; attempt <= retryMax; attempt++ {
		resp, retry, wait, err := c.doAdAnalyticsAttempt(ctx, u.String())
		if err != nil {
			return nil, err
		}
		if retry {
			if attempt < retryMax && idempotent {
				if wait <= 0 {
					wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
				}
				if wait > maxRetryWait {
					wait = maxRetryWait
				}
				if err := sleepCtx(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
			// Retries exhausted on the last attempt: falling through here would return
			// (nil, nil) — a silent "success" with no data that panics on the caller's
			// *resp.Elements dereference. Surface the exhaustion as a terminal error
			// instead, same shape doRequest's inline 429 handling produces.
			return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: "GET", Path: "adAnalytics", Body: "rate limited: retries exhausted"}
		}
		return resp, nil
	}
	panic("linkedin makeAdAnalyticsRequest: unreachable post-loop return")
}

// doAdAnalyticsAttempt performs a single Ad Analytics GET attempt. It returns
// (resp, false, 0, nil) on success, (nil, true, wait, nil) when the caller
// should retry after wait, or (nil, false, 0, err) on a terminal error.
func (c *Client) doAdAnalyticsAttempt(ctx context.Context, rawURL string) (*AdAnalyticsResponse, bool, time.Duration, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, false, 0, fmt.Errorf("new request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		if isPreSendDialError(err) {
			return nil, false, 0, fmt.Errorf("linkedin GET adAnalytics: %w", err)
		}
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := c.parseRetryAfter(resp)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil, true, wait, nil
	}

	// Read the response body.
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("read response body: %w", err)}
		}
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: fmt.Sprintf("read response body: %v", err)}
	}

	if int64(buf.Len()) > maxResponseBytes {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
		}
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := buf.String()
		if len(text) > 400 {
			text = text[:400]
		}
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: text}
	}

	var analytics AdAnalyticsResponse
	if err := json.Unmarshal(buf.Bytes(), &analytics); err != nil {
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode response: %w", err)}
	}
	if analytics.Elements == nil {
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode response: missing or null \"elements\" field")}
	}

	return &analytics, false, 0, nil
}

// dateRangeForWindow computes the start and end dates for the given metrics window,
// relative to the current time (c.now()). Each window is computed as:
//   - today: today
//   - last_7_days: 6 days ago through today (7 days inclusive)
//   - last_30_days: 29 days ago through today (30 days inclusive)
//   - this_month: the 1st of this month through today
//   - last_month: the 1st through the last day of the PREVIOUS calendar month
//
// Both boundaries are derived from the first-of-month anchor (never via
// AddDate(0, -1, 0) on today's day-of-month), since time.AddDate normalizes an
// invalid day-of-month (e.g. subtracting a month from the 31st) into the
// following month rather than erroring — that would silently shift both
// this_month and last_month's boundaries on 29th/30th/31st-of-month days.
//
// All dates are normalized to UTC before extraction to ensure LinkedIn's API
// receives consistent UTC dates regardless of the client's local timezone. For
// example, a client in Asia/Tokyo (UTC+9) on Jan 15 local time will extract
// year/month/day from UTC (Jan 15 UTC, not Jan 14 UTC), so the query requests
// Jan 15 UTC metrics.
func (c *Client) dateRangeForWindow(window model.MetricsWindow) (start, end time.Time, err error) {
	now := c.now().UTC()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	firstOfThisMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	switch window {
	case model.MetricsWindowToday:
		start, end = today, today

	case model.MetricsWindowLast7Days:
		start = today.AddDate(0, 0, -6) // 6 days before today = 7 days inclusive
		end = today

	case model.MetricsWindowLast30Days:
		start = today.AddDate(0, 0, -29) // 29 days before today = 30 days inclusive
		end = today

	case model.MetricsWindowThisMonth:
		start = firstOfThisMonth
		end = today

	case model.MetricsWindowLastMonth:
		// One day before the first of this month is always the last day of the
		// previous month, regardless of how many days that month has.
		lastDayOfLastMonth := firstOfThisMonth.AddDate(0, 0, -1)
		start = time.Date(lastDayOfLastMonth.Year(), lastDayOfLastMonth.Month(), 1, 0, 0, 0, 0, now.Location())
		end = lastDayOfLastMonth

	default:
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}

	return start, end, nil
}
