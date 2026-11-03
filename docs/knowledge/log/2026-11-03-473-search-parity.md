# 2026-11-03 — #473 Search parity: display paths, ad-group negatives, income and parental exclusions

**Update** — The live KubeCon NA 2026 Search campaign uses settings an LFX-built Search
campaign could not carry: an RSA display path (`/kubecon`), ad-group-level negative keywords
beside the campaign-level ones, and income-range criteria. The NA 2025 Display reference also
excludes income ranges and parental status. Each behaviour below was checked with validateOnly
probes on 2026-11-03 (v23). The 15/16 path boundary was probed in ASCII; the double-width
weighting and the refused characters follow the RSA headline and Performance Max path rules and
were not probed separately.

- `path1`/`path2` (`CampaignInput.Path1`/`Path2`, `AdSpec.Path1`/`Path2`) are sent on the
  responsive search ad, each at most 15 display-width units (15 accepted, 16 refused
  `TOO_LONG`), `path2` only with `path1`. An ad that sets neither takes the campaign pair.
  Search only.
- `adGroups[].negativeKeywords` (`AdGroupSpec.NegativeKeywords`) are negative ad-group keyword
  criteria in the group's own criteria mutate, beside the campaign-level list, never inherited.
- `excludedIncomeRanges` and `excludedParentalStatuses` are negative AD-GROUP criteria on every
  ad group. Income ranges are taken on Search and Display. Parental status is taken on Display
  only, because Google refused it on a Search ad group (`OPERATION_NOT_PERMITTED_FOR_CONTEXT`).
  Both are refused on Demand Gen, Video and Performance Max.

`createAdGroupTargeting` now takes an `adGroupCriteriaSet`, so a Search group's keywords,
audiences, negatives and exclusions share one atomic `adGroupCriteria:mutate`. Display calls
it with the exclusions alone after its ad group is created. New result fields:
`adGroups[].negativeKeywordCriteriaIds`, `adGroups[].demographicCriteriaIds` and
`adGroupDemographicCriteriaIds`. Positive (targeting or observation) demographic criteria are
not added; no probe covered them.

Search campaign image assets (`AD_IMAGE`, `BUSINESS_LOGO`, `BUSINESS_NAME`) are left to a
follow-up: they need a fetch phase on the Search path and their own stored-asset slots.
