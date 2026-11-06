# 2026-11-05 — #497 observe-only demographics on Search ad groups

**Update** — `googleAdsConfig.observedDemographics` (`CampaignInput.ObservedDemographics`)
puts the `AGE_RANGE`, `GENDER` and `INCOME_RANGE` dimensions in observation mode on every
Search ad group, matching the live KubeCon NA 2026 Search campaign. Each named dimension is
declared `bidOnly: true` in the ad group's `targetingSetting.targetRestrictions`, after
`AUDIENCE` when audience segments are set. Every value of it that is not excluded gets a
positive ENABLED ad-group criterion in the group's criteria mutate, after the exclusions.

- An exclusion in the same dimension keeps working: excluded values get no observed criterion.
  A dimension whose every value is excluded is refused.
- `PARENTAL_STATUS` is refused on Search (`OPERATION_NOT_PERMITTED_FOR_CONTEXT`, probed
  2026-11-03 for the exclusion). Every other channel refuses the field before the budget.
- New result fields: `adGroups[].observedDemographicCriteriaIds` and
  `adGroupObservedDemographicCriteriaIds`.

Built from the v23 field names. The positive demographic criteria and the demographic
`bidOnly` restrictions on a Search ad group have not yet been checked with a validateOnly
probe, so the field is behind a new deployment gate, `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED`
(chart default `"false"`). Dispatch refuses a non-empty list on any channel before anything
upstream exists unless the gate is exactly `"true"`. Turn it on in lfx-v2-argocd once the probe
passes.
