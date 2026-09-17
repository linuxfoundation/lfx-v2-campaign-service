# 2026-09-17 — GHCR cleanup workflow: schedule defaults to real deletions

**Fix** — `github.event.inputs` is undefined on a `schedule` trigger, so
`--dry-run=${{ github.event.inputs.dry-run || 'true' }}` always evaluated to
`true` on the weekly cron run, meaning the schedule could only ever preview
and never actually delete anything.

Changed the `dry-run` expression in
`.github/workflows/ghcr-image-cleanup.yaml` to check `github.event_name`
first: scheduled runs now default to `false` (real deletions), while
`workflow_dispatch` keeps defaulting to `true` (preview) so a maintainer can
check a manual run before it deletes anything. Applied the same expression to
the run summary's `DRY_RUN` value so the two stay consistent.

Updated `docs/knowledge/architecture/ghcr-image-cleanup.md`'s Triggers
section to describe the corrected defaults.
