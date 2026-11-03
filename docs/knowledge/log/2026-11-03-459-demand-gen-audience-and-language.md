# 2026-11-03 — #459 Demand Gen takes one Audience and languages on its ad group

**Update** — A Demand Gen campaign built here could carry no audience and no language: the
preflight refused `AudienceSegments` and `Languages` on that channel. The live KubeCon NA
2026 Demand Gen campaigns are built on both.

Three facts shape the change, each verified by a validateOnly probe on 2026-11-03 at v23.
A Demand Gen ad group is AUDIENCE-GROUPED: Google refuses a user list or custom audience
attached to it directly (`CANNOT_ADD_AUDIENCE_SEGMENT_CRITERION_WHEN_AUDIENCE_GROUPED_IS_SET`),
even with `useAudienceGrouped=false`, so the segments must sit inside a Google Ads Audience.
The ad group takes exactly one `audience` criterion (`ONE_AUDIENCE_ALLOWED_PER_AD_GROUP`).
It takes `language` criteria next to it, while a campaign-level language on Demand Gen is
refused. A foreign customer id is refused too, but only after the budget.

`googleAdsConfig.demandGenAudience` (`internal/dispatch/googleads.go`) maps to
`CampaignInput.DemandGenAudience`: one existing `customers/<id>/audiences/<id>` resource,
never created here. `validateDemandGenAudience` (`demandgen_targeting.go`) refuses it off
Demand Gen, refuses any other shape, and refuses another account's Audience, all before the
budget mutate. `validateCriteriaPlan` now refuses only ad schedules, devices and demographics
on Demand Gen, so `Languages` reach it. `AudienceSegments` stay refused there, with a message
naming the audience-grouped reason and `DemandGenAudience`.

`createDemandGenAdGroupTargeting` attaches the Audience and the languages in one atomic
`adGroupCriteria:mutate` after geo, kept apart from geo's mutate, with the usual partial
result. Ids land in `AudienceCriteriaIDs` and `TargetingCriterionIDs` (adGroupCriterion ids
here). The api-catalog `audienceSegments`, `demandGenAudience` and `languages` entries, and
the googleads and dispatch concepts, say the same.

Tests: `demandgen_targeting_test.go` (payload, refusals, partials),
`TestValidateCriteriaPlan_AcceptsLanguagesOnDemandGen`, and
`TestGoogleAds_DemandGenAudienceAndLanguagesReachTheAdGroup` /
`TestGoogleAds_DemandGenAudienceRefusalsArePreCreate` through dispatch.
