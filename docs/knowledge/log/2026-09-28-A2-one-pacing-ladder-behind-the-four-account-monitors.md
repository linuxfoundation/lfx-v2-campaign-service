# 2026-09-28 — A2: one pacing ladder behind the four account monitors

**Update** — no behaviour change. Four identical ladders replaced by one helper; the guards that
actually differ are untouched.

`monitor_google.go`, `monitor_linkedin.go`, `monitor_meta.go` and `monitor_reddit.go` each carried a
private three-constant block and a four-arm `switch` deriving the pacing label. Read side by side
they look like four platform rules. They are not: all four are `< 50 → underspending`,
`> 100 → overspending`, `> 90 → constrained`, else `normal` — the same numbers, the same order, the
same boundary strictness.

What made them read as different was vocabulary. Google named its 100 `googlePacingOverspending`
and its 90 `googlePacingConstrainedFrom`; the other three named the same numbers
`…PacingConstrained` and `…PacingNormal`. Two files, `metaPacingNormal = 90` and
`googlePacingConstrainedFrom = 90`, are the same boundary under names that disagree about which
band it belongs to. That is the more interesting half of this: the duplication was legible, but the
*sameness* was not, and a reviewer comparing two of these files would reasonably conclude the
platforms had been tuned apart.

They now call `pacingLabelFor(pct)` in `monitor_shared.go`, whose three constants are named for the
boundary each one **is** rather than the band it happens to gate —
`monitorPacingUnderspendingBelow`, `monitorPacingHealthyTo`, `monitorPacingOverspendingAbove`.

**The guard stays per-platform, and that is the point.** Each monitor decides for itself whether a
campaign has a pacing figure worth placing at all — Google guards not at all, Meta on `!unknown`,
LinkedIn on `hasBudget`, Reddit by routing a missing flight through `fetchFailedRow`. Those four
genuinely differ, because the platforms report budget differently. Only the arithmetic was shared,
so only the arithmetic moved; the divergence that matters is dealt with separately, on its own
ticket.

Worth stating plainly, since the package header used to say the opposite: this is **not** a merge
onto `pacing.go`'s `Thresholds`/`ComputePacing`. That path runs 50/100/130 with `Constrained` as an
inclusive top, against a different input shape. Routing the monitor through it would move every
operator-facing alerting band as a side effect of a refactor, which is a decision, not a cleanup.
The headers in `pacing.go`, `monitor_google.go` and
`docs/knowledge/code/internal-service-rules.md` now draw that line where it actually falls — around
the ladder, not around the four files.

The old justification for the duplication was the OLD-vs-NEW differential diff against the still-
live BFF. That diff is no longer the plan of record, which removes the reason; the fidelity comments
covering the pacing constants come out with it. linuxfoundation/lfx-self-serve#3019 recorded the
BFF-side half of the same problem — Meta and LinkedIn read the shared `CAMPAIGN_PACING_THRESHOLDS`
there while Google and Reddit hardcode the identical numbers locally, so an edit to that constant
moves two platforms and silently leaves two behind. One ladder here closes that gap in both
directions.

`go test -race ./internal/service/rules/...` passes unchanged — the existing per-platform monitor
tests are the check that the ladder did not move, and none of them needed editing.
