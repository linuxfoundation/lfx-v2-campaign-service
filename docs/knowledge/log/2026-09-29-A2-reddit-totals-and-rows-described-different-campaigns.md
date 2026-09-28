# 2026-09-29 — A2: Reddit's totals and its rows described different campaigns

**Fix** — behaviour change, operator-facing and response-shape, Reddit only (plus a
response-field removal that touches all four). Refs `linuxfoundation/lfx-self-serve#3022`.

Reddit's `accountTotals` came from `/ad_accounts/{id}/reports`, a single account-wide call made
independently of the per-campaign rows, ported from `reddit-ads.service.ts`'s
`fetchAccountMetrics`. That call is **unfiltered**: it reports every campaign on the account,
including archived and deleted ones, and including campaigns whose own per-campaign metrics fetch
failed. The rows beside it are filtered to the statuses the monitor displays.

So the aggregate and the list described different populations, and nothing in the response said
so. The sharpest evidence was inside the totals object itself: `campaign_count` was passed in by
the caller as `len(rows)` — the filtered count — sitting next to a `spend` figure covering a
superset. An operator adding up the campaigns in front of them could not reach the total, and had
nothing to tell them why.

Reddit now sums its rows, like Google, Meta and LinkedIn. The aggregate and the campaigns array
agree by construction rather than by coincidence, on every platform.

## What that removed

With no platform left that needs a separate totals read, the entire capability was dead weight
rather than an extension point, so it goes:

- `service.AccountTotalsReader`, `Orchestrator.ReadAccountTotals`, the `read_account_totals`
  upstream metric op, and `errAccountTotalsContractViolation` — a sentinel that existed solely so
  one caller could log a broken adapter at ERROR rather than WARN.
- `RedditDispatcher.ReadAccountTotals`, `reddit.Client.FetchAccountTotals`, and the
  `reddit.AccountTotals` type.
- The whole fallback branch in `monitorAccount` — the capability-absent arm, the failed-read arm,
  the contract-violation arm and their three log lines. `monitorTotalsFallback` is no longer a
  fallback and is now just `monitorTotals`.

**`derived_from_rows` comes off the wire with them.** It existed to distinguish Reddit's
platform-native figure from a row sum standing in for one that failed. With every platform's
totals a row sum by contract, the field could only ever report `false`, and a required response
field that is constant is worse than no field: it implies a distinction the response no longer
makes. Removing a required attribute is a design change, so `design/connection.go` and
`make apigen` are part of this commit. Nothing consumes it — checked `lfx-self-serve` for both
spellings, no reads — and these endpoints are still behind the cutover flag.

## What is genuinely lost

Worth stating rather than glossing: the account-wide number was a **true** number. Spend on
archived campaigns, and on campaigns whose metrics fetch failed, was in it and is not in the row
sum. A Reddit account total from this endpoint will now read lower than the same window in
Reddit's own UI whenever the account has archived spend.

That is the right trade. The endpoint's job is to explain the campaigns it is showing; a total the
caller cannot reconcile against the list beside it is not more informative for being larger, and
the alternative — keeping the native figure and disclosing the population gap on the wire — buys
accuracy on a number this response is not the place to look up.

## Tests

`TestMonitorRedditAccount_TotalsSumTheReturnedRows` is the regression test.
`TestMonitorAccount_TotalsSumTheReturnedRows` pins the now-uniform contract, and
`TestMonitorAccount_TotalsExcludeRowsTheRuleEngineDropped` pins which rows are summed — the
post-rule-engine ones, so a Google scratch campaign's spend cannot appear in a total whose
campaigns array does not contain it.

The three tests covering the removed branches
(`TestMonitorAccount_TotalsReaderErrorFallsBackToRowSum`,
`TestMonitorAccount_ContractViolationFallsBackToRowSum`, and the `FallsBackToRowSumTotals`
capability-absent test) are removed with the code they covered, as are Reddit's two dispatcher
validation tests for `ReadAccountTotals` and the `read_account_totals` row in the orchestrator's
upstream-metrics table.
