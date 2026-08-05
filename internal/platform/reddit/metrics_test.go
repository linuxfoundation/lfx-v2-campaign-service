// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func newMetricsTestClient(t *testing.T, apiHandler http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(apiHandler)
	t.Cleanup(apiSrv.Close)

	return NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
	)
}

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotPath string
	var gotBody map[string]any

	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":"25.50"}]}`))
	})

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 1000 {
		t.Errorf("expected 1000 impressions, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 50 {
		t.Errorf("expected 50 clicks, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 25_500_000 {
		t.Errorf("expected 25500000 costMicros, got %d", metrics.CostMicros)
	}
	if metrics.CTR != 0.05 {
		t.Errorf("expected CTR 0.05, got %f", metrics.CTR)
	}

	mu.Lock()
	method, path, body := gotMethod, gotPath, gotBody
	mu.Unlock()
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if path != "/api/v3/ad_accounts/t2_test/reports" {
		t.Errorf("expected path /api/v3/ad_accounts/t2_test/reports, got %s", path)
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatal("expected a data envelope in the request body")
	}
	ids, _ := data["campaign_ids"].([]any)
	if len(ids) != 1 || ids[0] != "camp_123" {
		t.Errorf("expected campaign_ids [camp_123], got %v", data["campaign_ids"])
	}
}

func TestGetCampaignMetrics_NoActivity(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
}

func TestGetCampaignMetrics_EmptyCampaignID(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request should not reach the server for an invalid campaign id")
	})

	_, err := client.GetCampaignMetrics(context.Background(), "", model.MetricsWindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
}

func TestGetCampaignMetrics_InvalidCampaignIDCharacters(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request should not reach the server for a path-injection-shaped id")
	})

	_, err := client.GetCampaignMetrics(context.Background(), "123/../other", model.MetricsWindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
}

func TestGetCampaignMetrics_UnsupportedWindow(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request should not reach the server for an unsupported window")
	})

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindow("last_90_days"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("expected ErrUnsupportedWindow, got %v", err)
	}
}

func TestGetCampaignMetrics_MalformedResponseIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for a malformed response")
	}
}

func TestGetCampaignMetrics_InvalidSpendIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":"not-a-number"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for an unparseable spend value")
	}
}

func TestDateRangeForWindow_Today(t *testing.T) {
	fixed := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	start, end, err := dateRangeForWindow(model.MetricsWindowToday, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-03-15" || end != "2026-03-15" {
		t.Errorf("expected 2026-03-15/2026-03-15, got %s/%s", start, end)
	}
}

func TestDateRangeForWindow_Last7Days(t *testing.T) {
	fixed := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	start, end, err := dateRangeForWindow(model.MetricsWindowLast7Days, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-03-09" || end != "2026-03-15" {
		t.Errorf("expected 2026-03-09/2026-03-15, got %s/%s", start, end)
	}
}

func TestDateRangeForWindow_Last30Days(t *testing.T) {
	fixed := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	start, end, err := dateRangeForWindow(model.MetricsWindowLast30Days, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-02-14" || end != "2026-03-15" {
		t.Errorf("expected 2026-02-14/2026-03-15, got %s/%s", start, end)
	}
}

func TestDateRangeForWindow_ThisMonth(t *testing.T) {
	fixed := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	start, end, err := dateRangeForWindow(model.MetricsWindowThisMonth, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-03-01" || end != "2026-03-15" {
		t.Errorf("expected 2026-03-01/2026-03-15, got %s/%s", start, end)
	}
}

// TestDateRangeForWindow_LastMonth_MonthEndBoundary pins the "today is the last day of
// the month" edge case, mirroring the LinkedIn client's March-31 regression test: last
// month must be the FULL prior calendar month, not a naive 30-day-back subtraction.
func TestDateRangeForWindow_LastMonth_MonthEndBoundary(t *testing.T) {
	fixed := time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)
	start, end, err := dateRangeForWindow(model.MetricsWindowLastMonth, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-02-01" || end != "2026-02-28" {
		t.Errorf("expected 2026-02-01/2026-02-28, got %s/%s", start, end)
	}
}

func TestGetCampaignMetrics_MissingAccountID(t *testing.T) {
	client := NewClient(testCreds, AccountConfig{AccountID: "", Label: "no account"}, WithNowFunc(fixedRedditClock()))

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error when the account id is not configured")
	}
}
