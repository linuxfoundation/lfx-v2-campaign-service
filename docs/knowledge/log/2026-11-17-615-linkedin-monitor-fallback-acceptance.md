# 2026-11-17 — #615 LinkedIn monitor fallback: acceptance tests

**Verification** — The LinkedIn half of #615 (account-monitor reads fall back to the LF system
account for a project with no LinkedIn connection of its own) shipped with #581 part A (#628,
v1.0.37): `resolveLinkedInMonitorCredentials` resolves through `resolveExisting` keyed on the
monitored account, the system credential reads only its default account or a grant, and the
orchestrator reduces the read to the project's recorded campaigns. Its tests pinned each rule on
its own, with fake dispatchers at the service layer.

`internal/dispatch/linkedin_monitor_fallback_test.go` now pins #615's acceptance end to end,
through the real `LinkedInDispatcher` and `Orchestrator`, as the Meta and Reddit fallback tests
do:

- two projects with no connection of their own, both with campaigns on the shared system account
  538170226 (which also holds a campaign this service never recorded), each name the account
  through `DefaultMonitorAccount` without an upstream call and each get back only their own
  campaign, read with the system credential;
- a campaign in a granted account is read on that account with the system credential;
- nothing recorded is an empty monitor with no upstream call;
- an account the system row neither defaults to nor grants is a 404 with nothing sent, with
  forced-system mode off and on;
- a disconnected project still reads its system-account campaigns.

Mutation-checked: own-only monitor resolution (the pre-#628 rule), keeping every upstream row,
dropping the empty-scope early return, and dropping the system-account guard each fail the
matching tests. No behaviour change.
