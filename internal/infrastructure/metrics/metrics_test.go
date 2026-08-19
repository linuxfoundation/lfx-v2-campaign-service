// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// scrape renders the registry's current exposition text.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

// TestSafePlatformBoundsCardinality is the cardinality guard: a provider outside
// the closed set must NOT reach a label. This is the failure that takes a
// Prometheus server down, so it is pinned on the mapping function directly and
// again end-to-end through a scrape below.
func TestSafePlatformBoundsCardinality(t *testing.T) {
	for _, known := range []model.Provider{
		model.ProviderGoogleAds, model.ProviderLinkedInAds, model.ProviderMetaAds,
		model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderMicrosoftAds,
		model.ProviderHubSpot,
	} {
		if got := SafePlatform(known); got != string(known) {
			t.Errorf("SafePlatform(%q) = %q, want %q", known, got, known)
		}
	}

	// Values that a config file, a database row, or a crafted payload could
	// produce. Each must collapse to the fixed token.
	hostile := []model.Provider{
		"",
		"not-a-platform",
		model.Provider("campaign-id-01HXYZ"),
		model.Provider("project:" + strings.Repeat("a", 300)),
		"https://ads.example.com/accounts/12345",
	}
	for _, h := range hostile {
		if got := SafePlatform(h); got != PlatformUnknown {
			t.Errorf("SafePlatform(%q) = %q, want %q — unbounded label reached a metric", h, got, PlatformUnknown)
		}
	}
}

// TestSafeJobStatusBoundsCardinality pins the same property for job statuses,
// which are also a bare string type.
func TestSafeJobStatusBoundsCardinality(t *testing.T) {
	for _, known := range []model.JobStatus{
		model.JobQueued, model.JobRunning, model.JobSucceeded, model.JobPartial, model.JobFailed,
	} {
		if got := SafeJobStatus(known); got != string(known) {
			t.Errorf("SafeJobStatus(%q) = %q, want %q", known, got, known)
		}
	}
	for _, h := range []model.JobStatus{"", "job-01HXYZ", "cancelled"} {
		if got := SafeJobStatus(h); got != PlatformUnknown {
			t.Errorf("SafeJobStatus(%q) = %q, want %q", h, got, PlatformUnknown)
		}
	}
}

// TestUnknownPlatformDoesNotReachScrape is the end-to-end cardinality guard: it
// proves the collapse happens on the recording path, not merely in a helper a
// caller could bypass.
func TestUnknownPlatformDoesNotReachScrape(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	secret := "brief-01HXYZSECRET"
	r.RecordDispatch(ctx, model.Provider(secret), OutcomeSuccess)
	r.RecordUpstreamCall(ctx, model.Provider(secret), "read_metrics", CallError, 0.1)

	body := scrape(t, r)
	if strings.Contains(body, secret) {
		t.Errorf("unbounded platform value %q reached the exposition output:\n%s", secret, body)
	}
	if !strings.Contains(body, `platform="`+PlatformUnknown+`"`) {
		t.Errorf("expected the unknown platform to be recorded as %q; got:\n%s", PlatformUnknown, body)
	}
}

// TestOutcomeEnumIsClosed proves an unrecognised outcome cannot mint a series.
func TestOutcomeEnumIsClosed(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	r.RecordDispatch(ctx, model.ProviderGoogleAds, "failed-because-token-abc123-expired")
	body := scrape(t, r)
	if strings.Contains(body, "abc123") {
		t.Errorf("unbounded outcome value reached the exposition output:\n%s", body)
	}
	if !strings.Contains(body, `outcome="`+PlatformUnknown+`"`) {
		t.Errorf("expected an unrecognised outcome to collapse to %q; got:\n%s", PlatformUnknown, body)
	}
}

// TestRecordedMetricsAppearInScrape pins that the instruments are actually wired
// to the served registry — the failure where /metrics returns 200 and reports
// nothing, which is worse than a missing endpoint because a dashboard built on it
// reads zero as a measurement.
func TestRecordedMetricsAppearInScrape(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	r.RecordDispatch(ctx, model.ProviderGoogleAds, OutcomeSuccess)
	r.RecordJobTransition(ctx, model.JobSucceeded)
	r.RecordUpstreamCall(ctx, model.ProviderMetaAds, "toggle_status", CallOK, 0.25)

	body := scrape(t, r)
	for _, want := range []string{
		"campaign_dispatch_total",
		`platform="google-ads"`,
		`outcome="success"`,
		"campaign_job_transitions_total",
		`status="succeeded"`,
		"campaign_upstream_calls_total",
		"campaign_upstream_call_duration_seconds",
		`operation="toggle_status"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output missing %q:\n%s", want, body)
		}
	}
}

// TestPoolGaugesAbsentWithoutPool pins that no pool wired means NOTHING is
// observed, rather than zeroes. A zero is a measurement: reporting "0 max
// connections" for a service running without a database is indistinguishable
// from a pool that has collapsed, and would fire an exhaustion alert.
func TestPoolGaugesAbsentWithoutPool(t *testing.T) {
	r := newTestRegistry(t)

	// No SetPoolStats call at all.
	if body := scrape(t, r); strings.Contains(body, "campaign_db_pool_max_connections") {
		t.Errorf("pool gauges were exported with no pool wired:\n%s", body)
	}

	// Explicitly wired but reporting not-ok (cold start before the pool opens).
	r.SetPoolStats(func() (PoolStats, bool) { return PoolStats{}, false })
	if body := scrape(t, r); strings.Contains(body, "campaign_db_pool_max_connections") {
		t.Errorf("pool gauges were exported while the stats source reported not-ok:\n%s", body)
	}
}

// TestPoolGaugesExportedWhenWired is the positive half of the guard above.
func TestPoolGaugesExportedWhenWired(t *testing.T) {
	r := newTestRegistry(t)
	r.SetPoolStats(func() (PoolStats, bool) {
		return PoolStats{
			AcquiredConns: 3, IdleConns: 5, TotalConns: 8, MaxConns: 10,
			CanceledAcquires: 1, EmptyAcquires: 2,
		}, true
	})

	body := scrape(t, r)
	for _, want := range []string{
		"campaign_db_pool_acquired_connections",
		"campaign_db_pool_idle_connections",
		"campaign_db_pool_total_connections",
		"campaign_db_pool_max_connections",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output missing %q:\n%s", want, body)
		}
	}
}

// TestHandlerServesPrometheusTextFormat pins the content type, since a scraper
// selects its parser from it.
func TestHandlerServesPrometheusTextFormat(t *testing.T) {
	r := newTestRegistry(t)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text exposition format", ct)
	}
}

// TestNilRegistryRecordsAreSafe pins that the record helpers tolerate a nil
// receiver, which is what makes the container's typed-nil injection safe.
func TestNilRegistryRecordsAreSafe(t *testing.T) {
	var r *Registry
	ctx := context.Background()
	r.RecordDispatch(ctx, model.ProviderGoogleAds, OutcomeSuccess)
	r.RecordJobTransition(ctx, model.JobFailed)
	r.RecordUpstreamCall(ctx, model.ProviderGoogleAds, "read_metrics", CallOK, 0.1)
	if err := r.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown on nil registry = %v, want nil", err)
	}
}

// TestUpstreamDurationUsesSecondScaleBuckets pins that the latency histogram is
// exported with SECOND-scale bucket boundaries.
//
// Without the view, the OTel SDK's default explicit boundaries apply. Those are
// chosen for millisecond values, and their first positive bucket is (0,5] -- fed
// seconds, every healthy upstream call (a Google/Meta create is single-digit
// seconds; a Microsoft request is capped at 30s) collapses into that one bucket and
// the p50/p95/p99 the histogram exists to answer cannot separate 50ms from 4s.
//
// The assertion is on the SCRAPE OUTPUT rather than on the boundaries variable, so
// it fails if the view stops applying -- a view whose selector misses does not error,
// it silently restores the defaults.
func TestUpstreamDurationUsesSecondScaleBuckets(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.RecordUpstreamCall(context.Background(), model.ProviderGoogleAds, "create_campaign", "ok", 3.2)
	body := scrape(t, r)

	// Sub-second boundaries are the whole point: they are what the ms-scale default
	// lacks, so their presence proves the view applied.
	for _, want := range []string{
		`le="0.01"`, `le="0.1"`, `le="0.5"`, `le="1"`, `le="2.5"`, `le="5"`, `le="20"`, `le="45"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream duration histogram is missing bucket %s:\n%s", want, body)
		}
	}

	// The SDK default boundaries run to 10000; their presence would mean the view did
	// not apply and the ms-scale defaults are still in force.
	if strings.Contains(body, `le="10000"`) {
		t.Errorf("histogram still carries the SDK default ms-scale buckets:\n%s", body)
	}

	// A 3.2s observation must land at or below the 5s boundary, not in a catch-all.
	if !strings.Contains(body, "campaign_upstream_call_duration_seconds_bucket") {
		t.Errorf("no bucket series exported for the upstream duration histogram:\n%s", body)
	}
}

// TestSafeOperationBoundsShape pins the boundary guard on the `operation` label.
//
// It is a SHAPE guard, not a closed enum: a new compile-time constant must pass
// through untouched (a closed map would silently collapse a newly instrumented call
// to "unknown"), while anything with the shape of a DERIVED string -- an id, a URL,
// whitespace, or unbounded length -- must degrade to the bounded token instead of
// minting one time series per distinct value.
func TestSafeOperationBoundsShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
		want string
	}{
		{"existing constant", "toggle_status", "toggle_status"},
		{"a future constant passes through", "create_audience", "create_audience"},
		{"empty", "", PlatformUnknown},
		{"campaign id", "camp_9f3b21a7c4", PlatformUnknown},
		{"url", "https://ads.example.com/v1/campaigns", PlatformUnknown},
		{"whitespace", "toggle status", PlatformUnknown},
		{"uppercase hex digest", "A1B2C3D4", PlatformUnknown},
		{"digits", "campaign_12345", PlatformUnknown},
		{"over length", strings.Repeat("a", 41), PlatformUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeOperation(tc.op); got != tc.want {
				t.Errorf("safeOperation(%q) = %q, want %q", tc.op, got, tc.want)
			}
		})
	}
}

// TestDerivedOperationDoesNotReachScrape plants an id-shaped operation token and
// asserts it is absent from the exposition output, mirroring
// TestUnknownPlatformDoesNotReachScrape for the operation label.
func TestDerivedOperationDoesNotReachScrape(t *testing.T) {
	r := newTestRegistry(t)
	const secretish = "campaign_9f3b21a7c4e8"
	r.RecordUpstreamCall(context.Background(), model.ProviderGoogleAds, secretish, CallOK, 1.5)

	body := scrape(t, r)
	if strings.Contains(body, secretish) {
		t.Errorf("a derived operation token reached the scrape output:\n%s", body)
	}
	if !strings.Contains(body, PlatformUnknown) {
		t.Errorf("the derived operation did not degrade to %q:\n%s", PlatformUnknown, body)
	}
}

// TestPoolNewConnectionsExported pins that PoolStats.NewConnsCount reaches the
// scrape. The field was previously collected from the pool but exported by no
// instrument, so the struct implied a series /metrics did not actually serve.
func TestPoolNewConnectionsExported(t *testing.T) {
	r := newTestRegistry(t)
	r.SetPoolStats(func() (PoolStats, bool) {
		return PoolStats{
			AcquiredConns: 3, IdleConns: 5, TotalConns: 8, MaxConns: 10,
			NewConnsCount: 42, CanceledAcquires: 1, EmptyAcquires: 2,
		}, true
	})

	body := scrape(t, r)
	if !strings.Contains(body, "campaign_db_pool_new_connections_total") {
		t.Errorf("scrape output missing the new-connections counter:\n%s", body)
	}
	if !strings.Contains(body, "42") {
		t.Errorf("the new-connections value was not exported:\n%s", body)
	}
}
