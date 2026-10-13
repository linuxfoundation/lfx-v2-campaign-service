# 2026-10-13 — LFXV2-2665 Reddit keyword performance read

**Update** — New `campaign_manager` read `get-reddit-ads-keywords`,
`GET /projects/{projectId}/reddit-ads/keywords`: one read-only row per (campaign, keyword) from the
Reddit Ads API v3 `KEYWORD` report breakdown, top 50 by impressions with `truncated`. See
[Reddit Keyword Performance](../architecture/keyword-performance-reddit.md).

- **KEYWORD semantics.** Per Reddit's published v3 spec, `KEYWORD` is in the `breakdowns` and
  `fields` enums and the row's `keyword` is the keyword TEXT ("The keyword for the ad.",
  string-or-null). There is no keyword id, match type or status, so the row addresses no action;
  keywords are still acted on through the existing keyword-targeting read/removal.
- **Shared reader.** The audience read's request, refusal and decode steps moved to
  `internal/platform/reddit/report_breakdown.go` (renamed `MaxReportCampaigns`,
  `ErrReportScopeTooLarge`/`ErrReportScopeInvalid`); the dispatcher's gate/credential/scope prelude
  is shared as `redditReportClient`. Same 8-campaign cap (one report per campaign), own connection
  only, bound account only, fail-closed decoding, `REDDIT_METRICS_ENABLED` gate before the scope
  lookup. The keyword read's 409s use `ErrKeywordReportScope*`.
- **Keyword text** is kept verbatim but a blank, over-200-byte, invalid-UTF-8, control-character
  or `<`/`>` value fails the read; a `null` keyword is a row with no `text`.
- Chart route regex, RuleSet entry and parity rows for `reddit-ads/keywords`; API catalog row; the
  keyword-targeting concept now points at this read instead of saying there are no per-keyword
  metrics.
- Verified only against Reddit's published schema, not a live account.
