// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	// Mock server that returns ad analytics metrics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/adAnalytics" {
			http.Error(w, fmt.Sprintf("unexpected path: %s", r.URL.Path), http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("q") != "analytics" {
			http.Error(w, "missing q parameter", http.StatusBadRequest)
			return
		}

		// Return a successful analytics response
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
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
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "urn:li:sponsoredCampaign:123", model.MetricsWindowToday)
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
	if metrics.CTR != expectedCTR {
		t.Errorf("expected CTR %f, got %f", expectedCTR, metrics.CTR)
	}
}

func TestGetCampaignMetrics_NoActivity(t *testing.T) {
	// Mock server returning empty analytics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"elements": []}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "urn:li:sponsoredCampaign:123", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
}

func TestGetCampaignMetrics_ZeroImpressions(t *testing.T) {
	// Zero impressions should result in zero CTR (no divide-by-zero)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"elements": [{"impressions": 0, "clicks": 0, "costInUsd": 0}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "urn:li:sponsoredCampaign:123", model.MetricsWindowThisMonth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.CTR != 0 {
		t.Errorf("expected CTR 0 when impressions are 0, got %f", metrics.CTR)
	}
}

func TestGetCampaignMetrics_MissingCostUSD(t *testing.T) {
	// Cost can be omitted (nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"elements": [{"impressions": 100, "clicks": 10}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "account123"},
		WithBaseURL(server.URL),
	)

	ctx := context.Background()
	metrics, err := client.GetCampaignMetrics(ctx, "account123", "urn:li:sponsoredCampaign:123", model.MetricsWindowLastMonth)
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
	_, err := client.GetCampaignMetrics(ctx, "", "urn:li:sponsoredCampaign:123", model.MetricsWindowToday)
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
	_, err := client.GetCampaignMetrics(ctx, "account123", "urn:li:sponsoredCampaign:123", model.MetricsWindow("invalid_window"))
	if err == nil {
		t.Fatal("expected error for unsupported window, got nil")
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

	// Should be 00:00 to 23:59:59.999999999 of Jan 15, 2025 UTC
	expectedStart := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 1, 15, 23, 59, 59, 999999999, time.UTC)

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
	expectedEnd := time.Date(2025, 1, 15, 23, 59, 59, 999999999, time.UTC)

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
	expectedEnd := time.Date(2025, 1, 15, 23, 59, 59, 999999999, time.UTC)

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
	expectedEnd := time.Date(2024, 12, 31, 23, 59, 59, 999999999, time.UTC)

	if !start.Equal(expectedStart) {
		t.Errorf("start: expected %v, got %v", expectedStart, start)
	}
	if !end.Equal(expectedEnd) {
		t.Errorf("end: expected %v, got %v", expectedEnd, end)
	}
}
