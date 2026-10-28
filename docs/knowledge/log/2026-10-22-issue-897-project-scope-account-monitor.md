# 2026-10-22 — project-scope the account monitors on shared ad accounts

**Fix** — the Google Ads, Meta, Reddit and X account monitors now return only the requesting
project's own campaigns (lfx-self-serve-ops#897, a follow-up to ops#54). Each of those platforms
runs ONE ad account shared by every foundation, and the monitors read that account whole, so a
`campaign_manager` of one project saw every foundation's campaigns, budgets and spend.

**The direct-call path was open too (the issue's ask 2).** Authorization is a single gateway
check — `campaign_manager` on `project:{projectId}` — and the service only verified identity, so
calling the endpoint directly leaked exactly as the BFF did. lfx-self-serve#4093 contained the
leak in the BFF by making those monitors root-only; this change fixes it in the service, for
every caller, ROOT included, so that gate can later be lifted.

**How.** A new optional dispatcher capability, `service.AccountMonitorScoper`, marks a platform
as shared-tenancy; the Google Ads, Meta, Reddit and X dispatchers implement it
(`internal/dispatch/account_monitor_scope.go`) by delegating to the provenance helpers the
keyword and audience reads already use. `ReadAccountCampaignMetrics` and
`ReadReportedAccountCampaigns` read the project's scope from SQL first. An empty scope answers
200 with an empty monitor and no upstream call. Otherwise the scope is checked against
`account_id`, and the account read is filtered to the scoped ids before the rule engine and the
totals run. X's saved report keeps the account-wide rows; only the response is scoped.
The project's soft-deleted campaigns stay in the monitor, because a local delete leaves the
upstream campaign running. Each one is shown when it is proven to belong to `account_id` and
dropped (logged) otherwise, never a 409, since a deleted row has no re-dispatch remedy.

**New 409s.** A recorded campaign whose provenance names another account (or, on Reddit, none)
fails the read closed rather than returning a partial picture. Google, Meta and Reddit gain a
`ConflictError` response. X's `AccountMonitorConflictError` gains `reason` values
`campaign_account_mismatch` and `campaign_scope_invalid`.

**Behaviour shifts.** ROOT now also sees project-only data on these four monitors. A project
with no campaigns and no connection gets 200 empty instead of 404, the trade-off the insight
reads already make.

**Unchanged.** LinkedIn (an account per foundation) and Microsoft keep their account-wide reads.
Microsoft's tenancy (the issue's ask 3) is still not recorded, so `architecture.md` keeps it
"Not yet recorded". The BFF follow-up — lifting `requireSharedAccountMonitorAccess` on the
Meta/Reddit/X arms and adding a Google campaign-service arm — is separate work in lfx-self-serve.
