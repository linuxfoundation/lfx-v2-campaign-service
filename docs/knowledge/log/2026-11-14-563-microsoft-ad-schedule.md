# 2026-11-14 — #563 Microsoft ad schedule (dayparting) at create

**Update** — Gap 10 of the Microsoft Advertising gap review (#563) adds
`microsoftConfig.adSchedules`, `[{day, startHour, endHour, bidAdjustment?}]`, sent as
`DayTimeCriterion` entries on the new campaign.

- `microsoft.ValidateAdSchedules` holds the rules: at most 49 windows, Monday..Sunday, whole hours
  0-23 to 1-24, end after start, no overlap on a day, bid adjustment a whole percent -90..900.
- On a campaign this run created the windows are attached in one call right after the radius
  targets and before the ad group, fail-closed like the other campaign criteria; a campaign found
  by name stops before the ad group. Ids are recorded as `adScheduleCriterionIds`.
- A wrong type, a missing or fractional hour, a rule breach, or the list while
  `MICROSOFT_AD_SCHEDULE_ENABLED` is off is a synchronous 400. The chart sets the flag to
  `"false"`. Not role-gated.

Not verified live: no Microsoft call was made. The shape is from the Campaign Management v13
`DayTimeCriterion` reference.
