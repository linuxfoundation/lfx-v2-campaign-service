# 2026-11-17 — #581 part C: adoption from the LF system row's accounts

**Update** — Part C of #581, stacked on part B: `list-linkedin-ads-adoptable-campaigns` and
adopt-campaign's `account_id`, behind `LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED` and
`global_marketing_ops`.

- Pre-PR review: the `CampaignAdopter` contract comment, the api-catalog adopt row and
  `internal-service.md` § Campaign adoption now scope "adoption never loads the LF row" to an
  adoption without `account_id`, and describe the `account_id` path (flag and ops gate before the
  brief load, 500 for a missing or broken LF row, 400 for an unaddressable account, 201 for an
  exact repeat).
