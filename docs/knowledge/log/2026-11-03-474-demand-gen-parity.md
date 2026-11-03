# 2026-11-03 — #474 Demand Gen conversion bidding, devices, schedule, sitelinks and several ads

**Update** — The live KubeCon NA 2026 Demand Gen campaigns bid maximize conversions, exclude
Connected TV, carry an ad schedule and sitelinks, and run several ads per ad group. A Demand
Gen create here allowed maximize clicks only, refused devices, schedules and extensions, and
created one ad per ad group.

Each shape was checked by validateOnly probes on 2026-11-03 at v23, with the non-shared
budget every LFX create makes. `maximizeConversions: {}`, `targetCpa: {targetCpaMicros}` and
`maximizeConversionValue: {}` were accepted on a Demand Gen campaign, and
`maximizeConversions: {targetCpaMicros}` was refused (OPERATION_NOT_PERMITTED_FOR_CONTEXT).
That payload is version-scoped to v23: `target_cpa.*` left the selectable set in v25, so a
client version bump re-probes it.
The 2026-08-14 refusal that fenced the channel to maximize clicks
(BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET) was about a shared budget. Campaign
criteria `{device: CONNECTED_TV, bidModifier: 0}` and MOBILE at 0, a campaign ad schedule, a
sitelink asset with its SITELINK campaign asset, and two video ads in one ad group were
accepted. Demographic criteria on a Demand Gen ad group were refused
(CANNOT_ADD_AUDIENCE_SEGMENT_CRITERION_WHEN_AUDIENCE_GROUPED_IS_SET).

`demandGenBiddingStrategies` now holds maximize clicks (still the default), maximize
conversions, target CPA and maximize conversion value. A target CPA on Demand Gen renders as
the standalone `targetCpa` (`biddingPlan.standaloneTargetCPA`); target ROAS is refused there
because it was not probed.

Ad schedules and device criteria are Demand Gen CAMPAIGN criteria, sent by
`createCampaignTargetingCriteria` without the languages (which stay on the ad group) after
the conversion goal and before the ad groups, with ids in the new
`CampaignResult.CampaignTargetingCriterionIDs`. Only the probed shapes pass: a schedule
without a bid modifier and a device with bid modifier 0, CONNECTED_TV in the vocabulary for
this channel alone (`demandGenDeviceTypes`). Demographic exclusions stay refused, with a
message that says they belong inside the Audience `demandGenAudience` references.
`validateAssetPlan` admits sitelinks on Demand Gen and refuses the other extension types; the
cascade reuses `createCampaignAssets`.

`googleAdsConfig.demandGenAds` maps to `CampaignInput.DemandGenAds` (the single group) and a
`demandGenAdGroups` entry's `ads` to `DemandGenAdGroup.Ads` (`demandgen_ads.go`): up to five
named ads, each one image or video creative, labels unique within the group, a video ad named
`<ad group name> - <label>` within Google's 255 characters. Each ad is created by its own
requests; a failure on ad j returns ads 1..j-1 complete. Two video ads in one group were
probed; several image ads, or image and video mixed, were not, and are admitted unprobed. A
single group with several ads is recorded in `adGroups`, so launch flips every ad. Each
`adGroups` entry carries `plannedAds` and `ads` — one `{name, id, creativeAssetIds}` per ad
attempted — so a failed ad's assets are recorded against that ad, and the scalar
`creativeAssetIds` stays the first ad's beside `adId`. `googleAdsToggleTargets` reads a
group with fewer `adIds` than `plannedAds` as incomplete (`incomplete_ad_groups`) while
still toggling the ads it has. Dispatch mapping, stored-asset slots and
snapshot sanitization cover the lists. The api-catalog entries and the googleads and dispatch
knowledge concepts are updated.
