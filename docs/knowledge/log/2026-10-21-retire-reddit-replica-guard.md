# 2026-10-21 — retire the Reddit replica guard

**Fix** — the unconditional Reddit replica guard in
`charts/lfx-v2-campaign-service/templates/deployment.yaml` is removed. It was a render-time
`{{ fail }}` refusing `replicaCount` > 1 or autoscaling past one, and it broke the production
ArgoCD sync outright: `helm template` could not generate the manifest, so every unrelated change
to this service was blocked while the production values file sets three replicas.

Two things were wrong with it. Its own comment claimed "this costs the default deploy nothing: the
chart ships replicaCount: 1 and no HPA" — true of the chart default, and never checked against
`lfx-v2-argocd`'s values, which set three. And its premise over-read the Reddit account monitor:
the monitor is ungated by `REDDIT_METRICS_ENABLED` and therefore always ROUTED, but nothing POLLS
it — no ticker, cron or subscriber in `internal/` or `cmd/` reaches
`ConnectionService.MonitorRedditAdsAccount`, the audience read or the keyword read. So N replicas
× 45 report requests a minute against an ad account's 60 is a ceiling concurrent user traffic
could reach, not a rate the service sustains, and reaching it costs a 429 on a Monitoring-tab
read. Blocking all deploys to avert that is the larger harm.

The ledger in `internal/platform/reddit/report_budget.go` is unchanged and still per process. The
fix that lifts the ceiling is a shared ledger — a token bucket keyed by ad account in Postgres.
Recorded in the chart, the budget's doc comment and
`docs/knowledge/kubernetes/deployment.md`: two shortcuts that look like fixes are not. A lease
like the X stats jobs' (`postgres.StatsJobLease`) makes a non-owner pod refuse with
`domain.ErrStatsJobLeaseNotHeld`, which is fine for X's flag-gated background jobs and would fail
most Monitoring-tab loads here; and dividing 45 by the replica count leaves 15 at three replicas,
below the 24 a single audience read (`MaxReportCampaigns` 8 × 3 breakdowns) already needs.

Every other place that rested on one replica was swept at the same time, because the guard's
claim had propagated. `docs/knowledge/architecture/audience-insights-reddit.md` and
`keyword-performance-reddit.md` both still said the chart refuses more than one replica;
migration `000030_brief_delivery_stage_key.up.sql` said its safety rested on `Recreate` AND
`replicaCount: 1`, with "revisit if either changes" — a condition that had already fired, and a
hazard that does not exist: `Recreate` scales the old ReplicaSet to zero and waits for
termination before scaling up the new one, so the ordering guarantee is replica-count
independent and only restoring `RollingUpdate` reopens the overlap. The same correction applies
to `docs/knowledge/code/internal-infrastructure-postgres.md` and to the two chart comments on
`Replace=true` and the rollout strategy. One real consequence of three replicas had never been
written down and now is: `Recreate` makes every rollout a full outage, and the
`podDisruptionBudget` does not soften it because the Deployment controller deletes pods rather
than evicting them.

`TestDeploymentRefusesRedditReportsOnMultipleReplicas` is replaced by
`TestDeploymentAllowsMultipleReplicasForReddit`, which asserts the production shape renders AND
that the render carries the requested `spec.replicas` — a guard that clamped the count instead
of failing would otherwise pass. The X guard
(`TestDeploymentRefusesXStatsJobsOnMultipleReplicas`) is untouched and remains the chart's only
replica guard — it is flag-scoped, `TWITTER_METRICS_ENABLED` gates the X monitor too, and a
runtime lease already confines those stats jobs to one pod; its test no longer has to judge which
of two guards fired.

One consequence of retiring the Reddit guard is recorded beside the X guard rather than acted on:
until now the chart refused ANY render above one replica, so the X guard could never be the one
that fired. It is reachable from here, and turning `TWITTER_METRICS_ENABLED` on in a deployed
environment would reproduce exactly this incident. No environment sets the flag, so it is latent.
Whether it should also become a non-fatal signal needs the same poller audit the Reddit paths
got — the lease covers the background stats jobs but not the user-facing X monitor — and that
audit has not been done.
