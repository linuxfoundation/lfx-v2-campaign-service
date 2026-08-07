# 2026-08-07 — LFXV2-2023: pin the discovery wiring and the nil-result guard

**Update** — Review round on the account-discovery endpoint. Three findings, all the same
class: the endpoint is wired and the guards are written, but the tests beside them prove
less than they appear to.

**Fix** — `TestConnectionService_AccountDiscoveryNeedsTheOrchestrator` calls
`SetOrchestrator` by hand, so it proves discovery is 503 without the injection while never
executing a line of `container.go`; deleting either injection site left it green.
`TestContainer_BothStartupPathsInjectTheOrchestrator` now asserts the call in
`wireLiveBackends` (live pool at boot) and in `retryDatabaseInit` (cold start behind an
unreachable DB), each bounded at the next top-level func so one cannot cover for the other.
Both sites need a live pgxpool to reach in a unit test, so this follows the source-assertion
pattern already used for the shutdown-order invariants in this package. Verified binding by
deleting each call in turn.

**Fix** — `TestOrchestrator_ReadAccounts_NilNilListerResultIsError` covers the guard that
turns a lister returning `(nil, nil)` into a 503. The account tests otherwise only used a
non-nil EMPTY slice, which returns through the happy path and never evaluates the branch —
the same trap the metrics path already has a dedicated test for. It also asserts the error is
NOT `ErrAccountsUnsupported`: that sentinel is a 400 meaning the platform has no lister at
all, and this platform has one that misbehaved.

**Fix** — Two source comments still described the pre-endpoint world.
`GoogleAdsDispatcher.ListAccounts` called itself a staged adapter that nothing calls, and
`domain.ErrAccountsUnsupported` called itself reserved and unreferenced. Both now describe
the wired behaviour, including where the sentinel is produced and why it maps to 400.
