# 2026-11-05 — #499 Display demographic targeting narrows through negatives

**Fix** — #507 sent each targeted demographic dimension `bidOnly: false` in the Display ad
group's `targetingSetting`. A validateOnly probe on 2026-11-05 (v23, paused LFX Display campaign
24367877221, ad group 204064966271) refused that with
`TARGETING_SETTING_DEMOGRAPHIC_CRITERION_TYPE_GROUPS_MUST_BE_SET_TO_TARGET_ALL`: Google requires
every demographic dimension to target all. Every create using `displayDemographics` would
therefore have failed at the ad group, after the budget and campaign existed. The field was
behind `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED` (off), so nothing reached the account.

- `displayAdGroupTargetingSetting` is the pre-#499 setting again: the five-entry list (four
  demographic dimensions `bidOnly: true`, then `AUDIENCE` `bidOnly: false`) when the group has
  audiences, and nothing otherwise.
- A targeted dimension now narrows through NEGATIVE criteria. The named values are positive
  ENABLED criteria, and every other value of that dimension (UNDETERMINED included unless named)
  is a negative criterion, less values an `excluded*` list already excludes. This is how the
  Google Ads UI stores checked and unchecked brackets. The same probe accepted positive
  `ageRange`/`gender`/`incomeRange`/`parentalStatus` criteria beside a negative `ageRange` in one
  `adGroupCriteria:mutate`.
- `displayDemographics` is classified as ungated in the marketing-ops field list (#505), which
  crossed with #507 and left that test failing on main.
- The `displayDemographics` gate check no longer shadows the dispatcher receiver.
