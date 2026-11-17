# 2026-11-17 — #615 Meta and Reddit monitors read the LF system account

**Fix** — The Meta and Reddit account monitors can now read campaigns created through the LF
system account, as Google's can since #591. Before, both used only the project's own connection,
so a project with no connection of its own (prod `tlf`, 17 Nov), or any project while
forced-system creation is on, got a 404 for campaigns this service had created.

- The rule Google adopted in #591 now lives in `credsSource.monitorResolver`
  (`internal/dispatch/creds.go`): `resolveExisting` with the monitored `account_id` as the
  creation account, so the project's own row when it is configured for it, otherwise the LF
  system row when that one is, otherwise the project's own row again. When the LF system row is
  what resolved for an account it is not configured for, the read is a 404 (`noOwnConnection`)
  before any client is built or request sent.
- `MetaDispatcher.ReadAccountMonitor` resolves through the new `resolveMetaMonitorClient`;
  Reddit's `resolveMonitorClient` through the same resolver. `resolveGoogleAdsMonitorClient`
  calls it too, with no change in behaviour. `resolveOwnedMetaDiscovery` remains for
  `ProbeConnection` only.
- The account guards (`requireMetaManagedAccount`, Reddit's equality check) still run against
  whichever row resolved. Both reads stay reduced to the project's own recorded campaigns
  (`scopeAccountMonitor`, `filterMonitorRows`, 409 on a provenance mismatch), and a project
  with nothing recorded still gets an empty 200 with no upstream call.
- `account_id` stays required on both: the `AccountMonitorDefaulter` is still Google's only.
- The design descriptions of `monitor-meta-ads-account` and `monitor-reddit-ads-account` now
  state the credential rule (`make apigen` regenerated the specs and the generated service docs).
- Tests: `meta_monitor_fallback_test.go` and `reddit_monitor_fallback_test.go`. The
  `RefusesSystemFallback` tests for Meta and Reddit are removed, and the resolver-identity table
  now expects `resolveExisting` for their monitor entry points.
- LinkedIn's monitor is unchanged: it is not campaign-scoped, and stays with #581.

Not verified live: no Meta or Reddit call was made. Self-serve must still call the monitor for a
project without its own connection, as lfx-self-serve#5622 did for Google.
