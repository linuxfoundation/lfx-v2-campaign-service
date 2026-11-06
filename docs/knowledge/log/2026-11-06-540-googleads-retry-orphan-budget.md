# 2026-11-06 — #540 a retry after a successful Google Ads create no longer leaves an orphan budget

**Fix** — Every Google Ads create now checks for a live campaign of the composed name before its
first write. A retry of a create that already succeeded therefore sends no
`campaignBudgets:mutate` and reports `campaign_name_taken` with the existing campaign's budget.

- **Evidence (#518 probe, 2026-11-06, account 8666746580).** A Demand Gen create succeeded
  (campaign `24433950855` on budget `16006171193`). A retry with identical inputs reused both
  Audiences by name and then created a new, unattached budget `16011237348` (5.00/day,
  `reference_count 0`). The campaign create then failed `DUPLICATE_CAMPAIGN_NAME`, and the result
  carried the NEW budget's id. The cause is that Google renames a non-shared budget to its
  campaign's name once attached. Nothing then holds the composed budget name, so the
  DUPLICATE_NAME reuse never ran.
- **Fix.** `refuseIfCampaignExists` (`internal/platform/googleads/campaign_precheck.go`) is called
  by Search, Demand Gen (before its Audiences), Display, Performance Max and the retained Video
  cascade. It runs the read-only `FindCampaignByName`, then on a hit `GetCampaignSettings` for the
  attached budget id. A hit returns the name partial with that `CampaignBudgetID`, no
  `CampaignID`, and `campaignExistsBeforeCreateError`, which `CreateOutcome` classifies as
  `campaign_name_taken`. An unverifiable read is a pre-create `(nil, err)`. The client never
  adopts; `adoptExisting` remains the only adoption path.
- **Unchanged.** When no campaign exists, every retry arm behaves as before: budget DUPLICATE_NAME
  reuse, ambiguous-create resolution, and Demand Gen Audience reuse (#529/#531).
- **Tests.** `campaign_precheck_test.go` has one test per channel (Demand Gen with and without
  Audiences). Each fails with that channel's call removed: the retry sends `campaignBudgets:mutate`
  (`audiences:mutate` for Demand Gen with Audiences). Two more tests cover the unverifiable-lookup
  and unreadable-budget refusals. `TestGoogleAds_Adoption_IsOptInAndOffByDefault` now pins that a
  dispatch without `adoptExisting` refuses rather than adopts. Not live-verified: no live call was
  made for this change.
