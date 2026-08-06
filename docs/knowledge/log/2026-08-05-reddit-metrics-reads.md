# 2026-08-05 — Reddit metrics reads (LFXV2-2995, UNVERIFIED contract)

**Creation** — Implemented live-read campaign metrics for Reddit Ads (LFXV2-2995), the last of
five platforms closing MetricsReader parity with Google Ads (GA-5, PR #70): `internal/platform/
reddit.GetCampaignMetrics` and `internal/dispatch/reddit.go`'s `ReadMetrics`.

Reddit's v3 reporting endpoint is undocumented publicly (LFXV2-2995 investigation, recorded
BLOCKED before this work started): no path, params, or response schema are confirmable from any
public source, only from a gated developer portal / private Postman collection. Rather than stay
blocked indefinitely, this implements a best-effort guess built ONLY from this package's own
proven v3 conventions (`POST /ad_accounts/{id}/reports`, the `{"data":...}` envelope every other
endpoint uses) and flags every assumption inline as UNVERIFIED — see the new "Metrics reads"
section in `internal-platform-reddit.md`. This is a materially weaker guarantee than the Meta/
LinkedIn/X metrics clients, which had public API docs to build a documented-but-unverified
assumption from; Reddit has no such spec to point to at all. Treat this as a placeholder pending
`adsapi-partner-support@reddit.com` or Postman collection access, not a confirmed integration.

Also added `model.MetricsWindow`/`model.CampaignMetrics` and `service.MetricsReader` — this
branch was cut directly from `main` (GA-5/#70 is still an open, unmerged epic-stacked PR), so it
carries its own copy of that scaffold, mirroring the Meta/LinkedIn/X metrics branches. The
published `briefs.GetCampaignMetrics` endpoint calls `orch.ReadCampaignMetrics` and is
production-reachable for Reddit campaigns; the metrics read capability IS live and available to
callers, though backed by an unverified API contract. Do not treat this as a confirmed
integration — the request/response shapes remain placeholders pending official Reddit API
verification.

**Update** — Review fixes on PR #75 (2026-08-05): corrected documented acceptance/risk posture to
acknowledge the brief.go endpoint is production-reachable for Reddit metrics reads (not a deferred
scaffold); added comprehensive spend-validation tests (`NaN`, `Infinity`, negative, out-of-range);
`GetCampaignMetrics` guards (already in place) reject non-finite and out-of-range `spend` values
before converting to micros, preventing silent `CostMicros` corruption; `metrics_test.go`'s
httptest handlers use `t.Error` instead of `t.Fatal` (`FailNow` is only valid on the test
goroutine); the knowledge doc now explicitly distinguishes an explicit empty `data` array (real
"no activity") from a missing/malformed `data` field (a decode error, not zero-activity);
`api-catalog.md`'s knowledge-doc link is now properly labeled; and verified
`internal/dispatch/reddit_test.go` has full `ReadMetrics` coverage (success path, missing
platform campaign ID guard, connection-resolution error propagation, unsupported window).

**Fix** — `GetCampaignMetrics` aggregated report rows without checking that a row's
`campaign_id` actually matched the requested campaign (Copilot finding, PR #75
review). Since both the report contract and the `campaign_ids` filter are explicitly
UNVERIFIED (see the reporting-contract caveat above), an extra row or a silently
ignored filter would attribute another campaign's impressions/clicks/spend to the
requested one with no error. Rows are now rejected outright (as a decode error, same
as other malformed-response handling) if `row.CampaignID != id`, including a blank
`campaign_id`. Added `TestGetCampaignMetrics_MismatchedRowCampaignIDIsDecodeError`
and `TestGetCampaignMetrics_BlankRowCampaignIDIsDecodeError`.
