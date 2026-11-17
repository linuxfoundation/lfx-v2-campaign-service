# 2026-11-17 — #581 part C: adoptable candidates validated, valid adopt example

**Fix** — Two review findings on part C (PR #630).

- `linkedin.Client.ListAccountCampaignRefs` returned the monitor's campaign rows unchecked, so a
  page such as `{"elements":[{}],"metadata":{}}` became a candidate list of empty choices. It now
  refuses the whole list with `ErrUntrustworthyCampaignList` (503 from
  `list-linkedin-ads-adoptable-campaigns`, through `classifyDiscoveryError`'s default arm) when any
  page fails identityjson (a repeated key, exact or case-folded), or a row lacks a valid campaign id
  (`ValidateCampaignID`), a non-blank name, or a status of exactly ACTIVE or PAUSED, or an id appears
  twice across the walk. The monitor's own tolerance of an untrusted page is unchanged; the page
  trust it already computed is now carried as `budgetCurrencies.pageUntrusted`.
- adopt-campaign's published example combined `account_id` with `platform: google-ads`, which the
  service refuses. The `platform` attribute's example is now `linkedin-ads`, so the composed
  example (OpenAPI and CLI) is a valid LinkedIn adoption from the LF system account.
- Tests: `TestListAccountCampaignRefs_RefusesUntrustworthyCandidates` (15 cases),
  `TestListAccountCampaignRefs_DuplicateAcrossPages_IsRefused`,
  `TestListAccountCampaignRefs_ValidPage_IsReturned`, and the 503 arm in
  `TestListLinkedinAdsAdoptableCampaigns`. Every refusal case fails without the checks.
