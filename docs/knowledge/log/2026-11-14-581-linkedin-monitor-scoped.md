# 2026-11-14 — #581 LinkedIn monitor and insights are campaign-scoped and read granted accounts

**Update** — Step 3 of #581. A project creating through the LF system row (tlf, event children, a
disconnected project) can now monitor its LinkedIn campaigns and read their audience insights;
before, both answered "no LinkedIn Ads connection of its own" (404).

- `LinkedInDispatcher` implements `service.AccountMonitorScoper` + `AccountMonitorDefaulter`
  (`linkedInScopeForAccount`, strict recorded provenance), mirroring Google Ads #591/#592.
- `ReadAccountMonitor` resolves with `resolveExisting(account_id)`; the system credential reads only
  its default account or a grant. Audience insights resolve on the account the scope records, and
  through the system row require every campaign to record it.
- `monitor-linkedin-ads-account`: `account_id` optional, 409 `AccountMonitorScopeConflictError`
  declared (design + `make apigen`); `classifyMonitorError` covers LinkedIn.
- Behaviour change: a project with its own LinkedIn connection now sees only its recorded campaigns
  in the monitor (was account-wide).
- Docs: api-catalog monitor row, architecture.md Account Tenancy and Current Platform Accounts,
  account-monitor endpoints concept.
- Tests: `TestLinkedInMonitorScope`, `TestLinkedIn_ListAccountCampaignMetrics_SystemRowReadsOnlyItsOwnAccounts`,
  `TestLinkedIn_ReadAudience_SystemRowNeedsRecordedGrantedAccounts`,
  `TestMonitorLinkedinAdsAccount_IsScopedAndDefaultsTheAccount`, `TestClassifyMonitorError`,
  `TestResolverIdentity_LinkedIn` (now `resolveExisting` for the three reads). Each fails under the
  matching mutation.
