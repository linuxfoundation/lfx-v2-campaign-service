# 2026-10-15 — Microsoft Ads is credentials-first: admitted to account discovery at both gates

**Update** — issue #181 (the Microsoft half; X was admitted in LFXV2-3319).

- `accountDiscoveryProviders` (`internal/bootstrap/sysacct.go`) now includes
  `ProviderMicrosoftAds`, so `bootstrap-system-account -provider microsoft-ads` accepts a first
  install with no `-account-id` and `-clear-account-id` on a Microsoft row. Microsoft held both
  halves already: discovery (`MicrosoftDispatcher.ListAccounts`, LFXV2-3064) and a `Dispatch` that
  resolves through `validateMicrosoftConnection`, which tags an empty id with
  `ErrAccountNotSelected`.
- In the same change `MicrosoftAdsConnectionConfig` (`design/connection.go`) dropped
  `Required("account_id")`, regenerated with Goa (`AccountID` is now `*string`), so an account-less
  connection can be created over HTTP too. The two gates were relaxed together, as X's were.
  An explicit `""` still fails the pattern; only the missing key moved in `apivalidation`.
- `customer_id` is unchanged: already optional at both gates and absent from `requiredConfigKeys`.
  Discovery walks every customer the credential reaches without it, and the client omits the
  `CustomerId` header when it is empty. A later `-account-id` rotation keeps a stored one.
- Tests: `TestInstallMicrosoftIsCredentialsFirst` (install with credentials only, with and without
  `customer_id`; bind the discovered id; clear it; shapes still enforced),
  `TestCreateMicrosoftAds_WithoutAccountID`, `TestUpdateMicrosoftAds_BindsDiscoveredAccountToCredentialsOnlyRow`,
  and the `apivalidation` missing-key case. `TestAccountDiscoveryProvidersIsASubsetOfAccountListers`
  now derives its candidates from `model.AllProviders()` filtered by `IsPaidAds()`.
- Concepts updated: `code/internal-bootstrap.md`, `code/internal-service.md`,
  `code/internal-dispatch.md`. Rosters in `README.md` and `docs/api-catalog.md` updated to match.
- Still outside the map: Reddit and LinkedIn. Both are eligible (LinkedIn's create path names a
  missing account through `resolveLinkedInCredentials` since #352) and are deliberately not
  admitted, a sequencing decision.
