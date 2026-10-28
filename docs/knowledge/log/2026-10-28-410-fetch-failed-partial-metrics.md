# 2026-10-28 — fetch_failed: a metrics failure can leave partial delivery, not only zero

**Fix** — three documentation-accuracy findings accepted on #402 and deferred to #410. No
behaviour changes and no code path moves.

1. **A metrics failure does not always leave zeroes.** `internal/platform/googleads/monitor.go`
   expects one aggregated GAQL row per campaign (`segments.date` is filtered but not selected),
   but it does not rely on that: it sums any repeated rows for a campaign. If one of several rows
   has an `impressions`, `clicks` or `cost_micros` value that fails `parseMetricInt`, the
   campaign is marked `FetchFailed` and that row is skipped, but the rows that did parse keep
   their `+=` contribution. So a flagged Google row can carry real but incomplete delivery. If
   the campaign's only row fails, impressions, clicks and spend stay zero and `Conversions` stays
   nil (absent, not a measured zero). The `fetch_failed` and
   `unmeasured_campaigns` design attributes (regenerated through `make apigen` into `gen/**` and
   the kodata OpenAPI copies), `model.AccountCampaignMetrics.FetchFailed`,
   `model.AccountMonitorTotals.UnmeasuredCampaigns`, `rules.fetchFailedRow` (which also no longer
   promises an "underspending" label and a HIGH-priority action item: partial spend could as well
   read as overspending, so it now says an unreliable label or action item), `monitorTotals`,
   `docs/api-catalog.md` and `architecture/account-monitor-endpoints.md` now say "zero or
   partial". Both are unsuitable for pacing and action-item evaluation.
2. **Meta has no per-campaign insights call.** Meta makes one account-level
   `/insights?level=campaign` read. If that read fails, the whole monitor read errors. So a Meta
   metrics failure is per row: its insights row will not parse and its metrics stay zero. A
   campaign-level budget from the campaign list, if the campaign has one, may still be real. The
   ad-set budget fallback is skipped for such a row, so a budget held on ad sets is not read. That is not Meta's only
   cause: an
   unreadable or negative campaign budget also sets `FetchFailed`, with its insights read fine
   (the budget shape, already listed). `model.AccountCampaignMetrics.FetchFailed` no longer gives
   Meta as an example of an outright per-campaign call failure. Reddit stays as that example,
   and Meta is described as a per-row parse failure.
3. **Orphaned short line.** The paragraph in `internal/service/rules/monitor_google.go` that #402
   only partly rewrapped is now rewrapped to the width of the lines above it.

The closing rule was already right in every text and is unchanged: a renderer must not treat any
of a `fetch_failed` row's fields, zero or not, as a fully trusted reading.
