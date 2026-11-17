# 2026-11-17 — #581 LinkedIn monitor scope-selection errors reach the caller

**Fix** — A LinkedIn account-monitor read whose credential cannot be resolved now fails with that
error instead of answering an empty monitor (Copilot on #628). `MonitorScoperForConnection`
returned nil on a resolution failure, which kept the LF system row's rule. That rule omits every
campaign that records no creation account, so when `account_id` was supplied and every campaign
was unrecorded the scope came out empty. `ReadAccountCampaignMetrics` then answered an empty
monitor before `ReadAccountMonitor` could report the failure. A broken connection, or an account
the connection does not serve, therefore looked like zero campaigns and zero spend.

- `service.AccountMonitorConnectionScoper.MonitorScoperForConnection` now returns
  `(AccountMonitorScoper, error)`. LinkedIn returns `resolveLinkedInMonitorCredentials`' error
  unchanged, the same one the read returns (the no-connection 404, connection not usable,
  `ErrAccountNotManagedByConnection`), so `classifyMonitorError` maps it exactly as before. A nil
  scoper with a nil error still means "keep the dispatcher's own rule".
- `monitorScoperFor` returns the error. `scopeAccountMonitor` and `revalidateAccountMonitorScope`
  fail the read with it, so the live and report-backed monitors both surface it.
- Unchanged: a project with no campaigns still gets its empty monitor before any credential is
  resolved. When the credential resolves to the LF system row, unrecorded campaigns are still
  omitted.
- Tests: `TestReadAccountCampaignMetricsUsesTheConnectionScoper` gains the all-unrecorded case
  (the dispatcher's own rule omits every campaign, the connection fails to resolve, and the read
  must return the error without reading) and a no-campaigns case where resolving would fail.
  `TestRevalidateAccountMonitorScopeSurfacesAResolutionFailure` covers revalidation.
  `TestLinkedInMonitorScopeByConnection` now expects the read's own error for an account no
  connection serves (`ErrNotFound`, identical to `ReadAccountMonitor`'s) and for an own
  connection naming another account (`ErrAccountNotManagedByConnection`). With the fallback
  restored, the service cases returned `Rows: []` with no error and the dispatch cases returned a
  nil error.
