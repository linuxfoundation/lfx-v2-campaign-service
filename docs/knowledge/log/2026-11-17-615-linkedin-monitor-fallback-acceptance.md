# 2026-11-17 — #615 LinkedIn monitor fallback: acceptance tests

**Verification** — The LinkedIn half of #615 (account-monitor reads fall back to the LF system
account for a project with no LinkedIn connection of its own) shipped with #581 part A (#628,
v1.0.37): `resolveLinkedInMonitorCredentials` resolves through `resolveExisting` keyed on the
monitored account, the system credential reads only its default account or a grant, and the
orchestrator reduces the read to the project's recorded campaigns. Its tests pinned each rule on
its own, with fake dispatchers at the service layer.

`internal/dispatch/linkedin_monitor_fallback_test.go` now pins #615's acceptance end to end,
through the real `LinkedInDispatcher`, `Orchestrator` and (for an omitted `account_id`)
`ConnectionService.MonitorLinkedinAdsAccount`, as the Meta and Reddit fallback tests do:

- two projects with no connection of their own, both with campaigns on the shared system account
  538170226 (which also holds a campaign this service never recorded), each call the monitor
  without `account_id`; the response names 538170226, and each gets back only its own campaign
  with its own spend, impressions and totals, read with the system credential, with both the
  campaign list and the Ad Analytics read filtered to that account;
- a campaign in a granted account is read on that account (list and analytics) with the system
  credential;
- nothing recorded is an empty monitor naming no account, with no upstream call;
- an account the system row neither defaults to nor grants is a 404 with nothing sent, with
  forced-system mode off and on;
- a disconnected project still reads its system-account campaigns.

Mutation-checked: own-only monitor resolution (the pre-#628 rule), keeping every upstream row,
dropping the empty-scope early return, dropping the system-account guard, reading analytics on
another account, attributing one campaign's metrics to another, and ignoring the defaulted account
each fail the matching tests. No behaviour change.

Stale text corrected with it: `account-monitor-endpoints.md` still listed LinkedIn among the
own-connection-only, account-wide monitors with no scope 409; the `AccountMonitor.campaigns`
description and the LinkedIn audience method's description (and its `api-catalog.md` row) still
said account-wide and own-connection-only. So did the LinkedIn Audience Insights concept (and its
index description), the `api-catalog.md` account-monitor row, and the credential-resolver tables
in `code/internal-dispatch.md` and `code/internal-platform-linkedin.md`. All now describe the #581
behaviour, including the audience read's 409s on the system credential (an unrecorded creation
account, or an account the system row neither defaults to nor grants) as distinct from the
no-connection 404.
