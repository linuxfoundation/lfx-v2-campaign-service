# 2026-09-21 PR #215 monitor account endpoints round 31 doc fixes

**Docs** — Fixed the three remaining documentation-only threads on PR #215's
review (GitHub review threads `PRRT_kwDOTI9SG86j6d14`,
`PRRT_kwDOTI9SG86j6d2G`, `PRRT_kwDOTI9SG86j6d2S`), each a doc/comment
inaccuracy with no behavior change:

1. `design/connection.go`'s `monitor-google-ads-account` `Description(...)`
   claimed `{project_id}` resolves the stored connection credential "exactly
   as" `GET .../connection-google-ads/accounts does." That was true before
   the round-16/17 trust-boundary fix (see
   [account-monitor-endpoints.md](../architecture/account-monitor-endpoints.md)'s
   Trust boundary section) but has been false since: `/accounts` still
   permits the shared LF system-account fallback via `d.creds.resolve`
   (`internal/dispatch/googleads.go`'s `resolveGoogleAdsDiscoveryClient`),
   while the monitor endpoint calls `d.creds.resolveOwned`
   (`resolveOwnedGoogleAdsDiscoveryClient`) and refuses that fallback,
   returning 404 for a project with no Google Ads connection of its own.
   Rewrote the description to state the owned-only, no-fallback,
   404-on-absence behavior instead of the false equivalence, and ran
   `make apigen` to regenerate the Goa HTTP/OpenAPI layer from the DSL
   change.
2. `internal/domain/model/monitor.go`'s `AccountMonitorActionItem` doc
   comment still listed "Meta's single-page Graph insights read" among the
   platforms' deliberately-preserved divergences. This file's own Meta
   pagination fix (see the architecture doc's "Known-verbatim-ported
   quirks" section) already made that false — the port paginates both the
   campaign and insights edges to exhaustion. Corrected the comment to say
   so.
3. `internal/platform/linkedin/monitor.go`'s `AccountCampaignRow.FetchFailed`
   field comment said the flag "marks a campaign the analytics pivot read
   did not return a row for." That is the exact inverse of the contract this
   same file already documents two lines above it in `ListAccountCampaigns`:
   a campaign LinkedIn omits from a *successful* pivot response is a
   legitimate "no delivery" zero, left unflagged; `FetchFailed` is set only
   for a genuine per-row parse failure — a malformed `dailyBudget`/
   `totalBudget` amount (`fetchAccountCampaignList`) or an unparseable
   `costInUsd` (`monitorMetricsRow.FetchFailed`, ORed into the row rather
   than overwritten). Rewrote the comment to state that contract and
   cross-reference `ListAccountCampaigns`.

Verified clean after all three fixes plus regeneration: `gofmt -l .`,
`go vet ./...`, `go build ./...`, and `go test ./...` (every package) all
pass with no changes to test behavior — these were documentation-only
corrections.
