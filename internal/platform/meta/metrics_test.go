// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMetricsTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", Label: "test"}, WithBaseURL(srv.URL))
}

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var gotPath, gotAuth string
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"50","spend":"12.34"}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowLast7Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.CampaignID != "camp_123" || m.Window != WindowLast7Days {
		t.Fatalf("unexpected campaign/window: %+v", m)
	}
	if m.Impressions != 1000 || m.Clicks != 50 {
		t.Fatalf("unexpected impressions/clicks: %+v", m)
	}
	if m.CostMicros != 12_340_000 {
		t.Fatalf("costMicros = %d, want 12340000", m.CostMicros)
	}
	wantCtr := 50.0 / 1000.0
	if m.Ctr != wantCtr {
		t.Fatalf("ctr = %v, want %v", m.Ctr, wantCtr)
	}
	if !strings.HasPrefix(gotPath, "/camp_123/insights?") || !strings.Contains(gotPath, "date_preset=last_7d") {
		t.Fatalf("unexpected request path: %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("unexpected Authorization header: %s", gotAuth)
	}
}

func TestGetCampaignMetrics_DefaultsWindowWhenEmpty(t *testing.T) {
	var gotQuery string
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "camp_123", "")
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Window != WindowLast30Days {
		t.Fatalf("window = %q, want default %q", m.Window, WindowLast30Days)
	}
	if !strings.Contains(gotQuery, "date_preset=last_30d") {
		t.Fatalf("unexpected query: %s", gotQuery)
	}
}

func TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 || m.Ctr != 0 {
		t.Fatalf("expected zero-value metrics, got %+v", m)
	}
}

func TestGetCampaignMetrics_ZeroImpressionsAvoidsDivideByZero(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"0","clicks":"0","spend":"0"}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Ctr != 0 {
		t.Fatalf("ctr = %v, want 0", m.Ctr)
	}
}

func TestGetCampaignMetrics_OmittedMetricsAreZero(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 {
		t.Fatalf("expected zero metrics for omitted fields, got %+v", m)
	}
}

func TestGetCampaignMetrics_RejectsEmptyCampaignID(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called for an invalid campaign id")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "   ", WindowToday); err == nil {
		t.Fatal("expected an error for an empty campaign id")
	}
}

func TestGetCampaignMetrics_RejectsUnsupportedWindow(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called for an unsupported window")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "camp_123", MetricsWindow("LAST_QUARTER")); err == nil {
		t.Fatal("expected an error for an unsupported window")
	}
}

func TestGetCampaignMetrics_NonNumericMetricFieldIsTransportError(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"not-a-number","clicks":"5","spend":"1.00"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday); err == nil {
		t.Fatal("expected an error for a non-numeric impressions field")
	}
}

func TestGetCampaignMetrics_NonNumericSpendIsTransportError(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"5","clicks":"1","spend":"not-a-number"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday); err == nil {
		t.Fatal("expected an error for a non-numeric spend field")
	}
}

func TestGetCampaignMetrics_UpstreamErrorPropagates(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"OAuthException","code":1}}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "camp_123", WindowToday); err == nil {
		t.Fatal("expected the upstream error to propagate")
	}
}
