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
  richer per-campaign field (`campaign_url`, a direct link built by
  `buildGoogleAdsCampaignURL` in `internal/dispatch/googleads.go`, mirroring
  the BFF's `buildGoogleAdsUrl`) is `Optional` and documented Google-only. An
  earlier draft also declared a speculative `ad_groups`/`AccountMonitorAdGroup`
  nesting; removed (2026-09-18) once the BFF source showed no ad-group/keyword
  data exists anywhere in the monitor response being ported — only in a
  wholly separate `getKeywords` endpoint.
- `internal/service/orchestrator.go`'s `AccountMetricsReader` capability +
  `Orchestrator.ReadAccountCampaignMetrics` follow the same optional-capability,
  type-assertion pattern as `AccountLister`/`MetricsReader`.
- Each dispatcher (`internal/dispatch/{googleads,linkedin,meta,reddit}.go`)
  resolves its monitor read's credential via `credsSource.resolveOwned` —
  the project's own connection only, never the LF system-account fallback
  chain (`resolve` → `resolveWithFallback` → `systemConn` → forced-system)
  that account discovery still uses. This is deliberately narrower than
  discovery: see the Trust boundary section below for why (round-16/17
  review). Google Ads/LinkedIn/Meta build an account-agnostic client the
  same way discovery does; Reddit additionally scopes the requested
  `account_id` to its own resolved connection's single account, since a
  Reddit connection is bound to exactly one ad account.
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

A fourth, caught by round-16 local review: the explicit `time_range` fix
above also silently changed which calendar days a default-window request
covers. Meta's own `last_30d` preset **excludes today** — it is the trailing
30 days as of Meta's last completed reporting day — but the explicit range
`{since: today-(days-1), until: today}` **includes today**, so a `days=30`
request now returns 29 full days plus one partial trailing day instead of
30 full days, biasing pacing toward "underspending" for that partial day.
This is a deliberate, documented divergence, not a defect to fix: it is kept
because it matches the days-1-ending-today convention Google/Reddit's
monitor dispatchers already use, so all four platforms answer "last N days"
identically rather than Meta alone excluding today the way its removed
preset did. See `fetchAccountCampaignInsights`'s doc comment in
`internal/platform/meta/monitor.go` for the same note next to the code.

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
guard (`internal/dispatch/reddit.go`) skipped its own check entirely when the
incoming id was empty (`want != "" && got != "" && ...`) — no longer true as
of round-17 review: both sides are now guaranteed non-empty before this guard
runs (`reddit.ValidateAccountID` upstream, `resolveRedditClientWithCreds`'s
own empty-account-id refusal), so the emptiness conditions were dropped as
dead code and the guard is now a plain equality check. Google is the one
exception — `gaqlSearchForCustomer` already rejects non-digit ids downstream
with a clear error — but still gained the same design-layer guard for
symmetry. Fix: `MaxLength(64)` on all four. LinkedIn (`^[0-9]+$`) and Meta
(`^act_[0-9]+$`) get `Pattern` instead, reusing the same patterns their
existing `*ConnectionConfig` types already enforce — the regex itself already
rejects an empty string, so a separate `MinLength(1)` would be redundant
there.

Google and Reddit initially got `MinLength(1)` rather than a design-layer
`Pattern`, on the reasoning that their regexes — Google's `customerIDRE`
(`internal/platform/googleads/client.go`, `^[0-9]+$`) and Reddit's
`accountIDRe` (`internal/platform/reddit/client.go`, `^[A-Za-z0-9_]+$`) —
were owned and enforced inside the platform client package itself, and that
duplicating them at the Goa design layer would create two definitions of
"valid Google/Reddit account id" that could drift apart silently. Under that
design, their `MinLength`/`MaxLength`-only attributes still admitted a
malformed-but-nonempty id (e.g. `"abc"` for Google, which is digits-only
upstream) past Goa entirely, unlike LinkedIn/Meta's `Pattern`. Rather than
leave that id to reach `gaqlSearchForCustomer`'s or Reddit's own unsentineled
shape error — which `classifyDiscoveryError`'s default arm maps to an opaque
503 — each dispatcher's `ListAccountCampaignMetrics` validated the shape
itself (`googleads.ValidateCustomerID`; Reddit's existing `accountIDRe` check
inside `ListAccountCampaigns`) and wrapped the failure in a new sentinel,
`domain.ErrAccountIDMalformed`, which `classifyDiscoveryError` maps to 400.

A follow-up local review round closed the equivalent gap for LinkedIn/Meta's
non-HTTP callers: `LinkedInDispatcher.ListAccountCampaignMetrics` and
`MetaDispatcher.ListAccountCampaignMetrics` previously relied entirely on
Goa's `Pattern` and passed `account_id` straight to their platform clients
with no dispatcher-level check, so a caller that bypasses the HTTP layer
(a test, or a future non-HTTP entry point) hit the same unsentineled 503
Google/Reddit had. Both dispatchers now call a new exported
`linkedin.ValidateAccountID` / `meta.ValidateAccountID` (each reusing that
package's existing `accountIDRE`) before resolving any credential, wrapping
a shape failure in `domain.ErrAccountIDMalformed` — the same defense-in-depth
pattern Google/Reddit already had.

A later round reversed the "ownership, not precedent" decision above: Google
Ads and Reddit's `account_id` attributes now carry `Pattern(`^[0-9]+$`)` and
`Pattern(`^[A-Za-z0-9_]+$`)` respectively, mirroring `customerIDRE`/
`accountIDRe` exactly and dropping `MinLength(1)` (the pattern itself already
rejects the empty string). The drift risk the original reasoning worried
about is real, but the fix is a guard, not avoidance: none of the four
attributes carries `MinLength` any more, and
`internal/apivalidation/monitor_account_id_drift_test.go` asserts each design
`Pattern` and its platform-package counterpart classify the same ids
identically, so the two copies cannot silently separate.

Net effect: all four platforms answer a malformed id with a clean 400 at the
Goa design layer for an ordinary HTTP caller (Goa never calls the handler).
Every dispatcher's own shape check — `googleads.ValidateCustomerID`,
`linkedin.ValidateAccountID`, `meta.ValidateAccountID`,
`reddit.ValidateAccountID` — still runs too, wrapping a failure in the same
`domain.ErrAccountIDMalformed`; it is now uniform defense-in-depth for a
non-HTTP caller that bypasses Goa entirely on any of the four platforms,
not the primary gate for two of them.

## Trust boundary: `{project_id}` no longer means "or the system fallback"

Round 15 documented, without fixing, a credential-scope gap: `{project_id}`
in these URLs meant "whose credential resolves the platform client," and
that resolution walked the same fallback chain (`resolve` →
`resolveWithFallback` → `systemConn`) discovery already used — so a project
with no connection of its own for a platform was served the shared LF system
credential, and could read another project's spend/budget/campaign data for
any account id that credential reaches. Round 16 escalated this to Critical
because these four endpoints are externally routed (this branch's
`httproute.yaml`/`ruleset.yaml` chart changes), and fixed it for
Google/LinkedIn/Meta by adding a parallel "owned discovery" resolver per
platform (`resolveOwnedGoogleAdsDiscoveryClient`,
`resolveLinkedInOwnedDiscoveryCredentials`, `resolveOwnedMetaDiscoveryClient`)
that refuses the system fallback the way `resolveOwned`
(`internal/dispatch/creds.go`) already does for the adoption flow. A project
with no connection of its own now gets `domain.ErrNotFound` → 404 via the
existing `classifyDiscoveryError` arm, before any account id is even
considered.

Round 17 review found the round-16 fix incomplete: Reddit's
`resolveMonitorClient` still resolved via `d.creds.resolve` (the fallback-
permitting resolver), relying solely on an accountID-equality check against
the resolved connection's own account to constrain access. That check
constrains *which* account is read, not *whose* credential is lent — a
project with no Reddit connection of its own that supplied the shared LF
system account's id (recoverable from its own past campaigns'
`redditCreationAccountID`, if it ever dispatched through the fallback) still
satisfied the equality check and was served every campaign on that shared
account. Fixed the same way as the other three: `resolveMonitorClient` now
calls `d.creds.resolveOwned` instead of `d.creds.resolve`.

**Residual exposure, by design, left open:** this fix is scoped to the four
monitor reads only. `ListAccountCampaignMetrics`'s sibling paths — dispatch
(campaign creation) and per-campaign `ReadMetrics` — still resolve *with* the
system fallback on every platform, so a project operating entirely on the LF
system ad account can still create campaigns and read their per-campaign
metrics; it is only the account-wide monitor read that now refuses to serve
that project at all, rather than scoping to what it created. This mirrors
the adoption flow's own precedent and its stated limit: a project on a
shared credential has no "own" account to monitor, the same way it has no
"own" account to adopt into. Google Ads carries a further, unavoidable
residual even for a project WITH its own connection — see
`resolveOwnedGoogleAdsDiscoveryClient`'s doc comment
(`internal/dispatch/googleads.go`): Google Ads is one shared customer across
every foundation, so the fix closes the system-fallback gap but cannot make
the endpoint project-scoped in the way LinkedIn/Meta/Reddit's per-project ad
accounts are.

### Expected interaction with `LFX_FORCE_SYSTEM_ADS_ACCOUNT`

The `resolveOwned*` resolvers above never consult `forceSystemPaidAds`
(`internal/dispatch/creds.go`) — deliberately: an account-monitor read must
never answer with another tenant's spend, so it resolves only the project's
own connection regardless of that flag. In a deployment running with
`LFX_FORCE_SYSTEM_ADS_ACCOUNT=true` this means monitor reads behave
differently from the create path on the same project: a project with no
connection of its own still gets the endpoint's ordinary no-connection
404 rather than falling through to the system account the create path uses,
and a project WITH its own connection resolves credentials for its own
account, which will not see campaigns actually living on the forced system
account (for Reddit specifically, the account-id equality check turns this
into a 400 `ErrAccountNotManagedByConnection` instead). Neither outcome is a
monitor-endpoint bug — read it as this flag's forced-system deployments
trading monitor visibility for the create path's convenience, not as a
regression to chase (round-19 review).

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
4. Three more false-absence/false-zero defects, found by Copilot's second PR
   review pass and fixed in round 24: Google Ads'
   `internal/platform/googleads/monitor.go` converted a present-but-unparseable
   `campaign_budget.amount_micros` into a trusted real `$0` budget instead of
   `FetchFailed`; LinkedIn's `internal/platform/linkedin/monitor.go` treated an
   absent campaign-list `metadata` block the same as an exhausted cursor,
   silently returning a partial campaign list as complete; and the same file's
   analytics decode used a value-typed `elements` slice, so a null/absent/empty
   analytics response was indistinguishable from a genuine zero-activity
   response and was read as measured zero delivery rather than a failed
   fetch. See
   [2026-09-19-215-monitor-account-endpoints-round24-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round24-fixes.md)
   for the fixes and their pinning tests.
5. Round 24's own budget fix (item 4) had two more defects, caught by the
   next local review pass: `microsToUSD` treated *any* negative
   `amount_micros`, not just Google's exact `-1` sentinel, as a legitimate
   zero budget, so a genuinely malformed negative value was silently
   accepted instead of marking `FetchFailed`; and the budget/`FetchFailed`
   check only ran on a campaign id's first-sighting GAQL row, missing a
   malformed value on a later row of the same multi-row-per-campaign query.
   See
   [2026-09-19-215-monitor-account-endpoints-round25-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round25-fixes.md).
6. Round 24's budget fix (item 4) also left `FetchFailed`'s published
   contract — the doc comment on `AccountCampaignRow.FetchFailed`, the
   `fetch_failed` design attribute description, and its `docs/api-catalog.md`
   row — describing only the metrics-fetch-failure case, which implies zero
   metrics whenever the flag is set. That's inaccurate for the budget case:
   a row can have `FetchFailed=true` with genuinely non-zero metrics if only
   its budget was unparseable. All three texts now describe both causes. See
   [2026-09-19-215-monitor-account-endpoints-round26-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round26-fixes.md).
