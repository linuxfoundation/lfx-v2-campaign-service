# 2026-09-17 — GHCR cleanup workflow: fix stale rollout-guard claim

**Fix** — `docs/knowledge/architecture/ghcr-image-cleanup.md`'s "Known
tradeoff" paragraph about idle PR-branch images said `dry-run` guards the
rollout of the wider `tag-selection=both` scope. That was left over from
before the scheduled trigger's `dry-run` default changed to `false`
(see `2026-09-17-ghcr-cleanup-schedule-dry-run-default.md`); nothing now
enforces a preview before the first scheduled deletion under the wider
scope.

Reworded the paragraph to describe a manual `workflow_dispatch` run with
`dry-run: true` as an available preflight a maintainer can choose to run,
rather than a rollout guard the workflow enforces.
