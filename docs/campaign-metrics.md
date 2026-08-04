# Campaign Metrics Ingestion

How the campaign service reads performance numbers (impressions, clicks, spend,
conversions) back from the ad platforms, stores them per day per campaign, and
serves them over the API.

Until this feature, every ad client was **write-only**: the service could create a
campaign and pause/resume it, but could not read back a single performance number.
The Campaigns page could show what was launched, not what it did.

## 1. The attribution problem (read this before summing anything)

Ad platforms **do not agree** on what a conversion is, nor on the window in which
one is counted:

| Platform    | Default conversion attribution                                          |
| ----------- | ----------------------------------------------------------------------- |
| Google Ads  | Per-conversion-action click-through windows (commonly 30 days), plus a configurable engaged-view/view-through window. Conversions are counted at the time of the **click**, not the conversion, and may be **fractional** (data-driven attribution splits credit across touchpoints). |
| Meta        | Default 7-day click / 1-day view.                                        |
| LinkedIn    | Its own click/view window, configured per campaign.                     |

Three consequences drive the design:

1. **A naive cross-platform sum of conversions is wrong in a way that looks
   plausible** — which is worse than showing nothing. The same user converting once
   can be counted by two platforms; Google's fractional credit does not add to
   Meta's whole-number counting.
2. **Spend is safely summable.** Money leaving an account is money leaving an
   account, regardless of attribution model. The only caveat is currency (below).
3. **Impressions and clicks are summable with a caveat** — they are *delivery*
   counts, not attributed outcomes, so they double-count a *person* reached on two
   platforms but they do not double-count an *event*. Summing them is defensible;
   summing conversions is not.

This distinction is encoded **in the model and the API payload**, not merely in a
comment:

- `model.CampaignMetric` stores a single platform's numbers **raw**, exactly as the
  platform reported them, alongside the platform's own JSON response.
- `model.MetricsSummary` carries an explicit `AttributionBasis` and a
  `ConversionsComparable bool`. When a summary spans more than one attribution
  basis, `ConversionsComparable` is **false** and the summed conversions field is
  **omitted entirely** rather than emitted with a footnote nobody reads.
- The API response carries the same fields, so a consumer that ignores the caveat
  physically cannot read a cross-basis conversion total: the field is absent.

### Currency

Spend is stored with the **currency the platform reported it in**. The service does
**no FX conversion** — it has no rate source and a wrong rate is worse than no
rate. A summary over rows of mixed currency sets `CurrencyMixed` and omits the
spend total for the same reason conversions are omitted across bases.

## 2. Schema — `campaign_metrics` (migration 000011)

One row per campaign per day.

| Column        | Type            | Notes                                                    |
| ------------- | --------------- | -------------------------------------------------------- |
| `id`          | UUID PK         | `gen_random_uuid()`                                       |
| `campaign_id` | UUID NOT NULL   | FK → `campaigns(id)` ON DELETE CASCADE                    |
| `metric_date` | DATE NOT NULL   | The platform's own reporting day                          |
| `impressions` | BIGINT          | Delivery count                                            |
| `clicks`      | BIGINT          | Delivery count                                            |
| `spend`       | NUMERIC(18,6)   | **Money — never a float.** 6dp holds micro-denominated spend exactly |
| `conversions` | NUMERIC(18,6)   | Numeric, not integer: Google reports **fractional** conversions under data-driven attribution |
| `currency`    | TEXT            | ISO 4217, the currency `spend` is denominated in          |
| `attribution_basis` | TEXT      | The platform's conversion-counting basis for this row      |
| `raw`         | JSONB           | The platform's own response row, for auditability          |
| `fetched_at`  | TIMESTAMPTZ     | When the service last fetched this row                     |

**`UNIQUE (campaign_id, metric_date)`** makes a re-fetch idempotent: the repo
upserts with `ON CONFLICT (campaign_id, metric_date) DO UPDATE`. This matters
because platforms **restate** recent days — Google Ads finalises conversions days
after the click. The sweeper therefore re-fetches a trailing window and overwrites,
rather than inserting duplicates.

`spend` and `conversions` are `NUMERIC`, never `DOUBLE PRECISION`. Float money
accumulates representation error under summation; `NUMERIC` sums exactly, and pgx
maps it to a `decimal`-safe string rather than a lossy `float64`.

The read path is `WHERE campaign_id = $1 AND metric_date BETWEEN $2 AND $3 ORDER BY
metric_date`, so the UNIQUE index `(campaign_id, metric_date)` already serves it as
a range scan on its leftmost column — no second index is created (a redundant index
costs write throughput for nothing).

## 3. `MetricsFetcher` — the per-platform interface

```go
type MetricsFetcher interface {
    FetchMetrics(ctx context.Context, campaign *model.Campaign, from, to time.Time) ([]model.CampaignMetric, error)
}
```

It sits **alongside** `PlatformDispatcher`, not inside it — mirroring how
`StatusToggler` is an optional capability the orchestrator type-asserts for. A
platform that cannot report yet simply does not satisfy the interface usefully, and
the caller gets a clean typed sentinel rather than a fabricated zero.

### Implemented vs deferred

| Platform    | Status      | Why                                                                    |
| ----------- | ----------- | ---------------------------------------------------------------------- |
| Google Ads  | **Implemented** | The client already has a real, tested GAQL transport: `gaqlSearch` does POST `customers/{id}/googleAds:search` with cursor pagination, retry-on-429, page-token loop protection, and row/byte OOM caps. Metrics are a new GAQL query over that verified primitive — no new API surface is guessed. |
| Meta        | Deferred    | The client has a `doRequest` helper that supports GET, but there is **no committed code or test fixture** describing the `/insights` edge response shape. |
| LinkedIn    | Deferred    | No analytics code and no fixture anywhere in `internal/platform/linkedin`. The `adAnalytics` finder shape at LinkedIn-Version 202602 cannot be verified from this repo. |
| Microsoft   | Deferred    | Reporting is a SOAP Reporting service on a **different endpoint** from the Campaign Management API the client speaks — a submit/poll/download-ZIP flow, not a request/response read. |
| Reddit / X  | Deferred    | No reporting code or fixture.                                          |

The deferral is deliberate and follows the repo's own rule: **do not invent an API
response shape.** A fetcher that decodes a guessed JSON shape does not fail loudly
when the guess is wrong — it decodes to zeros and reports a campaign that spent
money as having spent nothing. Every deferred platform returns
`domain.ErrMetricsUnsupported`, which the sweeper skips quietly and the API maps to
a clear "not supported yet" rather than an empty-but-successful result.

### The Google Ads query

```sql
SELECT segments.date,
       metrics.impressions,
       metrics.clicks,
       metrics.cost_micros,
       metrics.conversions,
       customer.currency_code
FROM campaign
WHERE campaign.id = <id>
  AND segments.date BETWEEN '<from>' AND '<to>'
```

Notes that matter:

- `metrics.cost_micros` is **micros** — 1/1,000,000 of the account currency. It is
  converted with integer-safe decimal arithmetic (`micros / 1e6` computed as a
  scaled `NUMERIC` string), never via `float64`, so a spend of `1234567` micros is
  stored as exactly `1.234567` and not `1.2345669999...`.
- `segments.date` makes each returned row **one day**, which is exactly the table's
  grain — no client-side bucketing, so no timezone re-bucketing bug. The day
  boundary is the **ad account's** timezone, which is the platform's own definition
  and the only one it will report against.
- Per `docs/api-catalog.md` and the client's own note, Google Ads API v23 **rejects**
  `campaign.start_date` / `campaign.end_date`; those were replaced by
  `*_date_time`. The reporting window here uses `segments.date`, which is a separate
  concern and remains valid.
- `conversions` is a `double` in the API and **may be fractional**; it is carried
  through as a decimal string, not rounded to an integer.

## 4. The sweeper

`MetricsSweeper` is modelled directly on `Orchestrator.StartRecoverySweeper`, with
the same lifecycle discipline:

- Its lifetime is owned by a **dedicated `sweeperCtx`**, cancelled via a
  `sync.Once` at the very **start** of `Shutdown` — before any drain — so a sweep
  blocked in the DB or mid-HTTP-call is interrupted promptly and its shutdown never
  competes with another phase's budget.
- The goroutine is tracked by a `sync.WaitGroup` so `Shutdown` waits for it to
  return before the pool closes.
- Each sweep's context derives from `sweeperCtx` (**never** detached), so
  cancellation propagates into the in-flight statement.
- A cancellation-caused error during shutdown is **not** logged as an error.

### Timeout budget

The sweeper must stop within the container's close budget. `ContainerCloseTimeout`
is `dispatchDrainTimeout + service.CancelGracePeriod`, asserted in
`internal/container.init()`. Because the sweeper is cancelled **first** and
`Shutdown` merely waits on the shared `WaitGroup`, its per-sweep timeout is *not* an
additive term of that budget — it is bounded by cancellation, not by expiry. The
per-sweep timeout is nonetheless kept at or below `metricsSweepTimeout`, which is
asserted at init to not exceed `CancelGracePeriod`, so even a sweep that ignores
cancellation cannot outlive the close budget.

### Cadence and window

- `metricsSweepInterval` (default 1h, env `METRICS_SWEEP_INTERVAL`) — hourly is
  well inside every platform's reporting-API rate budget while keeping the
  dashboard's numbers no more than an hour stale.
- `metricsRestatementWindow` (default 3 days, env `METRICS_RESTATEMENT_WINDOW`) —
  each sweep re-fetches the trailing N days, not just yesterday, because platforms
  restate recent conversions. The `(campaign_id, metric_date)` upsert makes the
  overlap free.
- `METRICS_SWEEP_ENABLED` (default false) gates the whole thing, so the feature
  ships dark and is switched on per environment.

All three are declared in `charts/lfx-v2-campaign-service/values.yaml`. **An env var
read by code and absent from the chart is a silently dead feature in every deployed
environment** — that bug has already shipped once on this stack, and a parity test
now guards against it.

## 5. The endpoint

```
GET /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/metrics
```

Follows the sibling endpoints exactly: bearer token, project scoping, and the
standard typed error set (`BadRequest` / `NotFound` / `Conflict` /
`InternalServerError` / `ServiceUnavailable`).

Optional `from` / `to` query parameters (YYYY-MM-DD) bound the window; they default
to the trailing 30 days. `from` after `to` is a 400.

The response is:

- `metrics` — the per-day rows, ascending by date, each with its own platform,
  currency and attribution basis.
- `summary` — the explicitly-derived rollup, carrying `attribution_basis`,
  `conversions_comparable`, and `currency`. `conversions` is **absent** when
  `conversions_comparable` is false; `spend` is **absent** when the rows span
  multiple currencies.

No ETag: unlike the campaign/audience resources, metrics have no `version` column
and no optimistic-concurrency story — they are an append/upsert time series that
changes underneath the caller by design (restatement). The sibling endpoints emit
`etag` because they mirror a row `version`; inventing one here would imply a
concurrency contract this resource does not have.
