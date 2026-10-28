# 2026-10-28 — fetch_failed: a metrics failure can leave partial delivery, not only zero

**Fix** — three documentation-accuracy findings accepted on #402 and deferred to #410. No
behaviour changes and no code path moves.

1. **A metrics failure does not always leave zeroes.** `internal/platform/googleads/monitor.go`
   sums the GAQL rows per campaign (`segments.date` returns several rows for one campaign). A row
   whose `impressions`, `clicks` or `cost_micros` fails `parseMetricInt` marks the campaign
   `FetchFailed` and is skipped, but the rows that did parse keep their `+=` contribution. So a
   flagged Google row can carry real but incomplete delivery, not zero. The `fetch_failed` and
   `unmeasured_campaigns` design attributes (regenerated through `make apigen` into `gen/**` and
   the kodata OpenAPI copies), `model.AccountCampaignMetrics.FetchFailed`,
   `model.AccountMonitorTotals.UnmeasuredCampaigns`, `rules.fetchFailedRow`, `monitorTotals`,
   `docs/api-catalog.md` and `architecture/account-monitor-endpoints.md` now say "zero or
   partial". Both are unsuitable for pacing and action-item evaluation.
2. **Meta has no per-campaign insights call.** Meta makes one account-level
   `/insights?level=campaign` read. If that read fails, the whole monitor read errors. A Meta row
   is flagged only when its own insights row will not parse, and its budget, set from the
   campaign list first, is still real. `model.AccountCampaignMetrics.FetchFailed` no longer gives
   Meta as an example of an outright per-campaign call failure. Reddit stays as that example,
   and Meta is described as a per-row parse failure.
3. **Orphaned short line.** The paragraph in `internal/service/rules/monitor_google.go` that #402
   only partly rewrapped is now rewrapped to the width of the lines above it.

The closing rule was already right in every text and is unchanged: a renderer must not treat any
of a `fetch_failed` row's fields, zero or not, as a fully trusted reading.
