// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package metrics exposes the service's Prometheus metrics and the instruments
// the rest of the service records into.
//
// It owns a DEDICATED MeterProvider backed by the OpenTelemetry Prometheus
// exporter, rather than recording into the global provider that pkg/utils wires
// for OTLP. The two are deliberately separate: OTEL_METRICS_EXPORTER defaults to
// "none" (see pkg/utils/otel.go), which leaves the global provider a no-op, so
// instruments registered against it would make /metrics serve an empty page in
// the default deployment — the scrape would succeed and report nothing, which is
// worse than not existing because a dashboard built on it reads zero as a
// measurement. Owning the provider here makes /metrics self-contained: it works
// with no OTLP collector configured, and enabling OTLP later changes nothing
// about what this endpoint serves.
//
// # Label cardinality
//
// Prometheus cardinality is the classic way to take down a monitoring system: a
// label whose value space is unbounded creates one time series per distinct
// value, and the series are retained for the whole retention window. NOTHING in
// this package may carry a campaign id, brief id, project id, job id, account id
// or URL as a label value. `platform` and the job-status label map through closed
// sets (SafePlatform / SafeJobStatus) that collapse anything outside them to
// "unknown", and the dispatch and call outcomes are small closed enums declared as
// constants below. `operation` is bounded by SHAPE instead of a closed set — see
// safeOperation for why a map would be the wrong tool there.
//
// No metric name, label or help string may carry a credential, DSN or token.
// Nothing here is derived from request payloads or stored connection state.
package metrics

import (
	"context"
	"net/http"
	"sync"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// PlatformUnknown is the label value substituted for any provider outside the
// closed set in internal/domain/model. model.Provider is a string type, so a
// value can be constructed from a config file, a database row or a future
// provider constant without this package being updated. Passing such a value
// through verbatim would let a malformed or attacker-influenced provider mint
// unbounded time series, so SafePlatform maps it to this fixed token.
const PlatformUnknown = "unknown"

// Dispatch outcome values. A CLOSED enum: dispatch results collapse into these
// four buckets so `outcome` can never carry an upstream error string, which is
// unbounded and can embed ids or response fragments.
const (
	// OutcomeSuccess — the campaign was created (or reused) upstream.
	OutcomeSuccess = "success"
	// OutcomeSkipped — dispatch was deliberately not attempted for this platform.
	OutcomeSkipped = "skipped"
	// OutcomeFailure — dispatch was attempted and did not succeed.
	OutcomeFailure = "failure"
	// OutcomePanic — a dispatcher panicked and was recovered.
	OutcomePanic = "panic"
)

// Upstream call outcome values. A CLOSED enum, for the same reason as the
// dispatch outcomes above.
const (
	// CallOK — the upstream platform call returned successfully.
	CallOK = "ok"
	// CallError — the upstream platform call failed.
	CallError = "error"
)

// safePlatforms is the closed set of provider values allowed as a label. It is
// keyed on model.Provider so adding a provider constant without adding it here
// yields "unknown" (a visible, bounded degradation) rather than an unbounded
// label.
var safePlatforms = map[model.Provider]string{
	model.ProviderGoogleAds:    string(model.ProviderGoogleAds),
	model.ProviderLinkedInAds:  string(model.ProviderLinkedInAds),
	model.ProviderMetaAds:      string(model.ProviderMetaAds),
	model.ProviderRedditAds:    string(model.ProviderRedditAds),
	model.ProviderTwitterAds:   string(model.ProviderTwitterAds),
	model.ProviderMicrosoftAds: string(model.ProviderMicrosoftAds),
	model.ProviderHubSpot:      string(model.ProviderHubSpot),
}

// SafePlatform renders a provider as a bounded label value, collapsing anything
// outside the closed set to PlatformUnknown.
func SafePlatform(p model.Provider) string {
	if v, ok := safePlatforms[p]; ok {
		return v
	}
	return PlatformUnknown
}

// safeJobStatuses is the closed set of job statuses allowed as a label, for the
// same reason as safePlatforms: model.JobStatus is a string type.
var safeJobStatuses = map[model.JobStatus]string{
	model.JobQueued:    string(model.JobQueued),
	model.JobRunning:   string(model.JobRunning),
	model.JobSucceeded: string(model.JobSucceeded),
	model.JobPartial:   string(model.JobPartial),
	model.JobFailed:    string(model.JobFailed),
}

// SafeJobStatus renders a job status as a bounded label value, collapsing
// anything outside the closed set to PlatformUnknown.
func SafeJobStatus(s model.JobStatus) string {
	if v, ok := safeJobStatuses[s]; ok {
		return v
	}
	return PlatformUnknown
}

// safeOutcomes and safeCallOutcomes bound the outcome labels the same way. A
// caller passing an unrecognised outcome is a programming error, but it must
// degrade to a fixed token rather than mint a series.
var safeOutcomes = map[string]struct{}{
	OutcomeSuccess: {}, OutcomeSkipped: {}, OutcomeFailure: {}, OutcomePanic: {},
}

var safeCallOutcomes = map[string]struct{}{
	CallOK: {}, CallError: {},
}

func safeOutcome(o string) string {
	if _, ok := safeOutcomes[o]; ok {
		return o
	}
	return PlatformUnknown
}

func safeCallOutcome(o string) string {
	if _, ok := safeCallOutcomes[o]; ok {
		return o
	}
	return PlatformUnknown
}

// PoolStats is the subset of pgxpool.Stat the pool gauges report. It is an
// interface-free struct so internal/infrastructure/metrics does not import the
// postgres package (which would be a dependency cycle once the pool reports
// into it).
type PoolStats struct {
	AcquiredConns    int64
	IdleConns        int64
	TotalConns       int64
	MaxConns         int64
	NewConnsCount    int64
	CanceledAcquires int64
	EmptyAcquires    int64
}

// PoolStatsFunc reports the current pool statistics, or ok=false when no pool is
// wired (the no-database mode, or during a cold start before the pool opens).
// Returning ok=false rather than a zero PoolStats is deliberate: a zero is a
// measurement, and reporting "0 connections, 0 max" for a service that has no
// pool at all is indistinguishable from a pool that has collapsed.
type PoolStatsFunc func() (PoolStats, bool)

// Registry owns the Prometheus registry, the MeterProvider and the instruments.
type Registry struct {
	registry *prometheus.Registry
	provider *sdkmetric.MeterProvider

	dispatchTotal    metric.Int64Counter
	jobTransitions   metric.Int64Counter
	upstreamCalls    metric.Int64Counter
	upstreamDuration metric.Float64Histogram

	// mu guards poolStats, which is swapped in after construction: the container
	// opens the pool on a background goroutine during a cold start, while the
	// observable callbacks registered below may already be reading it.
	mu        sync.RWMutex
	poolStats PoolStatsFunc
}

// upstreamDurationBoundaries are the explicit bucket boundaries, in SECONDS, for
// campaign_upstream_call_duration_seconds.
//
// They are set from the call budgets this service actually enforces, NOT from the
// OTel SDK default. The default boundaries ({0, 5, 10, 25, 50, 75, 100, 250, ...})
// are chosen for millisecond-valued observations; fed seconds, their first positive
// bucket is (0,5], so every healthy upstream call — a Google/Meta create is single-
// digit seconds, a Microsoft request is capped at 30s — lands in one bucket and the
// p50/p95/p99 this histogram exists to answer cannot tell 50ms from 4s apart.
//
// The ladder spans 10ms (a fast cached read) to 45s, the largest ceiling any
// instrumented call can reach (toggleCallTimeout). The 20s and 45s boundaries are
// deliberately ON the read and toggle ceilings so a bucket edge coincides with
// "this call timed out", and 30s sits on the Microsoft per-request ceiling.
var upstreamDurationBoundaries = []float64{
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 45,
}

// upstreamDurationView pins the bucket boundaries above onto the upstream-latency
// histogram. It selects by instrument name so the other instruments (counters and
// the pool observables) keep their defaults.
func upstreamDurationView() sdkmetric.Option {
	return sdkmetric.WithView(sdkmetric.NewView(
		sdkmetric.Instrument{
			Name: upstreamDurationInstrument,
			Kind: sdkmetric.InstrumentKindHistogram,
		},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: upstreamDurationBoundaries,
				NoMinMax:   false,
			},
		},
	))
}

// upstreamDurationInstrument is the instrument name, shared by the registration
// below and the view that sets its buckets. They MUST agree: a view whose selector
// misses simply does not apply, silently restoring the ms-scale defaults.
const upstreamDurationInstrument = "campaign_upstream_call_duration_seconds"

// New builds a Registry with its own Prometheus registry and MeterProvider.
//
// The registry is created empty rather than using prometheus.DefaultRegisterer:
// the default registerer carries the process and Go collectors as a global
// side effect and is shared with any library that registers into it, so a
// duplicate registration elsewhere would panic at init. Owning the registry
// makes what /metrics serves an explicit list. The Go and process collectors are
// added deliberately below — they are bounded and are what answers "is this pod
// leaking goroutines or file descriptors".
func New() (*Registry, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	exp, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp), upstreamDurationView())
	meter := provider.Meter("github.com/linuxfoundation/lfx-v2-campaign-service")

	r := &Registry{registry: reg, provider: provider}

	if r.dispatchTotal, err = meter.Int64Counter(
		"campaign_dispatch_total",
		metric.WithDescription("Campaign dispatch attempts by platform and outcome."),
	); err != nil {
		return nil, err
	}

	if r.jobTransitions, err = meter.Int64Counter(
		"campaign_job_transitions_total",
		metric.WithDescription("Campaign job state transitions by resulting status."),
	); err != nil {
		return nil, err
	}

	if r.upstreamCalls, err = meter.Int64Counter(
		"campaign_upstream_calls_total",
		metric.WithDescription("Upstream ad-platform API calls by platform, operation and outcome."),
	); err != nil {
		return nil, err
	}

	if r.upstreamDuration, err = meter.Float64Histogram(
		upstreamDurationInstrument,
		metric.WithDescription("Upstream ad-platform API call latency by platform and operation."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, err
	}

	if err := r.registerPoolGauges(meter); err != nil {
		return nil, err
	}

	return r, nil
}

// SetPoolStats wires (or clears) the database pool statistics source. The
// container calls this once the pool opens, which during a cold start is after
// the server is already serving /metrics.
func (r *Registry) SetPoolStats(f PoolStatsFunc) {
	r.mu.Lock()
	r.poolStats = f
	r.mu.Unlock()
}

func (r *Registry) currentPoolStats() PoolStatsFunc {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.poolStats
}

// registerPoolGauges registers the DB pool observables. They are asynchronous
// (observed at scrape time) because the pool's own counters are the source of
// truth — mirroring them into synchronous instruments would require hooking
// every acquire and release.
func (r *Registry) registerPoolGauges(meter metric.Meter) error {
	acquired, err := meter.Int64ObservableGauge(
		"campaign_db_pool_acquired_connections",
		metric.WithDescription("Database connections currently acquired from the pool."),
	)
	if err != nil {
		return err
	}
	idle, err := meter.Int64ObservableGauge(
		"campaign_db_pool_idle_connections",
		metric.WithDescription("Idle database connections in the pool."),
	)
	if err != nil {
		return err
	}
	total, err := meter.Int64ObservableGauge(
		"campaign_db_pool_total_connections",
		metric.WithDescription("Total database connections currently in the pool."),
	)
	if err != nil {
		return err
	}
	maxConns, err := meter.Int64ObservableGauge(
		"campaign_db_pool_max_connections",
		metric.WithDescription("Configured maximum size of the database pool."),
	)
	if err != nil {
		return err
	}
	canceled, err := meter.Int64ObservableCounter(
		"campaign_db_pool_canceled_acquires_total",
		metric.WithDescription("Pool acquires canceled by a context before a connection was obtained."),
	)
	if err != nil {
		return err
	}
	empty, err := meter.Int64ObservableCounter(
		"campaign_db_pool_empty_acquires_total",
		metric.WithDescription("Pool acquires that had to wait because no connection was idle."),
	)
	if err != nil {
		return err
	}
	// Exported rather than dropped: PoolStats.NewConnsCount is already collected from
	// the pool, and connection-establishment rate is what separates "the pool is busy"
	// from "the pool is churning" -- a steady climb here against a flat total means
	// connections are being opened and discarded rather than reused.
	newConns, err := meter.Int64ObservableCounter(
		"campaign_db_pool_new_connections_total",
		metric.WithDescription("Database connections established by the pool since start."),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		f := r.currentPoolStats()
		if f == nil {
			return nil
		}
		s, ok := f()
		if !ok {
			// No pool wired. Observe NOTHING rather than zeroes: a zero here is
			// indistinguishable from a pool that has collapsed to no connections,
			// and an alert on pool exhaustion would fire against a service that
			// simply runs without a database.
			return nil
		}
		o.ObserveInt64(acquired, s.AcquiredConns)
		o.ObserveInt64(idle, s.IdleConns)
		o.ObserveInt64(total, s.TotalConns)
		o.ObserveInt64(maxConns, s.MaxConns)
		o.ObserveInt64(canceled, s.CanceledAcquires)
		o.ObserveInt64(empty, s.EmptyAcquires)
		o.ObserveInt64(newConns, s.NewConnsCount)
		return nil
	}, acquired, idle, total, maxConns, canceled, empty, newConns)
	return err
}

// RecordDispatch records one campaign dispatch attempt.
func (r *Registry) RecordDispatch(ctx context.Context, platform model.Provider, outcome string) {
	if r == nil || r.dispatchTotal == nil {
		return
	}
	r.dispatchTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("platform", SafePlatform(platform)),
		attribute.String("outcome", safeOutcome(outcome)),
	))
}

// RecordJobTransition records a campaign job reaching a state.
func (r *Registry) RecordJobTransition(ctx context.Context, status model.JobStatus) {
	if r == nil || r.jobTransitions == nil {
		return
	}
	r.jobTransitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("status", SafeJobStatus(status)),
	))
}

// safeOperation is a SHAPE guard on the operation label, not a closed enum.
//
// A closed map is deliberately avoided: the operation vocabulary is per-platform
// and grows as call sites are instrumented, so a map here would silently collapse
// a newly instrumented call to "unknown" -- the failure mode is invisible, because
// the metric keeps being served and simply stops distinguishing the new operation.
//
// What this DOES catch is the boundary case the constants cannot: a future caller
// passing a DERIVED string. Every legitimate token is a short lower-snake literal,
// so anything carrying an id, a URL, whitespace or arbitrary length is rejected to
// the bounded PlatformUnknown rather than minting a series. This bounds the damage
// of a mistake without penalising a correct new constant.
func safeOperation(op string) string {
	const maxOperationLen = 40
	if op == "" || len(op) > maxOperationLen {
		return PlatformUnknown
	}
	for _, r := range op {
		if (r < 'a' || r > 'z') && r != '_' {
			return PlatformUnknown
		}
	}
	return op
}

// RecordUpstreamCall records one upstream ad-platform API call and its latency.
//
// operation is a caller-supplied token and MUST be a compile-time constant (the
// callers pass literals such as "create_campaign"), never anything derived from
// a request, a URL or an upstream response. It passes through safeOperation, a
// shape guard rather than a closed enum -- see that function for why the
// vocabulary is deliberately open-ended.
func (r *Registry) RecordUpstreamCall(ctx context.Context, platform model.Provider, operation, outcome string, seconds float64) {
	if r == nil || r.upstreamCalls == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("platform", SafePlatform(platform)),
		attribute.String("operation", safeOperation(operation)),
		attribute.String("outcome", safeCallOutcome(outcome)),
	)
	r.upstreamCalls.Add(ctx, 1, attrs)
	if r.upstreamDuration != nil {
		r.upstreamDuration.Record(ctx, seconds, attrs)
	}
}

// Handler serves the Prometheus text exposition format.
//
// It is UNAUTHENTICATED by design, exactly as /livez and /readyz are: the
// kubelet and the Prometheus scraper have no bearer token. Like the health
// probes, the path is NOT published in the Helm HTTPRoute or Heimdall RuleSet,
// so it is reachable only from inside the cluster and never through the public
// gateway. Nothing it serves is request-scoped or credential-derived.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

// Shutdown flushes and stops the MeterProvider.
func (r *Registry) Shutdown(ctx context.Context) error {
	if r == nil || r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}
