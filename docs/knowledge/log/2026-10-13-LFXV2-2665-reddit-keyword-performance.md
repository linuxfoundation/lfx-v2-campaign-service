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
  is shared as `redditReportClient`, which takes the read's enable func and checks it first (so a
  read built on it cannot ship ungated) and returns the trimmed bound account id that keys each
  read's guard. Same 8-campaign cap (one report per campaign), own connection
  only, bound account only, fail-closed decoding, `REDDIT_METRICS_ENABLED` gate before the scope
  lookup. The keyword read's 409s use `ErrKeywordReportScope*`.
- **Keyword text** is kept verbatim but a blank, over-200-character (runes, as the removal's Goa
  `MaxLength(200)` counts), invalid-UTF-8, control-character, invisible-formatting (Unicode `Cf`:
  zero-width, bidi), line/paragraph-separator (`Zl`/`Zp`) or `<`/`>` value fails the read; a `null`
  keyword is a row with no `text`.
- **Breakdown key case.** The shared reader matches the breakdown value's key case-insensitively
  (`rowKeyFold`), as the old audience struct decode did and as the row's counters still are; the
  refactor had briefly made it case-sensitive.
- Chart route regex, RuleSet entry and parity rows for `reddit-ads/keywords`; API catalog row; the
  keyword-targeting concept now points at this read instead of saying there are no per-keyword
  metrics.
- Built on PR #306 as merged to main: the read refuses rows with no recorded creation account,
  and a scope with both a foreign and an unrecorded row is `ErrCampaignProvenanceMixed` (409
  naming both remedies, via the shared insights classifier); every request attempt takes a slot in
  the ad account's reporting budget (sharing the audience read's insight allowance) right before
  the send, after the token refresh, and a pre-send dial failure releases it; the chart's
  unconditional single-replica guard covers it; and it has its own single-flight guard with a 60 s
  success cache. Tests: the keyword read's Mixed-provenance cases in `reddit_keywords_test.go`
  (dispatch and service) and `TestReportBudgets_KeywordReadSpendsOnlyOnSentAttempts`, whose dial
  case counts reservations through a test-only `ReportBudgets.onReserve` hook to prove a slot was
  taken and then released.
- Verified only against Reddit's published schema, not a live account.
- `kubernetes/httproute.md`, `kubernetes/ruleset.md` and `kubernetes/deployment.md` list
  `reddit-ads/keywords` and say `REDDIT_METRICS_ENABLED` gates the keyword read too.
