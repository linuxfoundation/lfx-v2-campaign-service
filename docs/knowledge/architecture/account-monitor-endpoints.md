---
type: "Architecture Doc"
title: "Account-Monitor Endpoints"
description: "Four new account-scoped monitor endpoints ported from the LFX One BFF's four separate rule engines, one per ad platform."
resource: "internal/service/connection_monitor.go"
---

# Account-Monitor Endpoints

`GET /projects/{project_id}/connection-{google,linkedin,meta,reddit}-ads/account-monitor?account_id=&days=`

Ports the LFX One BFF's `/api/campaigns/monitor` family (Google, LinkedIn,
Meta, Reddit — Meta ships with a pagination fix, not a verbatim port; see
below). `{project_id}` means "whose credential do I resolve," the same way
it already does for the existing `connection-*-ads/accounts` discovery
endpoints — these are *account*-scoped reads (every campaign the credential
reaches), not project-scoped ones.

## Shape

- `days` is a plain `Int` (7–90), not `model.MetricsWindow` — the BFF
  computes an explicit date range rather than snapping to a fixed enum, and
  the port preserves that so the numbers match exactly.
- One shared `AccountMonitor` result type (`design/connection.go`); Google's
  richer per-campaign fields (`campaign_url`, ad-group/keyword nesting) are
  `Optional` and documented Google-only.
- `internal/service/orchestrator.go`'s `AccountMetricsReader` capability +
  `Orchestrator.ReadAccountCampaignMetrics` follow the same optional-capability,
  type-assertion pattern as `AccountLister`/`MetricsReader`.
- Each dispatcher (`internal/dispatch/{googleads,linkedin,meta,reddit}.go`)
  reuses its platform's existing discovery credential resolver — the same
  fallback chain (`credsSource.resolve` → `resolveWithFallback` → `systemConn`
  → forced-system) that account discovery already uses.
- The four rule engines (`internal/service/rules/monitor_*.go`) are ported as
  four separate files, deliberately **not** unified onto the shared
  `internal/service/rules` package (`pacing.go`/`actions.go`) — unifying
  would change output and break the empty-diff proof against the legacy BFF
  path. Follow-up ticket #7 tracks that unification.
- `internal/service/connection_monitor.go`'s `monitorAccount` is the shared
  handler body: validate → resolve backend → `ReadAccountCampaignMetrics` →
  per-platform `evaluate` closure → `ReadAccountTotals` (Reddit's only —
  its totals come from a separate account-level call, not a row sum) →
  `monitorTotalsFallback` for everyone else.

## Known-verbatim-ported quirks

Five threshold/labeling bugs from the BFF are carried over on purpose, so the
OLD-vs-NEW differential diff stays a meaningful faithfulness check rather
than a mix of "moved" and "fixed": LinkedIn's `MED`-vs-`MEDIUM` sort-map key
mismatch, Google/Reddit's local pacing literals (not the shared
`Thresholds`), Reddit's hardcoded `conversions: 0` in its rule input, Reddit's
underspend threshold/label mismatch (fires at `<40`, labeled `<50`), and
Reddit's totals coming from an independent upstream call rather than a row
sum. Each has (or will have) its own follow-up issue.

Meta's pagination is the one exception — the legacy BFF silently truncates
past 100 campaigns, which is a data-completeness defect rather than a
threshold quirk, so the port paginates fully instead of copying the bug.

## Correctness bugs found during local differential verification

Local OLD-vs-NEW verification (Google only, so far) surfaced three real port
defects, since fixed:

1. `internal/platform/googleads/monitor.go`'s `ListAccountCampaigns` GAQL
   query was missing the `advertising_channel_type`, `status`, and
   `metrics.impressions > 0` filters that
   `campaign-metrics.service.ts`'s `getMonitorData` applies — without them
   the query returns every campaign the account has ever run (including
   years of `REMOVED` history), not just currently active ones.
2. `monitorAccount`'s totals fallback summed the raw pre-rule-engine rows
   (`metricsRows`) instead of the post-filter rows the response's
   `campaigns` array actually contains (`rows`) — a rule engine like
   `EvaluateGoogleMonitor` drops "zz"-prefixed campaigns, so the two counts
   disagreed by exactly that many campaigns.
3. `AccountMonitorTotals.conversions` was declared `Int64` in both the Goa
   design and the domain model, truncating every row's fractional
   conversions before summing. The per-campaign `conversions` attribute was
   already `Float64` — the totals field is now `Float64` too, matching the
   BFF's plain float sum in `aggregateTotals`.

LinkedIn and Reddit have not yet had the same class of check (missing
platform-query scope filters) run against them.
