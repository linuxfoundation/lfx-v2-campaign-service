# 2026-09-19 monitor account endpoints — round 25 review fixes

**Fix** — The mandatory `/lfx-skills:lfx-local-review` trio run against round
24's commit (`a5998d5e`) surfaced two further real defects in that commit's
own fix, plus stale citations left over from drafting it. Both are fixed
here, addressing the `general` reviewer's Important findings.

1. `internal/platform/googleads/monitor.go`'s `microsToUSD` treated *every*
   negative `amount_micros` as Google's `-1` "no budget set" sentinel
   (`if n < 0 { return 0, true }`), so a genuinely malformed value like `-5`
   was silently accepted as a legitimate zero budget — the exact class of
   false-zero bug round 24 was meant to close, reintroduced by its own fix.
   Now only `n == -1` is treated as the sentinel; any other negative returns
   `(0, false)`, so the caller marks the row `FetchFailed=true` instead.
   `TestMicrosToUSD_EmptyAndSentinelAreNotFailures` gained a `"-5"` case
   pinning `ok=false`.
2. The same file's budget/`FetchFailed` check in `ListAccountCampaigns` only
   ran on a campaign id's first-sighting GAQL row (inside the `if !ok`
   block), because `segments.date` makes this query multi-row per campaign
   (see the doc comment above `ListAccountCampaigns`). A malformed
   `amount_micros` on a later row for an already-seen campaign was never
   checked. `budgetOK` is now computed once per row and checked
   unconditionally, independent of whether the row created or reused the
   accumulator.
3. Six comment citations across
   `internal/platform/googleads/{monitor.go,monitor_test.go}` and
   `internal/platform/linkedin/{monitor.go,monitor_test.go}` said "Copilot
   review, round-19-rerun" — a round label that doesn't exist in this repo's
   actual review history for these findings, left over from an earlier
   drafting pass. Updated to "round-24 review", the round that actually
   introduced these fixes. Left the *unrelated*, accurate "round-19 review"
   citations (the metrics-loop `FetchFailed` comment in `monitor.go` and the
   LinkedIn absent-from-analytics-is-zero comment) untouched — those
   describe genuinely different, older fixes.

Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`, and
`go test ./... -count=1` (full repo suite) all clean/passing.
