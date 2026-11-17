# 2026-11-14 — #563 Microsoft device, age and gender bid adjustments at create

**Update** — Gap 8 of the Microsoft Advertising gap review (#563) adds
`microsoftConfig.deviceBidAdjustments`, `ageBidAdjustments` and `genderBidAdjustments`, objects of
segment name to whole percent, sent as biddable `DeviceCriterion`, `AgeCriterion` and
`GenderCriterion` entries on the new campaign.

- `microsoft.ValidateBidAdjustments` holds the rules: known segments only (any casing, each once),
  devices -100..900, age and gender -90..900, whole percents.
- Set on the campaign rather than the ad group, in one call right after the ad schedule and before
  the ad group, fail-closed like the other campaign criteria; a campaign found by name stops
  before the ad group. Ids are recorded as `bidAdjustmentCriterionIds`.
- A wrong type, a rule breach, or any of them while `MICROSOFT_BID_ADJUSTMENTS_ENABLED` is off is a
  synchronous 400. The chart sets the flag to `"false"`. Not role-gated.

Not verified live: no Microsoft call was made. Before the flag is turned on, a live create must
confirm that a device criterion can be added to a new campaign.
