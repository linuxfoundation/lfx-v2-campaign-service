# 2026-11-17 — #581 part B: create-time account selection and the create-account read

**Update** — Part B of #581, stacked on part A: `linkedInConfig.adAccountId` may name one of the LF
system row's grants (behind `LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED`, default `"false"`), the
pickers take `account_id`, and `GET /projects/{projectId}/linkedin-ads/create-account` reports the
account a create actually uses.

- Pre-PR review: the pickers honour a granted `account_id` only while the flag is on, like every
  other way of naming a grant (`TestLinkedIn_PickersAndAccountsListFollowTheGrants` gains the
  flag-off case); `TestListLinkedinAdsLookups_PassTheAccountDown` drives both picker handlers.
- api-catalog: the accounts-list change is a new line under LinkedIn Ads gotchas rather than an
  edit to the long `{provider}/accounts` row; the ad-account section points at `create-account`'s
  `selectable_accounts`.
