---
type: "Architecture Doc"
title: "Microsoft Keyword Insights"
description: "The Microsoft Advertising keyword and age/gender audience reads behind the Optimize tab: Google's keyword row shape and (age group, gender) buckets with no device dimension, both scoped to the project's own campaigns on its own connection and served from saved asynchronous reports (one store holding two report kinds) with metrics_as_of and metrics_pending."
resource: "internal/service/keyword_report.go"
---

# Microsoft Keyword Insights

Also the Microsoft age/gender audience read — see [Audience](#audience-age-and-gender-no-device).

`GET /projects/{project_id}/microsoft-ads/keywords?window=` (`get-microsoft-ads-keywords`,
`campaign_manager`, LFXV2-2665) — the Microsoft counterpart of
`GET /projects/{project_id}/google-ads/keywords`, so the Optimize tab's keyword table works for
both platforms.

## Exposure

The Google read is Google-NAMED (`get-google-ads-keywords`, `/google-ads/keywords`), not
platform-generic, so Microsoft gets a parallel method rather than a `platform` parameter. Its
rows are the Google read's row type (`GoogleAdsKeyword`) unchanged: `criterion_id` is the
Microsoft `KeywordId` and `ad_group_id` its `AdGroupId`, the same numeric (ad group, keyword)
pair a Microsoft keyword action takes. `cost_micros` is Microsoft's `Spend` (account currency, no
FX) × 10⁶, `ctr` is clicks/impressions as a fraction, `quality_score` is absent when Microsoft
reports `--`. The envelope repeats Google's four fields and adds three:

- `metrics_as_of` / `metrics_pending` — exactly as the report-backed account monitors publish
  them (see [Account-Monitor Endpoints](account-monitor-endpoints.md)).
- `conversions_complete` — false when Microsoft left a row's `ConversionsQualified` blank
  (typically no Universal Event Tracking); those rows carry `conversions: 0`, which is then not a
  measurement. Google's row type requires `conversions`, so the caveat lives on the envelope.
- `data_incomplete` — the served report's "Potential Incomplete Data" flag: the window's last day
  may still be aggregating, so counters may still rise.

`window` accepts `today`, `last_7_days`, `last_30_days` (default), `this_month`, `last_month` —
the windows the Microsoft client maps to a UTC date range; the decoder refuses the other two, and
the service's own 400 (for a non-HTTP caller) is built from the same set, so it never lists a
window this read refuses. The RESPONSE's `window` uses the same five-value enum, so the
generated contract never advertises `yesterday` or `last_14_days` as a value this read can
return. It deliberately does not go through the Google reads' window helper.

**Two refusal messages are a cross-repo contract.** lfx-self-serve's Microsoft keyword table
shows "not connected" rather than a read failure on exactly two campaign-service messages: the
404 `no microsoft ads connection configured for this project` and the 400 `keyword and audience
insights are not supported for this platform` (also returned while `MICROSOFT_METRICS_ENABLED`
is off). Neither carries another discriminator, so
`TestGetMicrosoftAdsKeywords_PinsTheNotConnectedMessages` pins both strings; rewording either
needs the matcher there changed in step.

## Audience: age and gender, no device

`GET /projects/{project_id}/microsoft-ads/audience?window=` (`get-microsoft-ads-audience`,
`campaign_manager`, LFXV2-2665) — Microsoft's `AgeGenderAudienceReportRequest`, served by the
same saved-report machinery, gate, window enum, scope rules and error classification as the
keyword read (`Orchestrator.ReadReportedAudience`, `service.AudienceReportReader`,
`internal/dispatch/microsoft_audience_report.go`, `internal/platform/microsoft/audience_report.go`).

- **No device dimension.** `AgeGenderAudienceReportColumn` carries `AgeGroup` and `Gender` but no
  device column, so this is NOT the Google audience read's age/gender/device shape:
  `Orchestrator.ReadAudienceInsights` still answers 400 "not supported" for Microsoft, and a device
  breakdown would need a second report (a second pending half per key, half-finished answers) —
  out of scope.
- **Request:** `Type: AgeGenderAudienceReportRequest`, `Format: Csv`, `Aggregation: Summary`, no
  `TimePeriod` (not allowed with Summary), `ReturnOnlyCompleteData: false`, no `Filter`/`MaxRows`,
  `Columns: CampaignId, AgeGroup, Gender, Impressions, Clicks, Spend`, `Scope.Campaigns` only, the
  shared UTC `reportTime` — `submitCampaignScopedReport`, shared with the keyword report so neither
  can drift onto a wider scope.
- **Fold:** one row per (campaign, age group, gender), columns by header name (all six required, a
  repeated header refused), a repeated key SUMMED as the keyword fold sums; a non-id campaign, a
  blank/invalid-UTF-8/control-or-format-character/over-64-byte label, a negative or non-finite counter, an
  overflow or more than 64 distinct (age, gender) pairs fails the WHOLE read (no partial rows).
- **Response** (`MicrosoftAdsAudience`): `buckets` of `{age_group, gender, impressions, clicks,
  cost_micros, ctr}` summed over the campaigns the project owns NOW, impressions-descending;
  `bucket_count`; `metrics_as_of`, `metrics_pending`, `data_incomplete` as on the keyword read.
  No `account_currency` (the report has no currency column) and no conversions.
- **Errors:** the keyword read's, with the audience scope sentinels
  (`ErrAudienceScopeTooLarge`/`ErrAudienceScopeInvalid`, shared with the Meta audience read) so a
  409 names the read that refused; the gated-off 400 is the keyword read's exact text.

## Why a saved report

Microsoft serves keyword performance only through the asynchronous Reporting service
(`KeywordPerformanceReportRequest`), which takes minutes; the read has 20 seconds. So it reuses
the account monitor's pattern: serve the last finished report, check a pending one once per
request, submit the next when none is building and the last is missing, older than 30 minutes, or
does not cover every campaign the project now owns. The first read returns no rows with
`metrics_pending: true`.

A finished report is served ONLY while it covers the project's current campaign scope. A
campaign dispatched after the report was built makes the next read return no rows (and submit a
new report) rather than a partial table the response could not mark as partial — the same
refuse-rather-than-under-report rule the Google read applies to a mixed-account scope. Rows for a
campaign the project no longer owns are dropped from a covering report.

The store is a sibling table, `keyword_insight_reports` (migration `000038`), not a `kind`
column on `account_monitor_reports`: its key is a reporting window rather than a day count (and
`days` is in 000035's primary key), each half records its campaign scope, and its rows are a
different type. See [internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).

**One store, two report kinds** (`model.InsightReportKind`: `keywords`, `age_gender`). Migration
`000041` adds an `age_gender_`-prefixed READY/PENDING/last-failure column set to the same row
rather than a `report_kind` discriminator: a discriminator would have to join the primary key,
and the N-1 binary's `ON CONFLICT (project_id, platform, account_id, report_window)` needs a
unique index on exactly those four columns, so that would not be expand-only. Each kind's four
statements name only its own columns (`KeywordReportRepo` implements both
`domain.KeywordReportRepository` and `domain.AudienceReportRepository`), so a keyword report can
never be read, completed, failed or served as an audience report, and one kind building never
blocks the other's mark. The orchestrator's collect/refresh steps are one generic implementation
over both kinds (`insightReportDriver`, `internal/service/insight_report.go`).

## Trust boundary

- The project's OWN Microsoft connection only (`resolveOwned`; a project with none is 404, never
  served from the LF system account), and only the account it is bound to.
- The scope is the project's own campaigns, read from this service's database by `project_id`;
  an empty scope answers an empty result without touching the store or any upstream call. The
  rollout gate and the window are checked FIRST, by `KeywordReportEnabled` (no connection, no
  scope), so with `MICROSOFT_METRICS_ENABLED` off a project with no Microsoft campaigns gets
  the same 400 as one with many, not an empty 200 that depends on project data.
- The report `Scope` is `Campaigns` only — never `AccountIds`, because Microsoft documents the
  scope as the UNION of its elements.
- Provenance: if ANY campaign in scope records a creation account other than the bound one, the
  read is 409 (`ErrCampaignAccountMismatch`) rather than the matching subset.
- The scope is de-duplicated (one Microsoft campaign can be held by two live rows — the scope
  query's DISTINCT includes the result blob and Microsoft has no live-row uniqueness index), with
  the provenance check still run over every row. More than 300 DISTINCT campaigns (the documented
  `Campaigns` ceiling) is 409 (`ErrKeywordReportScopeTooLarge`).
- A stored campaign id that is not a canonical Microsoft id is 409
  (`ErrKeywordReportScopeInvalid`), refused locally — it would otherwise fail every submission
  and leave the read silently empty.
- Microsoft rejecting the campaign-only scope itself (error 2027) is the same rejection on every
  read, so it is tagged `ErrServiceDefect` (a logged 500) and fails the read, instead of being
  logged as a transient submit failure forever. The scope is not widened to `AccountIds`.
- Every other refusal happens in `KeywordReportAccount` (which re-checks the gate and window),
  before the store or any upstream call.
- Off unless `MICROSOFT_METRICS_ENABLED=true` (400 "not supported"), like the other Microsoft
  reporting reads, because the Reporting contract has not been exercised against a live account.

## Acting on a row: the Microsoft campaign-ref lookup

A row's `campaign_id` is Microsoft's numeric CampaignId, while the Microsoft keyword levers
(`apply-keyword-actions`, `add-negative-keywords`) are keyed by this service's campaign UUID
under its brief. `GET /projects/{project_id}/microsoft-ads/campaign-ref?platform_campaign_id=`
(`resolve-microsoft-ads-campaign`, `campaign_manager`, LFXV2-2665) bridges the two, as
`google-ads/campaign-ref` does for Google:

- Same payload (digits only, no leading zero, within int64), same `platform-campaign-resolution`
  result and same errors as the Google method; both call one helper,
  `resolvePlatformCampaignRef` in `internal/service/connection_keywords.go`, with the platform
  fixed by the route, so one route never answers for the other platform's id space.
- A pure read of this service's own `campaigns` rows (`CampaignRepo.ResolvePlatformCampaign`,
  project-scoped in SQL, soft-deleted rows excluded). Microsoft is never contacted and the route
  is not gated on `MICROSOFT_METRICS_ENABLED`; the lever it addresses runs its own ad-account
  and connection guards.
- Unowned id: 200 with an empty `matches`. Storage fault: 500. Backend not wired: 503.
- **More than one match is reachable here**, unlike Google: migration 000020's unique index is
  scoped to `google-ads` because Microsoft ids are minted per ad account, so a project
  re-pointed between accounts can hold two live rows for the same id. Every match is returned and
  the caller must refuse rather than pick.
- Meta, Reddit and X gained the same lookup in LFXV2-2665 (`/{meta,reddit,twitter}-ads/campaign-ref`)
  through the same helper, each with its own platform id rule; see `internal-service`.

## Sources

Verified 2026-10-05 against learn.microsoft.com (Reporting v13):
[KeywordPerformanceReportRequest](https://learn.microsoft.com/en-us/advertising/reporting-service/keywordperformancereportrequest?view=bingads-13),
[KeywordPerformanceReportColumn](https://learn.microsoft.com/en-us/advertising/reporting-service/keywordperformancereportcolumn?view=bingads-13),
[AccountThroughAdGroupReportScope](https://learn.microsoft.com/en-us/advertising/reporting-service/accountthroughadgroupreportscope?view=bingads-13),
[KeywordStatusReportFilter](https://learn.microsoft.com/en-us/advertising/reporting-service/keywordstatusreportfilter?view=bingads-13),
[AgeGenderAudienceReportColumn](https://learn.microsoft.com/en-us/advertising/reporting-service/agegenderaudiencereportcolumn?view=bingads-13);
and 2026-10-07:
[AgeGenderAudienceReportRequest](https://learn.microsoft.com/en-us/advertising/reporting-service/agegenderaudiencereportrequest?view=bingads-13),
AgeGenderAudienceReportColumn again (required columns AgeGroup, Gender, TimePeriod — TimePeriod
"expected for all aggregation types except Summary").
UNVERIFIED LIVE: the CSV column spellings in a real download, whether a campaign-only scope draws
error 2027, how `QualityScore`/`KeywordStatus` render in practice, and the literal `AgeGroup`/
`Gender` values (capitalisation, and whether an unknown bucket appears) — passed through verbatim.
