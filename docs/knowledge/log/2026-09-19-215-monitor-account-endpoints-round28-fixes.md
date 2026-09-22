# 2026-09-19 monitor account endpoints — round 28 review fixes

**Fix** — The Claude-Opus fallback trio, run against the combined round
24–27 diff, found four residual issues, one of them privacy:

1. Round 27's widening of `fetchFailedRow`'s doc comment (in
   `internal/service/rules/monitor_shared.go`) correctly broadened the
   helper's scope to cover `monitor_reddit.go`'s empty-`StartDate` branch,
   but left one sentence saying the row "still" carries its `FetchFailed`
   flag "either way." That's false for the Reddit case: `fetchFailedRow`
   sets only `PacingUnknown`, never `FetchFailed`. The sentence now
   distinguishes the two Google fetch-failure causes (which do set
   `FetchFailed`) from the Reddit empty-`StartDate` case (which sets only
   `PacingUnknown`).
2. `internal/platform/linkedin/monitor.go`'s absent-`metadata` guard
   comment claimed it mirrors `accounts.go`'s adAccount picker
   (`accounts.go:202-211`), but only the metadata-nil half of that guard —
   `accounts.go` additionally dedups repeated page cursors with a `seen`
   set, which this loop does not have (bounded only by `monitorMaxPages`).
   The comment now says so explicitly instead of overclaiming parity. No
   behavior change: the gap predates this range and is bounded, not a hang.
3. `internal/platform/googleads/monitor.go`'s `microsToUSD` checked
   `s == ""` before trimming whitespace, so a whitespace-only
   `amount_micros` (e.g. `" "`) took the malformed-data path (`FetchFailed`,
   dropped from pacing/action-item evaluation) instead of the
   legitimate-zero path the plain empty string gets — semantically the same
   input, different outcome. Now trims once, up front, before the empty
   check. `TestMicrosToUSD_EmptyAndSentinelAreNotFailures` gained a
   whitespace-only case (`"  "`) alongside `""`.
4. **Privacy.** Two lines added by round 24's own log entry
   (`docs/knowledge/log/2026-09-19-215-monitor-account-endpoints-round24-fixes.md`)
   named a human reviewer by GitHub handle in prose. An earlier round of
   this cycle (round 27) declined to touch this, reasoning it was a
   pre-existing, repo-wide convention spanning a dozen files and not
   something to fix piecemeal — that reasoning does not hold for this
   specific file: the round-24 log entry is itself new within this review's
   range (base commit `c1a8f099`), so redacting it needed no wider sweep of
   pre-existing files to keep the range clean. Both lines now describe the
   source impersonally ("a human reviewer's ... nit", "a human reviewer's
   nit on PR #215") instead of by handle. The wider repo-convention
   question (a dozen pre-existing files elsewhere) remains untouched and
   undecided — that is a separate, deliberate call for the repo's
   maintainers, not a side effect of this fix cycle.

Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`, and
`go test ./internal/platform/googleads/... ./internal/platform/linkedin/...
./internal/service/... ./internal/service/rules/... -count=1` all
clean/passing; `go run ./cmd/okfvalidate ./docs/knowledge` reports the
bundle conformant.
