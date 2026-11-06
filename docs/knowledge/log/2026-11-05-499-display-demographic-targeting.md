# 2026-11-05 — #499 positive demographic targeting on Display

**Update** — `googleAdsConfig.displayDemographics` (`CampaignInput.DisplayDemographics`)
targets age ranges, genders, income ranges and parental statuses on every Display ad group,
matching the KubeCon NA 2025 Display reference, which targets as well as excludes.

- Each targeted dimension is declared `bidOnly: false` in the ad group's
  `targetingSetting.targetRestrictions`; the other demographic dimensions keep Google's default
  `bidOnly: true`, and `AUDIENCE` follows only when the group has audience lists. With neither,
  no `targetingSetting` is sent, as before.
- Each value is a positive ENABLED ad-group criterion in the same atomic mutate as the group's
  income and parental exclusions.
- A value both targeted and in its `excluded*` list is refused before any request, naming both
  fields. Every channel but Display refuses the field. It names no account resource, so no
  audience gate applies; its unprobed shape sits behind `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED`
  (#497's gate), which dispatch checks before anything upstream exists.
- New result fields: `adGroups[].demographicTargetingCriteriaIds` and
  `adGroupDemographicTargetingCriteriaIds`.

Built from the v23 field names. The positive criteria and the demographic `bidOnly: false`
restrictions on a Display ad group have not yet been checked with a validateOnly probe.
