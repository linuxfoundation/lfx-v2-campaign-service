---
type: "Architecture Doc"
title: "Reddit Keyword Performance"
description: "The Reddit keyword performance read: one read-only row per (campaign, keyword) from the Reddit Ads API v3 KEYWORD report breakdown, top 50 by impressions, sharing the audience read's report reader, trust boundary, limits, gate and fail-closed decoding; acting on a keyword stays with the keyword-targeting levers."
resource: "internal/platform/reddit/keyword_performance.go"
---

# Reddit Keyword Performance

`GET /projects/{project_id}/reddit-ads/keywords?window=` (`get-reddit-ads-keywords`, LFXV2-2665), a
`campaign_manager` read. The full contract is the `/reddit-ads/keywords` row of
[the API catalog](../../api-catalog.md).

## What Reddit reports

From Reddit's published Ads API v3 OpenAPI document (read as described in
[Reddit Audience Insights](audience-insights-reddit.md)), operation
`POST /ad_accounts/{ad_account_id}/reports`:

- `KEYWORD` is in both the `breakdowns` and the `fields` enums.
- The response row's `keyword` is typed `["string","null"]`, described as "The keyword for the
  ad.", example `internet` — **the keyword TEXT**.
- The `Metric` schema carries no keyword id, match type or status (its only keyword-related
  property is `keyword`). So nothing on a row can address a mutation: **the rows are read-only.**

## Choices

- **Shared reader.** `internal/platform/reddit/report_breakdown.go` holds what the audience and
  keyword reads share: the pre-request refusals (`prepareBreakdownRead`: window, bound account,
  scope — `MaxReportCampaigns` = 8, `ErrReportScopeTooLarge` / `ErrReportScopeInvalid`), the
  per-(campaign, breakdown) fan-out (3 in flight, 10 pages), the raw-body `identityjson.Check`, row
  validation through `reportRow.validate` (missing or `null` counter refused, foreign row refused),
  the breakdown-value read (absent key refused, `null` → unattributed) and the decode error class.
  The dispatcher prelude (`redditReportClient`: gate, window, own connection, bound account,
  provenance) is shared the same way; only the 409 sentinels differ — this read reports
  `ErrKeywordReportScopeTooLarge` / `ErrKeywordReportScopeInvalid`.
- **One report per campaign** (8 at most), for the audience read's reason: the single-term
  `campaign:id==<id>` filter is the only verified form. The cap bounds reports, not requests;
  every request attempt — each page, each 429 retry — takes a slot in the ad account's per-process
  reporting budget (`report_budget.go`), and this read's attempts count against the same 24-slot
  insight allowance as the audience read (both go through `readBreakdownReports`, which marks its
  context `withInsightsReports`). The slot is taken in the client's shared send path
  (`requestWithThrottleRetryCounted`): after the token refresh and the request build, immediately
  before the send, so an attempt that is never sent (cancelled context, failed refresh) spends
  none, and a pre-send dial failure (DNS, connection refused) hands its slot back. No slot before
  the deadline: the read fails whole (503). The ledger is per process, so the chart refuses more
  than one replica outright (`templates/deployment.yaml`, unconditionally, because the ungated
  account monitor spends the same budget).
- **Strict provenance**, as the audience read (the same `redditScopeForAccount`): a campaign that
  records no creating account is refused (409, re-dispatch), one created under another account is
  refused (409, reconcile), and a scope with BOTH is `ErrCampaignProvenanceMixed` joined with both
  sentinels — a 409 naming both remedies — all before Reddit is contacted.
- **Identical concurrent reads share one upstream read** (`d.keywords`, a `redditReportGuard`), and a
  success is reused for 60 s within the same UTC day.
- **Rows per (campaign, keyword), not summed.** Reddit's only keyword action is removing a keyword
  from ONE campaign's ad-group targeting, so a row says which campaign it served in. Ranked by
  impressions, then campaign id, then text; capped at 50 with `truncated`, like the Google and
  Microsoft keyword reads.
- **Keyword text is kept verbatim** — phrases, punctuation, non-ASCII — but a blank value, one over
  200 bytes (the removal's own bound), invalid UTF-8, a control character, or `<` / `>` fails the
  read rather than being published. A `null` keyword is a row with no `text` (delivery attributed
  to no keyword).
- **Gated by `REDDIT_METRICS_ENABLED`** (default off), checked before the scope lookup.

## Acting on a keyword

[Keyword Targeting on Reddit and X](keyword-targeting-reddit-x.md) is where a Reddit keyword is
read and removed (`get-keyword-targeting` / `remove-keyword-targeting`, by exact text, on one
campaign) and where a negative keyword is added (`add-negative-keywords`). A row here can point an operator at a keyword to look at there; it is not itself a
handle, and the report's text has not been verified, against a live account, to equal the
targeting entry byte for byte.

## Not verified

No live Reddit account has been read: whether `keyword` is ever `null`, whether the report's text
matches the targeting entries exactly (case, spacing), and what a campaign with no keyword delivery
returns are all unconfirmed.
