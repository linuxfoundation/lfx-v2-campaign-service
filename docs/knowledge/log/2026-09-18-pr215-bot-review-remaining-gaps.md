# 2026-09-18 PR #215 bot review — remaining gaps

**Fix** — Reconciling the original PR #215 bot review comments (Cursor +
Copilot) against the code found three gaps the round-19/round-21 local review
cycles had not caught, distinct from those rounds' own findings:

1. Google's account-monitor rows never carried `campaign_url`, and the design
   layer's speculative `ad_groups` nesting had no source data anywhere in the
   BFF being ported (confirmed by reading
   `campaign-metrics.service.ts` — its only ad-group/keyword data lives in a
   wholly separate `getKeywords` endpoint, never in the monitor response).
   Implemented `campaign_url` for real (`buildGoogleAdsCampaignURL` in
   `internal/dispatch/googleads.go`, mirroring the BFF's
   `buildGoogleAdsUrl(campaignId)`), and removed `ad_groups` /
   `AccountMonitorAdGroup` entirely from `design/connection.go` rather than
   inventing behavior the BFF never had.
2. `internal/platform/reddit/monitor.go`'s `fetchMonitorReport` read a
   malformed `/reports` body as a legitimate zero-delivery measurement,
   matching the BFF's optional-chaining `?.metrics ?? []` — but that silently
   converts an upstream-data failure into a fabricated zero, indistinguishable
   from a campaign that genuinely had no activity. Now returns an error on
   decode failure, which the existing caller already turns into
   `FetchFailed=true` on that row.
3. A stray typo ("query. was") in
   `docs/knowledge/log/2026-09-17-monitor-account-endpoints.md`.

Verification: `go build ./...`, `go vet ./...`, `gofmt -l .`, and
`go run ./cmd/okfvalidate ./docs/knowledge` all clean; `go test ./... -count=1`
fully green.
