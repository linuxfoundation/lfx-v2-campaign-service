# 2026-08-05 — Microsoft status toggle: review round + log-split merge

**Update** — PR #63 (LFXV2-2810) review round covering commits 462c6c4 and
661b21a, which updated the `internal-dispatch.md` concept, two error messages
(`brief.go`, `microsoft.go`), and added `Status` assertions to three
pause-path tests, but had not yet been logged. Those changes clarified the
ACTIVATE/PAUSE ordering asymmetry in prose and made the "orphan ad" refusal
error message action-agnostic rather than ACTIVATE-specific.

**Fix** — `internal-platform-microsoft.md`'s Status toggle section stated
"ACTIVATE works upward, children first," which reads as a strict leaf-to-root
walk. The actual PUT order is `AdGroups → Ads → Campaigns`
(`internal/platform/microsoft/campaign.go`, `UpdateCampaignAndChildrenStatus`
non-PAUSE branch) — Ads is deeper than AdGroups in the tree, yet AdGroups PUTs
first, so it is children-before-parent, not a depth-ordered walk. Reworded to
state the concrete PUT order and moved the full cascade-ordering/child-id-skip
rules (previously only in `internal-dispatch.md`, removed by the log/dispatch
split below) into this platform's own Status toggle section, matching where
every other platform's toggle behavior now lives.

**Merge** — Resolved the conflict from `docs/knowledge/log.md`'s split into
per-entry fragments and `internal-dispatch.md`'s per-platform prose split
(both landed on `main` after this branch forked): took main's fragment
structure and pointer-style `internal-dispatch.md`, folding this branch's
Microsoft cascade-ordering detail into `internal-platform-microsoft.md`
instead of the now-removed inline summary.
