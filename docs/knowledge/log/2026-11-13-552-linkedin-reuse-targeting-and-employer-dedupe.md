# 2026-11-13 — #552 LinkedIn: no creatives on a reused campaign with targeting; employer aliases count once

**Fix** — two #557 review findings on `linkedInConfig.targeting`.

- **Reused campaign.** `createSponsoredCampaign` reuses a same-name campaign in the event's group,
  and caller targeting reaches only the body of a campaign this run creates. A retry could
  therefore attach creatives to a campaign without the requested matched audiences or registrant
  exclusions while the row recorded them. With `Targeting` set, `CreateCampaign` now stops right
  after the campaign is ensured — no conversion, image, dark post or creative, and the audience
  is not touched — and returns the partial result (group, campaign id and name) with a
  `targeting NOT applied to reused campaign` error for reconciliation, as the Microsoft client
  does for its create-time-only targeting. A profile create keeps reusing the campaign as before.
  Pinned by `TestCreateCampaign_ReusedCampaignWithTargetingStopsBeforeCreatives`.
- **Employer aliases.** `cleanFacet` counted `urn:li:company:<n>` and `urn:li:organization:<n>`
  as two values, so both forms for 101 ids were refused at the 201st URN although the merge and
  the wire carry 101 employers. `employers` are now de-duplicated by `employerKey` before the cap,
  keeping the first URN. Pinned by `TestValidateTargeting_EmployerAliasesCountOnce`.
