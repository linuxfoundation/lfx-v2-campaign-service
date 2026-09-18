# 2026-09-18 monitor account endpoints — round 23 review fixes

**Fix** — Fixed the two remaining open findings from PR #215's human review
(`dealako`), verified individually against current code rather than assumed
fixed from commit-message pattern-matching (7 of the reviewer's 9 comments
were already resolved by earlier rounds; one — Meta/Reddit/LinkedIn never
setting `PacingUnknown` for a no-budget row — was previously left unchanged
by deliberate design decision, see the round-22 entry).

1. Reddit's per-campaign fan-out (`internal/platform/reddit/monitor.go`,
   `ListAccountCampaigns`) interpolated the Reddit-returned campaign id
   (`e.ID`) into the `/reports` request path with no validation — every
   sibling path that interpolates a Reddit-supplied or caller-supplied id
   (`updateEntityStatus`, `GetCampaignMetrics`, the account id itself)
   already rejects one that fails `accountIDRe` first; this fan-out was the
   one path that skipped that guard, letting a malformed upstream id
   retarget this project's live bearer token at an arbitrary Reddit path.
   Added the same `accountIDRe.MatchString(e.ID)` check before the report
   call; on failure the row is marked `FetchFailed=true` and the request is
   never made. New test
   `TestListAccountCampaigns_MalformedCampaignID_MarksFetchFailedWithoutRequest`
   pins both the failed row and that `fetchMonitorReport` is never called.

2. The same fan-out unconditionally seeded `AccountCampaignRow.StartDate`/
   `EndDate` from the report window before checking whether Reddit's own
   `start_time`/`end_time` parsed, so a campaign with no reported flight
   window looked identical to one whose flight exactly matched the report
   range. `internal/service/rules/monitor_reddit.go`'s `redditPacingPct`
   then read that fabricated window as real, producing a plausible-looking
   but fictitious pacing percentage (e.g. "3% of budget spent") against a
   schedule the campaign never had. Removed the seeding — `StartDate`/
   `EndDate` now stay empty unless Reddit's own fields parse — and added a
   `m.StartDate == ""` branch in `EvaluateRedditMonitor`, mirroring
   `monitor_shared.go`'s `fetchFailedRow` convention: sets
   `PacingUnknown=true`, keeps the placeholder `MonitorPacingNormal` label,
   and still runs `redditActionItems` at `pacingPct=0` so the
   flight-independent zero-delivery/CTR/no-conversion checks keep firing
   (the underspend action item is correctly suppressed, since it requires
   `pacingPct>0`). This is a genuinely different case from `FetchFailed`:
   the metrics themselves are real, only the flight window is unknown. New
   tests `TestListAccountCampaigns_NoStartTime_LeavesStartDateEmpty` and
   `TestEvaluateRedditMonitor_EmptyStartDate_SetsPacingUnknown` pin both
   halves.

Fixing both in one batched pass (per standing guidance against iterative
one-finding-at-a-time review loops) surfaced that the campaign-id fix broke
three pre-existing tests that used hyphenated fixture ids (`camp-0`, `camp-1`)
— `accountIDRe` accepts only `[A-Za-z0-9_]`, matching real Reddit id formats,
which never contain hyphens. Updated those fixtures to hyphen-free ids
(`camp0`, `campOK`) rather than loosening the regex, since the regex reflects
the real upstream charset and the hyphens were only ever a test-fixture
convenience.

Verification: `go build ./...`, `go vet ./...`, `gofmt -l .`,
`go test ./...` (full repo suite), and `go run ./cmd/okfvalidate ./docs/knowledge`
all clean.
