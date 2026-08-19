# 2026-08-18 — LFXV2-3221 Prometheus /metrics endpoint and service metrics

**Creation** — `GET /metrics` serves the Prometheus text exposition format, with
instruments chosen for what this service actually is: a broker that creates paid ad
campaigns on other people's platforms.

**Why a dedicated MeterProvider rather than the global one.** The repo already wires an
OpenTelemetry `MeterProvider` in `pkg/utils/otel.go`, so recording into
`otel.GetMeterProvider()` looked like the obvious reuse. It is a trap:
`OTEL_METRICS_EXPORTER` defaults to `none`, so the global provider is a **no-op** in the
default deployment, and instruments registered against it would make `/metrics` return
`200` with nothing in it. That is worse than the endpoint not existing — the scrape
succeeds, the target reports healthy, and a dashboard built on it reads zero as a
measurement rather than as absence. `internal/infrastructure/metrics` therefore owns its
own provider backed by the Prometheus exporter. The OTLP pipeline is untouched and
independent; enabling it later changes nothing about what this endpoint serves.

The exporter is pinned at `v0.66.0` deliberately: it is the release whose `go.mod`
requires `go.opentelemetry.io/otel v1.44.0` **exactly**, the version this repo already
pins, so adding metrics forced no OTel SDK upgrade. `go mod tidy` was verified to leave
the OTel core at v1.44.0 rather than merely being edited to it.

**What is instrumented, and why these.** `campaign_dispatch_total` (platform, outcome)
answers whether campaigns are landing on each platform; `campaign_job_transitions_total`
(status) makes a stuck-job alert expressible as the gap between `running` and the
terminal states; `campaign_upstream_calls_total` and
`campaign_upstream_call_duration_seconds` (platform, operation, outcome) cover the ad
platforms this service brokers through; and the `campaign_db_pool_*` series cover pool
health. All of it was instrumented where the data ALREADY sits — the orchestrator's
`run` loop and its five upstream call sites, plus `Container.setPool`. No code was
restructured to create a metric.

A recovered dispatcher panic gets its **own** outcome rather than folding into `failure`.
A panic is a bug in this service; an upstream platform refusing a campaign is not, and
collapsing them hides the former in the noise of the latter.

Upstream calls are timed only AFTER the pre-platform guards pass. A "no dispatcher
registered" refusal returns in nanoseconds and never touches the network, so including
those would drag every latency quantile toward zero and make the histogram describe
local bookkeeping instead of platform behaviour.

**Cardinality is the thing that had to be got right.** An unbounded label mints one
retained time series per distinct value — the classic Prometheus outage. `model.Provider`
and `model.JobStatus` are both bare `string` types, so a value can arrive from a config
file, a database row or a future constant without this package being updated. Both are
mapped through closed sets that collapse anything unrecognised to `unknown`, and the
outcome labels are closed enums guarded the same way. Mutation-verified: making
`SafePlatform` return the raw value fails two tests, including an end-to-end one that
scrapes the registry and asserts a planted id string does not appear in the output.

`operation` is the one label bounded by review rather than by a map, and that is
recorded rather than hidden: the vocabulary is per-platform, so a closed map would
silently collapse a newly instrumented call to `unknown`. Every caller passes a
compile-time constant.

Outcome labels derive from an error's PRESENCE only, never its value — upstream errors
embed account ids and response fragments. No metric name, label or help string carries a
credential, DSN or token.

**Absent pool metrics rather than zeroes.** When no pool is wired — no-database mode, or
a cold start before the pool opens — the `campaign_db_pool_*` series are not exported at
all. Exporting zeroes would repeat the failure-as-measurement shape this bundle already
warns about: `max_connections=0` for a service deliberately running without a database is
indistinguishable from a pool that has collapsed, and would fire a false exhaustion
alert. Mutation-verified: exporting zeroes on the not-ok path fails
`TestPoolGaugesAbsentWithoutPool`.

**Excluded from the OpenAPI by construction, not by annotation.** `/livez` and `/readyz`
are Goa methods kept out of the published spec by `Meta("swagger:generate", "false")`.
`/metrics` is instead mounted directly on the muxer and never declared in `design/`, so
it cannot be published even if someone forgets the annotation. `gen/` is untouched and
`make apigen` was not needed. The test asserts the OUTCOME (absent from every generated
document) rather than the mechanism, so it keeps holding if `/metrics` is ever moved into
the design with the annotation instead.

That test needed a correction worth recording: the first version searched for the
substring `/metrics:` and failed immediately, because the service legitimately publishes
`…/campaigns/{id}/metrics` and `…/briefs/{id}/metrics`. A substring search can never fail
for the reason the test exists, so it now matches the path as a whole KEY, and asserts a
genuinely published path IS detected so the absence checks cannot pass vacuously.

**Unauthenticated, and off the gateway.** `/metrics` is served without credentials
because the scraper has none — the same reason the health probes are. It is deliberately
NOT added to the chart's `HTTPRoute` or Heimdall `RuleSet`: like `/livez` and `/readyz`
it is scraped on the pod IP, in-cluster only, and adding it to either would expose
operational internals through the public gateway. The chart parity test passes unchanged,
which is the evidence that no route/rule drift was introduced.

**Chart.** The commented-out `prometheus.io/*` example in `values.yaml` is now a real
default. `prometheus.io/port` is rendered from `service.port` in the TEMPLATE rather than
hardcoded in values, so the scrape port cannot drift from the port the container listens
on — verified by rendering with `--set service.port=9090` and seeing the annotation
follow. No new port and no new Service: the endpoint shares the API port, which is what
the original commented example assumed.

**No new configuration.** There is no environment variable to enable `/metrics` and no
metrics port setting, so the README's env-var contract gained no rows — the README says
so explicitly rather than leaving a reader to wonder which knob turns it on.

**Dependency note, flagged rather than buried.** Adding the Prometheus exporter pulls
`prometheus/common`, which requires `golang-jwt/jwt/v5 >= 5.3.0`, so MVS raises this
repo's pin from `v5.2.2` on the library that verifies Heimdall bearer tokens. Pinning it
back was attempted and REVERTED: `go mod tidy` re-raises it, so the pin would silently
disappear the moment anyone ran tidy, which is worse than the honest bump. The auth suite
passes on the new version. This is a real change to an auth-surface dependency and is
called out for a reviewer rather than left in the lockfile diff.

**Tests** — Sixteen new cases across three files. Twelve mutations were run, each made to
COMPILE (a build break proves nothing), and every one failed a test: unbounded platform,
unbounded job status, unbounded outcome, pool zeroes on the not-ok path, `OK` checked
before `Skipped` in the outcome mapping, panic folded into failure, both job-transition
records dropped, the dispatch record dropped, the route unmounted, the route mounted with
an empty body regardless of registry, and a `/metrics` path planted in a generated spec.

**Scope — what was deliberately not done.** No RED-method HTTP request metrics
(rate/errors/duration per route): the `path` label is the single easiest way to blow up
cardinality on a service whose routes carry `{projectId}`, `{briefId}` and `{campaignId}`,
and doing it safely needs the Goa route PATTERN rather than the request path, which is a
larger change than this ticket. No metrics on the audience-build or indexer paths, no
alerting rules, and no Grafana dashboard.
