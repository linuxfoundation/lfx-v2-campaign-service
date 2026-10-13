# 2026-10-13 — One 50/90/100 pacing ladder for the brief view and every monitor (D2)

**Update** — product decision D2 (Monitor & Optimize brief, made 2026-10-13): the pacing ladder
is 50/90/100 everywhere. `internal/service/rules/ladder.go` now has ONE ladder,
`StandardLadder()`, replacing `BriefViewLadder` (50/100/130) and `AccountMonitorLadder`
(50/90/100). `DefaultThresholds` (the brief view's `ComputePacing` → `Evaluate`) and
`pacingLabelFor` (every `EvaluateXMonitor`) both place on it through the same
`PacingLadder.Label`.

- **Client-visible, brief path only** (`GET .../briefs/{briefId}/metrics`, the only caller of
  `ComputePacing`/`Evaluate`): 90–100% of plan moves from `normal` to `constrained`, and
  100–130% from `constrained` to `overspending`. A campaign exactly on plan is now `constrained` and raises a
  `budget_constrained` (`MED`) item. The account monitors' bands do not move.
- `Evaluate`'s `budget_constrained` item keeps its rule token and priority, but its issue text now
  follows the label: `constrained` reads "Budget constrained — N% of expected spend…" (action
  "…or accept it may exhaust early"), `overspending` keeps "Spending ahead of plan — N%…".
  "Ahead of plan" would have been false for the 90–100% band.
- Rounding is unchanged and still per caller: Google, Microsoft, Meta, Reddit and X round the
  percentage before placing it, LinkedIn and the brief view do not. One ladder does not unify
  that, and D2 did not ask it to.
- NaN handling is unchanged: `Label` places NaN in overspending (unreachable on the brief path),
  `pacingLabelFor` keeps its normal guard.
- The brief view no longer matches lfx-self-serve's `CAMPAIGN_PACING_THRESHOLDS` (50/100/130);
  nothing in this service reads that constant.
- Tests: on-plan brief cases now expect `constrained`; `TestComputePacing_D2LadderBoundaries`
  pins 49.9/50/89.9/90/90.1/100/100.1 on the brief path, and
  `TestPin_BriefView_ActionItemsAtBoundaries` pins each band's item text.
- Docs: `code/internal-service-rules.md` and `architecture/account-monitor-endpoints.md` replace
  "D2 is open" with the decision; `docs/api-catalog.md`'s brief-metrics row states the bands and
  the shift.
