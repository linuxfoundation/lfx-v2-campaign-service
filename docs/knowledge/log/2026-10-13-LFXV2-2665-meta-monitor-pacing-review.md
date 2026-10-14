# 2026-10-13 — Meta monitor paces spend and plan over the same ad sets and days (LFXV2-2665, PR #316 review)

**Fix** — Fourth round of PR #316 review fixes, all one theme: Meta pacing must compare spend and
expected spend over the SAME ad sets and the SAME days, and where that cannot be established
the row is `pacing_unknown`, never a false alert.

- **Daily pacing counts only the flight's days in the window.** `metaExpectedSpend` expected
  `BudgetDay × days` whatever the flight, so a campaign started 7 days ago at full budget read
  ~23% on `days=30`. It now expects `BudgetDay` on the window's flight days (start and end
  inclusive; an unreported date is the window's edge), and a flight outside the window is unknown.
  On the ad-set path a daily budget is used only when the active ad sets share one window.
- **Lifetime end date inclusive, open-ended lifetime unknown.** The end date is counted
  inclusively (pinned by a last-day-only test), and a lifetime flight with no end date is
  `pacing_unknown`: there is no flight length to prorate over, and reading it as "ends today"
  expected the whole budget at once.
- **One window snapshot.** The service took `now` before the read while the Insights read took
  its own clock reading for `since`/`until`, so a request crossing UTC midnight paced one day's
  plan against the next day's spend. `fetchAccountCampaignInsights` now returns the bounds it
  sent, every row carries them (`model.AccountCampaignMetrics.MetricsWindowStart`/`End`), and
  `rules.EvaluateMetaMonitor(rows)` takes no clock or `days`; a row with no window is unknown.
- **Non-ACTIVE ad sets make the campaign unknown.** Paused ad sets' budgets were dropped from the
  sum while their earlier spend stayed in the campaign Insights (two ad sets at 10/day for a week,
  one then paused: 140 spent against 70 expected, a false overspend). When an ad set stopped is
  not on the read, so its budget cannot be prorated; any ad set not `ACTIVE` now leaves the
  campaign's ad-set budget unknown. Residual: Meta's default edge omits DELETED ad sets.
- **Invalid JSON from the ad-set read no longer fails the monitor.** `do` decoded the 2xx body
  first and returned a `transportError`; the read now takes it undecoded through a `rawBody` sink,
  and a body that is not valid JSON is an `adSetBudgetsUnreadableError` — budgets unknown,
  delivery metrics kept.
- **`bidToMinorUnits` overflow guard restored.** A scaled bid at or above
  `float64(math.MaxInt64)` is refused exactly as `budgetToMinorUnits` refuses it, instead of
  converting to an implementation-defined (possibly negative) minor-unit bid.

No API, design, gen or migration change. See `architecture/account-monitor-endpoints.md` and
`code/internal-platform-meta.md`.
