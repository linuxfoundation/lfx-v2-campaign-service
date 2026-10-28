# 2026-10-27 — the fetch_failed contract stops naming one platform

**Fix** — `fetch_failed` means some part of an account-monitor row could not be trusted, and every
text describing it said which part by naming the single platform that had a partial case when the
text was written: "either its per-campaign metrics fetch failed outright (numeric fields left at
their zero value), or, for Google Ads, its budget field was present but unparseable alongside
otherwise-good metrics".

That binding went stale. LinkedIn (`dailyBudget`/`totalBudget`), Meta (`daily_budget`/
`lifetime_budget`), Microsoft Ads and X all now mark a row `fetch_failed` for an unparseable budget
read beside good metrics; X does the same for an unreadable flight; and LinkedIn marks a row whose
`costInLocalCurrency` would not parse, leaving spend unknown next to real impressions and clicks —
a shape none of the texts described at all.

So a client reading the contract literally learned the wrong thing twice over: that a `fetch_failed`
row's numerics are zero, and that the one exception is Google Ads. The closing "MUST check this
before treating any of this row's fields as trusted" kept a conforming client safe; the enumeration
in front of it was simply false (#219).

## What changed

Text only — no behaviour, no new field, no request added or removed.

- `design/connection.go`'s `fetch_failed` attribute, regenerated through `make apigen` into
  `gen/**` and the kodata OpenAPI copies.
- `docs/api-catalog.md`'s account-monitor row.
- `model.AccountCampaignMetrics.FetchFailed` (`internal/domain/model/monitor.go`).
- `rules.fetchFailedRow` (`internal/service/rules/monitor_shared.go`).
- `monitorTotals` (`internal/service/connection_monitor.go`), which also now names the one case
  that really does skew a sum: a LinkedIn row carrying `Spend=0` with real impressions and clicks.

Each states the general rule first — which part failed is not conveyed, and it is not always the
metrics — then gives the shapes as **examples rather than a closed set**, so a seventh platform's
partial case cannot make them false again.

## Scope

Deliberately unchanged: the per-platform client doc comments, such as
`googleads.AccountCampaignRow.FetchFailed` and the LinkedIn/Meta/Reddit equivalents. Each describes
one client, where an exhaustive list is accurate and stays accurate.

## Review-round corrections

The first pass of the widened texts introduced three defects of its own, all caught in review:

- `model.AccountCampaignMetrics.FetchFailed` named `SpendUSD`, which is not a field on that type —
  the metrics fields are `Spend`, `BudgetDay` and `TotalBudget`. `rules.fetchFailedRow` named
  `BudgetDailyUSD` twice for the same reason, and `rules.googleActionItems`' pre-existing
  `budgetOK` comment named it once more. All now name the real fields.
- The design attribute and `docs/api-catalog.md` both said a failed per-campaign metrics fetch
  leaves **every** numeric field at zero. That is the same overstatement this entry is about,
  one level down: Reddit sets `TotalBudget`/`DailyBudget` from the account-level campaign-list
  entry *before* the per-campaign report call, and on a report failure sets only `FetchFailed`
  and `FailureClass` (`internal/platform/reddit/monitor.go`), so the budget on such a row is
  real. Both texts now scope the zeroes to that read's delivery metrics (spend, impressions,
  clicks, ctr) and say fields sourced from a different call may still be real.
- Three comments had been reflowed into orphaned short lines. Rewrapped.

## Tests

None added: no code path changed, and the existing per-cause tests (for example
`TestListAccountCampaigns_MalformedMetrics_MarksFetchFailed` and
`TestListAccountCampaigns_MalformedBudget_MarksFetchFailed`) already pin the behaviour the corrected
text now describes. `make test` green.

Refs #219
