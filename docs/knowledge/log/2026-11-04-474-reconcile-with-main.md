# 2026-11-04 — #474 reconciled with #469, #472, #479 and #481 on main

**Update** — #482 was rebased from the pre-merge #481 branch onto main, which now carries
#469 (Demand Gen audience and languages), #472 (Display audiences and placement exclusions),
#478, #479 (per-channel `utm_medium`) and #481 (Demand Gen video ads, several ad groups and
the ad-group audience gate).

Tracking: #479 tags every final URL with the channel's `utm_medium`, and #474's Demand Gen
sitelinks go through the same builder, so a Demand Gen sitelink is tagged `display`, not
Search's `paid-search`. `TestValidateAssetPlan_AcceptsSitelinksOnDemandGen` used to require
the Demand Gen sitelink to equal Search's byte for byte; it now requires `display` and
equality beyond the medium. The create-path tests pin the medium on the sitelink and on
every one of several ads, video and image. The api-catalog `sitelinks` entry and the
googleads concept say so.

Gate: #481's `googleAdsDemandGenAdGroupAudiencesSet` keys on a group's `audience`, whatever
its ads are, so a group with an `ads` list and an audience is refused with the flag off;
`demandGenAds` carries no audience and is not gated.
`TestGoogleAds_DemandGenAdsAndTheAudienceGate` pins both. #482 adds no input that names a
resource of the shared account (#468): `demandGenAds` and a group's `ads` carry images,
YouTube ids and a call-to-action enum, and the fields it newly admits on Demand Gen
(bidding, ad schedules, devices, sitelinks) name none.

Fences: #472's Display-only fields stay refused on Demand Gen, and #474's per-ad results
(`AdGroupResult.PlannedAds` and `Ads`) sit beside #481's `GeoCriterionIDs`,
`TargetingCriterionIDs` and `CreativeAssetIDs` without overlap.
