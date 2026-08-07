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
	if metrics.Ctr != 0.05 {
		t.Errorf("expected CTR 0.05, got %f", metrics.Ctr)
	}
	if metrics.CampaignID != "camp_123" {
		t.Errorf("expected CampaignID camp_123, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowLast7Days {
		t.Errorf("expected Window last_7_days, got %q", metrics.Window)
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
	// The whole request body is asserted, not just campaign_ids. The window translation
	// (dateRangeForWindow) and the breakdown/field selection decide WHICH numbers come
	// back, so a wrong date range or a dropped field yields a well-formed response for
	// the wrong period or with a missing metric — nothing downstream can detect that.
	// The client's clock is pinned at 2026-07-01 (fixedRedditClock), so last_7_days is
	// the inclusive 7-day span ending today.
	if got := data["starts_at"]; got != "2026-06-25" {
		t.Errorf("expected starts_at 2026-06-25, got %v", got)
	}
	if got := data["ends_at"]; got != "2026-07-01" {
		t.Errorf("expected ends_at 2026-07-01, got %v", got)
	}
	breakdowns, _ := data["breakdowns"].([]any)
	if len(breakdowns) != 1 || breakdowns[0] != "campaign_id" {
		t.Errorf("expected breakdowns [campaign_id], got %v", data["breakdowns"])
	}
	fields, _ := data["fields"].([]any)
	wantFields := []string{"impressions", "clicks", "spend"}
	if len(fields) != len(wantFields) {
		t.Fatalf("expected fields %v, got %v", wantFields, data["fields"])
	}
	for i, want := range wantFields {
		if fields[i] != want {
			t.Errorf("fields[%d]: expected %q, got %v", i, want, fields[i])
		}
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
	if metrics.CampaignID != "camp_123" {
		t.Errorf("expected CampaignID camp_123, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowToday {
		t.Errorf("expected Window today, got %q", metrics.Window)
	}
}

func TestGetCampaignMetrics_EmptyCampaignID(t *testing.T) {
	var mu sync.Mutex
	var handlerCalled bool
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handlerCalled = true
		mu.Unlock()
	})

	_, err := client.GetCampaignMetrics(context.Background(), "", model.MetricsWindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
	mu.Lock()
	if handlerCalled {
		t.Error("request should not reach the server for an invalid campaign id")
	}
	mu.Unlock()
}

func TestGetCampaignMetrics_InvalidCampaignIDCharacters(t *testing.T) {
	var mu sync.Mutex
	var handlerCalled bool
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handlerCalled = true
		mu.Unlock()
	})

	_, err := client.GetCampaignMetrics(context.Background(), "123/../other", model.MetricsWindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
	mu.Lock()
	if handlerCalled {
		t.Error("request should not reach the server for a path-injection-shaped id")
	}
	mu.Unlock()
}

func TestGetCampaignMetrics_UnsupportedWindow(t *testing.T) {
	var mu sync.Mutex
	var handlerCalled bool
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handlerCalled = true
		mu.Unlock()
	})

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindow("last_90_days"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("expected ErrUnsupportedWindow, got %v", err)
	}
	mu.Lock()
	if handlerCalled {
		t.Error("request should not reach the server for an unsupported window")
	}
	mu.Unlock()
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

func TestGetCampaignMetrics_NullDataFieldIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A null data field should not be treated as zero activity; it is a malformed
		// response, not a valid "no metrics" response. Only an explicit empty array means
		// genuine zero activity.
		_, _ = w.Write([]byte(`{"data":null}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for a null data field")
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

func TestGetCampaignMetrics_NaNSpendIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":"NaN"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for NaN spend value")
	}
}

func TestGetCampaignMetrics_InfinitySpendIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":"Inf"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for Infinity spend value")
	}
}

func TestGetCampaignMetrics_MismatchedRowCampaignIDIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_999","impressions":10,"clicks":1,"spend":"1.00"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error when a row's campaign_id does not match the requested campaign")
	}
}

func TestGetCampaignMetrics_BlankRowCampaignIDIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"","impressions":10,"clicks":1,"spend":"1.00"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error when a row carries a blank campaign_id")
	}
}

func TestGetCampaignMetrics_NegativeSpendIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":"-100.50"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for negative spend value")
	}
}

func TestGetCampaignMetrics_OversizedSpendIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Spend larger than MaxInt64/1_000_000
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":"9223372036854.776"}]}`))
	})

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for out-of-range spend value")
	}
}

// TestGetCampaignMetrics_CounterGuardsAreDecodeErrors covers the four counter branches
// that the spend path already had coverage for: negative impressions, negative clicks,
// and the checked additions that stop a running total from wrapping past MaxInt64.
// Without these, reordering the overflow check relative to the addition would silently
// reintroduce wrapped totals — the same bug class the spend-overflow fix was written for.
func TestGetCampaignMetrics_CounterGuardsAreDecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "negative impressions",
			data: `{"campaign_id":"camp_123","impressions":-1,"clicks":5,"spend":"1.00"}`,
		},
		{
			name: "negative clicks",
			data: `{"campaign_id":"camp_123","impressions":10,"clicks":-1,"spend":"1.00"}`,
		},
		{
			// Two rows whose impressions sum past MaxInt64. One row cannot trip the guard:
			// the running total starts at zero, so the overflow branch is only reachable
			// with a second row, which is why these cases need two.
			name: "impressions total overflows",
			data: `{"campaign_id":"camp_123","impressions":9223372036854775807,"clicks":1,"spend":"1.00"},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":"1.00"}`,
		},
		{
			name: "clicks total overflows",
			data: `{"campaign_id":"camp_123","impressions":1,"clicks":9223372036854775807,"spend":"1.00"},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":"1.00"}`,
		},
		{
			// Two rows whose spend totals (in micros) exceed MaxInt64. Each individual row's
			// spend passes the per-row overflow check (line 139), but their converted micros sum
			// past MaxInt64 and must be caught at the checked addition (line 152-153). Without
			// this case, removing or misordering the cost accumulation guard would silently
			// reintroduce wrapped negative totals.
			name: "cost total overflows",
			data: `{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":"5000000000000"},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":"5000000000000"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[` + tt.data + `]}`))
			})

			if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
				t.Fatalf("expected a decode error for %s", tt.name)
			}
		})
	}
}

// TestGetCampaignMetrics_MultipleRowsAccumulate exercises the decode loop with more than
// one row. Every other test uses a single-element data array, so nothing pinned the
// accumulation itself. Multi-row responses (one row per day in the window) are a plausible
// real shape, and since the report contract here is UNVERIFIED it is one this scaffold may
// well meet first in production.
func TestGetCampaignMetrics_MultipleRowsAccumulate(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"campaign_id":"camp_123","impressions":1000,"clicks":40,"spend":"10.00"},` +
			`{"campaign_id":"camp_123","impressions":600,"clicks":20,"spend":"5.25"},` +
			`{"campaign_id":"camp_123","impressions":400,"clicks":20,"spend":"0.75"}` +
			`]}`))
	})

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 2000 {
		t.Errorf("expected 2000 impressions summed across rows, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 80 {
		t.Errorf("expected 80 clicks summed across rows, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 16_000_000 {
		t.Errorf("expected 16000000 costMicros summed across rows, got %d", metrics.CostMicros)
	}
	// CTR is recomputed from the TOTALS (80/2000), not averaged per row — a per-row mean
	// would give 0.0333, which is what this assertion rules out.
	if metrics.Ctr != 0.04 {
		t.Errorf("expected CTR recomputed from totals (0.04), got %f", metrics.Ctr)
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
