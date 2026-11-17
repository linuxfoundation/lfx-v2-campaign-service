# 2026-11-17 — #581 the create-account read validates the LF system row

**Fix** — `get-linkedin-ads-create-account` now answers the documented 500 for an unusable LF
system connection (Copilot on #629). `LinkedInDispatcher.CreateAccount` read the row through
`creds.resolve`, which decrypts the credential but never validates it. An active system row
whose credential document was `{}` therefore answered 200, naming a default account that every
create would then fail on.

- When the resolved row is the system row, `CreateAccount` now runs it through
  `resolveLinkedInCredentials` (bound to the row already resolved, so nothing is read twice). An
  inactive, undecodable or token-less system row, or one with no account id, returns that error
  tagged `ErrSystemConnectionNotUsable`, which `classifyDiscoveryError` maps to 500.
- Unchanged: a project-owned row is not validated. The read still reports its stored status, so
  the Implement tab can tell the project to repair a connection it owns.
- Tests: `TestLinkedIn_CreateAccountReportsWhatACreateUses` gains a system row with `{}`
  credentials (must carry `ErrSystemConnectionNotUsable` and `ErrCredentialsIncomplete`) and a
  project row with the same document (must still report its account and stored status). Without
  the fix the system case returned the default account with a nil error.
  `TestGetLinkedinAdsCreateAccount` pins the service's 500 for that error.
