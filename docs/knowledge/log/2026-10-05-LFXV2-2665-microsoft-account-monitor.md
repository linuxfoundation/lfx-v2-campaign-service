# 2026-10-05 — Microsoft Ads account monitor, served from saved reports

**Creation** — `GET /projects/{project_id}/connection-microsoft-ads/account-monitor` joins the
four live account monitors. Microsoft was the only platform with campaign creation and no
monitor read, so a campaign created through this service was invisible to the Monitor tab.

It is not a port of the LinkedIn path, because that path cannot work for Microsoft. Its
delivery metrics come only from the Reporting service, and Microsoft documents that reports
"complete within minutes" and should be polled at 2–15 minute intervals, while the monitor read
has 20 seconds. So the read is split: the campaign list is read live; the metrics come from the
last report that finished, saved in the new `account_monitor_reports` table (migration
`000035`); and each request checks the pending report once or submits the next one, without
waiting. The response carries `metrics_as_of` and `metrics_pending` so a reader can judge the
numbers, and before the first report finishes every row is `fetch_failed` rather than a zero.

The pieces: `service.AccountReportReader` (a new optional capability beside
`AccountMetricsReader`), `Orchestrator.ReadReportedAccountCampaigns` (one call budget for the
whole sequence), `domain.AccountReportRepository` / `postgres.AccountReportRepo`
(compare-and-set on the report id), `microsoft` client primitives in `monitor.go`,
`dispatch/microsoft_monitor.go` (own connection only, bound account only, strict id check),
`rules.EvaluateMicrosoftMonitor` (shared ladder; Microsoft-specific HIGH findings for
`Suspended` and budget-exhausted statuses), and the HTTPRoute/RuleSet/parity rows. The store and
the capability are platform-neutral so a second asynchronous platform can reuse them.

It ships behind `MICROSOFT_METRICS_ENABLED`, like the per-campaign Microsoft read, because the
Reporting contract has still not been exercised against a live account.

**Also checked, no change.** The gap list flagged `microsoft.go`'s `applyCampaignConfig` call as
persisting raw config into the unencrypted `config_snapshot`. The repo's rule strips URLs and
URLs embedded in prose; `microsoftConfig` carries no URL field (the registration URL comes from
the brief and never enters it), only keyword text, match types, a timezone, a budget, a CPC bid
and geo codes. So nothing there is in the class the sanitizers exist for.
