---
type: "Go Package"
title: "internal/infrastructure/metrics"
description: "Prometheus registry and instruments served at the unauthenticated /metrics endpoint: dispatch outcomes, job transitions, upstream platform latency and DB pool health, with every label bounded to a closed set."
resource: "internal/infrastructure/metrics"
---

# internal/infrastructure/metrics

Owns the Prometheus registry served at `GET /metrics`, plus the instruments the
orchestrator records into.

## A dedicated MeterProvider, not the global one

The package builds its **own** `sdkmetric.MeterProvider` backed by the
OpenTelemetry Prometheus exporter, rather than recording into the global provider
`pkg/utils` wires for OTLP. `OTEL_METRICS_EXPORTER` defaults to `none`
(`pkg/utils/otel.go`), which leaves the global provider a no-op — so instruments
registered against it would make `/metrics` serve an empty page in the default
deployment. That is worse than the endpoint not existing: the scrape succeeds, and
a dashboard built on it reads zero as a measurement rather than as absence.

Owning the provider makes `/metrics` self-contained. It works with no OTLP
collector configured, and enabling OTLP later changes nothing about what this
endpoint serves. The two pipelines are independent by design.

The Prometheus registry is also created empty rather than reusing
`prometheus.DefaultRegisterer`, which carries global collectors as a side effect
and is shared with any library that registers into it — a duplicate registration
elsewhere would panic at init. The Go and process collectors are then added
deliberately, so what `/metrics` serves is an explicit list.

## Label cardinality is the load-bearing constraint

An unbounded label creates one time series per distinct value, retained for the
whole retention window — the classic way to take a Prometheus server down.
**Nothing here may carry a campaign id, brief id, project id, job id, account id
or URL as a label value.**

`model.Provider` and `model.JobStatus` are both bare `string` types, so a value
can be constructed from a config file, a database row or a future constant without
this package being updated. `SafePlatform` and `SafeJobStatus` map through closed
sets and collapse anything else to `unknown`; the dispatch and call outcomes are
closed enums guarded the same way. A provider constant added to the domain without
being added here therefore degrades to a visible `unknown` rather than minting an
unbounded label.

`operation` on the upstream instruments is the one label bounded by review rather
than by a map: the vocabulary is per-platform, and a closed map would silently
collapse a newly instrumented call to `unknown`. Every caller passes a
compile-time constant.

No metric name, label or help string carries a credential, DSN or token, and the
outcome labels are derived from an error's PRESENCE only — never its value, which
can embed account ids and response fragments.

## Absent pool metrics rather than zeroes

`PoolStatsFunc` returns an `ok` flag, and when it reports false the callback
observes **nothing**. Exporting zeroes instead would be a lie in the shape the
repo already guards against elsewhere: `max_connections=0` for a service running
without a database is indistinguishable from a pool that has collapsed, and would
fire a false exhaustion alert. The gauges are asynchronous because the pool's own
counters are the source of truth; mirroring them into synchronous instruments
would mean hooking every acquire and release.

`Container.setPool` is the single chokepoint that wires the stats source, so both
the live fast path and the cold-start retry report pool statistics — a per-call-site
wiring would let the retry path recover and silently report nothing.

## The endpoint

`Handler()` serves the text exposition format. It is mounted directly on the Goa
muxer in `cmd/campaign-service/server.go` rather than declared in `design/`, so it
cannot be published in the OpenAPI documents even if someone forgets the
`Meta("swagger:generate", "false")` annotation that excludes `/livez` and
`/readyz`. It is unauthenticated for the same reason those are — the scraper
carries no bearer token — and, like them, is absent from the chart's `HTTPRoute`
and Heimdall `RuleSet`, so it is reachable only in-cluster.

See [internal/infrastructure/metrics](../../../internal/infrastructure/metrics).
