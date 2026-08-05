// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
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
					"costInUsd": 25.50
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
		_, _ = fmt.Fprint(w, `{"elements": [{"impressions": 0, "clicks": 0, "costInUsd": 0}]}`)
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
		_, _ = fmt.Fprint(w, `{"elements": [{"impressions": 10, "clicks": 1, "costInUsd": 1.0}]}`)
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

// TestDateRangeForWindow_TimezoneHandling verifies that date ranges are
// constructed correctly when the client clock is in a non-UTC timezone.
// This tests the fix for the timezone bug: converting midnight-in-local-time
// to UTC could shift the date (e.g. Aug 5 00:00 UTC+10 becomes Aug 4 14:00 UTC),
// so we must NOT convert to UTC before extracting date components for the
// LinkedIn Ad Analytics API.
func TestDateRangeForWindow_TimezoneHandling(t *testing.T) {
	// Simulate a client clock in UTC+10 (Australian Eastern time).
	// 2025-01-15 10:30:45 AEDT (UTC+11 due to daylight saving) = 2025-01-14 23:30:45 UTC
	// But for simplicity, we'll use 2025-08-05 10:30:45 in a UTC+10 fixed offset zone.
	fixedLoc := time.FixedZone("UTC+10", 10*3600)
	fixedTime := time.Date(2025, 8, 5, 10, 30, 45, 0, fixedLoc)

	client := NewClient(
		Credentials{AccessToken: "test"},
		RuntimeConfig{},
		WithClock(func() time.Time { return fixedTime }),
	)

	// Today in the client's local time is 2025-08-05 (not 2025-08-04 UTC).
	// The dateRangeForWindow should return the date in the client's local time,
	// not in UTC.
	start, end, err := client.dateRangeForWindow(model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extract the date components using the time's own location (UTC+10 offset).
	// This should give 2025-08-05, not 2025-08-04.
	if got, want := start.Day(), 5; got != want {
		t.Errorf("today start day: expected %d, got %d", want, got)
	}
	if got, want := start.Month(), time.August; got != want {
		t.Errorf("today start month: expected %v, got %v", want, got)
	}
	if got, want := start.Year(), 2025; got != want {
		t.Errorf("today start year: expected %d, got %d", want, got)
	}

	if got, want := end.Day(), 5; got != want {
		t.Errorf("today end day: expected %d, got %d", want, got)
	}
	if got, want := end.Month(), time.August; got != want {
		t.Errorf("today end month: expected %v, got %v", want, got)
	}
	if got, want := end.Year(), 2025; got != want {
		t.Errorf("today end year: expected %d, got %d", want, got)
	}
}
