// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000"}}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.CampaignID != "555" || m.Window != WindowLast30Days {
		t.Fatalf("got %+v", m)
	}
	if m.Impressions != 1000 || m.Clicks != 40 || m.CostMicros != 25_000_000 {
		t.Errorf("got %+v", m)
	}
	if want := 0.04; m.Ctr != want {
		t.Errorf("Ctr = %v, want %v", m.Ctr, want)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "campaign.id = 555") || !strings.Contains(body, "DURING LAST_30_DAYS") {
		t.Errorf("query body = %s", body)
	}
}

func TestGetCampaignMetrics_MultipleRowsIsAnErrorNotAPartialSum(t *testing.T) {
	// A segmenting query returns one row per segment. Reading rows[0] alone would
	// report 1000 impressions out of 3000 — a 3x UNDERREPORT that is indistinguishable
	// from a genuinely quiet campaign, which is exactly why this must fail loudly.
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+
			`{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000"}},`+
			`{"campaign":{"id":"555"},"metrics":{"impressions":"2000","clicks":"60","costMicros":"50000000"}}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err == nil {
		t.Fatalf("multiple rows must be an error, not a silent first-row read; got %+v", m)
	}
	if m != nil {
		t.Errorf("metrics must be nil on error, got %+v", m)
	}
	// The diagnostic has to name the count, or the operator cannot tell this apart
	// from any other upstream failure.
	if !strings.Contains(err.Error(), "got 2") {
		t.Errorf("error must report the row count it saw, got %q", err)
	}
}

func TestGetCampaignMetrics_DefaultsWindowWhenEmpty(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", "")
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Window != WindowLast30Days {
		t.Errorf("Window = %q, want default %q", m.Window, WindowLast30Days)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "DURING LAST_30_DAYS") {
		t.Errorf("query body = %s", body)
	}
}

func TestGetCampaignMetrics_NoActivityInWindowReturnsZeroValue(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 || m.Ctr != 0 {
		t.Errorf("expected zero-value metrics for no-activity window, got %+v", m)
	}
	if m.CampaignID != "555" {
		t.Errorf("CampaignID = %q, want 555", m.CampaignID)
	}
	// The assertion the other four counters do not make for us: Conversions is a POINTER,
	// so a zero-value struct satisfies "conversions == 0" by being nil, and nil means
	// "this platform cannot measure conversions" — the opposite of what an empty result
	// set means for Google. Assert the pointer is non-nil AND that it holds zero.
	if m.Conversions == nil {
		t.Fatal("Conversions = nil for a no-activity window; Google measured this window and " +
			"got zero, and nil is reserved for platforms that cannot report conversions at all")
	}
	if *m.Conversions != 0 {
		t.Errorf("*Conversions = %v, want 0", *m.Conversions)
	}
}

func TestGetCampaignMetrics_ZeroImpressionsAvoidsDivideByZero(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"0","clicks":"0","costMicros":"0"}}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Ctr != 0 {
		t.Errorf("Ctr = %v, want 0", m.Ctr)
	}
}

func TestGetCampaignMetrics_RejectsNonNumericCampaignID(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be sent for an invalid campaign id")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "555; DROP", WindowLast30Days); err == nil {
		t.Fatal("expected an error for a non-numeric campaign id, got nil")
	}
}

func TestGetCampaignMetrics_RejectsUnsupportedWindow(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be sent for an unsupported window")
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "555", "ALL_TIME"); err == nil {
		t.Fatal("expected an error for an unsupported window, got nil")
	}
}

func TestGetCampaignMetrics_MalformedResponseIsTransportError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":"not-an-object"}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days); err == nil {
		t.Fatal("expected an error for a malformed metrics row, got nil")
	} else if _, ok := err.(*transportError); !ok {
		t.Errorf("error type = %T, want *transportError: %v", err, err)
	}
}

func TestGetCampaignMetrics_NonNumericMetricFieldIsTransportError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"not-a-number","clicks":"40","costMicros":"25000000"}}]}`)
	})
	_, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err == nil {
		t.Fatal("expected an error for a non-numeric metrics field, got nil")
	}
	if _, ok := err.(*transportError); !ok {
		t.Errorf("error type = %T, want *transportError: %v", err, err)
	}
	// The offending value must NOT reach the error text. It comes verbatim from the upstream
	// body and the service renders this error into a warning log, so echoing it is a log
	// injection vector. The FIELD name must be there — that is what a responder needs.
	if strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error echoes the raw upstream value, which reaches the log stream: %v", err)
	}
	if !strings.Contains(err.Error(), "impressions") {
		t.Errorf("error does not name the field that failed to parse: %v", err)
	}
}

// TestWindowFor_CoversEveryModelWindow pins the whole public-window-to-GAQL contract. Every
// branch is a distinct literal that is concatenated into the query's DURING clause, so a
// wrong one compiles, passes the injection allow-list, and silently reports the WRONG
// REPORTING PERIOD — a failure no other test can catch, because the only visible symptom is
// plausible numbers for the wrong dates. The table is built from the model constants, so a
// window added to the platform-agnostic vocabulary without a mapping here fails on the
// unmapped-input case below rather than falling through to a default range.
func TestWindowFor_CoversEveryModelWindow(t *testing.T) {
	cases := []struct {
		in   model.MetricsWindow
		want MetricsWindow
	}{
		{model.MetricsWindowToday, "TODAY"},
		{model.MetricsWindowYesterday, "YESTERDAY"},
		{model.MetricsWindowLast7Days, "LAST_7_DAYS"},
		{model.MetricsWindowLast14Days, "LAST_14_DAYS"},
		{model.MetricsWindowLast30Days, "LAST_30_DAYS"},
		{model.MetricsWindowThisMonth, "THIS_MONTH"},
		{model.MetricsWindowLastMonth, "LAST_MONTH"},
	}
	// Literals are spelled out above rather than referenced as WindowToday etc. on purpose:
	// comparing a constant to itself would pass no matter what either one says. These are the
	// exact strings GAQL defines.
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			got, err := WindowFor(tc.in)
			if err != nil {
				t.Fatalf("WindowFor(%q) returned an error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("WindowFor(%q) = %q, want the GAQL literal %q — this queries the wrong reporting period", tc.in, got, tc.want)
			}
			if _, ok := validMetricsWindows[got]; !ok {
				t.Errorf("WindowFor(%q) produced %q, which the injection allow-list rejects; the translation and the guard have diverged", tc.in, got)
			}
		})
	}
	if len(cases) != len(validMetricsWindows) {
		t.Errorf("the table covers %d windows but the client allows %d; a window was added without a translation case", len(cases), len(validMetricsWindows))
	}
}

// TestWindowFor_UnmappedWindowIsAnError pins the other half: a model window with no mapping
// must fail loudly rather than fall through to a default range, which would report numbers
// for a period the caller never asked for.
func TestWindowFor_UnmappedWindowIsAnError(t *testing.T) {
	got, err := WindowFor(model.MetricsWindow("last_quarter"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("error = %v, want ErrUnsupportedWindow", err)
	}
	if got != "" {
		t.Errorf("returned window = %q, want empty on error", got)
	}
}

func TestGetCampaignMetrics_UpstreamErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(srv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(srv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	if _, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days); err == nil {
		t.Fatal("expected an error on a 5xx metrics response, got nil")
	}
}

func TestGetCampaignMetrics_OmittedMetricsAreZero(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Google Ads omits zero-valued metrics from REST JSON, leaving empty strings
		// after unmarshaling. This test verifies they are treated as zeros, not errors.
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"","clicks":"","costMicros":""}}]}`)
	})

	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.CostMicros != 0 || m.Ctr != 0 {
		t.Errorf("expected zero-value metrics for omitted fields, got %+v", m)
	}
}

// Conversions must be REQUESTED, or Google returns a row without it and every campaign
// reads as unmeasured. Pins the GAQL SELECT rather than the response handling.
func TestGetCampaignMetrics_RequestsConversions(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000","conversions":3}}]}`)
	})
	if _, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "metrics.conversions") {
		t.Errorf("GAQL query does not select metrics.conversions, so no campaign can ever "+
			"report one; query body = %s", body)
	}
}

// metrics.conversions is declared DOUBLE upstream and is serialized as a BARE JSON NUMBER,
// unlike the int64 metrics Google encodes as strings. The fixture uses that documented
// encoding: decoding it into a string field would fail on every converting campaign.
func TestGetCampaignMetrics_ConversionsDecodedFromJSONNumber(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000","conversions":12}}]}`)
	})
	m, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions is nil for a response that carried conversions:12")
	}
	if *m.Conversions != 12 {
		t.Errorf("Conversions = %v, want 12", *m.Conversions)
	}
}

// Google credits FRACTIONAL conversions under data-driven and position-based attribution,
// which is why the field is a double. The fraction is carried through UNCHANGED: rounding
// 0.4 to 0 would report a converting campaign as having produced nothing, and the
// no_conversions rule reads exactly this number to decide whether to raise a finding.
func TestGetCampaignMetrics_FractionalConversionsArePreserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want float64
	}{
		{"a fraction below one is not rounded to zero", "0.4", 0.4},
		{"a fraction below one is not rounded up either", "0.8", 0.8},
		{"a fraction above one keeps its remainder", "2.4", 2.4},
		{"an exact half is not rounded", "2.5", 2.5},
		{"an exact zero stays zero", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","conversions":`+tc.raw+`}}]}`)
			})
			m, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
			if err != nil {
				t.Fatalf("GetCampaignMetrics: %v", err)
			}
			if m.Conversions == nil {
				t.Fatalf("Conversions is nil for conversions:%s", tc.raw)
			}
			if *m.Conversions != tc.want {
				t.Errorf("conversions:%s became %v, want %v: rounding a fractional conversion "+
					"reports a converting campaign as having produced none",
					tc.raw, *m.Conversions, tc.want)
			}
		})
	}
}

// An ABSENT conversions member on a Google row is a measured ZERO, not "unmeasured".
//
// metrics.conversions is always in this method's SELECT list, and Google Ads REST encodes
// responses as proto3 JSON, which omits fields holding the default value — the same reason
// parseMetricInt already treats an omitted impressions/clicks as a measured 0. Leaving it nil
// meant no_conversions could never fire for a Google campaign that genuinely converted
// nobody, which is the rule's entire purpose. nil is reserved for the four platforms that
// cannot report a campaign-level conversion count at all.
func TestGetCampaignMetrics_AbsentConversionsIsMeasuredZero(t *testing.T) {
	absent := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40"}}]}`)
	})
	m, err := absent.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m.Conversions == nil {
		t.Fatal("Conversions is nil for a selected-but-omitted field: proto3 JSON omits a " +
			"zero, so this is a measured 0 and no_conversions must be able to fire on it")
	}
	if *m.Conversions != 0 {
		t.Errorf("Conversions = %v, want 0", *m.Conversions)
	}

	measured := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","conversions":0}}]}`)
	})
	m2, err := measured.GetCampaignMetrics(context.Background(), "555", WindowLast30Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if m2.Conversions == nil {
		t.Fatal("Conversions is nil for an explicit conversions:0, erasing a real measurement")
	}
	if *m2.Conversions != 0 {
		t.Errorf("Conversions = %v, want 0", *m2.Conversions)
	}
}

// A negative or non-finite conversion count is upstream corruption, not a small number.
// Passing it through would put a figure the dashboard renders as a measurement into the
// response — the same guard this file already applies to the other counters.
func TestGetCampaignMetrics_MalformedConversionsIsAnError(t *testing.T) {
	for _, raw := range []string{"-1", "1e400"} {
		c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"555"},"metrics":{"impressions":"1000","clicks":"40","conversions":`+raw+`}}]}`)
		})
		if _, err := c.GetCampaignMetrics(context.Background(), "555", WindowLast30Days); err == nil {
			t.Errorf("conversions:%s was accepted; a malformed count became a measurement", raw)
		}
	}
}
