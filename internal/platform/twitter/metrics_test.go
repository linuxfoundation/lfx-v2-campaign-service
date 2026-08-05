// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetCampaignMetricsHappyPath(t *testing.T) {
	// Test the happy path: valid campaign ID, valid window, server returns metrics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request path contains stats
		if !strings.Contains(r.URL.Path, "/stats") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		// Return a mock stats response
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"1000","clicks":"50","spend":"100.00"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

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
	if metrics.CostMicros != 100_000_000 { // 100 USD in micros
		t.Errorf("expected 100000000 costMicros, got %d", metrics.CostMicros)
	}
	if metrics.Ctr != 0.05 { // 50 / 1000
		t.Errorf("expected CTR 0.05, got %f", metrics.Ctr)
	}
	if metrics.Window != WindowLast7Days {
		t.Errorf("expected window %s, got %s", WindowLast7Days, metrics.Window)
	}
}

func TestGetCampaignMetricsDefaultWindow(t *testing.T) {
	// Test that empty window defaults to WindowLast7Days
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"0","clicks":"0","spend":"0"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Window != WindowLast7Days {
		t.Errorf("expected default window %s, got %s", WindowLast7Days, metrics.Window)
	}
}

func TestGetCampaignMetricsZeroActivity(t *testing.T) {
	// Test campaign with no activity (empty response)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 0 {
		t.Errorf("expected 0 impressions for empty response, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 0 {
		t.Errorf("expected 0 clicks for empty response, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 0 {
		t.Errorf("expected 0 costMicros for empty response, got %d", metrics.CostMicros)
	}
	if metrics.Ctr != 0 {
		t.Errorf("expected CTR 0 for empty response, got %f", metrics.Ctr)
	}
}

func TestGetCampaignMetricsZeroImpressionsDivideByZero(t *testing.T) {
	// Test that CTR is 0 when impressions is 0 (no divide-by-zero)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"0","clicks":"5","spend":"10.00"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Ctr != 0 {
		t.Errorf("expected CTR 0 when impressions are 0, got %f", metrics.Ctr)
	}
}

func TestGetCampaignMetricsEmptyFieldsAreZero(t *testing.T) {
	// Test that omitted (empty string) metric fields are treated as zero
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return response with empty metric fields
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"","clicks":"","spend":""}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected all metrics to be 0 for empty fields, got impressions=%d clicks=%d costMicros=%d",
			metrics.Impressions, metrics.Clicks, metrics.CostMicros)
	}
}

func TestGetCampaignMetricsInvalidCampaignID(t *testing.T) {
	// Test that campaign IDs with invalid characters are rejected
	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL("https://api.example.com"),
		WithAPIVersion("12"),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "invalid-id-with-dash", WindowLast7Days)
	if err == nil {
		t.Fatal("expected error for invalid campaign ID, got nil")
	}
	if !strings.Contains(err.Error(), "alphanumeric") {
		t.Errorf("expected error about alphanumeric requirement, got: %v", err)
	}
}

func TestGetCampaignMetricsUnsupportedWindowLast30Days(t *testing.T) {
	// Test that 30-day window returns an error explaining the API limitation
	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL("https://api.example.com"),
		WithAPIVersion("12"),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", MetricsWindow("LAST_30_DAYS"))
	if err == nil {
		t.Fatal("expected error for unsupported 30-day window, got nil")
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("expected error mentioning 7-day API limit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported window") {
		t.Errorf("expected error about unsupported window, got: %v", err)
	}
}

func TestGetCampaignMetricsUnsupportedWindowThisMonth(t *testing.T) {
	// Test that month-based windows return an error explaining the API limitation
	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL("https://api.example.com"),
		WithAPIVersion("12"),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", MetricsWindow("THIS_MONTH"))
	if err == nil {
		t.Fatal("expected error for unsupported THIS_MONTH window, got nil")
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("expected error mentioning 7-day API limit, got: %v", err)
	}
}

func TestGetCampaignMetricsUnsupportedWindowLastMonth(t *testing.T) {
	// Test that LAST_MONTH window returns an error explaining the API limitation
	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL("https://api.example.com"),
		WithAPIVersion("12"),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", MetricsWindow("LAST_MONTH"))
	if err == nil {
		t.Fatal("expected error for unsupported LAST_MONTH window, got nil")
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("expected error mentioning 7-day API limit, got: %v", err)
	}
}

func TestGetCampaignMetricsNonNumericFieldTransportError(t *testing.T) {
	// Test that non-numeric field values cause a transport error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return response with invalid numeric field
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"not-a-number","clicks":"50","spend":"100"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err == nil {
		t.Fatal("expected error for non-numeric impressions field, got nil")
	}
	// Should be a transport error (not a pre-send error)
	errStr := err.Error()
	if !strings.Contains(errStr, "decode campaign metrics row") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

func TestGetCampaignMetricsUpstreamError(t *testing.T) {
	// Test that upstream errors are propagated
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":"internal server error"}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	_, err := client.GetCampaignMetrics(context.Background(), "12345", WindowLast7Days)
	if err == nil {
		t.Fatal("expected error from upstream server, got nil")
	}
}

func TestGetCampaignMetricsSupportedWindowToday(t *testing.T) {
	// Test that TODAY window is supported and works
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"campaign_id":"12345","impressions":"500","clicks":"25","spend":"50.00"}]}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{
			ConsumerKey:       "key",
			ConsumerSecret:    "secret",
			AccessToken:       "token",
			AccessTokenSecret: "token_secret",
		},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(server.URL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)

	metrics, err := client.GetCampaignMetrics(context.Background(), "12345", WindowToday)
	if err != nil {
		t.Fatalf("unexpected error with TODAY window: %v", err)
	}

	if metrics.Window != WindowToday {
		t.Errorf("expected window TODAY, got %s", metrics.Window)
	}
	if metrics.Impressions != 500 {
		t.Errorf("expected 500 impressions, got %d", metrics.Impressions)
	}
}
