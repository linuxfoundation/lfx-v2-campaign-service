# 2026-11-07 — #563 Microsoft remarketing lists at create (observed and excluded)

**Update** — `microsoftConfig.audienceIds`, `excludedAudienceIds` and `audienceBidAdjustment`
(gap 4 of the Microsoft Advertising gap review, #563) put remarketing lists on the ad group a
Microsoft create makes. Observed lists are bid only and carry an optional whole-percent bid
adjustment. Excluded lists suppress, for example registrants.

- The rules live once in `microsoft.ValidateAudiences`: decimal ids, at most 50 per list, no
  repeat, no id in both lists, and an adjustment from -90 to 900 that is accepted only with
  observed lists. `MicrosoftDispatcher.PreflightCreate` refuses a breach or a wrong JSON type with
  a synchronous 400 (`invalid campaign config: microsoftConfig.<field>: ...`).
- Before anything is created, one `GetAudiencesByIds` call (`Type: RemarketingList`) must return
  every id as a remarketing list. Otherwise the job fails with nothing created.
- After the keywords, the lists are attached to the confirmed ad group with
  `AddAdGroupCriterions` (`CriterionType: Audience`): exclusions first as
  `NegativeAdGroupCriterion`, then observed lists as `BiddableAdGroupCriterion` with a
  `BidMultiplier`. Outcomes are recorded as `audiences` in the result (a 4152 duplicate is
  `ALREADY_PRESENT`). On an ad group reused from an earlier attempt no list is sent, because
  its status and audience setting are not read. A shortfall makes the campaign
  `created_degraded` and never fails the job.
- The lists belong to the shared LF account, so a microsoft-ads create that sets either list
  needs `global_marketing_ops` on the project (`service.MicrosoftMarketingOpsInputs`, the same
  single check as the other platforms; 403, or 503 when the check cannot be made).
- Default-off deployment flag `MICROSOFT_AUDIENCES_ENABLED`; the chart sets `"false"`.

Not verified live: no Microsoft call was made. The shapes are from the Campaign Management v13
reference. Before turning the flag on, a live create should confirm the lookup, the criterion
echo, and that a new ad group really defaults to bid only for audiences.
