# 2026-11-16 — #584 a capped LinkedIn campaign runs on a continuous schedule

**Fix** — the first #584 commit sent a capped lifetime campaign with `totalBudget`,
`dailyBudget` AND `runSchedule.end`. LinkedIn documents the pair only as "a Daily Budget and a
Total Budget with a Continuous Schedule" (Lifetime Pacing), and its 2020 changes stopped
accepting a total and a daily budget on a fixed schedule, so the live create would most likely
have been refused. Misha Rautela's parallel #584 branch had it right; this ports her approach.

- `createSponsoredCampaign`: with a cap, `runSchedule` carries the start only. Without a cap
  the body is unchanged.
- `group_capped.go` (Misha's): the shared campaign group now bounds the campaign, so
  `ensureCampaignGroupCoversEnd(…, capped)` refuses a capped create into a reused group ending
  later or never, and any widening of a group holding a capped, no-end campaign. Both are
  `ErrCampaignGroupScheduleConflict`; before a write, a `PreflightRefusal` carrying
  `ErrCreateConfigInvalid`.
- Monitor (`linkedinExpectedSpend`, Misha's): a capped no-end campaign is paced against
  min(cap × window days, total).
- Settings readback: such a campaign's `end_date` reads `unknown` (`CompareSettingsField` with
  no upstream value), never `diverged`. Comment only.
- Tests: Misha's `group_capped_test.go` and monitor cases, with a `budgetCreateInput` helper;
  `TestCreateCampaign_LifetimeBudgetWithDailyCap` now asserts no `end` with a cap and an `end`
  without one. Restoring the end, or dropping the capped flag at the call site, fails a test.
- Not verified live: cs#579's first live create will confirm the shape.
