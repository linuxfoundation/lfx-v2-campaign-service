// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newMetricsTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", Label: "test"}, WithBaseURL(srv.URL))
}

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth string
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"50","spend":"12.34"}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowLast7Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.CampaignID != "23847290" || m.Window != WindowLast7Days {
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
	mu.Lock()
	path, auth := gotPath, gotAuth
	mu.Unlock()
	if !strings.HasPrefix(path, "/23847290/insights?") || !strings.Contains(path, "date_preset=last_7d") {
		t.Fatalf("unexpected request path: %s", path)
	}
	if auth != "Bearer tok" {
		t.Fatalf("unexpected Authorization header: %s", auth)
	}
}

func TestGetCampaignMetrics_DefaultsWindowWhenEmpty(t *testing.T) {
	var mu sync.Mutex
	var gotQuery string
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", "")
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Window != WindowLast30Days {
		t.Fatalf("window = %q, want default %q", m.Window, WindowLast30Days)
	}
	mu.Lock()
	query := gotQuery
	mu.Unlock()
	if !strings.Contains(query, "date_preset=last_30d") {
		t.Fatalf("unexpected query: %s", query)
	}
}

func TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
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

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
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

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 {
		t.Fatalf("expected zero metrics for omitted fields, got %+v", m)
	}
}

func TestGetCampaignMetrics_RejectsEmptyCampaignID(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for an invalid campaign id")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "   ", WindowToday); err == nil {
		t.Fatal("expected an error for an empty campaign id")
	}
}

func TestGetCampaignMetrics_RejectsNonNumericCampaignID(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for a non-numeric campaign id")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "123/../other", WindowToday); err == nil {
		t.Fatal("expected an error for a non-numeric campaign id")
	}
}

func TestGetCampaignMetrics_RejectsUnsupportedWindow(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for an unsupported window")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "23847290", MetricsWindow("LAST_QUARTER")); err == nil {
		t.Fatal("expected an error for an unsupported window")
	}
}

func TestGetCampaignMetrics_NonNumericMetricFieldIsTransportError(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"not-a-number","clicks":"5","spend":"1.00"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday); err == nil {
		t.Fatal("expected an error for a non-numeric impressions field")
	}
}

func TestGetCampaignMetrics_NonNumericSpendIsTransportError(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"5","clicks":"1","spend":"not-a-number"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday); err == nil {
		t.Fatal("expected an error for a non-numeric spend field")
	}
}

// TestGetCampaignMetrics_SpendAtInt64BoundaryOverflows pins the exact float64 rounding
// edge the overflow guard must catch: math.MaxInt64 is not exactly representable as a
// float64, so float64(math.MaxInt64) rounds UP to 2^63 (one past the real int64 max).
// A spend value whose scaled-to-micros product rounds to exactly 2^63 must still be
// rejected — comparing the UNROUNDED product with '>' lets it slip through, since
// 2^63-0.5 rounds to 2^63 but is not itself '> 2^63'.
func TestGetCampaignMetrics_SpendAtInt64BoundaryOverflows(t *testing.T) {
	// 9223372036854775808.5 / 1e6 scales to (2^63 + 0.5), which rounds to 2^63 on the
	// nearest-even boundary — the exact case an unrounded '>' comparison misses.
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"5","clicks":"1","spend":"9223372036854.775808"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday); err == nil {
		t.Fatal("expected an error for spend that overflows int64 once scaled to micros")
	}
}

func TestGetCampaignMetrics_UpstreamErrorPropagates(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"OAuthException","code":1}}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday); err == nil {
		t.Fatal("expected the upstream error to propagate")
	}
}
