# 2026-11-17 — #581 grant install on one connection, strict grant verification, legacy LinkedIn monitor campaigns

**Fix** — `ConnectionRepo.ReplaceLinkedInAccountGrants` now returns `ErrNotFound` or
`ErrPreconditionFailed` promptly on a pool of one connection. Before, when the version-gated
`UPDATE` matched no row, it called `r.Get` to tell a missing row from a stale version while its
transaction still held the pool's only connection. `Get` then waited for a second connection
that could never be free, and the installer failed with `context deadline exceeded` instead of
the documented error (Greptile on #628).

- The missing-vs-stale probe is now a `SELECT EXISTS (... WHERE project_id = $1 AND status <>
  'deleted')` run through `tx`, the same filter `Get` uses. No pool call is made while the
  transaction is open.
- The other new repository code in this PR (`Get` → `loadLinkedInGrants`) opens no transaction,
  so it had no such wait.
- Test: `TestLinkedInGrantsReplaceFailsPromptlyOnAPoolOfOne`
  (`internal/infrastructure/postgres/dbtest/linkedin_grants_live_test.go`) opens its own
  `pgxpool` with `MaxConns = 1` and checks that the stale-version and missing-row cases return the
  right error within a 5 s deadline, and that the connection is released afterwards. Without the
  fix, both cases ran until the 5 s deadline and returned `get connection: context deadline
  exceeded`.

**Fix** — The grant installer no longer accepts a pairing that LinkedIn does not state (Copilot on
#628). `VerifyLinkedInGrant` called `VerifyAccountOrgReference`, the connection test's check,
which returns nil when the account's `reference` is absent, person-scoped or malformed. In those
cases the installer stored an org id that LinkedIn never confirmed.

- New `linkedin.Client.VerifyGrantAccountOrg` (`internal/platform/linkedin/accounts.go`) is the
  strict form. LinkedIn must report an organization for the account, and it must equal `orgID`.
  Otherwise the result is a confirmed `ErrOrgVerificationFailed`. Both methods share
  `verifyAccountOrgReference`, and `requireReference` is the only difference.
- `VerifyLinkedInGrant` uses the strict form. The connection test still uses
  `VerifyAccountOrgReference` and keeps its permissive nil.
- Tests: `TestVerifyLinkedInGrant` (`internal/dispatch/linkedin_grants_test.go`) now covers the
  missing, empty, person-scoped, malformed and mismatched references, which are all refused, and
  the matching org, which is accepted. It also checks that the connection test still returns nil
  for the first three. When the installer is pointed back at the permissive check, the four
  "no stated org" cases fail.

**Fix** — A project on its own LinkedIn connection keeps its LinkedIn monitor when some of its
campaigns were created before accounts were recorded (lfx-one on #628). Since #581,
`requireRecordedMonitorProvenance` returned a 409 for the whole read whenever any live campaign
had no recorded `accountId`. Before #581 the monitor read the project's own account in full, so
after deploy such a project would have lost a monitor it used to have. Prod was not checked for
how many projects this affects.

- New optional capability `service.AccountMonitorConnectionScoper`
  (`internal/service/orchestrator.go`). Through `monitorScoperFor`, `scopeAccountMonitor` and
  `revalidateAccountMonitorScope` ask the dispatcher for a scoper that matches the connection
  serving the read. A project with no campaigns is answered before any credential is resolved.
- `LinkedInDispatcher.MonitorScoperForConnection` (`internal/dispatch/account_monitor_scope.go`)
  resolves the credential through `resolveLinkedInMonitorCredentials`:
  - On the project's own connection, an unrecorded campaign is kept, as belonging to that
    connection's account. `requireLinkedInManagedAccount` has already proven that account is
    `account_id`.
  - On the LF system row, or when the credential cannot be resolved, `linkedInMonitorScope` leaves
    the campaign out and logs a warning, so the whole monitor no longer fails. The response cannot
    report omitted campaigns without a change to its type.
  - A recorded campaign from another account is still a 409 on either connection.
- Omitting `account_id` when no live campaign records one is still a 409: `MonitorAccountForScope`
  has no context to resolve a connection with. A caller on its own connection passes its account,
  as it had to before #581.
- The design description of `monitor-linkedin-ads-account` and its Conflict text were updated
  (`make apigen`), along with `docs/api-catalog.md`, the account-monitor concept doc and
  `internal-dispatch.md`.
- Tests: `TestLinkedInMonitorScope` now expects the system rule to leave out unrecorded campaigns.
  New tests: `TestLinkedInMonitorScopeByConnection` and
  `TestReadAccountCampaignMetricsUsesTheConnectionScoper`. Both fail when the own-connection path
  is given the old strict rule and when the orchestrator hook is bypassed.
