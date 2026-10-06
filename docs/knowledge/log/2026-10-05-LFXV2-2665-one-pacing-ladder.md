# 2026-10-05 — One pacing ladder implementation, both ladders preserved (LFXV2-2665)

**Update** — the two pacing band switches in `internal/service/rules` are now one,
`PacingLadder.Label` in `ladder.go`, fed by two named ladders: `BriefViewLadder` (50/100/130,
the brief view's `DefaultThresholds`) and `AccountMonitorLadder` (50/90/100, every
`EvaluateXMonitor` through `pacingLabelFor`). No band moved. Which ladder is correct is open
product decision D2, and switching a caller's ladder moves operator-facing alert bands.

- Copies found: `labelFor` (`pacing.go`) and `pacingLabelFor`'s own switch and constants
  (`monitor_shared.go`). They agreed on boundary inclusivity for every finite value and differed
  only on NaN: the brief switch put it in overspending, the monitor switch in normal.
  `pacingLabelFor` keeps normal with an explicit guard.
- The priority rank (`priorityRank`) and the monitor's unknown-pacing row (`unknownPacingRow`)
  already had one copy each. The brief view's unknown value was written out ten times as
  `Pacing{Label: PacingUnknown}` in `pacing.go` and `internal/service/brief.go`; it is now
  `UnknownPacing()`.
- Rounding stays with the callers: Google, Microsoft, Meta, Reddit and X round before the
  ladder, LinkedIn and the brief view do not.
- `ladder_pin_test.go` pins every boundary of both ladders through every caller, and was green
  before the refactor.

Concepts updated: `code/internal-service-rules.md`,
`architecture/account-monitor-endpoints.md`; also `docs/api-catalog.md`.
