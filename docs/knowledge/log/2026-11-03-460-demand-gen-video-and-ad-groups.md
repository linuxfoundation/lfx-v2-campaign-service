# 2026-11-03 — #460 Demand Gen video ads and more than one ad group

**Update** — The live KubeCon NA 2026 YouTube campaign is a Demand Gen campaign with video
responsive ads in two ad groups. A Demand Gen create here had no video ad and exactly one ad
group, so that campaign could not be built at all.

Both shapes were checked by validateOnly probes on 2026-11-03 at v23. A Demand Gen
`adGroupAd` with `ad.demandGenVideoResponsiveAd` was accepted with YouTube video assets, an
image logo, `[{text}]` headlines, long headlines and descriptions, `businessName` as an
AdTextAsset object, and an optional CALL_TO_ACTION asset; `ad.name` is required. The YouTube
and call-to-action assets can be created in the same `googleAds:mutate` as the ad by
temporary resource name. Two Demand Gen ad groups under one campaign, each with its own video
ad, were accepted.

`googleAdsConfig.demandGenVideoCreative` maps to `CampaignInput.DemandGenVideoCreative`
(`demandgen_video_creative.go`): 1-5 bare YouTube ids, exactly one logo, 1-5 headlines (40,
probed: 41 refused), long headlines (90) and descriptions (90), a business name (25) and an
optional call to action from five probed CallToActionType values. The composed ad name is
held to Google's 255-character limit before the budget. The creative is refused off Demand
Gen and beside `demandGenCreative`. The logo is fetched before the budget and uploaded by
`assets:mutate`; then one atomic `googleAds:mutate` creates the YouTube and call-to-action
assets and the PAUSED ad, named `<ad group name> - Video ad`. A short 2xx from that request
is UNCONFIRMED but still records each same-account video or call-to-action asset id it
carried (#481 review), as the logo upload's short arm does.

`googleAdsConfig.demandGenAdGroups` maps to `CampaignInput.DemandGenAdGroups`
(`demandgen_adgroups.go`): up to five named groups, each with its own Audience and exactly
one image or video creative; geo and languages go on every group. Duplicate names, a group
without a creative, both creatives, a foreign Audience and campaign-level creative or
Audience fields set beside the list are all refused before the budget. The Demand Gen
cascade now walks a list of groups — one, built from the campaign-level fields, when the list
is empty, which leaves that path unchanged — and reports each group in `AdGroups` with the
first group in the scalar fields. A failure on group k returns groups 1..k-1 complete and what
exists of k.

The api-catalog Google Ads entries and the googleads and dispatch concepts are updated.

Tests: `demandgen_video_creative_test.go` and `demandgen_adgroups_test.go` (payloads,
refusals naming the group, the shared 64 MiB total, the ad-name limit, partial and
UNCONFIRMED results), `TestAdTypeWireKeysMatchTheAdOneof`, and
`googleads_demandgen_video_adgroups_test.go` (mapping, snapshot, pre-budget refusals through
dispatch).
