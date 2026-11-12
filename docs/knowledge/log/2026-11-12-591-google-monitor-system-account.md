# 2026-11-12 — #591 Google Ads monitor reads the LF system account

**Fix** — The Google Ads account monitor can now read campaigns created through the LF system
account. Before, it used only the project's own connection, so a project with no connection of
its own, or any project while forced-system creation is on, got a 404 for campaigns this service
had created.

- `GoogleAdsDispatcher.ReadAccountMonitor` reads `account_id` through
  `resolveGoogleAdsMonitorClient`. That resolves via `credsSource.resolveExisting` with
  `account_id` as the creation account: the project's own row when it is configured for it,
  otherwise the LF system row when that one is. The system credential reads no other customer
  (404, no upstream call).
- The read is still reduced to the project's own recorded campaigns (`scopeAccountMonitor`,
  `filterMonitorRows`). That reduction is why the round-16 refusal is no longer needed.
- `account_id` is optional on `monitor-google-ads-account`. Omitted,
  `Orchestrator.DefaultMonitorAccount` names the customer the project's campaigns record
  (`AccountMonitorDefaulter`, Google only). No campaigns gives an empty monitor. Live campaigns
  under more than one customer, or none recording one, give the 409 `campaign_account_mismatch`.
  Soft-deleted campaigns, used only when there are no live ones, never cause a 409: unrecorded
  ones are skipped and the customer recorded most is named.
- A project that disconnected its own connection can still monitor its own system-created
  campaigns, as it can already pause them.
- `resolveOwnedGoogleAdsDiscoveryClient` is removed. `resolveOwnedGoogleAdsDiscovery` remains for
  `ProbeConnection`. The client construction is shared in `googleAdsDiscoveryClientFor`.
- LinkedIn, Meta and Reddit monitors are unchanged.

Not verified live: no Google Ads call was made.
