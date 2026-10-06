# 2026-10-05 — Microsoft keyword insights, served from saved reports

**Creation** — `GET /projects/{project_id}/microsoft-ads/keywords` (`get-microsoft-ads-keywords`)
gives the Optimize tab's keyword table a Microsoft source; until now only Google had one. The
Google read is Google-named, so this is a parallel method returning Google's row type unchanged
(`criterion_id` = `KeywordId`, `ad_group_id` = `AdGroupId`, the pair the Microsoft keyword
actions take), plus `metrics_as_of`, `metrics_pending` and `conversions_complete`.

Microsoft serves keyword performance only through the asynchronous Reporting service, so the
read reuses the account monitor's saved-report pattern: `service.KeywordReportReader`
(`KeywordReportAccount` with every refusal and no upstream call, then submit and a single-poll
check), `Orchestrator.ReadReportedKeywordPerformance`, and a sibling store,
`keyword_insight_reports` (migration `000038`, expand-only) — a sibling rather than a `kind`
column because its key is a window and 000035's `days` is in that table's primary key. A saved
report is served only while it covers every campaign the project owns. The report scope is
`Campaigns` only (never `AccountIds`), from the project's own connection and bound account, with
the provenance rule of the Google read (any mismatched campaign → 409). Columns and scope were
verified against learn.microsoft.com; nothing has been run against a live account, so the read
is behind `MICROSOFT_METRICS_ENABLED`.

Audience demographics are not added: Microsoft's age/gender report has no device dimension. The
keyword pause/remove and negative-keyword writes are a separate branch. See
[Microsoft Keyword Insights](../architecture/microsoft-keyword-insights.md).
