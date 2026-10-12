# 2026-10-07 — LFXV2-2665 a stale pending insight report no longer blocks the new period

**Fix** — #292 review (Greptile P1), both report-backed Microsoft reads (keyword and audience share
the step). When a `today` or `this_month` report was still BUILDING as the UTC date changed, the
read dropped the previous period's finished report but left the old pending report in the key's
single pending slot, and nothing is submitted while a report is pending — so the current period
showed nothing, with nothing building, until the old report finished or was abandoned (up to an
hour).

`supersedeOtherPeriodPending` now runs before the collect step: a pending report whose stored
dates (`pending_window_start` / `pending_window_end`, already in 000038/000041 — no migration) are
not the dates the window means now is cleared through the existing compare-and-set on the pending
id (recorded as last failure "superseded: …") without being polled, and the current period's report
is submitted on the same read (`metrics_pending: true`). A request that loses the compare-and-set
adopts the pending half as it now stands instead of submitting again. A late collection of the old
report cannot complete, and is never served as the new period (`discardOtherPeriod`). Tests pin a
day and a month boundary for both reads, including the lost race. See
[Microsoft keyword insights](../architecture/microsoft-keyword-insights.md).
