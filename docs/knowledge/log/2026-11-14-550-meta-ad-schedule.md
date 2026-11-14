# 2026-11-14 — #550 Meta ad scheduling on lifetime budgets (gap 16)

**Creation** — Gap 16 of the Meta gap review (#550). A Meta ad set ran around the clock.
`metaConfig.adSchedule` (`days` 0–6 plus whole-hour `startMinute` / `endMinute`) is sent as the ad
set's `adset_schedule` with `timezone_type: ADVERTISER`, so the hours are the ad account's time zone
as the owner decided (2026-11-14), and `pacing_type: ["day_parting"]`, which a schedule needs.

Meta schedules only lifetime-budget ad sets, so a schedule on a daily budget is refused, as are no
days, a day outside 0–6, and minutes that are not whole hours with 0 ≤ start < end ≤ 1440. Every
refusal is a synchronous 400, re-checked in the job and in the client before any request. The budget
lever writes only the budget kind an ad set already has, so it cannot break the rule later.

Behind `META_AD_SCHEDULE_ENABLED` (default off) until a live probe. Not live-verified: the payload,
the whole-hour rule, and the settings readback, which does not report the schedule.

Tests: `delivery_options_test.go` (the schedule and pacing on the ad set, none without one, ten
refusals with no request) and `meta_ad_schedule_test.go` (flag off and on, a daily budget, wrong
types beside a broken budget, Dispatch's wiring and refusal with no request). Eighteen hand
mutations each fail a test.
