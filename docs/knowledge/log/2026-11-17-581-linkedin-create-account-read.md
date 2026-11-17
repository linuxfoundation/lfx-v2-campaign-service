# 2026-11-17 — #581 LinkedIn create-account read: the account a create actually uses

**Update** — Adds `GET /projects/{projectId}/linkedin-ads/create-account`
(`get-linkedin-ads-create-account`, `campaign_manager`), the backend contract for
lfx-self-serve#5764 rows 1/21. Prompted by the 2026-11-17 prod test on `tlf`: the Implement tab's
account dropdown is not sent (self-serve's `buildLinkedInConfig` drops `adAccountId`), so every
create went to the stored account, which was on billing hold, and nothing showed which account
that was.

- Reports the account and org a create resolves to (`creds.resolve`: own connection, LF system
  fallback, forced-system redirect), `source` (`project`/`system`), the connection's stored
  `status`, and `selectable_accounts`: what `linkedInConfig.adAccountId` may name (the default,
  plus the system row's grants while `LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED` is on).
- Stored ids only; nothing is sent to LinkedIn, so billing-hold or serving state is not reported.
- Chart: `linkedin-ads/create-account` added to the `project-api` rule and the HTTPRoute matcher;
  parity cases added.
- Tests: `TestLinkedIn_CreateAccountReportsWhatACreateUses`, `TestGetLinkedinAdsCreateAccount`;
  each fails under its mutation.
