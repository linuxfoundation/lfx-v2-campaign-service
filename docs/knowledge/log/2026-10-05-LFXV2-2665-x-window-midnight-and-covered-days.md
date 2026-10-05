# 2026-10-05 — Report-backed monitor windows: skipped DST midnights and the covered days

**Fix** — three follow-ups to the merged account-monitor PRs #254 and #255, each where the
service stated something about a window or an error that was not exactly true.

- **X: a skipped local midnight dated the window a day early.** `accountReportWindow` built its
  bounds with `time.Date(…, 0, 0, 0, 0, loc)`. Where a DST spring-forward skips 00:00
  (America/Santiago on 2026-09-06), Go normalizes the nonexistent midnight BACK to 23:00 of
  the previous day, so the window took an hour of the previous day and the persisted
  first/last day came out one day early. The new `localDayStart` returns the first instant
  that actually exists on the day (local midnight, or the instant after a skipped one), so
  queried == reported still holds. Two Santiago cases (the skipped day as today, and as the
  window's first day) pin it, and the queried == reported assertion now checks against
  `localDayStart`. The `ErrReportWindowNotWholeHours` and `accountReportWindow` comments now
  speak of day starts rather than midnights.
- **The covered window is exposed (copilot[bot] on #254).** Dropping a day from a 90-day X
  window across a DST fall-back made the read cover 89 days while the response echoed
  `days: 90`, described as "the trailing-days window this read covers", and the real window
  was never serialized. The account-monitor response now carries optional
  `metrics_window_start` / `metrics_window_end` (`YYYY-MM-DD`, first and last calendar day,
  both inclusive), set by the report-backed platforms from the saved report's own window
  (`model.ReportedAccountRead.MetricsWindowStart/End`) in `monitorReportedAccount`. On X the
  days are the account's local days; on Microsoft Ads they are the UTC dates sent as
  `CustomDateRangeStart`/`CustomDateRangeEnd` (inclusive), aggregated in the report's GMT
  (Europe/London) zone. Both are absent before the first report finishes and omitted on the
  four live-read platforms. `days` is now described as the REQUESTED window. Tests: X's 90-day
  fall-back read exposes 2026-08-24..2026-11-20 (89 days) with `days` 90; Microsoft's read
  exposes its window; no report yet → omitted (Microsoft and X); Google and Reddit omit them.
- **Corrected wording for the Microsoft retried-PUT rule (copilot[bot] on #255).** The
  `putUpdate` comment, the internal/platform/microsoft concept and the merged log entry
  `2026-10-05-LFXV2-2665-microsoft-budget-put-retry.md` said every non-success after a retry
  is wrapped in `retriedUnconfirmedError`. The code wraps only errors that are not already
  unconfirmed. The accurate statement, now in the comment and the concept: after a retried 429
  every failure is unconfirmed; only errors not already unconfirmed are wrapped in
  `retriedUnconfirmedError`. The merged log entry is left as it was; this entry supersedes its
  wording.
