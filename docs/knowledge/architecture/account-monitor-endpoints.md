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

Meta has two deliberate departures rather than the usual verbatim port. Its
pagination is the first — the legacy BFF silently truncates past 100
campaigns, which is a data-completeness defect rather than a threshold
quirk, so the port paginates fully instead of copying the bug. Its insights
window is the second: the BFF's `getMetaAnalytics` hardcoded
`date_preset=last_30d`, ignoring its own caller-supplied `days` entirely (a
`days=7` request silently got 30 days of spend) — a data-correctness bug,
not a threshold/labeling quirk, so the port renders an explicit
`time_range` from the caller's `days` instead.

A third, caught by local review rather than the differential diff: the Meta
client's monitor path originally read the bare wall clock (`time.Now()`)
rather than the client's injected clock to compute that `time_range`,
making the request non-deterministic and untestable — LinkedIn's equivalent
`monitor.go` path already used the injected clock. Fixed in `1061e662` to
read the injected clock, with a test (`internal/platform/meta/monitor_test.go`)
pinning the request against a fixed clock the way LinkedIn's
`monitor_test.go` does.

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

A fourth issue, flagged by automated PR review rather than the differential
diff: the four `account_id` payload attributes had no `MinLength`, `Pattern`,
or `MaxLength` — `Required()` only gates JSON-key presence, not shape — so an
empty or malformed id passed Goa's own validation and reached the platform
client, which fails with an opaque upstream error instead of a clean 400.
Verified reachable on three of the four dispatchers: LinkedIn's and Meta's
`ListAccountCampaignMetrics` pass `account_id` straight to the platform
client with no check at all, and Reddit's `resolveMonitorClient` mismatch
guard (`internal/dispatch/reddit.go`) skips its own check entirely when the
incoming id is empty (`want != "" && got != "" && ...`). Google is the one
exception — `gaqlSearchForCustomer` already rejects non-digit ids downstream
with a clear error — but still gained the same design-layer guard for
symmetry. Fix: `MaxLength(64)` on all four. LinkedIn (`^[0-9]+$`) and Meta
(`^act_[0-9]+$`) get `Pattern` instead, reusing the same patterns their
existing `*ConnectionConfig` types already enforce — the regex itself already
rejects an empty string, so a separate `MinLength(1)` would be redundant
there.

Google and Reddit additionally get `MinLength(1)` rather than a design-layer
`Pattern`, even though both already have an equally-established regex to
reuse — Google's `customerIDRE` (`internal/platform/googleads/client.go`,
`^[0-9]+$`) and Reddit's `accountIDRe` (`internal/platform/reddit/client.go`,
`^[A-Za-z0-9_]+$`) are exactly as established as LinkedIn's/Meta's. The reason
is ownership, not precedent: those two regexes are owned and enforced inside
the platform client package itself, not duplicated at the Goa design layer —
declaring a second copy of the same shape in `design/connection.go` would
create two definitions of "valid Google/Reddit account id" that could drift
apart silently (a client-side regex change would not fail any design-layer
test). Enforcement instead happens one layer down, in the dispatcher,
immediately before the credential-dependent client call — see below.

Google and Reddit's `MinLength`/`MaxLength`-only attributes still admit a
malformed-but-nonempty id (e.g. `"abc"` for Google, which is digits-only
upstream) past Goa entirely, unlike LinkedIn/Meta's `Pattern`, which refuses
it at the HTTP boundary. Rather than leave that id to reach
`gaqlSearchForCustomer`'s or Reddit's own unsentineled shape error —
which `classifyDiscoveryError`'s default arm maps to an opaque 503 — each
dispatcher's `ListAccountCampaignMetrics` now validates the shape itself
(`googleads.ValidateCustomerID`; Reddit's existing `accountIDRe` check inside
`ListAccountCampaigns`) and wraps the failure in a new sentinel,
`domain.ErrAccountIDMalformed`, which `classifyDiscoveryError` maps to 400.
Net effect: all four platforms now answer a malformed id with a clean 400 —
LinkedIn and Meta at the design layer (Goa never calls the handler), Google
and Reddit at the dispatcher layer (the handler runs, then refuses).

A follow-up local review round closed the remaining gap for non-HTTP callers:
`LinkedInDispatcher.ListAccountCampaignMetrics` and
`MetaDispatcher.ListAccountCampaignMetrics` previously relied entirely on
Goa's `Pattern` and passed `account_id` straight to their platform clients
with no dispatcher-level check, so a caller that bypasses the HTTP layer
(a test, or a future non-HTTP entry point) hit the same unsentineled 503
Google/Reddit had. Both dispatchers now call a new exported
`linkedin.ValidateAccountID` / `meta.ValidateAccountID` (each reusing that
package's existing `accountIDRE`) before resolving any credential, wrapping
a shape failure in `domain.ErrAccountIDMalformed` — the same defense-in-depth
pattern Google/Reddit already had, now present on all four platforms.

## Correctness bugs found during PR review

Three more real defects, caught by automated PR review rather than the
differential diff, since fixed:

1. All four `EvaluateGoogleMonitor`/`EvaluateLinkedInMonitor`/
   `EvaluateMetaMonitor`/`EvaluateRedditMonitor` ran a `FetchFailed` row's
   placeholder zero-value metrics through pacing/action-item evaluation
   instead of skipping it — fabricating findings (a bogus "underspending" or
   "no delivery" HIGH item) against a campaign whose metrics call to the
   platform actually failed. Each now checks `FetchFailed` at the top of its
   loop and returns the row unevaluated (still present in the response's
   `campaigns` array, with the flag intact, but excluded from pacing/action
   items). See `AccountCampaignMetrics.FetchFailed`'s doc comment
   (`internal/domain/model/monitor.go`) for the contract this enforces.
2. `googleActionItems`' underspending item text hardcoded a 30-day window
   (`m.BudgetDay*30`) even though the pacing percentage right next to it was
   already computed from the caller's real `days` parameter — a `days=7`
   request would show a pacing number for 7 days next to expected-spend text
   for 30. Now `m.BudgetDay*float64(days)`.
3. `monitorAccount` (`internal/service/connection_monitor.go`) aborted the
   whole endpoint with an error whenever Reddit's separate account-totals
   call (`AccountTotalsReader.ReadAccountTotals`) failed, discarding the
   per-campaign rows and action items already fetched successfully. A
   totals-call error now falls back to `monitorTotalsFallback` the same way
   the capability-absent (`!ok`) arm already did — the per-campaign data is
   the response's primary content, and the account-wide totals are a
   secondary, derivable figure not worth a 5xx over.
