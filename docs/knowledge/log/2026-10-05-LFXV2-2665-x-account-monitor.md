# 2026-10-05 — X Ads account monitor, served from saved stats jobs

**Creation** — `GET /projects/{project_id}/connection-twitter-ads/account-monitor` joins the
account monitors as the second REPORT-BACKED one, on the machinery the Microsoft monitor
introduced: `TwitterDispatcher` implements `service.AccountReportReader`, and the read goes
through `Orchestrator.ReadReportedAccountCampaigns` and the `account_monitor_reports` table
unchanged — no migration, no fork.

Report-backed for every `days` value, not only the long ones. X's synchronous stats are capped
at 7 days per request and share one 250-requests-per-15-minutes budget across every foundation
on the LF token; its asynchronous stats jobs cover up to 90 days. One path means a 7-day and a
30-day view are read the same way.

The pieces: `internal/platform/twitter/monitor.go` (`ListAccountCampaigns` with line-item
flights in the account's timezone; `SubmitAccountCampaignReport` — `active_entities`, then one
paced stats job per ≤20 active campaigns, returned as ONE comma-joined composite report id, or
the sentinel `none` when nothing was active; `CheckAccountCampaignReport` — one job-status read,
then unsigned, bounded gzip downloads; `ValidateMonitorAccountID`; a new `WithClock` option;
embedded `time/tzdata`), `internal/dispatch/twitter_monitor.go` (own connection only, bound
account only, strict id check, cached client so job creation shares the write pacer),
`rules.EvaluateTwitterMonitor` (daily budget first, else total prorated over the flight;
zero-delivery HIGH gated on the flight; no conversions rule, since X conversions are never
reported), `MonitorTwitterAdsAccount` with its own descriptor, the design method, and the
HTTPRoute/RuleSet/parity and drift-test rows. Twitter moved into the shared discovery+monitor
branch of the HTTPRoute regex, which is now three branches.

No enable flag: X's per-campaign metrics read has never had one to ride, unlike Microsoft's.
The X contract is followed from docs.x.com and has not been exercised against a live account;
the open points are marked UNVERIFIED in `monitor.go`.
