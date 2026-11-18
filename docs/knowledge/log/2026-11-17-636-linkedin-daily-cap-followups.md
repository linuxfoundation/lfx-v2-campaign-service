# 2026-11-17 — #636 LinkedIn daily cap follow-ups from the #607 review

**Fix** — two gaps the cs#607 review (lifetime budget with a daily cap) left for a follow-up.

- **Budget currency guard.** With a lifetime total and a daily cap, the budget read kept only
  the daily budget's currency, so a USD cap over a total whose currency was missing or
  different allowed a USD `totalBudget` write. Budget edits now check the currency of every
  present budget.
- **Pacing a capped campaign with no end.** The monitor paced its window against the whole
  total, so the window that spent the last of the total raised a false HIGH "Underspending".
  `fillSpendBeforeWindow` makes one more account-level analytics read for the spend before the
  window (`SpendBeforeWindow`), and `linkedinExpectedSpend` caps the window at what remains.
  A remainder under a tenth of the daily cap (`linkedinSpentRemainderOfCap`) counts as spent,
  because LinkedIn stops a lifetime budget a little short of its total ($999.70 of $1,000); a
  spent total gives unknown pacing. The read is best-effort: on failure the whole total is the cap.
- Tests: `TestEvaluateLinkedInMonitor_CappedNoEndLastWindowIsNotUnderspending`,
  `..._CappedNoEndTotalSpentBeforeWindowIsUnknown` and
  `..._CappedNoEndStoppedJustShortOfTotalIsUnknown` (the last fails with a `<= 0` check), plus
  the currency cases in `budget_update_test.go` and `account_monitor_currency_test.go`.
- KB: `internal-platform-linkedin.md` (spend before the window, budget currency) and
  `internal-dispatch.md` (every present budget's currency is checked).
