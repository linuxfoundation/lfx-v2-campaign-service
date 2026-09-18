# 2026-09-18 monitor account endpoints — round 22 review fixes

**Fix** — Fixed the findings from a further local pre-PR review round (general,
repo_code, repo_learnings run once, per explicit instruction not to auto-rerun
the trio after this cycle's fixes).

1. (`general`) `metaActionItems`' underspending action item hardcoded
   `m.TotalBudget` as the message denominator, which is `0` whenever
   `metaPacingPct` derived `pacingPct` from the flat `BudgetDay*days` branch
   instead of the schedule-based one — producing a self-contradicting
   "$X.XX of $0.00" message for every daily-budget-funded active campaign.
   `internal/service/rules/monitor_meta.go`'s `metaActionItems` now takes
   `days` and falls back to `m.BudgetDay*float64(days)` when `TotalBudget<=0`,
   matching the expectation `metaPacingPct` itself used to derive that
   percentage. New test in `monitor_meta_test.go` pins the fixed denominator.
2. (`general`) `linkedinPacingPct`'s doc comment claimed `rangeStart` was
   `now - days days`; the code (`now.AddDate(0, 0, -(days-1))`) was already
   correct — the same inclusive-of-today convention Google/Reddit/Meta's
   monitor dispatchers share, pinned by
   `internal/platform/linkedin/monitor_test.go`'s
   `TestListAccountCampaigns_UsesInjectedClockNotWallClock`. Corrected the
   comment; no behavior change.
3. (`repo_code`) `docs/api-catalog.md`'s account-monitor endpoint row cited
   action-item priorities as lowercase `` `high`/`med`/`low` ``, contradicting
   the uppercase `HIGH`/`MED`/`LOW` enum used everywhere else (`design/connection.go`'s
   `Enum("HIGH", "MED", "LOW")`, `model.MonitorPriorityHigh` etc.). Corrected
   the casing.
4. (`repo_code`) `docs/knowledge/code/internal-service-rules.md`'s overview
   read as though it were the only rule engine in `internal/service/rules`,
   with no mention of the four separate, deliberately unshared `monitor_*.go`
   engines added by this branch. Added a paragraph disambiguating the two and
   linking to `docs/knowledge/architecture/account-monitor-endpoints.md`.
   None of the four `internal-platform-{googleads,linkedin,meta,reddit}.md`
   concept files mentioned the new account-monitor read at all; added a short
   "Account-monitor read" section to each, pointing at its `monitor.go`, the
   architecture doc, and its platform-specific verbatim-ported quirks.

**Not fixed, by deliberate design decision rather than oversight:** the
`general` reviewer also raised that Meta/Reddit/LinkedIn's rule engines never
set `AccountCampaignMetrics.PacingUnknown` when a campaign has no usable
budget, computing `pacingPct=0`/label `normal` instead. `PacingUnknown`'s
contract (`model.go`, and `design/connection.go`'s wire-level doc) is narrowly
and consistently scoped to "flight dates unavailable," not "budget
unavailable" — and it is currently set ONLY by `monitor_shared.go`'s
`fetchFailedRow`, for the `FetchFailed` case. Meta's own `metaPacingPct` doc
comment already draws exactly this distinction from its own budget-based
`unknown`-return-value oddity (itself pinned as intentionally-not-a-bug by
`TestMetaPacingPct_UnknownIsAlwaysFalse`). Treated as a disclosed, internally
consistent design choice, not a contract violation — left unchanged for all
three platforms.

Verification: `go build ./...`, `go vet ./...`, `gofmt -l .`,
`go test ./internal/service/rules/... -v` (Meta/LinkedIn suites), and
`go run ./cmd/okfvalidate ./docs/knowledge` all clean.
