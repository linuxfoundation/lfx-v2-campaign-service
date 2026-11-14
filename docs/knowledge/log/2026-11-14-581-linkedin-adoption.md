# 2026-11-14 — #581 LinkedIn adoption from the system row's accounts

**Update** — Step 5 of #581, behind the existing `LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED` flag. An
ops user can now bind a LinkedIn campaign built by hand in one of the LF system row's ad accounts
(its default account 538170226 or a grant such as CNCF) to a brief, so it shows in that project's
scoped monitor and reaches the pause and budget levers.

- `GET /projects/{project_id}/linkedin-ads/adoptable-campaigns?account_id=` (new,
  `list-linkedin-ads-adoptable-campaigns`): the account's ACTIVE/PAUSED campaigns with
  `recorded_in_project`. `marketing-ops-api` gateway rule (httproute + RuleSet + parity test).
- adopt-campaign takes optional `account_id` (linkedin-ads only). The service requires the flag
  and `global_marketing_ops` (the create path's fail-closed check, now
  `requireGlobalMarketingOps`) before the brief is read, and puts the account in the lookup's
  context (`model.WithAdoptionAccount`). An exact repeat returns the bound row without contacting
  LinkedIn; the project-connection path keeps its 409. New 400/500 arms for an unaddressable
  account and a missing or broken LF system row.
- Dispatch: `systemAccountResolverForAdoption` (system row, default account or grant),
  `ListAdoptableCampaigns`, `linkedin.Client.ListAccountCampaignRefs`. No migration.
- Docs: api-catalog (adopt row, new row, ops-only list), README, deployment, dispatch and
  account-monitor concepts.
- Tests: `TestLinkedIn_AdoptFromASystemAccount`, `TestLinkedIn_AdoptFromASystemAccountRefusals`,
  `TestLinkedIn_ListAdoptableCampaigns`, `TestAdoptCampaign_AccountIDIsOpsOnlyAndFlagGated`,
  `TestAdoptCampaign_WithoutAccountIDIsUnchanged`, `TestAdoptCampaign_SystemAccountAdoptionIsIdempotent`,
  `TestAdoptCampaign_SystemAccountLookupErrors`, `TestListLinkedinAdsAdoptableCampaigns`,
  `TestLinkedInAdoptableCampaignsIsAMarketingOpsRead`; fourteen mutations each fail them.
