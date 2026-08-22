// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	var gotAuth, gotVersion, gotRestLi string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("LinkedIn-Version")
		gotRestLi = r.Header.Get("X-RestLi-Protocol-Version")
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
	auth, version, restLi := gotAuth, gotVersion, gotRestLi
	mu.Unlock()
	if path != "/adAnalytics" {
		t.Fatalf("unexpected request path: %s", path)
	}

	// The analytics path builds its own request rather than going through doRequest, so
	// it re-sets these three headers by hand. Nothing else in this package's tests covers
	// THAT copy — the shared-path contract is pinned separately in client_findings_test.go
	// — so deleting one of these lines would make every live metrics call fail while the
	// happy-path assertions above stayed green.
	if auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-token")
	}
	if version != apiVersion {
		t.Errorf("LinkedIn-Version = %q, want %q", version, apiVersion)
	}
	if restLi != "2.0.0" {
		t.Errorf("X-RestLi-Protocol-Version = %q, want %q", restLi, "2.0.0")
	}

	// Assert the RAW query too, not only the decoded one. Decoding erases the exact
	// distinction makeAdAnalyticsRequest depends on: a query built with
	// url.Values.Encode() would percent-encode the Rest.li structural parentheses and
	// colons, yet decode to the same text and satisfy every check below. LinkedIn's
	// finder needs the structure LITERAL and the URN values ESCAPED, so both halves are
	// pinned here.
	if !strings.Contains(query, "dateRange=(start:(day:") {
		t.Errorf("Rest.li dateRange structure was escaped in the raw query: %s", query)
	}
	if !strings.Contains(query, "campaigns=List(urn%3Ali%3AsponsoredCampaign%3A123456)") {
		t.Errorf("campaign URN was not escaped inside a literal List() in the raw query: %s", query)
	}
	if !strings.Contains(query, "accounts=List(urn%3Ali%3AsponsoredAccount%3Aaccount123)") {
		t.Errorf("account URN was not escaped inside a literal List() in the raw query: %s", query)
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
		withRetryBaseDelay(time.Millisecond),
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
		withRetryBaseDelay(time.Millisecond),
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

	t.Run("BodyReadError_redacted_in_5xx_path", func(t *testing.T) {
		// Create a server that returns 500 with a header that causes read to fail.
		// The response will say it has content but close immediately, causing a read error.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "10000000") // Lie about size
			w.WriteHeader(http.StatusInternalServerError)
			// Don't write the promised body; read will fail with EOF or connection error
		}))
		defer server.Close()

		client := NewClient(
			Credentials{AccessToken: "test-token"},
			RuntimeConfig{DefaultAccountID: "account123"},
			WithBaseURL(server.URL),
		)

		ctx := context.Background()
		_, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
		if err == nil {
			t.Fatalf("expected an error from body-read failure with 500")
		}

		// Extract the apiError from the chain and verify its Body field is redacted.
		// The Body field is NOT rendered by apiError.Error() but should still be safe.
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected an apiError in the chain, got: %T", err)
		}

		// Assert the COMPLETE sanitized value, not just that the redacted prefix is
		// present. A "contains prefix and not EOF" pair cannot fail: any leak that keeps
		// the prefix and appends raw I/O detail satisfies both halves. Equality is what
		// binds the absence of appended detail.
		// Matches the 2xx path's transportError.Err exactly: apiError.Body carries
		// redactBodyReadError's own message with no extra prefix.
		const wantBody = "read response body failed"
		if apiErr.Body != wantBody {
			t.Errorf("apiError.Body = %q, want exactly %q — anything appended is raw I/O detail that escaped redaction", apiErr.Body, wantBody)
		}
	})

	t.Run("BodyReadError_redacted_in_2xx_path", func(t *testing.T) {
		// Create a server that returns 200 with a header that causes read to fail.
		// The 2xx path returns a transportError with redactBodyReadError applied.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "10000000") // Lie about size
			// Status 200 signals success, but don't write the promised body
		}))
		defer server.Close()

		client := NewClient(
			Credentials{AccessToken: "test-token"},
			RuntimeConfig{DefaultAccountID: "account123"},
			WithBaseURL(server.URL),
		)

		ctx := context.Background()
		_, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
		if err == nil {
			t.Fatalf("expected an error from body-read failure with 200")
		}

		// Extract the transportError from the chain and verify its Err field is redacted.
		var transErr *transportError
		if !errors.As(err, &transErr) {
			t.Fatalf("expected a transportError in the chain, got: %T", err)
		}

		// Assert the COMPLETE sanitized value, for the same reason as the 5xx path above.
		const wantErr = "read response body failed"
		if errMsg := transErr.Err.Error(); errMsg != wantErr {
			t.Errorf("transportError.Err = %q, want exactly %q — anything appended is raw I/O detail that escaped redaction", errMsg, wantErr)
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

	t.Run("dial_error_text_never_reaches_the_error_string", func(t *testing.T) {
		// WithHTTPClient accepts an arbitrary RoundTripper, so the innermost dial error is
		// not this package's to trust: its text (here, net.DNSError.Name) can carry a URL or
		// a credential. The redacted error must render a fixed string while still being
		// classifiable via errors.As.
		const markerInCause = "CREDENTIAL_MARKER_IN_CAUSE"
		dialErr := &net.DNSError{Name: "api.linkedin.com/?token=" + markerInCause}
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAnalytics?q=analytics",
			Err: dialErr,
		}

		redacted := redactHTTPDoError(wrapped)
		if errStr := redacted.Error(); strings.Contains(errStr, markerInCause) {
			t.Errorf("redacted error rendered the untrusted dial cause: %v", errStr)
		}

		var dnsErr *net.DNSError
		if !errors.As(redacted, &dnsErr) {
			t.Errorf("redacted error lost DNSError classification: %v", redacted)
		}
	})

	t.Run("credential_wrapper_between_url_error_and_dns_cause_is_dropped", func(t *testing.T) {
		// The layer carrying untrusted text is not necessarily the *url.Error. A custom
		// RoundTripper can return fmt.Errorf("Bearer <token>: %w", dnsErr), which
		// http.Client.Do then wraps in a *url.Error — so stripping only *url.Error layers
		// leaves a credential-bearing *fmt.wrapError as the cause, reachable by anything
		// that walks or renders the chain. The cause must be REBUILT from the
		// classification, not forwarded.
		const markerInWrapper = "CREDENTIAL_MARKER_IN_WRAPPER"
		const markerInName = "CREDENTIAL_MARKER_IN_NAME"
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAnalytics?q=analytics",
			Err: fmt.Errorf("Bearer %s: %w", markerInWrapper, &net.DNSError{
				Err: "no such host", Name: "api.linkedin.com/?token=" + markerInName, IsNotFound: true,
			}),
		}

		redacted := redactHTTPDoError(wrapped)

		// Walk the ENTIRE returned chain: it is not enough that the outermost Error() is
		// clean, because errors.As/Unwrap hand callers the inner layers directly.
		for e := redacted; e != nil; e = errors.Unwrap(e) {
			s := e.Error()
			if strings.Contains(s, markerInWrapper) || strings.Contains(s, markerInName) {
				t.Fatalf("untrusted text survived into the redacted chain at %T: %s", e, s)
			}
			if strings.Contains(s, "https://") {
				t.Fatalf("URL survived into the redacted chain at %T: %s", e, s)
			}
		}

		// Classification must survive the rebuild, including the IsNotFound bit callers
		// use to decide whether retrying could ever help.
		var dnsErr *net.DNSError
		if !errors.As(redacted, &dnsErr) {
			t.Fatalf("redacted error lost DNSError classification: %v", redacted)
		}
		if !dnsErr.IsNotFound {
			t.Error("rebuilt DNSError dropped IsNotFound; callers cannot tell a permanent lookup failure from a transient one")
		}
	})

	t.Run("unrecognized_dial_cause_collapses_to_the_default_deny_sentinel", func(t *testing.T) {
		// safeDialCause is default-deny: a classification it does not reproduce must not be
		// passed through, even when the caller's own text looks harmless.
		if got := safeDialCause(errors.New("UNRECOGNIZED_CAUSE_TEXT")); !errors.Is(got, errDialFailed) {
			t.Errorf("unrecognized cause = %v, want errDialFailed", got)
		}
	})

	t.Run("redacts_url_error_wrapping_context_canceled", func(t *testing.T) {
		// errors.Is matches context.Canceled through a *url.Error wrapper, but wrapper.Error()
		// still renders the full URL with account/campaign query values. Verify that
		// redactHTTPDoError returns the canonical sentinel, not the wrapper.
		const markerInURL = "CREDENTIAL_MARKER_IN_URL"
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAnalytics?" + markerInURL + "=true",
			Err: context.Canceled,
		}

		redacted := redactHTTPDoError(wrapped)
		errStr := redacted.Error()

		// The marker in the URL must not appear in the redacted error
		if strings.Contains(errStr, markerInURL) {
			t.Errorf("redacted error still contains URL marker: %v", errStr)
		}

		// But errors.Is must still work with the redacted result
		if !errors.Is(redacted, context.Canceled) {
			t.Errorf("redacted error lost context.Canceled classification: %v", redacted)
		}

		// And the redacted result must be exactly the canonical sentinel
		if redacted != context.Canceled {
			t.Errorf("redacted error is not the canonical context.Canceled sentinel: %v", redacted)
		}
	})

	t.Run("redacts_url_error_wrapping_context_deadline_exceeded", func(t *testing.T) {
		// Same as canceled but for deadline exceeded.
		const markerInURL = "DEADLINE_MARKER_IN_URL"
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAnalytics?" + markerInURL + "=true",
			Err: context.DeadlineExceeded,
		}

		redacted := redactHTTPDoError(wrapped)
		errStr := redacted.Error()

		// The marker must not appear
		if strings.Contains(errStr, markerInURL) {
			t.Errorf("redacted error still contains URL marker: %v", errStr)
		}

		// But errors.Is must work
		if !errors.Is(redacted, context.DeadlineExceeded) {
			t.Errorf("redacted error lost context.DeadlineExceeded classification: %v", redacted)
		}

		// And it must be the canonical sentinel
		if redacted != context.DeadlineExceeded {
			t.Errorf("redacted error is not the canonical context.DeadlineExceeded sentinel: %v", redacted)
		}
	})
}

// TestRedactBodyReadError verifies that the body-read redaction helper correctly
// handles context errors and I/O errors.
func TestRedactBodyReadError(t *testing.T) {
	t.Run("redacts_wrapped_context_canceled_marker", func(t *testing.T) {
		// Create a wrapped context.Canceled error (e.g., from a malicious RoundTripper
		// that wraps the sentinel in an error with a marker string).
		const markerInError = "MALICIOUS_BODY_READ_MARKER_CANCELED"
		wrappedErr := fmt.Errorf("body read failed: %s, wrapped: %w", markerInError, context.Canceled)

		redacted := redactBodyReadError(wrappedErr)

		// The marker must not appear in the output
		errStr := redacted.Error()
		if strings.Contains(errStr, markerInError) {
			t.Errorf("redacted error still contains malicious marker: %v", errStr)
		}

		// But classification must be preserved
		if !errors.Is(redacted, context.Canceled) {
			t.Errorf("redacted error lost context.Canceled classification: %v", redacted)
		}

		// And the result must be the canonical sentinel
		if redacted != context.Canceled {
			t.Errorf("redacted error is not the canonical context.Canceled sentinel: %v", redacted)
		}
	})

	t.Run("redacts_wrapped_context_deadline_exceeded_marker", func(t *testing.T) {
		// Same as canceled but for deadline exceeded
		const markerInError = "MALICIOUS_BODY_READ_MARKER_DEADLINE"
		wrappedErr := fmt.Errorf("body read failed: %s, wrapped: %w", markerInError, context.DeadlineExceeded)

		redacted := redactBodyReadError(wrappedErr)

		// The marker must not appear in the output
		errStr := redacted.Error()
		if strings.Contains(errStr, markerInError) {
			t.Errorf("redacted error still contains malicious marker: %v", errStr)
		}

		// But classification must be preserved
		if !errors.Is(redacted, context.DeadlineExceeded) {
			t.Errorf("redacted error lost context.DeadlineExceeded classification: %v", redacted)
		}

		// And the result must be the canonical sentinel
		if redacted != context.DeadlineExceeded {
			t.Errorf("redacted error is not the canonical context.DeadlineExceeded sentinel: %v", redacted)
		}
	})

	t.Run("redacts_io_errors", func(t *testing.T) {
		// Regular I/O errors should return the safe generic message
		ioErr := fmt.Errorf("connection reset by peer")
		redacted := redactBodyReadError(ioErr)
		errStr := redacted.Error()

		if !strings.Contains(errStr, "read response body failed") {
			t.Errorf("redacted error missing safe message: %v", errStr)
		}
		if strings.Contains(errStr, "connection reset") {
			t.Errorf("redacted error leaked raw I/O error details: %v", errStr)
		}
	})
}

// TestGetCampaignMetrics_MalformedJSONRedaction verifies that json.Unmarshal errors
// are redacted and do not leak response body fragments. json.UnmarshalTypeError.Value
// and json.SyntaxError can contain malformed JSON from the response body, which reaches
// the server log via BriefService.GetCampaignMetrics's default error branch. The response
// can be up to 10 MiB of unvalidated upstream content.
func TestGetCampaignMetrics_MalformedJSONRedaction(t *testing.T) {
	// The marker must be a NUMERIC literal, not a quoted string. For a string where an
	// int64 was expected, json.UnmarshalTypeError.Value is only the literal word "string"
	// — the offending bytes are never copied into it, so a marker-absence assertion built
	// on a quoted value cannot fail even if the raw decode error is wrapped verbatim. An
	// integer that overflows int64 is copied into Value as `number <literal>`, which is
	// what actually puts untrusted response bytes on the path to the server log and makes
	// the assertion below bind.
	const maliciousJSONMarker = "918273645509182736455091827364550"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"elements": [{"impressions": %s, "clicks": 0, "costInUsd": "0"}]}`, maliciousJSONMarker)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	_, err := client.GetCampaignMetrics(ctx, "account123", "123456", model.MetricsWindowToday)
	if err == nil {
		t.Fatalf("expected an error from malformed JSON")
	}

	// The malicious marker must NOT appear in the error string that would be logged.
	errStr := err.Error()
	if strings.Contains(errStr, maliciousJSONMarker) {
		t.Errorf("error string leaked malformed JSON value: %v", errStr)
	}

	// The error should contain "decode response" but not the raw JSON value
	if !strings.Contains(errStr, "decode response") {
		t.Errorf("error missing 'decode response' diagnostic: %v", errStr)
	}
	if !strings.Contains(errStr, "malformed JSON") {
		t.Errorf("error missing 'malformed JSON' safe message: %v", errStr)
	}
}

// TestValidateMetricsWindowMatchesDateRangeForWindow pins that the clock-free validator
// and the mapper agree for EVERY window in the shared vocabulary. They are two separate
// pieces of code answering the same question — the dispatcher asks the validator before
// resolving credentials, the client asks the mapper after — so a window added to one and
// not the other would produce a 400 at the gate and a working range downstream, or the
// reverse. Iterating over the full vocabulary rather than a hand-listed subset is what
// makes a NEW model.MetricsWindow fail here instead of silently defaulting to unsupported.
func TestValidateMetricsWindowMatchesDateRangeForWindow(t *testing.T) {
	all := []model.MetricsWindow{
		model.MetricsWindowToday,
		model.MetricsWindowYesterday,
		model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days,
		model.MetricsWindowLast30Days,
		model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
		model.MetricsWindow("not_a_window"),
	}
	c := NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{DefaultAccountID: "a"})
	for _, w := range all {
		validErr := ValidateMetricsWindow(w)
		_, _, mapErr := c.dateRangeForWindow(w)
		if (validErr == nil) != (mapErr == nil) {
			t.Errorf("window %q: ValidateMetricsWindow err=%v but dateRangeForWindow err=%v — the two must agree", w, validErr, mapErr)
		}
		if validErr != nil && !errors.Is(validErr, ErrUnsupportedWindow) {
			t.Errorf("window %q: ValidateMetricsWindow returned %v, want ErrUnsupportedWindow", w, validErr)
		}
	}
}

// TestClicksWithZeroImpressionsIsRejected covers the one metric relationship that stays
// decidable when a JSON key is absent: a click cannot happen without the impression that
// carried it. Without the guard, an element missing "impressions" decodes to zero and the
// read returns 200 with a non-zero click count beside Ctr=0 — a figure that looks real.
func TestClicksWithZeroImpressionsIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements":[{"clicks":5,"costInUsd":"1.00"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)
	_, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err == nil {
		t.Fatalf("expected an error for clicks with zero impressions, got nil")
	}
	if !strings.Contains(err.Error(), "clicks with zero impressions") {
		t.Errorf("error = %v, want it to name the clicks/impressions inconsistency", err)
	}
}

// TestCostInUsdToMicrosBoundsDecimalLength pins the length bound that keeps an
// adversarial decimal out of big.Rat. The 10 MiB response cap does not bound a single
// value, and the parse/scale path is super-linear in digit count and ignores the request
// context, so it keeps running past the 20s call deadline. Rejection must happen on
// LENGTH, before the value reaches big.Rat at all.
func TestCostInUsdToMicrosBoundsDecimalLength(t *testing.T) {
	long := strings.Repeat("9", maxCostDecimalLen+1)
	if _, err := costInUsdToMicros(long); err == nil {
		t.Fatalf("expected an error for a %d-byte decimal, got nil", len(long))
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to name the length bound (a later big.Rat rejection means the bound did not fire first)", err)
	}
	// The bound must not reject a realistic figure: int64 micros tops out around
	// 9.2e12 USD, so 13 integer digits plus a 2-digit cent fraction is the practical max.
	if _, err := costInUsdToMicros("9223372036854.77"); err != nil {
		t.Errorf("a realistic maximum spend was rejected: %v", err)
	}
}

// TestGetCampaignMetrics_ResponseCapBoundary pins the 10 MiB cap on the analytics path,
// which bypasses doRequest and therefore is NOT covered by the shared path's limit tests.
// Both sides of the boundary matter and for different reasons: a response of exactly
// maxResponseBytes must still be ACCEPTED (an off-by-one that rejected it would fail
// legitimate large reads), and one byte over must be REJECTED rather than decoded. The
// over-cap case is the security-relevant half — the body is read through
// LimitReader(maxResponseBytes+1), so a regression to LimitReader(maxResponseBytes) would
// silently hand json.Unmarshal a TRUNCATED prefix, and a prefix that happens to be valid
// JSON would be reported as real metrics.
func TestGetCampaignMetrics_ResponseCapBoundary(t *testing.T) {
	// Pad inside a string field so the body stays well-formed JSON at any length: the
	// point is the size check, not a decode failure.
	body := func(total int) string {
		prefix := `{"pad":"`
		suffix := `","elements":[{"impressions":10,"clicks":1,"costInUsd":"1.00"}]}`
		padLen := total - len(prefix) - len(suffix)
		if padLen < 0 {
			t.Fatalf("target size %d is smaller than the fixed JSON scaffolding", total)
		}
		return prefix + strings.Repeat("x", padLen) + suffix
	}

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"exactly at the cap is accepted", maxResponseBytes, false},
		{"one byte over the cap is rejected", maxResponseBytes + 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := body(tc.size)
			if len(payload) != tc.size {
				t.Fatalf("fixture built %d bytes, want %d", len(payload), tc.size)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()

			client := NewClient(
				Credentials{AccessToken: "test-token"},
				RuntimeConfig{DefaultAccountID: "account123"},
				WithBaseURL(server.URL),
			)
			metrics, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a %d-byte response was accepted; over-cap bodies must be rejected, not decoded from a truncated prefix", tc.size)
				}
				if !strings.Contains(err.Error(), "exceeds") {
					t.Errorf("error = %v, want it to name the size cap (any other error means the cap check did not fire)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a response of exactly maxResponseBytes must be accepted, got: %v", err)
			}
			if metrics.Impressions != 10 || metrics.Clicks != 1 {
				t.Errorf("metrics = %+v, want the fixture's 10 impressions / 1 click decoded intact", metrics)
			}
		})
	}
}

// TestGetCampaignMetrics_OverCapResponseKeepsConnectionReusable pins the drain on the
// over-cap path. The body is read through LimitReader(maxResponseBytes+1), so detection
// stops the read the moment the response goes over and leaves the remainder on the wire.
// Closing a response body that still has unread bytes makes net/http tear the connection
// down rather than return it to the idle pool, so every later metrics read pays a fresh
// TCP+TLS handshake — a steady cost for as long as the upstream keeps oversizing. Two
// requests against a keep-alive server must therefore land on ONE connection.
func TestGetCampaignMetrics_OverCapResponseKeepsConnectionReusable(t *testing.T) {
	// The excess must be more than ONE byte. The read is LimitReader(maxResponseBytes+1),
	// so a body of exactly maxResponseBytes+1 is consumed in full and nothing is left on
	// the wire — a fixture sized that way passes with or without the drain and proves
	// nothing. overBy is the number of bytes that genuinely remain unread.
	const overBy = 4096
	prefix := `{"pad":"`
	suffix := `","elements":[{"impressions":10,"clicks":1,"costInUsd":"1.00"}]}`
	payload := prefix + strings.Repeat("x", maxResponseBytes+overBy-len(prefix)-len(suffix)) + suffix

	var mu sync.Mutex
	conns := map[string]struct{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = struct{}{}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)
	for i := 0; i < 2; i++ {
		if _, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday); err == nil {
			t.Fatalf("call %d: over-cap response was accepted", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 1 {
		t.Errorf("two over-cap reads used %d connections, want 1 — the unread remainder is preventing keep-alive reuse", len(conns))
	}
}

// linkedinConvServer serves one adAnalytics response and captures the request query, so a
// test can assert both what was ASKED FOR and what was made of the answer.
func linkedinConvServer(t *testing.T, body string) (*Client, func() string) {
	t.Helper()
	var mu sync.Mutex
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	c := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)
	return c, func() string {
		mu.Lock()
		defer mu.Unlock()
		return gotQuery
	}
}

// LinkedIn returns ONLY impressions and clicks when `fields` is omitted, so a metric that is
// not named is absent rather than zero. If this request stops naming
// externalWebsiteConversions, every LinkedIn campaign silently reads as unmeasured.
func TestGetCampaignMetrics_RequestsExternalWebsiteConversions(t *testing.T) {
	c, query := linkedinConvServer(t, `{"elements":[{"impressions":1000,"clicks":50,"costInUsd":"25.50","externalWebsiteConversions":7}]}`)
	if _, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(query(), "externalWebsiteConversions") {
		t.Errorf("request does not name externalWebsiteConversions in `fields`, so LinkedIn "+
			"will never return it; query = %s", query())
	}
}

// The documented field name and type: `long`, per LinkedIn's Ads Reporting metrics schema.
// The fixture mirrors that published shape — a bare JSON integer on the analytics element.
func TestGetCampaignMetrics_DecodesExternalWebsiteConversions(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[{"impressions":1000,"clicks":50,"costInUsd":"25.50","externalWebsiteConversions":7}]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions is nil for a response carrying externalWebsiteConversions:7")
	}
	if *m.Conversions != 7 {
		t.Errorf("Conversions = %v, want 7", *m.Conversions)
	}
}

// The binding absent/zero case for this adapter. An element WITHOUT the key cannot be
// reported as a measured zero: on LinkedIn that shape most often means the metric was never
// returned, and turning it into 0 would flag a converting campaign as dead.
func TestGetCampaignMetrics_AbsentConversionsIsNilNotZero(t *testing.T) {
	absent, _ := linkedinConvServer(t, `{"elements":[{"impressions":1000,"clicks":50,"costInUsd":"25.50"}]}`)
	m, err := absent.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions != nil {
		t.Errorf("Conversions = %v for an element that never carried the field; an "+
			"unreported count became a measurement", *m.Conversions)
	}

	measured, _ := linkedinConvServer(t, `{"elements":[{"impressions":1000,"clicks":50,"costInUsd":"25.50","externalWebsiteConversions":0}]}`)
	m2, err := measured.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m2.Conversions == nil {
		t.Fatal("Conversions is nil for an explicit externalWebsiteConversions:0, erasing a real measurement")
	}
	if *m2.Conversions != 0 {
		t.Errorf("Conversions = %v, want 0", *m2.Conversions)
	}
}

// A campaign with no activity returns an empty elements array. The client always names
// externalWebsiteConversions in `fields`, so LinkedIn WAS asked for the metric and answered
// that the campaign did nothing — an answered zero, not an unmeasured window. The count is
// therefore a non-nil zero, matching googleads' no-rows branch.
//
// This assertion was previously inverted, on the reasoning that an empty array "says nothing
// about conversions having been MEASURED" and that a zero here was one "the rule would act
// on". Both halves were wrong: the metric was requested and the window was answered, and
// no_conversions is gated on Clicks >= minClicksForConversions (50) while this branch reports
// zero clicks, so the rule cannot fire on it either way.
func TestGetCampaignMetrics_NoElementsIsAnAnsweredZero(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions = nil on an empty elements array; LinkedIn answered this window " +
			"and the metric was in the fields list, so the zero is a measurement")
	}
	if *m.Conversions != 0 {
		t.Errorf("Conversions = %v, want 0", *m.Conversions)
	}
}

// Multiple elements aggregate, and only the ones that CARRIED the metric contribute — an
// element missing the key must not act as a zero addend that masks a partial response.
func TestGetCampaignMetrics_ConversionsAggregateAcrossElements(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[
		{"impressions":600,"clicks":30,"externalWebsiteConversions":4},
		{"impressions":400,"clicks":20,"externalWebsiteConversions":3}
	]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions is nil across two elements that both carried the metric")
	}
	if *m.Conversions != 7 {
		t.Errorf("Conversions = %v, want 7 (4+3): elements are not being summed", *m.Conversions)
	}
}

// Above 2^53, float64 can no longer represent every consecutive integer, so an int64
// count that survives a round trip through float64 is not guaranteed to come back
// unchanged. The aggregation must therefore hold its running total in an int64 for the
// WHOLE loop and widen once at the end, the way CostMicros already does — not convert to
// float64 and back on every element.
//
// The fixture is three elements: 2^53, then 1, then 1. The exact total, 2^53+2, IS
// representable as a float64, so a correct implementation reports it exactly and the
// single final widen loses nothing. Only the per-iteration round trip corrupts it: after
// the first element the running total is 2^53, and 2^53+1 is the first integer float64
// cannot represent, so element two rounds back down to 2^53 and element three does the
// same. Both increments are swallowed and the result is 2^53 — short by exactly the two
// conversions the campaign actually recorded.
func TestGetCampaignMetrics_ConversionsAggregateAboveFloat64IntegerPrecision(t *testing.T) {
	// 2^53 = 9007199254740992; the exact total below is 9007199254740994.
	c, _ := linkedinConvServer(t, `{"elements":[
		{"impressions":600,"clicks":30,"externalWebsiteConversions":9007199254740992},
		{"impressions":400,"clicks":20,"externalWebsiteConversions":1},
		{"impressions":200,"clicks":10,"externalWebsiteConversions":1}
	]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions is nil across three elements that all carried the metric")
	}
	const want = float64(1<<53) + 2
	if *m.Conversions != want {
		t.Errorf("Conversions = %.0f, want %.0f: the running total round-tripped through "+
			"float64 mid-loop and lost %.0f conversions above 2^53",
			*m.Conversions, want, want-*m.Conversions)
	}
}

// A negative count is malformed upstream data, not a small number. The sibling counters
// reject it for the same reason and this one must too, or it lands in the response as a
// figure that reads as authoritative.
func TestGetCampaignMetrics_NegativeConversionsIsAnError(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[{"impressions":1000,"clicks":50,"externalWebsiteConversions":-3}]}`)
	if _, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday); err == nil {
		t.Error("a negative externalWebsiteConversions was accepted as a measurement")
	}
}

// A response in which SOME elements carry externalWebsiteConversions and others omit it is
// an INCOMPLETE measurement, and the aggregate must withdraw to nil rather than publish the
// sum of the present ones. Summing only the elements that carried the metric presents a
// PARTIAL count to every consumer as a complete one, with nothing left in the type to say
// otherwise — and the failure mode is not hypothetical. Consider the fixture below: one
// element omits the field and a later one reports an explicit 0. Summing the present
// elements yields exactly 0, and because both elements' clicks still aggregate to 50 — the
// no_conversions rule's minClicksForConversions floor — the rule fires HIGH against a
// campaign whose true conversion count is simply unknown. That is the rule manufacturing
// its own finding rather than measuring one.
//
// This mirrors internal/platform/microsoft/metrics.go, whose convIncomplete flag poisons
// the WHOLE ConversionsQualified total when any single cell is blank, for the same reason
// and against the same rule. LinkedIn was not brought along when that discipline landed.
func TestGetCampaignMetrics_PartialConversionsCoverageIsNilNotAPartialSum(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[
		{"impressions":600,"clicks":30},
		{"impressions":400,"clicks":20,"externalWebsiteConversions":0}
	]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Clicks != 50 {
		t.Fatalf("Clicks = %d, want 50: the fixture must reach minClicksForConversions for "+
			"this test to describe the no_conversions firing it exists to prevent", m.Clicks)
	}
	if m.Conversions != nil {
		t.Errorf("Conversions = %v across elements where one OMITTED the metric and a later "+
			"one reported 0; an incomplete measurement was published as a measured zero, and "+
			"with %d clicks the no_conversions rule now fires HIGH on data LinkedIn never "+
			"fully reported", *m.Conversions, m.Clicks)
	}
}

// The withdrawal must not depend on element ORDER: a present value seen BEFORE the omission
// must be retracted just as a later one is never published. Accumulating into the response
// field directly would leave the first element's total already written when the second is
// found to be missing.
func TestGetCampaignMetrics_ConversionsWithdrawnWhenOmissionFollowsAValue(t *testing.T) {
	c, _ := linkedinConvServer(t, `{"elements":[
		{"impressions":600,"clicks":30,"externalWebsiteConversions":9},
		{"impressions":400,"clicks":20}
	]}`)
	m, err := c.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Conversions != nil {
		t.Errorf("Conversions = %v: a value from the first element survived a later element "+
			"omitting the metric, publishing a partial sum as a complete count", *m.Conversions)
	}
}

// TestGetCampaignMetrics_NoActivityConversionsIsMeasuredZero pins the convention that a
// well-formed empty `elements` array is an ANSWERED zero-activity window, not an unmeasured
// one. The client always names externalWebsiteConversions in `fields`, so LinkedIn was asked
// for the metric and replied that nothing happened.
//
// The assertion is deliberately two-sided: non-nil AND exactly zero. Asserting only
// non-nilness would pass on any garbage value, and asserting only nilness (the previous
// behaviour) is the bug this test exists to prevent regressing to. The fixture returns a real
// empty array over HTTP rather than constructing a struct, so the empty-elements branch is
// genuinely executed.
func TestGetCampaignMetrics_NoActivityConversionsIsMeasuredZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prove the metric really was requested: if the client stopped naming it, a nil
		// Conversions would be the honest answer and this test's premise would be void.
		if !strings.Contains(r.URL.RawQuery, "externalWebsiteConversions") {
			t.Errorf("expected externalWebsiteConversions in the fields list, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"elements": []}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "account123", "123456", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Conversions == nil {
		t.Fatal("expected a non-nil Conversions for an answered zero-activity window, got nil (unmeasured)")
	}
	if *metrics.Conversions != 0 {
		t.Errorf("expected Conversions 0, got %v", *metrics.Conversions)
	}
	// The sibling measurements must agree with it: all four describe the same answered window.
	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero impressions/clicks/cost, got %+v", metrics)
	}
}
