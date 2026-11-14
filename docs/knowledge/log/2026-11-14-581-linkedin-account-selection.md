# 2026-11-14 — #581 LinkedIn create-time account selection from the system row's grants

**Update** — Step 4 of #581, behind the new `LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED` flag (default
`"false"`; chart, README and deployment concept updated). Ships after steps 1–3, so every campaign
created in a granted account is already pausable, readable and monitorable.

- `linkedInConfig.adAccountId` may name one of the LF system row's grants while the flag is on; the
  campaign is created in that account under the grant's org and records it
  (`dispatch.selectLinkedInAccount`). Anything else is a synchronous 400 from `PreflightCreate`
  (`ErrCreateConfigInvalid`) and a pre-create refusal in `Dispatch`.
- `service.LinkedInMarketingOpsInputs` gates any non-empty `adAccountId` on `global_marketing_ops`
  while the flag is on (2026-11-05 owner decision: shared-account selectors stay ops-only).
- Pickers `list-linkedin-ads-audiences` / `-conversions` take optional `account_id` (design +
  `make apigen`), restricted to the resolved account or a grant (`model.WithPickerAccount`).
- With the flag on, the system scope's `connection-linkedin-ads/accounts` returns the default account
  and the grants, with no LinkedIn call.
- Docs: api-catalog (LinkedInConfig ad account, pickers, accounts), design/brief.go config text.
- Tests: `TestLinkedIn_CreateInAGrantedAccount`, `TestLinkedIn_AdAccountIDRefusals`,
  `TestLinkedIn_PickersAndAccountsListFollowTheGrants`, `TestLinkedInMarketingOpsInputs_AdAccountID`,
  `TestLinkedInPickerContextCarriesTheAccount`; each fails under its mutation.
