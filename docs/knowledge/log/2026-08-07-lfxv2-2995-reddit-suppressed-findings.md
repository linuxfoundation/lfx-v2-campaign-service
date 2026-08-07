# 2026-08-07 — Reddit metrics: full request-body assertions, gate documented in the chart concept

**Update** — Closed the suppressed Copilot findings on PR #75
(`internal/platform/reddit/metrics_test.go`, `docs/knowledge/kubernetes/deployment.md`, and
the PR description).

`TestGetCampaignMetrics_HappyPath` asserted the response mapping, the method, the path, the
`data` envelope and `campaign_ids` — but nothing else in the request body. The three
unasserted fields are precisely the ones that decide WHICH numbers come back:
`starts_at`/`ends_at` (produced by `dateRangeForWindow`, including the last-month
month-end boundary), `breakdowns`, and `fields`. A wrong date range or a dropped metric
yields a well-formed 200 for the wrong reporting period or with a metric silently missing,
and nothing downstream can detect either. All four are now asserted against literals — the
client's clock is pinned at 2026-07-01 by `fixedRedditClock`, so `last_7_days` is
`2026-06-25`..`2026-07-01` — matching the coverage the X Ads metrics test already had.
Revert-verified twice: shifting the window to `-13` days names the wrong `starts_at`,
dropping `spend` from the field list names the short list.

`REDDIT_METRICS_ENABLED` changes the Deployment's `app.environment` contract, and was
documented in `docs/api-catalog.md`, `internal-dispatch.md` and `internal-platform-reddit.md`
but not in the Kubernetes concept a cluster operator actually reads before setting values.
`docs/knowledge/kubernetes/deployment.md` now covers it alongside the other plain
(non-secret) values: default `"false"`, fails closed on anything that is not exactly
`"true"` (so a typo in a values override cannot enable an unverified integration), read per
call rather than at construction — a restart picks up a new value, no rebuild — and with the
gate off the read answers `ErrMetricsUnsupported`, which the service maps to the same 400 a
platform with no metrics adapter at all returns. **A config key documented only where the
code lives is undocumented for the person who sets it.**

The PR description carried two claims that were true when written and are not now. It said
Reddit metrics reads "ARE live and available to callers" — the gate lands them disabled by
default, which is the entire point of the gate — and it listed the shared
`model.MetricsWindow` / `model.CampaignMetrics` / `service.MetricsReader` scaffold as part of
this change, which was accurate while the branch was cut straight from `main` and stopped
being accurate when it was rebased onto the metrics foundation. Both corrected, with the
scaffold now called out explicitly as NOT in this diff.

The log-fragment-filename finding from the same review (`2026-08-06-reddit-metrics-null-data-test.md`
missing its ticket) was already resolved on this branch: the file is
`2026-08-06-lfxv2-2995-reddit-metrics-null-data-test.md`. No change needed.
