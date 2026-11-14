# 2026-11-13 — LinkedIn daily pacing no longer counts a daylight-saving hour as a day

**Fix** — `TestLinkedInMonitor_LocalCurrency` failed on a clean `main` from a Pacific-time
machine: pacing read 20% where 600/2900 ≈ 20.69% was expected. The cause was in the rule
engine, not in the test. `linkedinPacingPct` stepped its window back with `now.AddDate(0, 0,
-(days-1))` in whatever zone `now` carried. For the 30 days after a fall-back transition, that
put the window start 29 days and one hour back, and `math.Ceil` turned the hour into a 30th day.

- `linkedinPacingPct` (`internal/service/rules/monitor_linkedin.go`) now takes `now` in UTC
  before any arithmetic. That is the calendar `parseMonitorDate` and the LinkedIn analytics
  window already use, and it is how `redditPacingPct`'s daily branch already worked.
- Production runs in UTC, so live reads were not affected. Any caller with a zone-local clock
  was, including the tests.
- Without the fix, the new rules test fails on 348 samples: Los Angeles 2026-11-01..11-30,
  Berlin 10-25..11-23 and Sydney 04-05..05-04. It never fails in UTC.
- `TestLinkedInMonitor_LocalCurrency` now pins its clock to 2026-11-13 16:00 Los Angeles
  instead of `time.Now()`, so it is deterministic and fails without the fix under any `TZ`.
- Test: `TestLinkedinPacingPct_DaylightSavingDoesNotShiftTheWindow` (every day of 2026, four
  hours a day, in UTC, Los Angeles, Berlin and Sydney).
