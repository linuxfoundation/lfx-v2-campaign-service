# 2026-09-28 — A2: Reddit showed underspending on the row and alerted nowhere

**Fix** — behaviour change, operator-facing, Reddit only. Refs
`linuxfoundation/lfx-self-serve#3021`.

Reddit's pacing **label** came from the shared ladder, where under 50% of the prorated plan is
`underspending`. Its underspend **action item** fired at its own hardcoded `pacingPct < 40`. The
two numbers were never reconciled, so the 40-49% band was visible in one place and silent in the
other: the campaign row said `underspending`, and the action-item list — the thing an operator
reads to decide what to fix — said nothing about it.

The other three platforms key this item off the label itself (`label ==
model.MonitorPacingUnderspending`). Reddit now does too, and `redditUnderspendActionFloor` is
gone. One boundary decides both, so the row and the alert cannot disagree again.

Worth being exact about what the bug was, because one description of it in the migration notes is
wrong. `reddit-ads.service.ts`'s alert text is `"Underspending at {pacingPct}%"` — it does not
contain a literal `50`, so this was never a case of the message quoting one threshold while the
code used another. It was a plain numeric mismatch, 40 against 50, between two rules about the
same thing.

## The same change uncovered a fourth instance of the no-budget defect

Keying the alert off the label is only safe if the label is trustworthy, which sent me back
through Reddit's guard — and it had the defect the 2026-09-28 no-budget entry describes, by a
route that entry did not reach.

Reddit guarded on an **empty `StartDate`** alone. `redditPacingPct` needs both a `TotalBudget`
and a parseable start, and returns `0` when either is missing. So a campaign with a perfectly
good flight and no total budget passed the guard, hit the ladder with `0`, and came out
`underspending`. The all-platforms regression test written for that defect used a row with no
start date, which Reddit's guard *did* catch — so Reddit passed, and the other half of its own
condition stayed open.

`redditPacingPct` now returns `(pct, computable)`, and both halves route through
`unknownPacingRow` as one condition. Had the underspend item been switched to the label without
this, the fix would have *created* a false HIGH alert on every budget-less Reddit campaign with a
flight — which is the useful lesson here: the ported bugs are not independent of each other, and
fixing one can arm another that was previously masked. The old `pacingPct > 0` clause in the
action-item guard was, by accident, the only thing suppressing it.

`TestEvaluateRedditMonitor_BudgetlessCampaignWithAFlightIsPacingUnknown` pins that half
specifically; `TestRedditUnderspend_AlertMatchesTheLabel` replaces the test that had pinned the
40-vs-50 gap as intended behaviour, and now asserts the alert fires at 45%.
