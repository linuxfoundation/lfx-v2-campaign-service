# 2026-11-06 — the X stats-job replica guard becomes a NOTES.txt warning (#381)

**Fix** — `charts/lfx-v2-campaign-service/templates/deployment.yaml` no longer `fail`s the render
when `TWITTER_METRICS_ENABLED` is effectively on with `replicaCount` > 1 or autoscaling past one.
The same check, moved to `lfx-v2-campaign-service.xStatsMultiReplica` in the new
`templates/_helpers.tpl`, now prints a warning from the new `templates/NOTES.txt`. The chart has
no replica guard left. Deployments run three replicas (`lfx-v2-argocd`
`values/global/lfx-v2-campaign-service.yaml:4`), so with the old guard, turning the flag on in any
deployed environment would have stopped ArgoCD generating the manifest, which is the outage #378
fixed for Reddit. No environment sets the flag, so the guard had not fired yet.

The poller audit the issue asked for came first and found nothing polling the X paths:

- **The lease already covers the monitor.** The issue said the lease covered only the
  background stats jobs and left the user-facing X monitor uncovered. That is wrong. The
  monitor's job-creating step, `TwitterDispatcher.SubmitAccountReport`
  (`internal/dispatch/twitter_monitor.go:181`), calls `ownStatsJobs` before it contacts X, just
  as the audience read does (`internal/dispatch/twitter_audience.go:92`). A non-owner pod skips the
  submission and serves the saved report.
- **campaign-service.** No ticker, cron or subscriber in `internal/` or `cmd/` reaches
  `ConnectionService.MonitorTwitterAdsAccount` (`internal/service/connection_monitor.go:498`) or
  `Orchestrator.ReadReportedAccountCampaigns`. The service's tickers are the recovery, stuck-claim,
  job-retention, archived-asset and indexer-relay sweeps, the shutdown in-flight wait and the
  wizard progress stream. The one loop is `cmd/verify-metrics -poll-interval`
  (`cmd/verify-metrics/reads.go:251`). It is an operator CLI that runs its own in-process
  orchestrator for a bounded `-timeout`, and is not deployed.
- **lfx-self-serve.** `GET /api/campaigns/twitter/monitor` (`apps/lfx-one/src/server/routes/campaigns.route.ts:164`)
  calls campaign-service only from `getTwitterMonitor` (`campaign.controller.ts:4261`), which
  the UI calls only from `fetchReportBackedData` (`monitoring-tab.component.ts:616`). That function
  runs only on a user action: a date range change, a platform switch, the connection list landing,
  or the panel's Refresh button (`monitoring-tab.component.html:572`). Nothing calls it on an
  interval. When a report is still building, the tab tells the user to refresh in a few minutes
  (`monitoring-tab.component.ts:1301`) and does not re-read. The only `timer(0, …)` poll in
  `campaign.service.ts` (`:1299`) is `pollJobStatus`, which polls campaign-create jobs.
- **lfx-v2-argocd.** The flag appears in no values file, and no CronJob targets this service.

`TestDeploymentRefusesXStatsJobsOnMultipleReplicas` is replaced by
`TestDeploymentWarnsOnXStatsJobsWithMultipleReplicas`, which runs the same effective-value matrix
plus the production shape (flag on, three replicas). In every case the Deployment must render
and carry the requested `spec.replicas`. The `NOTES.txt` warning, read from a `helm install
--dry-run=client`, must appear exactly when the effective flag is on above one replica. Mutation
checks: the test fails if the `fail` is put back, if replicas are clamped to 1, if the helper never
fires, if it ignores `extraEnv`, autoscaling or `valueFrom`. The comments and docs that described
the refusal were updated: `values.yaml`, `README.md`, `docs/api-catalog.md`, `pacer.go`,
`client.go`, `domain/stats_job_lease.go`, and the deployment, internal-dispatch and
internal-platform-twitter knowledge pages.
