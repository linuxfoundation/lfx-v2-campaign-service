# 2026-11-06 — #513 Search user lists observed at the campaign level

**Update** — `googleAdsConfig.campaignAudienceSegments` (`CampaignInput.CampaignAudienceSegments`)
observes existing user lists at the Search campaign level, matching the live KubeCon NA 2026
Search campaign (24188338792), which observes its five lists as campaign criteria under the
campaign's `targetingSetting` AUDIENCE `bid_only`. Until now campaign-service could only observe
them on each ad group (`audienceSegments`), so an ad group added later got none of them.

- The Search campaign create carries `targetingSetting.targetRestrictions [{AUDIENCE, bidOnly:
  true}]` only when the field is set; every other create keeps its payload.
- Each list is a positive ENABLED `campaignCriterion.userList` in one `campaignCriteria:mutate`
  after the language, schedule, device and demographic criteria. New result field:
  `campaignAudienceCriteriaIds`.
- Refused before any request off Search, beside `audienceSegments` or any
  `adGroups[].audienceSegments`, and for anything but a canonical user list on this account.
- Gated on `global_marketing_ops` through `service.GoogleAdsMarketingOpsInputs`. No deployment
  flag.

**Verification** — validateOnly, v23, account 8666746580, 2026-11-06, nothing persisted: the
exact campaign create, criteria and ad group bodies the branch sends, replayed in one
`googleAds:mutate` with temporary budget and campaign ids, were accepted, as were the criteria on
paused LFX Search campaign 24426212391. A custom audience as a Search campaign criterion was
refused (`OPERATION_NOT_PERMITTED_FOR_CONTEXT`). The `targetingSetting` doc comment in
`campaign.go` no longer claims an ad group's setting cannot coexist with the campaign's.
