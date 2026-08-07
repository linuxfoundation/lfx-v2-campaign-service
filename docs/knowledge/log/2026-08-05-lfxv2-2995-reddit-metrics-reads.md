# 2026-08-05 — Reddit metrics reads (LFXV2-2995, UNVERIFIED contract)

**Update** — Implemented live-read campaign metrics for Reddit Ads (LFXV2-2995) via
`internal/platform/reddit.GetCampaignMetrics` and `internal/dispatch/reddit.go`'s `ReadMetrics`.

Reddit's v3 reporting endpoint is undocumented publicly (LFXV2-2995 investigation, recorded
BLOCKED before this work started): no path, params, or response schema are confirmable from any
public source, only from a gated developer portal / private Postman collection. Rather than stay
blocked indefinitely, this implements a best-effort guess built ONLY from this package's own
proven v3 conventions (`POST /ad_accounts/{id}/reports`, the `{"data":...}` envelope every other
endpoint uses) and flags every assumption inline as UNVERIFIED — see the new "Metrics reads"
section in `internal-platform-reddit.md`. This is a materially weaker guarantee than the Meta/
LinkedIn/X metrics clients, which had public API docs to build a documented-but-unverified
assumption from; Reddit has no such spec to point to at all.

The initial implementation included comprehensive spend-validation tests (`NaN`, `Infinity`,
negative, out-of-range) that reject non-finite and out-of-range `spend` values before converting
to micros, preventing silent `CostMicros` corruption. Guards also reject negative impressions/
clicks and detect total overflow before it corrupts accumulated metrics. `GetCampaignMetrics`
aggregates rows with a guard that rejects any row whose `campaign_id` does not match the
requested campaign (as a decode error, same as other malformed-response handling), and test
handlers use `t.Error` instead of `t.Fatal` to avoid terminating the test goroutine. The
knowledge doc explicitly distinguishes an explicit empty `data` array (real "no activity") from a
missing/malformed `data` field (a decode error, not zero-activity). The published
`briefs.GetCampaignMetrics` endpoint calls `orch.ReadCampaignMetrics` and is production-reachable
for Reddit campaigns; the metrics read capability IS live and available to callers, though
backed by an unverified API contract. Treat this as a placeholder pending
`adsapi-partner-support@reddit.com` or Postman collection access, not a confirmed integration.
