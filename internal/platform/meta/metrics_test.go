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
// rejected — a '>' comparison lets it slip through, since the product is not
// itself greater than float64(math.MaxInt64), which is also 2^63.
func TestGetCampaignMetrics_SpendAtInt64BoundaryOverflows(t *testing.T) {
	// This literal scales to exactly 2^63 — one more than MaxInt64, and equal to
	// float64(math.MaxInt64) — the exact case a '>' comparison misses.
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

// TestGetCampaignMetrics_MissingDataFieldIsNotZeroActivity pins the distinction the
// pointer slice exists for. `{"data":[]}` is Meta stating the campaign had no delivery;
// `{}` or `{"data":null}` is a malformed 2xx that states nothing. Both decode to length
// zero, so collapsing them would publish a confident "0 impressions, 0 clicks, $0 spend"
// for a campaign that may be spending — indistinguishable, to every consumer, from a
// measured zero. The counterpart is
// TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue above, which pins that the
// REAL empty case still succeeds; a fix that rejects both fails that one.
func TestGetCampaignMetrics_MissingDataFieldIsNotZeroActivity(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`} {
		t.Run(body, func(t *testing.T) {
			c := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})

			m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
			if err == nil {
				t.Fatalf("expected an error for a 2xx with no data field, got metrics %+v", m)
			}
			if m != nil {
				t.Errorf("expected nil metrics alongside the error, got %+v", m)
			}
			if !strings.Contains(err.Error(), "no data field") {
				t.Errorf("error = %v, want it to name the missing data field", err)
			}
		})
	}
}

// TestGetCampaignMetrics_NegativeCountersAreRejected covers impressions and clicks.
// These are counters: a negative one is malformed upstream data, not a small number.
// Passing it through yields a negative CTR in the public response — a value no consumer
// validates because it cannot legitimately occur. Matches the LinkedIn and Reddit
// readers, which reject the same shape.
func TestGetCampaignMetrics_NegativeCountersAreRejected(t *testing.T) {
	cases := map[string]string{
		"negative impressions": `{"data":[{"impressions":"-5","clicks":"1","spend":"1.00"}]}`,
		"negative clicks":      `{"data":[{"impressions":"5","clicks":"-1","spend":"1.00"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})

			m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
			if err == nil {
				t.Fatalf("expected a negative counter to be rejected, got %+v", m)
			}
			if !strings.Contains(err.Error(), "negative") {
				t.Errorf("error = %v, want it to name the negative counter", err)
			}
		})
	}
}

// TestGetCampaignMetrics_NegativeSpendIsRejected is the spend half. Finite was already
// checked; finite is not enough, since spend is non-negative by definition. A negative
// CostMicros would be absorbed as a credit by every downstream cost-per-click, pacing,
// and roll-up computation rather than rejected.
func TestGetCampaignMetrics_NegativeSpendIsRejected(t *testing.T) {
	c := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"10","clicks":"1","spend":"-3.50"}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
	if err == nil {
		t.Fatalf("expected negative spend to be rejected, got %+v", m)
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %v, want it to name the negative spend", err)
	}
}

// TestGetCampaignMetrics_MalformedValuesAreNotEchoed pins the property that every
// malformed-row error reports the field and the reason but never the VALUE.
//
// These errors travel to BriefService.GetCampaignMetrics's default branch and are
// logged there. safeErrSummary bounds and normalises the text but cannot tell whose
// text it is, so a printable secret sitting in an upstream field would survive it
// unchanged — the only place that can stop it is here, at the point the error is
// built.
//
// Each case names the exact bytes it plants in the offending field and asserts they
// appear nowhere in the error chain. A free-text marker only works for the branches
// reached by an UNPARSEABLE value; the negative and overflow guards run only after
// the value parses, so those cases plant a distinctive NUMERIC literal instead and
// assert on that. Getting this wrong is easy and silent: `"-1SECRETMARKER"` does not
// parse at all, so it lands on the not-an-integer branch and leaves the n < 0 branch
// untested while looking like it covers it.
//
// The cases cover every malformed branch: both counters (unparseable and negative)
// and all four spend guards (unparseable, non-finite, negative, overflow).
func TestGetCampaignMetrics_MalformedValuesAreNotEchoed(t *testing.T) {
	const marker = "SECRETMARKER"

	cases := map[string]struct {
		body   string
		secret string // the exact offending bytes; MUST NOT appear in the error
		want   string // a substring the error MUST still carry, so it stays diagnostic
	}{
		"unparseable impressions": {
			body:   `{"data":[{"impressions":"` + marker + `","clicks":"1","spend":"1.00"}]}`,
			secret: marker,
			want:   "impressions not an integer",
		},
		"negative impressions": {
			// Parses cleanly, so it reaches the n < 0 guard rather than stopping at
			// the syntax check. The digits stand in for the marker.
			body:   `{"data":[{"impressions":"-98765432","clicks":"1","spend":"1.00"}]}`,
			secret: "98765432",
			want:   "impressions negative counter",
		},
		"unparseable clicks": {
			body:   `{"data":[{"impressions":"1","clicks":"` + marker + `","spend":"1.00"}]}`,
			secret: marker,
			want:   "clicks not an integer",
		},
		"negative clicks": {
			body:   `{"data":[{"impressions":"1","clicks":"-87654321","spend":"1.00"}]}`,
			secret: "87654321",
			want:   "clicks negative counter",
		},
		"both counters malformed": {
			body:   `{"data":[{"impressions":"` + marker + `","clicks":"` + marker + `","spend":"1.00"}]}`,
			secret: marker,
			want:   "clicks", // both are named; the clicks half proves the second is not dropped
		},
		"unparseable spend": {
			body:   `{"data":[{"impressions":"1","clicks":"1","spend":"` + marker + `"}]}`,
			secret: marker,
			want:   "spend is not a number",
		},
		"non-finite spend": {
			// ParseFloat accepts the "Infinity" spelling and returns +Inf with no
			// error, so this reaches the IsInf guard. An out-of-range literal like
			// 1e999 would NOT: ParseFloat reports ErrRange for it, which stops one
			// branch earlier at "not a number".
			body:   `{"data":[{"impressions":"1","clicks":"1","spend":"Infinity"}]}`,
			secret: "Infinity",
			want:   "spend is not finite",
		},
		"negative spend": {
			body:   `{"data":[{"impressions":"1","clicks":"1","spend":"-7654.321"}]}`,
			secret: "7654.321",
			want:   "spend is negative",
		},
		"overflowing spend": {
			body:   `{"data":[{"impressions":"1","clicks":"1","spend":"9.87654321e299"}]}`,
			secret: "9.87654321e299",
			want:   "spend overflows int64 micros",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})

			m, err := c.GetCampaignMetrics(context.Background(), "23847290", WindowToday)
			if err == nil {
				t.Fatalf("expected a malformed row to be rejected, got %+v", m)
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error echoed the raw upstream value %q: %v", tc.secret, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q so it stays diagnostic", err, tc.want)
			}
		})
	}
}
