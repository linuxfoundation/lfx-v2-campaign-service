# 2026-11-26 — #672 Account monitors: opt-in account-wide read for global_marketing_ops

**Update** — The Google Ads, LinkedIn, Meta, Reddit and X account monitors take an optional
`scope=account`: every campaign on the account, whoever created it, each row naming the project
that recorded it (`recorded_project_id`, absent for a campaign created outside LFX). Only
`global_marketing_ops` is admitted (gateway rule on the project-api route plus a service-side 403),
and only while `ACCOUNT_MONITOR_ACCOUNT_SCOPE_ENABLED` is `true` (400 otherwise; the chart sets
`"false"`). Requests without `scope` are unchanged. See
[Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md).
