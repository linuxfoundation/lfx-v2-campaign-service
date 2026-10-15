# 2026-10-15 — LinkedIn's create path resolves through the shared resolver

**Fix** — `LinkedInDispatcher.Dispatch` now names an unselected account.

- `Dispatch` validated the connection inline, so an empty account id failed with a bare
  `notCreated` carrying no reason sentinel, while toggle and metrics already tagged it through
  `resolveLinkedInCredentials`. `Dispatch` now calls that helper (bound to `d.creds.resolve`, so
  creation is still governed by the forced-system flag) and wraps its error in `notCreated()`
  itself, so the dispatch claim is still released. The org id stays an inline, creation-only check.
- An account-less connection now carries `ErrConnectionNotUsable` + `ErrAccountNotSelected`, and
  the #208 job result reads the fixed connection-class sentence with
  `(reason: account_not_selected)`. Inactive, undecodable and incomplete credentials on create are
  tagged the same way as on toggle and metrics, including the LF-system attribution.
- LinkedIn now holds both bootstrap eligibility halves but is NOT added to
  `accountDiscoveryProviders`; admission is a separate decision.
- Tests: `TestDispatch_LinkedInUnselectedAccountNamesTheMissingChoice` (both scopes),
  `TestLinkedIn_DispatchIsGovernedByTheForcedSystemFlag`, and a Dispatch row in
  `TestLinkedIn_UnusableReasonsAreClassified` and `TestLinkedIn_UnusableConnectionIsTaggedOnEveryPath`.
- Concepts updated: `code/internal-dispatch.md`, `code/internal-bootstrap.md`,
  `code/internal-service.md`.
