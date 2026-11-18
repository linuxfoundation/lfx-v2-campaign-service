# 2026-11-17 — #633 a LinkedIn fixed-schedule lifetime budget must cover $10 per flight day

**Fix** — Prod campaign 930195766 was created with a $100 lifetime budget over 18 Nov–18 Dec
(31 days). LinkedIn derived $3.23/day, under its $10 daily minimum, so the campaign could not be
activated. The client checked only the flat $100 lifetime minimum.

- Rule (`linkedin.ValidateFixedScheduleLifetime`): an uncapped lifetime total on a fixed schedule
  must be at least max($100, $10 × days), at wire precision. Days are UTC calendar days from start
  through end, inclusive; a start that has begun counts from today.
- Checked before anything exists: `PreflightCreate` (synchronous 400), `Dispatch`, and
  `CreateCampaign` after `validateSchedule`.
- Budget edit (`WriteBudget`): a lifetime change on a campaign whose `runSchedule` has an end is
  held to the same floor over its whole schedule (400). `GetCampaignBudget` now reads
  `runSchedule`.
- Not affected: a lifetime budget with a daily cap (#584), which runs on a continuous schedule.
- Two existing tests used a $100 lifetime budget over 32 days, the shape this refuses. They now use
  a 10-day flight.
- Tests: `budget_flight_test.go` and `linkedin_lifetime_floor_test.go`. Disabling the rule fails
  all seven new tests.
- Not verified live: whether LinkedIn checks a mid-flight edit over the whole schedule or as
  remaining budget over remaining days. This uses the whole schedule; the two readings refuse
  different edits, so it stays open until a live check.
