// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func testClient(serverURL string) *Client {
	return NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(serverURL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)
}

func TestGetCampaignMetricsHappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"12345","id_data":[{"metrics":{"impressions":[1000],"clicks":[50],"billed_charge_local_micro":[100000000]}}]}]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.CampaignID != "12345" {
		t.Errorf("expected campaignID 12345, got %s", metrics.CampaignID)
	}
	if metrics.Impressions != 1000 {
		t.Errorf("expected 1000 impressions, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 50 {
		t.Errorf("expected 50 clicks, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 100_000_000 {
		t.Errorf("expected 100000000 costMicros, got %d", metrics.CostMicros)
	}
	if metrics.Ctr != 0.05 { // 50 / 1000
		t.Errorf("expected CTR 0.05, got %f", metrics.Ctr)
	}

	// This is the regression test for the "wrong X Ads stats URL path" finding:
	// stats is NOT nested under /accounts/{id} the way every other endpoint is —
	// it's /stats/accounts/{id}.
	mu.Lock()
	path, query := gotPath, gotQuery
	mu.Unlock()
	if !strings.HasSuffix(path, "/stats/accounts/account123") {
		t.Errorf("expected path ending in /stats/accounts/account123, got %s", path)
	}
	if strings.Contains(path, "/accounts/account123/stats") {
		t.Errorf("path used the wrong (accounts-then-stats) shape: %s", path)
	}

	decodedQuery, err := url.QueryUnescape(query)
	if err != nil {
		t.Fatalf("unescape query: %v", err)
	}
	if !strings.Contains(decodedQuery, "granularity=TOTAL") {
		t.Errorf("expected granularity=TOTAL in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "metric_groups=ENGAGEMENT,BILLING") {
		t.Errorf("expected metric_groups=ENGAGEMENT,BILLING in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "placement=ALL_ON_TWITTER") {
		t.Errorf("expected placement=ALL_ON_TWITTER in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "entity=CAMPAIGN") {
		t.Errorf("expected entity=CAMPAIGN in query, got %s", decodedQuery)
	}
	if !strings.Contains(decodedQuery, "entity_ids=12345") {
		t.Errorf("expected entity_ids=12345 in query, got %s", decodedQuery)
	}
}

func TestGetCampaignMetrics_NoActivity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
}

func TestGetCampaignMetrics_MissingMetricIsZeroNotError(t *testing.T) {
	// X omits a metric field entirely (not a zero) when there's no activity for
	// it — e.g. clicks/billed_charge_local_micro absent with impressions present
	// is a real "no clicks yet", not a malformed response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"12345","id_data":[{"metrics":{"impressions":[100]}}]}]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 100 {
		t.Errorf("expected impressions 100, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected clicks/cost 0 when omitted, got clicks=%d cost=%d", metrics.Clicks, metrics.CostMicros)
	}
}

func TestGetCampaignMetrics_EmptyCampaignID(t *testing.T) {
	client := testClient("http://example.invalid")

	_, err := client.GetCampaignMetrics(context.Background(), "", WindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
}

func TestGetCampaignMetrics_InvalidCampaignIDCharacters(t *testing.T) {
	client := testClient("http://example.invalid")

	// Path-injection-shaped id must be rejected before ever reaching doRequestAbs.
	_, err := client.GetCampaignMetrics(context.Background(), "123/../other", WindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
}

func TestGetCampaignMetrics_UnsupportedWindow(t *testing.T) {
	client := testClient("http://example.invalid")

	// This is the regression test for the "not a typed error" finding: callers
	// must be able to discriminate an unsupported-window rejection from an
	// upstream/transport failure via errors.Is, not string-matching.
	_, err := client.GetCampaignMetrics(context.Background(), "12345", MetricsWindow("LAST_30_DAYS"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("expected ErrUnsupportedWindow, got %v", err)
	}
}

func TestGetCampaignMetrics_DefaultsWindowWhenEmpty(t *testing.T) {
	var mu sync.Mutex
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Window != WindowLast7Days {
		t.Errorf("expected default window LAST_7_DAYS, got %s", metrics.Window)
	}

	mu.Lock()
	query := gotQuery
	mu.Unlock()
	decoded, _ := url.QueryUnescape(query)
	if !strings.Contains(decoded, "start_time=") || !strings.Contains(decoded, "end_time=") {
		t.Errorf("expected start_time/end_time in query, got %s", decoded)
	}
}

func TestDateRangeForWindow_Today(t *testing.T) {
	fixed := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	start, end := dateRangeForWindow(WindowToday, fixed)
	if start != "2025-01-15" || end != "2025-01-15" {
		t.Errorf("expected 2025-01-15/2025-01-15, got %s/%s", start, end)
	}
}

func TestDateRangeForWindow_Last7Days(t *testing.T) {
	fixed := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	start, end := dateRangeForWindow(WindowLast7Days, fixed)
	if start != "2025-01-09" || end != "2025-01-15" {
		t.Errorf("expected 2025-01-09/2025-01-15, got %s/%s", start, end)
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
		_, _ = fmt.Fprint(w, `{"data":[{"id":"12345","id_data":[{"metrics":{"impressions":[10],"clicks":[1],"billed_charge_local_micro":[500000]}}]}]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
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

func TestGetCampaignMetrics_MalformedResponseIsDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `not json`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	if _, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday); err == nil {
		t.Fatal("expected a decode error for a malformed response")
	}
}

func TestGetCampaignMetrics_EndTimeIsHourAligned(t *testing.T) {
	// Regression test: end_time must be hour-aligned (whole hour boundary).
	// X Ads API rejects non-aligned timestamps like T23:59:59Z, so end_time
	// should use an exclusive next-midnight bound (e.g., T00:00:00Z of the next day).
	var mu sync.Mutex
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)

	// Use WindowToday so we can predict the date: if "now" is 2025-01-15,
	// start should be 2025-01-15T00:00:00Z, end should be 2025-01-16T00:00:00Z (next day, exclusive).
	fixedNow := time.Date(2025, 1, 15, 14, 30, 0, 0, time.UTC)
	client.timeFn = func() time.Time { return fixedNow }

	_, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	query := gotQuery
	mu.Unlock()
	decoded, _ := url.QueryUnescape(query)

	// Extract start_time and end_time from the query.
	if !strings.Contains(decoded, "start_time=2025-01-15T00:00:00Z") {
		t.Errorf("expected start_time=2025-01-15T00:00:00Z in query, got %s", decoded)
	}
	// The end_time should be the NEXT day at T00:00:00Z (exclusive end), not the same day at T23:59:59Z.
	if !strings.Contains(decoded, "end_time=2025-01-16T00:00:00Z") {
		t.Errorf("expected end_time=2025-01-16T00:00:00Z (next day, hour-aligned, exclusive), got %s", decoded)
	}
	if strings.Contains(decoded, "T23:59:59Z") {
		t.Errorf("end_time should NOT be non-hour-aligned with T23:59:59Z, got %s", decoded)
	}
}

// TestGetCampaignMetrics_Last7DaysQueryParams pins the whole dateRangeForWindow →
// query-string chain for LAST_7_DAYS against a fixed clock. The existing coverage
// checks each link in isolation — TestDateRangeForWindow_Last7Days does the date math
// without an HTTP request, TestGetCampaignMetricsHappyPath checks the non-date params,
// and TestGetCampaignMetrics_EndTimeIsHourAligned only covers TODAY — so a regression
// in the LAST_7_DAYS wiring specifically could pass every one of them.
func TestGetCampaignMetrics_Last7DaysQueryParams(t *testing.T) {
	var mu sync.Mutex
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	client := testClient(server.URL)
	// now = 2025-01-15; LAST_7_DAYS covers the 7 calendar days INCLUDING today
	// (2025-01-09 through 2025-01-15), so end_time is the exclusive next-midnight
	// bound 2025-01-16 — a 7-day span, not 8.
	client.timeFn = func() time.Time { return time.Date(2025, 1, 15, 14, 30, 0, 0, time.UTC) }

	if _, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	query := gotQuery
	mu.Unlock()
	decoded, _ := url.QueryUnescape(query)

	if !strings.Contains(decoded, "start_time=2025-01-09T00:00:00Z") {
		t.Errorf("expected start_time=2025-01-09T00:00:00Z in query, got %s", decoded)
	}
	if !strings.Contains(decoded, "end_time=2025-01-16T00:00:00Z") {
		t.Errorf("expected end_time=2025-01-16T00:00:00Z (exclusive next-midnight bound), got %s", decoded)
	}
}

func TestGetCampaignMetrics_InvalidAccountID(t *testing.T) {
	// Regression test: account ID must be validated before interpolating into URL.
	// Other Twitter client paths guard the stored account ID with accountIDRe to
	// prevent path injection. GetCampaignMetrics must do the same.
	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account/../etc"}, // path-injection attempt
		WithBaseURL("http://example.invalid"),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
	if err == nil {
		t.Fatal("expected an error for invalid (path-injection-shaped) account ID")
	}
	// The error should be a validation error (context error), not a network error.
	if strings.Contains(err.Error(), "account") || strings.Contains(err.Error(), "credentials") {
		// Good — it's caught as a credential/config error, not a network issue.
	} else {
		t.Errorf("expected a validation-style error message, got %v", err)
	}
}
