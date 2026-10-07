# 2026-10-07 — LFXV2-2665 Microsoft age/gender audience insights

**Update** — Added `get-microsoft-ads-audience` (`GET /projects/{project_id}/microsoft-ads/audience?window=`,
`campaign_manager`): Microsoft Advertising age/gender buckets (`age_group`, `gender`, impressions,
clicks, `cost_micros` in the account currency, `ctr`) across the project's own campaigns, served
from a saved asynchronous `AgeGenderAudienceReportRequest` exactly as the keyword read is served
from its saved keyword report (`metrics_as_of`, `metrics_pending`, `data_incomplete`; same
`MICROSOFT_METRICS_ENABLED` gate and 400, same window enum, scope rules and error classification).
There is no device dimension — Microsoft's age/gender report has none, and a device breakdown
would need a second report (out of scope); the Google-shaped `ReadAudienceInsights` still refuses
Microsoft.

The saved-report store now holds two report kinds: migration `000041` (expand-only) adds an
`age_gender_`-prefixed column set to `keyword_insight_reports` rather than a `report_kind`
discriminator, which would have had to join the primary key the N-1 binary's upsert infers. The
keyword statements are unchanged; collect/refresh in the orchestrator and the four repo operations
are now generic over the kind. Contract verified against learn.microsoft.com only; not exercised
against a live account. See [Microsoft keyword insights](../architecture/microsoft-keyword-insights.md).
