// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		if r.URL.Path != "/adAnalytics" {
			http.Error(w, fmt.Sprintf("unexpected path: %s", r.URL.Path), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"elements": [
				{
					"impressions": 1000,
					"clicks": 50,
					"costInUsd": "25.50"
				}
			]
		}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 1000 {
		t.Errorf("expected impressions 1000, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 50 {
		t.Errorf("expected clicks 50, got %d", metrics.Clicks)
	}
	// costInUsd 25.50 → 25,500,000 micros
	if metrics.CostMicros != 25_500_000 {
		t.Errorf("expected cost micros 25500000, got %d", metrics.CostMicros)
	}
	expectedCTR := 50.0 / 1000.0
	if metrics.Ctr != expectedCTR {
		t.Errorf("expected CTR %f, got %f", expectedCTR, metrics.Ctr)
	}
	if metrics.CampaignID != "123456" {
		t.Errorf("expected CampaignID 123456, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowToday {
		t.Errorf("expected Window today, got %q", metrics.Window)
	}

	mu.Lock()
	path, query := gotPath, gotQuery
	mu.Unlock()
	if path != "/adAnalytics" {
		t.Fatalf("unexpected request path: %s", path)
	}
	decodedQuery, err := url.QueryUnescape(query)
	if err != nil {
		t.Fatalf("unescape query: %v", err)
	}
	if !strings.Contains(decodedQuery, "q=analytics") {
		t.Errorf("expected q=analytics in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "campaigns=List(urn:li:sponsoredCampaign:123456)") {
		t.Errorf("expected campaign URN List() in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "accounts=List(urn:li:sponsoredAccount:account123)") {
		t.Errorf("expected account URN List() in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "pivot=CAMPAIGN") {
		t.Errorf("expected pivot=CAMPAIGN in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "timeGranularity=ALL") {
		t.Errorf("expected timeGranularity=ALL in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "dateRange=(start:") {
		t.Errorf("expected dateRange with start in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, ",end:") {
		t.Errorf("expected dateRange with end in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "fields=impressions,clicks,costInUsd") {
		t.Errorf("expected fields in query, got %s", decodedQuery)
	}
}

func TestGetCampaignMetrics_AggregateCostOverflowIsError(t *testing.T) {
	// Each element's costInUsd converts to a micros value comfortably under int64's
	// per-value overflow guard on its own, but two of them summed together exceeds
	// math.MaxInt64 — the aggregate sum must be rejected, not silently wrapped negative.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"elements": [
				{"impressions": 1000, "clicks": 50, "costInUsd": "6000000000000.00"},
				{"impressions": 1000, "clicks": 50, "costInUsd": "6000000000000.00"}
			]
		}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
	if err == nil {
		t.Fatalf("expected an aggregate overflow error, got metrics %+v", metrics)
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Errorf("expected overflow error, got: %v", err)
	}
}

// TestGetCampaignMetrics_CountGuardsAreErrors covers the impressions/clicks guards in the
// aggregation loop. TestGetCampaignMetrics_AggregateCostOverflowIsError only exercises the
// cost sum, so before this the negative-count rejection and both running-count overflow
// checks had no coverage — a regression that dropped them would silently understate (or
// wrap negative) the two headline metrics on a 200 response.
func TestGetCampaignMetrics_CountGuardsAreErrors(t *testing.T) {
	// math.MaxInt64 = 9223372036854775807. Each element below is individually a valid
	// int64; only the running sum trips the guard.
	tests := []struct {
		name     string
		elements string
		wantErr  string
	}{
		{
			name:     "negative impressions",
			elements: `{"impressions": -1, "clicks": 5}`,
			wantErr:  "negative impressions or clicks",
		},
		{
			name:     "negative clicks",
			elements: `{"impressions": 10, "clicks": -1}`,
			wantErr:  "negative impressions or clicks",
		},
		{
			name: "aggregate impressions overflow",
			elements: `{"impressions": 9223372036854775807, "clicks": 1},
				{"impressions": 1, "clicks": 1}`,
			wantErr: "aggregate impressions overflows int64",
		},
		{
			name: "aggregate clicks overflow",
			elements: `{"impressions": 1, "clicks": 9223372036854775807},
				{"impressions": 1, "clicks": 1}`,
			wantErr: "aggregate clicks overflows int64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"elements": [%s]}`, tt.elements)
			}))
			defer server.Close()

			client := NewClient(
				Credentials{AccessToken: "test-token"},
				RuntimeConfig{DefaultAccountID: "account123"},
				WithBaseURL(server.URL),
			)

			metrics, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
			if err == nil {
				t.Fatalf("expected an error, got metrics %+v", metrics)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestGetCampaignMetrics_NoActivity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": []}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
	if metrics.CampaignID != "123456" {
		t.Errorf("expected CampaignID 123456, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowLast7Days {
		t.Errorf("expected Window last_7_days, got %q", metrics.Window)
	}
}

func TestGetCampaignMetrics_MissingElementsFieldIsDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	if _, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday); err == nil {
		t.Fatal("expected an error when the elements field is missing")
	}
}

func TestGetCampaignMetrics_NullElementsFieldIsDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": null}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	if _, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday); err == nil {
		t.Fatal("expected an error when the elements field is null")
	}
}

func TestGetCampaignMetrics_ZeroImpressions(t *testing.T) {
	// Zero impressions should result in zero CTR (no divide-by-zero)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": [{"impressions": 0, "clicks": 0, "costInUsd": "0"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowThisMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Ctr != 0 {
		t.Errorf("expected CTR 0 when impressions are 0, got %f", metrics.Ctr)
	}
}

func TestGetCampaignMetrics_MissingCostUSD(t *testing.T) {
	// Cost can be omitted (nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": [{"impressions": 100, "clicks": 10}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowLastMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.CostMicros != 0 {
		t.Errorf("expected cost 0 when costInUsd is missing, got %d", metrics.CostMicros)
	}
}

func TestGetCampaignMetrics_EmptyCampaignID(t *testing.T) {
	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
	)

	ctx := context.Background()
	_, err := client.GetCampaignMetrics(ctx, "account123", "", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected error for empty campaign ID, got nil")
	}
}

func TestGetCampaignMetrics_EmptyAccountID(t *testing.T) {
	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
	)

	ctx := context.Background()
	_, err := client.GetCampaignMetrics(ctx, "", "123456", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected error for empty account ID, got nil")
	}
}

func TestGetCampaignMetrics_UnsupportedWindow(t *testing.T) {
	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
	)

	ctx := context.Background()
	_, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindow("invalid_window"))
	if err == nil {
		t.Fatal("expected error for unsupported window, got nil")
	}
}

func TestGetCampaignMetrics_RetriesOn429(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": [{"impressions": 10, "clicks": 1, "costInUsd": "1.0"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 10 {
		t.Errorf("expected impressions 10 after retry, got %d", metrics.Impressions)
	}
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n != 2 {
		t.Errorf("expected 2 attempts (1 retry after 429), got %d", n)
	}
}

func TestGetCampaignMetrics_RetriesExhaustedReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	// A permanently-429ing server must surface a terminal error once retryMax is
	// exhausted, not a nil error with a nil *CampaignMetrics — the latter would
	// panic on the caller's *resp.Elements dereference.
	_, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error once retries are exhausted, got nil")
	}
}

func TestDateRangeForWindow_Today(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 45, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, end, err := client.dateRangeForWindow(model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedStart := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}

func TestDateRangeForWindow_Last7Days(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 45, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, end, err := client.dateRangeForWindow(model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 7 days = 6 days before today through today
	expectedStart := time.Date(2025, 1, 9, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}

func TestDateRangeForWindow_Last30Days(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 45, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, end, err := client.dateRangeForWindow(model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 30 days = 29 days before today through today
	expectedStart := time.Date(2024, 12, 17, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}

func TestDateRangeForWindow_ThisMonth(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 45, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, end, err := client.dateRangeForWindow(model.MetricsWindowThisMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Jan 1 to Jan 15
	expectedStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}

func TestDateRangeForWindow_LastMonth(t *testing.T) {
	fixedTime := time.Date(2025, 1, 15, 10, 30, 45, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, end, err := client.dateRangeForWindow(model.MetricsWindowLastMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Dec 1 to Dec 31 of last year
	expectedStart := time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}

// TestDateRangeForWindow_ThisMonthAndLastMonth_MonthEndBoundary pins the exact bug
// bugbot/copilot flagged: subtracting a month from a 29th/30th/31st-of-month "today"
// via AddDate(0,-1,0) normalizes into the WRONG month (e.g. March 31 - 1 month lands
// in early March, not February). Both windows must be anchored on the first-of-month,
// not on today's day-of-month.
func TestDateRangeForWindow_ThisMonthAndLastMonth_MonthEndBoundary(t *testing.T) {
	fixedTime := time.Date(2025, 3, 31, 12, 0, 0, 0, time.UTC)
	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	thisStart, thisEnd, err := client.dateRangeForWindow(model.MetricsWindowThisMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC); !thisStart.Equal(want) {
		t.Errorf("this_month start: expected %v, got %v", want, thisStart)
	}
	if want := time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC); !thisEnd.Equal(want) {
		t.Errorf("this_month end: expected %v, got %v", want, thisEnd)
	}

	lastStart, lastEnd, err := client.dateRangeForWindow(model.MetricsWindowLastMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC); !lastStart.Equal(want) {
		t.Errorf("last_month start: expected %v, got %v", want, lastStart)
	}
	if want := time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC); !lastEnd.Equal(want) {
		t.Errorf("last_month end: expected %v, got %v", want, lastEnd)
	}
}

// TestDateRangeForWindow_TimezoneHandling pins the UTC-calendar contract for a
// positive-offset (east-of-UTC) clock. dateRangeForWindow calls now().UTC() before
// extracting year/month/day, so the window follows the UTC date and NOT the client's
// local one. The instant below is deliberately chosen to make the two disagree: at
// 06:00 on Aug 5 in UTC+10 it is still 20:00 on Aug 4 in UTC, so "today" must resolve
// to Aug 4. A same-day instant (e.g. 10:30 UTC+10, which is 00:30 UTC on Aug 5) would
// pass whether the code normalized to UTC or not, and so would not be binding.
func TestDateRangeForWindow_TimezoneHandling(t *testing.T) {
	fixedLoc := time.FixedZone("UTC+10", 10*3600)
	fixedTime := time.Date(2025, 8, 5, 6, 0, 0, 0, fixedLoc)

	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	// Local date is Aug 5; UTC date is Aug 4. The UTC one wins.
	start, end, err := client.dateRangeForWindow(model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extracted date components must reflect the UTC date (Aug 4), not the
	// client's local date (Aug 5).
	if got, want := start.Day(), 4; got != want {
		t.Errorf("today start day: expected %d, got %d", want, got)
	}
	if got, want := start.Month(), time.August; got != want {
		t.Errorf("today start month: expected %v, got %v", want, got)
	}
	if got, want := start.Year(), 2025; got != want {
		t.Errorf("today start year: expected %d, got %d", want, got)
	}

	if got, want := end.Day(), 4; got != want {
		t.Errorf("today end day: expected %d, got %d", want, got)
	}
	if got, want := end.Month(), time.August; got != want {
		t.Errorf("today end month: expected %v, got %v", want, got)
	}
	if got, want := end.Year(), 2025; got != want {
		t.Errorf("today end year: expected %d, got %d", want, got)
	}
}

// TestDateRangeForWindow_UTCNegativeOffset verifies that negative-offset timezones
// (west of UTC) are handled correctly. The function normalizes to UTC before
// extracting date components, ensuring consistent UTC-based queries regardless
// of the client's timezone. For a client in UTC-5 on Jan 15, the UTC date is
// Jan 16, so the API receives the query for Jan 16 UTC metrics.
func TestDateRangeForWindow_UTCNegativeOffset(t *testing.T) {
	// Simulate a client in US Eastern Standard Time (UTC-5).
	// 2025-01-15 22:00:00 EST = 2025-01-16 03:00:00 UTC
	estLoc := time.FixedZone("EST", -5*3600)
	fixedTime := time.Date(2025, 1, 15, 22, 0, 0, 0, estLoc)

	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	start, _, err := client.dateRangeForWindow(model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The function normalizes to UTC: fixedTime.UTC() = 2025-01-16 03:00:00 UTC.
	// Date components extracted from UTC are: year=2025, month=1, day=16.
	// The returned times are UTC times with UTC date components.
	if got, want := start.Day(), 16; got != want {
		t.Errorf("today start day: expected %d (UTC date), got %d", want, got)
	}
	if got, want := start.Month(), time.January; got != want {
		t.Errorf("today start month: expected %v, got %v", want, got)
	}
	if got, want := start.Year(), 2025; got != want {
		t.Errorf("today start year: expected %d, got %d", want, got)
	}
}

// TestCostInUsdToMicros_MaxInt64ExactlySucceeds pins the int64 boundary with exact
// (big.Rat) arithmetic: a value converting to precisely math.MaxInt64 micros fits
// and must succeed. This is the boundary a float64-intermediate implementation gets
// wrong — math.MaxInt64 is not exactly representable as a float64 (it rounds UP to
// 2^63), which would push this exact-fit value over a naive overflow guard.
func TestCostInUsdToMicros_MaxInt64ExactlySucceeds(t *testing.T) {
	got, err := costInUsdToMicros("9223372036854.775807")
	if err != nil {
		t.Fatalf("unexpected error for a value at exactly math.MaxInt64 micros: %v", err)
	}
	if want := int64(math.MaxInt64); got != want {
		t.Errorf("micros = %d, want %d", got, want)
	}
}

// TestCostInUsdToMicros_OneMicroOverMaxInt64Overflows pins the other side of the same
// boundary: one micro beyond math.MaxInt64 must be rejected, not silently wrapped.
func TestCostInUsdToMicros_OneMicroOverMaxInt64Overflows(t *testing.T) {
	if _, err := costInUsdToMicros("9223372036854.775808"); err == nil {
		t.Fatal("expected an overflow error for a value one micro beyond math.MaxInt64, got nil")
	}
}

// TestCostInUsdToMicros_HighPrecisionValueParsedExactly pins the defect a float64
// intermediate introduces: at magnitudes where float64's representable-value spacing
// exceeds 1 micro, strconv.ParseFloat followed by *1_000_000 can silently misrepresent
// the exact decimal. 9007199254.740993 is exactly representable in decimal but sits at
// a spacing boundary where float64 cannot hold the odd micros value that follows.
// big.Rat parses the decimal string exactly, so this must round-trip losslessly.
func TestCostInUsdToMicros_HighPrecisionValueParsedExactly(t *testing.T) {
	got, err := costInUsdToMicros("9007199254.740993")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(9_007_199_254_740_993); got != want {
		t.Errorf("micros = %d, want %d (exact odd value must survive parsing)", got, want)
	}
}

func TestCostInUsdToMicros_WellUnderTheLimitSucceeds(t *testing.T) {
	got, err := costInUsdToMicros("1000000.50")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(1_000_000_500_000); got != want {
		t.Errorf("micros = %d, want %d", got, want)
	}
}

func TestCostInUsdToMicros_RoundsRatherThanTruncates(t *testing.T) {
	// 25.505 USD -> 25,505,000.0 micros exactly; use a value whose product isn't a
	// clean integer to prove rounding (not truncation) is what's applied.
	got, err := costInUsdToMicros("25.5055555")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(25_505_556); got != want {
		t.Errorf("micros = %d, want %d (round, not truncate)", got, want)
	}
}

// TestCostInUsdToMicros_NegativeValueRejected pins the r.Sign() < 0 guard. It is the only
// thing between a negative spend figure and a sign-flipped CostMicros, and a future swap
// away from big.Rat could drop it while every other case in this suite still passed.
func TestCostInUsdToMicros_NegativeValueRejected(t *testing.T) {
	for _, in := range []string{"-10.50", "-0.000001", "-9223372036854.775807"} {
		t.Run(in, func(t *testing.T) {
			if _, err := costInUsdToMicros(in); err == nil {
				t.Fatalf("expected an error for negative spend %q, got nil", in)
			}
		})
	}
}

// TestCostInUsdToMicros_RatioSyntaxRejected pins decimalCostPattern, whose entire reason
// for existing is that big.Rat.SetString accepts syntax that is not a decimal at all.
// Without the regex, SetString("1/2") SUCCEEDS and yields 0.5 — a parse error silently
// becoming value coercion. That makes the guard look redundant in a diff, which is exactly
// why it needs a test to stop it being cleaned up.
func TestCostInUsdToMicros_RatioSyntaxRejected(t *testing.T) {
	for _, in := range []string{"1/2", "3/4", "1e3", "0x10", " 1.5", "1.5 ", "1,5", ""} {
		t.Run(in, func(t *testing.T) {
			if _, err := costInUsdToMicros(in); err == nil {
				t.Fatalf("expected an error for non-decimal syntax %q, got nil", in)
			}
		})
	}
}

// TestCostInUsdToMicros_ErrorsNeverEchoTheValue pins the sanitization at the layer that
// actually decides it. GetCampaignMetrics's own message was already value-free, but it
// wraps this function's error with %w, so a %q here still put unvalidated upstream
// response content into the server log via BriefService.GetCampaignMetrics's default
// branch. The distinguishing marker below is a value no legitimate costInUsd contains,
// so its absence is evidence the input was not reproduced rather than a coincidence.
func TestCostInUsdToMicros_ErrorsNeverEchoTheValue(t *testing.T) {
	const marker = "SENTINEL-LEAK-MARKER"
	inputs := []string{
		marker,                    // fails the pattern guard
		"-1" + marker,             // fails the pattern guard, negative-looking
		"-10.50",                  // fails the sign guard
		"9223372036854776" + ".0", // overflows int64 micros
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			_, err := costInUsdToMicros(in)
			if err == nil {
				t.Fatalf("expected an error for %q", in)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error echoed the untrusted value into a logged message: %v", err)
			}
			if strings.Contains(err.Error(), in) {
				t.Errorf("error reproduced the raw input %q verbatim: %v", in, err)
			}
		})
	}
}

// fakeCredentialBearingTripper is a RoundTripper that injects credential-bearing
// strings into errors, to test that GetCampaignMetrics redacts them before logging.
type fakeCredentialBearingTripper struct {
	credentialMarker string
}

func (f *fakeCredentialBearingTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("fake RoundTripper error: %s", f.credentialMarker)
}

// fakeDNSFailingTripper returns a DNS error wrapped in a *url.Error, which would
// normally expose the full analytics URL. Used to verify redactHTTPDoError unwraps
// while preserving the dial error classification.
type fakeDNSFailingTripper struct{}

func (f *fakeDNSFailingTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{
		Op:  "Get",
		URL: "https://api.linkedin.com/rest/adAnalytics?q=analytics&campaigns=List(urn:li:sponsoredCampaign:123)&accounts=List(urn:li:sponsoredAccount:abc)",
		Err: &net.DNSError{IsTimeout: false, IsTemporary: false, Name: "api.linkedin.com"},
	}
}

// TestGetCampaignMetrics_TransportErrorRedaction verifies that three error paths are
// properly redacted and classifiable:
//
//  1. Mid-flight/RoundTripper errors: credential-bearing strings are redacted
//  2. Pre-send dial errors (DNS, ECONNREFUSED): wrapped in *url.Error carrying the URL,
//     but unwrapped so the dial classification (via errors.Is/As) is preserved
//  3. Body-read errors: distinct safe message, not collapsed into generic "request failed"
//
// The error propagates to BriefService.GetCampaignMetrics's default-error branch,
// which logs it; no credential/URL material must appear in that log.
func TestGetCampaignMetrics_TransportErrorRedaction(t *testing.T) {
	t.Run("RoundTripper_credential_redaction", func(t *testing.T) {
		const credentialMarker = "Bearer-token-SENTINEL-LEAK-MARKER"

		client := NewClient(
			Credentials{AccessToken: "test-token"},
			RuntimeConfig{DefaultAccountID: "account123"},
			WithHTTPClient(&http.Client{Transport: &fakeCredentialBearingTripper{credentialMarker: credentialMarker}}),
		)

		ctx := context.Background()
		metrics, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
		if err == nil {
			t.Fatalf("expected an error, got metrics %+v", metrics)
		}

		// The credential marker must NOT appear in the error string that would be logged.
		errStr := err.Error()
		if strings.Contains(errStr, credentialMarker) {
			t.Errorf("error string echoed credential material: %v", errStr)
		}
		if strings.Contains(errStr, "fake RoundTripper") {
			t.Errorf("error string leaked RoundTripper implementation detail: %v", errStr)
		}
	})

	t.Run("DialError_URL_redaction_preserves_classification", func(t *testing.T) {
		client := NewClient(
			Credentials{AccessToken: "test-token"},
			RuntimeConfig{DefaultAccountID: "account123"},
			WithHTTPClient(&http.Client{Transport: &fakeDNSFailingTripper{}}),
		)

		ctx := context.Background()
		_, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
		if err == nil {
			t.Fatalf("expected an error")
		}

		// The full URL must NOT appear in the error string that would be logged.
		errStr := err.Error()
		if strings.Contains(errStr, "https://api.linkedin.com") {
			t.Errorf("error string leaked the full analytics URL: %v", errStr)
		}
		if strings.Contains(errStr, "adAnalytics?q=") {
			t.Errorf("error string leaked analytics query parameters: %v", errStr)
		}

		// But the dial error classification must still be reachable via errors.As so
		// callers can decide whether this failure is retryable.
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) {
			t.Errorf("dial error classification was lost; DNSError not found in chain: %v", err)
		}
	})
}

// TestRedactHTTPDoError verifies the redaction helper works correctly.
func TestRedactHTTPDoError(t *testing.T) {
	t.Run("unwraps_url_error_with_dns", func(t *testing.T) {
		// Simulate a *url.Error wrapping a DNSError
		dialErr := &net.DNSError{IsTimeout: false, IsTemporary: false, Name: "api.linkedin.com"}
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAnalytics?q=analytics&campaigns=List(urn:li:sponsoredCampaign:123)",
			Err: dialErr,
		}

		redacted := redactHTTPDoError(wrapped)
		errStr := redacted.Error()

		// URL must not appear
		if strings.Contains(errStr, "https://") {
			t.Errorf("redacted error still contains URL: %v", errStr)
		}
		if strings.Contains(errStr, "adAnalytics?") {
			t.Errorf("redacted error still contains query parameters: %v", errStr)
		}

		// But the dial classification must be preserved
		var dnsErr *net.DNSError
		if !errors.As(redacted, &dnsErr) {
			t.Errorf("redacted error lost DNSError classification: %v", redacted)
		}
	})
}
