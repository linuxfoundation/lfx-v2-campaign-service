# 2026-09-18 monitor account endpoints — local review round 19 rerun

**Fix** — After round 19's fixes landed, the complete local pre-PR review
trio (general, repo_code, repo_learnings) reran pinned to the branch's
original base, per the standing rule that a rerun after fixes must widen
back to the original range rather than reviewing the fix commit in
isolation. `repo_code` came back clean; `general` raised two Important
findings and `repo_learnings` one Should-fix.

1. (`general`) `internal/service/rules/monitor_shared.go`'s `fetchFailedRow`
   doc comment, and the comment above
   `internal/service/rules/monitor_google_test.go`'s
   `TestEvaluateGoogleMonitor_SkipsFetchFailedRows`, both still claimed the
   Google `FetchFailed` branch was unreachable in production — true before
   round 19's fix to `internal/platform/googleads/monitor.go`, false after
   it. Updated both comments to point at the round-19 fix and at
   `TestListAccountCampaigns_MalformedMetrics_MarksFetchFailed`
   (`internal/platform/googleads/monitor_test.go`) as the test that actually
   exercises the now-reachable path.
2. (`general`) The four account-monitor endpoints' `resolveOwned*`
   resolvers deliberately never consult `LFX_FORCE_SYSTEM_ADS_ACCOUNT` /
   `forceSystemPaidAds` (`internal/dispatch/creds.go`) — a monitor read must
   never answer with another tenant's spend — but this was undocumented,
   risking being misread as a regression in a forced-system deployment.
   Documented the deliberate non-interaction in `forceSystemPaidAds`'s own
   doc comment and added a new subsection to
   `docs/knowledge/architecture/account-monitor-endpoints.md` explaining the
   resulting behavior difference from the create path under that flag.
3. (`repo_learnings`, `httptest-handler-state-needs-synchronized-handoff`)
   `internal/platform/reddit/monitor_test.go`'s
   `TestListAccountCampaigns_BoundsReportConcurrency` compared a wall-clock
   elapsed duration against the serial worst case, on top of the
   deterministic `maxInFlight` atomic-counter assertion — a fixed timing
   margin that `make test`'s `go test -race` run could erase under load.
   Removed the wall-clock assertion, keeping only the deterministic
   `maxInFlight` check as the test's sole pass/fail signal.

Verification: `go build ./...`, `go vet ./...`, `gofmt -l .`, and
`go run ./cmd/okfvalidate ./docs/knowledge` all clean;
`go test ./... -count=1` fully green.
