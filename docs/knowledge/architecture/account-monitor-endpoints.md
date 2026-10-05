---
type: "Architecture Doc"
title: "Account-Monitor Endpoints"
description: "Six account-scoped monitor endpoints, one per ad platform: four ported from the LFX One BFF's rule engines and read live, and Microsoft Ads and X served from saved asynchronous reports because their metrics cannot be read inside one request."
resource: "internal/service/connection_monitor.go"
---

# Account-Monitor Endpoints

`GET /projects/{project_id}/connection-{google,linkedin,meta,reddit,microsoft,twitter}-ads/account-monitor?account_id=&days=`

Microsoft Ads and X joined later and are the two REPORT-BACKED monitors — see
[Microsoft: a report-backed monitor](#microsoft-a-report-backed-monitor) and
[X: a second report-backed monitor](#x-a-second-report-backed-monitor). Everything below
that is not in those sections describes the four live reads.

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
  review). Google Ads builds an account-agnostic client the same way
  discovery does; Reddit, LinkedIn and Meta additionally scope the requested
  `account_id` to their own resolved connection's single account. A
  connection is bound to exactly one ad account (`account_id TEXT NOT NULL` on
  every provider table, one LIVE row per project via a `(project_id)` unique
  index partial to `WHERE status <> 'deleted'`), so
  a request naming any other account is a request mismatch and answers
  `domain.ErrAccountNotManagedByConnection` (400) before any upstream call.
  Reddit checked this first (round-18 review); LinkedIn and Meta followed,
  since the property is the shared schema's and not a Reddit quirk. Google
  Ads is the remaining gap and is tracked separately — it is one shared
  customer id across every foundation, so there is no second account to
  cross into today, and the change collides with an open PR.

  Refusing the LF system fallback does **not** make this check redundant:
  that is about whose CREDENTIAL resolves the client, this is about which
  ACCOUNT the request named. A project with its own active connection passes
  the fallback check and can still name a sibling project's account — and one
  LinkedIn token reaches several ad accounts (that is what `ListAccounts`
  enumerates), so the token is not the boundary either.

  LinkedIn is the one platform where this is LIVE rather than defensive: it
  genuinely runs several ad accounts across foundations (`tlf` and `lf-events`
  are two of them), so a project naming another project's account is a real,
  reachable request. The other ad platforms share a single account today, so
  their stored `account_id` IS the shared account and a legitimate request
  matches it — the guard is carried there against a future split, and never
  fires meanwhile. `docs/architecture.md` still lists Meta/Reddit/X as
  per-foundation in BOTH its "Account Tenancy" and "Current Platform
  Accounts" tables; both are stale against how the accounts are actually run
  and neither is the authority for this.

  LinkedIn keeps the empty stored account DISTINCT from a mismatch
  (`ErrAccountNotSelected` vs `ErrAccountNotManagedByConnection`) because its
  owned-discovery resolver deliberately returns success on
  `ErrAccountNotSelected` so `VerifyAccountOrg` can report a half-configured
  pairing itself. Reddit can use a plain equality check because its resolver
  already refused an empty stored account upstream. Meta layers its mismatch
  check onto the existing `requireMetaAccountID` so the two cannot drift.
- The four rule engines (`internal/service/rules/monitor_*.go`) were ported as
  four separate files so the empty-diff proof against the legacy BFF path
  stayed meaningful. That diff is no longer the plan of record, so what the
  four genuinely share now lives in `monitor_shared.go` — one pacing ladder
  (`pacingLabelFor`), one priority rank (`priorityRank`/`sortByPriority`),
  one unknown-pacing row. Each `EvaluateXMonitor` keeps its own guard for
  whether a campaign has a pacing figure worth placing at all, because the
  platforms report budget differently. They are still **not** routed onto
  `pacing.go`/`actions.go`, which run a different ladder (50/100/130) for
  the single-campaign brief path; merging the two read paths would move
  operator-facing alerting bands and remains its own decision.
- `internal/service/connection_monitor.go`'s `monitorAccount` is the shared
  handler body: validate → resolve backend → `ReadAccountCampaignMetrics` →
  per-platform `evaluate` closure → `monitorTotals`, which sums the
  post-`evaluate` rows on every platform, so the aggregate always describes
  exactly the campaigns array returned beside it.

## Ported BFF quirks, and where each one now stands

Five threshold/labeling bugs from the BFF were carried over on purpose, so the
OLD-vs-NEW differential diff stayed a meaningful faithfulness check rather
than a mix of "moved" and "fixed". **That diff is no longer the plan of
record**, which removes the reason to preserve them, so each is being fixed
against its own filed issue rather than frozen — and the "do not fix this"
comments come out with each fix.

| Ported quirk | Issue | Status |
| --- | --- | --- |
| LinkedIn's `MED`-vs-`MEDIUM` sort-map key mismatch sorted MED action items *behind* LOW ones | `linuxfoundation/lfx-self-serve#3018` | **Fixed** — one shared `priorityRank` |
| Google/Reddit's local pacing literals rather than a shared constant | `linuxfoundation/lfx-self-serve#3019` | **Fixed** — one shared `pacingLabelFor` |
| Reddit's hardcoded `conversions: 0` in its rule input | `linuxfoundation/lfx-self-serve#3020` | **Fixed** — absent, not a measured 0, on the row **and** in the account totals |
| Reddit's underspend threshold/label mismatch (fires at `<40`, labeled `<50`) | `linuxfoundation/lfx-self-serve#3021` | **Fixed** — the alert is keyed off the label |
| Reddit's account totals from an independent upstream call rather than a row sum | `linuxfoundation/lfx-self-serve#3022` | **Fixed** — every platform sums its rows |

Three further defects were found in this code rather than carried across it, so
none has a BFF-side ticket: a campaign with no budget at all presented as
though its pacing were known, which turned out to affect all four platforms in
two different ways. Google and Meta reported it as `underspending` outright, and
so did Reddit for a campaign that had a flight but no total budget. LinkedIn's
`hasBudget` guard genuinely held the row off the ladder but did not *say* so:
it went out as `normal` with `PacingUnknown` left false, which a consumer reads
as "on plan" — a quieter version of the same claim (**fixed**; all four now
route a budget-less campaign through `unknownPacingRow`; see the two log entries
of 2026-09-28) — and the
Google `zz`-prefix name filter, which dropped any campaign whose name merely
began with those two letters rather than only those using the operator's
`zz` scratch-naming convention (**fixed**; the prefix must now be followed
by a separator, or be the whole name, to count). The third is LinkedIn's
low-CTR rule, which was gated on `ctr > 0 && ctr < 0.3` — excluding a 0% CTR,
the worst case of the very thing it detects, with no "impressions but no
clicks" rule to catch it instead (**fixed**; gated on an impressions floor
like the other three platforms, so a 0% CTR now fires and an unserved
campaign does not).

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
`resolveLinkedInOwnedDiscoveryCredentials`, `resolveOwnedMetaDiscovery`)
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
   totals-call error fell back to the row sum the same way the
   capability-absent (`!ok`) arm already did — the per-campaign data is
   the response's primary content, and the account-wide totals are a
   secondary, derivable figure not worth a 5xx over. (That whole call, and
   the `AccountTotalsReader` capability behind it, were later removed by
   `#3022`; Reddit now sums its rows like everyone else.)
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
   contract — the doc comment on `model.AccountCampaignMetrics.FetchFailed`,
   the `fetch_failed` design attribute description, and its
   `docs/api-catalog.md` row — describing only the metrics-fetch-failure
   case, which implies zero metrics whenever the flag is set. That's
   inaccurate for the budget case: a row can have `FetchFailed=true` with
   genuinely non-zero metrics if only its budget was unparseable. All three
   texts now describe both causes. See
   [2026-09-19-215-monitor-account-endpoints-round26-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round26-fixes.md).
7. Round 26 (item 6) widened `model.AccountCampaignMetrics.FetchFailed`'s
   contract but missed three more texts describing the same field from a
   different angle, caught by the next local review pass:
   `googleads.AccountCampaignRow.FetchFailed`'s own doc comment (the
   platform-layer type, distinct from the domain-layer type item 6 fixed);
   `rules.fetchFailedRow`'s doc comment, which also mis-cited its GAQL
   metrics-parse line number after item 4's insertion and didn't mention that
   `monitor_reddit.go` reuses the same builder for its empty-`StartDate`
   case; and `monitorTotalsFallback`'s doc comment, which still claimed a
   `FetchFailed` row always contributes zero-value metrics to the account
   totals sum. All three now describe both causes. Also documented, as a
   comment only (no behavior change): `monitor_google.go` excludes a
   budget-only-failed row from every action item, not just the
   budget-dependent ones, unlike `monitor_reddit.go`'s empty-`StartDate`
   branch, which still runs its metrics-only action items. See
   [2026-09-19-215-monitor-account-endpoints-round27-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round27-fixes.md).
8. Round 27's own doc widening (item 7) left one sentence in
   `fetchFailedRow`'s comment inaccurate for the case it had just added to
   that comment's scope: it said the row "still" carries its `FetchFailed`
   flag regardless of cause, but `monitor_reddit.go`'s empty-`StartDate` row
   never sets `FetchFailed` — only `PacingUnknown`. Also fixed:
   `fetchAccountCampaignList`'s absent-`metadata` guard comment claimed full
   parity with `accounts.go`'s adAccount picker, which additionally dedups
   repeated page cursors; this loop does not, so the comment now says so
   instead of overclaiming. And `microsToUSD` checked for an empty
   `amount_micros` before trimming whitespace, so a whitespace-only value
   took the malformed-data path instead of the legitimate-zero path the
   empty string gets — now trims once, up front. Plus: a pre-existing,
   repo-wide convention of naming a human reviewer by GitHub handle was
   redacted in the two places it appeared in this feature's own round-24 log
   entry, since that file is new within this review range (unlike the
   dozen other pre-existing files elsewhere in the repo, redacting here
   needed no wider sweep to keep the range clean). See
   [2026-09-19-215-monitor-account-endpoints-round28-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round28-fixes.md).
9. Round 25's later-row budget check (item 5) only ever *marked* a
   later-row budget parse failure; it never let a later-row parse
   *success* overwrite a first row's failed one, so `BudgetDailyUSD` stayed
   stuck at `0` even when a good value was available on a later row for the
   same campaign — the mirror image of the bug item 5 fixed. `FetchFailed`
   is still set whenever any row's budget fails to parse, but the numeric
   value is no longer discarded once a good one is seen. Also: item 8's
   tip-tree redaction of the reviewer-handle privacy issue never reached the
   *history* that would be pushed — the original commit still carried the
   handle twice in its patch and once in its own message. Since this branch
   had never been pushed, an interactive rebase rewrote that commit in place
   (same "a human reviewer" phrasing item 8 already used forward) and
   replayed the later commits on top unchanged; only their SHAs moved. See
   [2026-09-19-215-monitor-account-endpoints-round29-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round29-fixes.md).
10. Five defects named on PR #215's own GitHub review threads (distinct from
    the local review trio rounds above), fixed in round 30: Reddit's
    `decodeCampaignList` treated a malformed/unrecognized campaign-list
    response the same as a legitimately empty one; LinkedIn's
    `parseUSDAmount` and Meta's `minorUnitsToWhole` both silently trusted a
    non-empty, unparseable budget string as a real `$0` instead of marking
    `FetchFailed` (the same class rounds 24/25 fixed for Google Ads and for
    LinkedIn's `costInUsd`); fixing LinkedIn's also surfaced a latent
    overwrite bug where a successful analytics read would clear an
    already-set budget-parse `FetchFailed`; Meta's `dateOnly` sliced a
    timestamp's first 10 characters without validating they formed a real
    calendar date; and the shared `sortByPriority` ran an O(n²) insertion
    sort, replaced with `sort.SliceStable` (pure efficiency, no output
    change). See
    [2026-09-19-215-monitor-account-endpoints-round30-fixes.md](../log/2026-09-19-215-monitor-account-endpoints-round30-fixes.md).
11. Three more doc-only inaccuracies named on PR #215's review threads,
    fixed in round 31 (no behavior change): the `monitor-google-ads-account`
    design description claimed `{project_id}` resolves credentials "exactly
    as" the `/accounts` picker does — untrue since the round-16/17 trust-
    boundary fix above made the monitor path `resolveOwned`-only with no
    system fallback, unlike `/accounts`; `AccountMonitorActionItem`'s doc
    comment (`internal/domain/model/monitor.go`) still said Meta's insights
    read was single-page, contradicting this same file's already-documented
    pagination fix; and `linkedin.AccountCampaignRow.FetchFailed`'s field
    comment described the flag as marking a campaign the analytics pivot
    omitted, the inverse of its actual contract — an omitted-but-successful
    pivot row is a legitimate zero (see `ListAccountCampaigns`'s own comment
    just above it), and `FetchFailed` is set only for a malformed budget or
    unparseable `costInUsd`. See
    [2026-09-21-215-monitor-account-endpoints-round31-doc-fixes.md](../log/2026-09-21-215-monitor-account-endpoints-round31-doc-fixes.md).
12. Two "previously missed" findings on the same review that submitted round
    31's docs threads, fixed in round 32: LinkedIn's
    `fetchAccountCampaignAnalyticsRaw` (`internal/platform/linkedin/monitor.go`)
    silently `continue`'d past an analytics row with an empty or malformed
    `pivotValues` — dropping it from the returned map entirely rather than
    rejecting the read — and Meta's `fetchAccountCampaignInsights`
    (`internal/platform/meta/monitor.go`) stored (or, for a parse failure,
    tracked as failed) an insights row keyed by `campaign_id` without first
    checking that field was non-empty. Both are the same false-zero bug class
    as item 10 above, one step earlier in the pipeline: a row absent from a
    *successful* response is read by `ListAccountCampaigns` as a legitimate
    "no activity" zero (see item at the top of this list and each file's own
    `ListAccountCampaigns` comment), so a row that exists but can't be
    attributed to a campaign id must not be silently dropped — that makes an
    unattributable upstream row indistinguishable from a real omitted-because-
    inactive campaign. Neither malformed-id row carries an id to key a
    per-campaign `FetchFailed` on (unlike a parse-failure row, which does),
    so both fixes reject the whole analytics/insights read instead of
    dropping just the one row. See
    [2026-09-21-215-monitor-account-endpoints-round32-unattributable-rows.md](../log/2026-09-21-215-monitor-account-endpoints-round32-unattributable-rows.md).

## Microsoft: a report-backed monitor

Microsoft Advertising cannot be read like the other four. Its delivery metrics come only
from the Reporting v13 service — submit, poll, download a zipped CSV — and Microsoft's own
guidance is that reports "complete within minutes" and should be polled at 2–15 minute
intervals. The monitor read runs inside `accountsCallTimeout` (20s); a synchronous port would
essentially never see a finished report. (The same arithmetic is why the per-campaign
Microsoft metrics read is default-OFF; see `internal/platform/microsoft/metrics.go`.)

So the read is split, and the state between requests is saved:

- **Capability.** `MicrosoftDispatcher` implements `service.AccountReportReader`
  (`ListAccountCampaigns`, `SubmitAccountReport`, `CheckAccountReport`), not
  `AccountMetricsReader`. All three resolve the project's OWN connection (`resolveOwned`)
  and refuse an account the connection is not bound to
  (`ErrAccountNotManagedByConnection`) — the trust boundary below applies unchanged.
- **Orchestration.** `Orchestrator.ReadReportedAccountCampaigns`, inside ONE
  `accountsCallTimeout` budget: read the campaign list live (its failure is the call's
  failure); check a pending report once — store it if finished (however late), drop it if
  Microsoft failed it, or if it is STILL pending or uncheckable past
  `accountReportAbandonAfter` (60m, Microsoft's own "consider trying again later" point);
  submit a new one when nothing is pending and the last finished report's as-of is missing or
  older than `accountReportFreshFor` (30m); fill each live campaign's metrics from the last
  finished report. The saved snapshot is read first, on the request context, so a slow list
  cannot turn the store read into a 503. A check, submit or save that fails is logged and
  never fails the read. Recording a submission that completed (`MarkAccountReportPending`)
  runs on its OWN short budget — `accountReportMarkTimeout` (5s) on a context detached from the
  read's cancellation — not on the shared, possibly spent, call budget: a report built upstream
  but not recorded would be resubmitted on every read and never collected. A submission the
  dispatcher declines for lack of budget (`domain.ErrAccountReportBudgetTooShort`) is logged as
  a skip and retried on a later read. Checking BEFORE abandoning matters: an age-only abandon threw away
  every report on an account viewed less than hourly, so it never showed metrics.
- **Store.** `account_monitor_reports` (migration `000035`), one row per
  (project, platform, account, days) with a READY half (served, possibly stale) and a
  PENDING half (building upstream). Completing or failing a report is a compare-and-set on
  its report id, so a request that collected an older report cannot clear a newer one's
  pending marker. Platform-neutral on purpose, and now SHARED: X's monitor keeps its rows in
  the same table under `platform = 'twitter-ads'`, with no schema change (see below).
- **Response.** Same `AccountMonitor` type, plus two Microsoft-only fields:
  `metrics_as_of` (when the report was REQUESTED — the point in time the data describes, never
  the later moment it was collected, which would overstate freshness; absent before the first
  one finishes) and
  `metrics_pending` (a newer report is building; always set on Microsoft, omitted on the
  live four). Before any report has finished, every row is `fetch_failed` and therefore
  skipped by the rules — unavailable metrics never read as a campaign spending nothing.
- **Report scope.** The submission is scoped by `AccountIds` — the account-wide union the
  per-campaign read deliberately avoids is exactly what this read wants — so a campaign
  absent from a finished report served nothing (spend/impressions/clicks zero, conversions
  left nil). A report Microsoft flags "Potential Incomplete Data" is ACCEPTED here, unlike the
  per-campaign read: the monitor's window always includes today, and `metrics_as_of` already
  tells the reader the numbers are as of a point in time.
- **Rules.** `rules.EvaluateMicrosoftMonitor`, on the shared ladder/rank/unknown-row helpers.
  Daily budgets only (v13 has no lifetime budget), so pacing follows Google's daily model. A
  shared-budget campaign arrives with `PacingUnknown` and is neither paced nor called a
  placeholder budget. Microsoft-specific HIGH findings: `Suspended`, and `BudgetPaused` /
  `BudgetAndManualPaused` (budget exhausted).
- **Gate and boundary.** Behind `MICROSOFT_METRICS_ENABLED` like the per-campaign read, until
  the Reporting contract is exercised against a live account; disabled, the endpoint answers
  the same 400 as a platform with no monitor. `account_id` is checked by the design `Pattern`
  `^[1-9][0-9]{0,17}$` and, identically, by `microsoft.ValidateMonitorAccountID` — not by the
  create path's `ValidateAccountID`, which trims and admits a 19th digit — and the drift test
  covers it, length included.

## X: a second report-backed monitor

X reuses the Microsoft machinery unchanged — `service.AccountReportReader`,
`Orchestrator.ReadReportedAccountCampaigns`, `account_monitor_reports`, the same freshness
(30m) and abandon (60m) timings, the same `metrics_as_of` / `metrics_pending` response fields —
with `TwitterDispatcher` as a second implementation. Nothing was forked.

- **Why report-backed, for every `days`.** X's synchronous stats are capped at 7 days per
  request and share a 250-requests-per-15-minutes budget across every foundation on the shared
  LF token; the asynchronous stats-jobs API covers up to 90 days per job and is limited by
  concurrent jobs per account instead (https://docs.x.com/x-ads-api/analytics). One path for
  every window, so a 7-day and a 30-day view never differ in how they were read.
- **List (live).** `campaigns` and `line_items` (with_deleted=false, with_draft=false,
  count=1000, line items filtered by ≤200 `campaign_ids`), on the strict cursor rule: anything
  short of X's documented null `next_cursor` fails the read. Status is `entity_status`
  verbatim; budgets are `*_local_micro` ÷ 1e6 in the account's currency (malformed →
  `fetch_failed`); the flight is the line items' earliest start and latest end, as dates in
  the ACCOUNT's timezone (read from the account resource), open-ended if any line item is.
- **Submit.** `active_entities` for the window, then one stats job per ≤20 active campaigns
  (`entity=CAMPAIGN`, `granularity=TOTAL`, `placement=ALL_ON_TWITTER`,
  `metric_groups=ENGAGEMENT,BILLING`), paced on the client's write pacer and never retried on
  a 429. The window is today-(days-1) 00:00 to the next midnight in the ACCOUNT's timezone,
  sent as whole UTC hours. Before the first job POST the time left on the call budget is
  checked against one pacer interval per job plus a 2s margin; if it cannot fit, the
  submission is declined whole (`twitter.ErrStatsJobBudget` → `domain.ErrAccountReportBudgetTooShort`)
  rather than stranding half-created jobs in X's 100 concurrent-job slots. The account timezone
  is cached on the shared client for a minute, so one read (list, then submit) reads the
  account once. The saved report id is ONE composite value — the jobs' `id_str`s
  comma-joined (≤10 jobs, ~210 bytes in a TEXT column). No active campaign gives the sentinel
  `none`, which Check answers as a finished empty report without calling X. An account with
  more than 200 active campaigns is refused rather than half-reported.
- **Check.** ONE job-status read for every job. Any failed or cancelled job fails the report
  (and so does a finished job whose file is gone); any job still building, or missing from
  X's answer, leaves it pending; otherwise every file is downloaded — unsigned, because X says
  the URL needs no authentication and our credentials must not go to a storage host — then
  gunzipped, bounded, and folded per campaign. The file URL is upstream data, so it is fetched
  only over https from exactly `ton.twimg.com`, the host X's documented job example serves
  results from (or the client's own API origin); any other host is refused with an error that
  does not echo the URL. Spend is `billed_charge_local_micro` ÷ 1e6;
  `Partial` is always true because X's billed charge settles over days.
- **Absence.** A campaign absent from a finished report served nothing, exactly as on
  Microsoft — and here that rests on `active_entities` too: a campaign it did not list had no
  activity in the window.
- **Conversions are never reported.** X splits them per event type under metric groups the
  monitor does not request, and reports nothing at all for an account with no conversion tag,
  which is indistinguishable from a measured zero. Every row's `conversions` is absent.
- **Rules.** `rules.EvaluateTwitterMonitor`: a daily budget paced as `BudgetDay` × the window
  days the flight covers (window closed by the exclusive midnight after its last day, as
  Reddit's daily branch settled); otherwise a total budget prorated over the flight; otherwise
  unknown. The window is the SAVED REPORT's own — its first and last day in the account's
  timezone, carried to the service as `ReportedAccountRead.MetricsWindowStart/End` — so the
  rules, the stats jobs and the line items' flight dates all count the account's calendar
  days. (An earlier draft derived "today" from the service's UTC clock: on a US/Pacific
  account every evening it counted a flight starting the next local day as scheduled and
  raised a false zero-delivery HIGH.) With no window the pacing and zero-delivery judgements
  are skipped. Microsoft's rules take no date at all (daily budget × days), so they did not
  have this flaw and are unchanged.
- **Boundary.** `resolveOwned` only, the bound account only
  (`ErrAccountNotManagedByConnection`), and `account_id` checked by the design `Pattern`
  `^[A-Za-z0-9]+$` + `MaxLength(64)` and identically by `twitter.ValidateMonitorAccountID` —
  the connection's own rule, so every storable id can be monitored. Every refusal makes zero
  upstream calls.
- **Gate.** Behind `TWITTER_METRICS_ENABLED` (chart default `"false"`), on the same terms as
  Microsoft's and Reddit's gates: only exactly `"true"` enables it, and disabled, all three
  `AccountReportReader` methods answer `ErrAccountMetricsUnsupported` — the same 400 as a
  platform with no monitor — before any credential is resolved. It gates only the monitor; X's
  per-campaign metrics read is a different endpoint and is not affected.
- **Unverified.** The whole X contract here follows docs.x.com and has not been exercised
  against a live X account; the specific open points (half-hour timezones, the queued and
  failed status spellings, whether job creation counts as a write) are marked UNVERIFIED in
  `internal/platform/twitter/monitor.go`.
