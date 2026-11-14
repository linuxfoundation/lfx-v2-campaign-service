# 2026-11-14 — #581 LinkedIn account grants: storage and installer

**Creation** — First step of the owner's 2026-11-14 decision for #581 ("system-row grants"): the
LF system LinkedIn row gains an operator-installed allow-list of extra `(ad account, org)` pairs.
No dispatch behaviour changes in this commit; with no grants installed everything is as before.

- Migration `000058_linkedin_ads_account_grants` (000057 is taken by `feat/586-linkedin-video-ads`):
  typed table, `PRIMARY KEY (connection_id, account_id)`, numeric `CHECK`s, `ON DELETE CASCADE`.
- `model.Connection.LinkedInGrants`, `model.LinkedInAccountGrant`, `model.FindLinkedInGrant`.
- `ConnectionRepo.Get` loads grants for LinkedIn rows; `ReplaceLinkedInAccountGrants`
  (`domain.LinkedInGrantWriter`) replaces the set and bumps the version in one transaction.
- `bootstrap.InstallLinkedInGrants` + `bootstrap-system-account -linkedin-grant account:org[:label]`
  (repeatable), `-clear-linkedin-grants`, `-grants-only` (no stdin). Each grant is verified with
  `dispatch.VerifyLinkedInGrant` before anything is written.
- Tests: `TestLinkedInGrantsReplaceIsVersionGatedAndReadBack` (live),
  `TestInstallLinkedInGrants*`, `TestParseLinkedInGrantKeepsColonsInTheLabel`,
  `TestPlanGrantInstallRefusesFlagsThatCannotTakeEffect`, `TestVerifyLinkedInGrant`. Mutations
  (skip verification, skip validation, drop the version bump, drop the grant load, drop the
  grants-only row-flag guard) each fail them.
