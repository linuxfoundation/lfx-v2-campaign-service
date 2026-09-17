# 2026-09-16 — automated GHCR stale image cleanup

**Creation** — added `.github/workflows/ghcr-image-cleanup.yaml`, a scheduled
(weekly) plus on-demand (`workflow_dispatch`) job that deletes stale,
untagged versions of the `lfx-v2-campaign-service/campaign-service` GHCR
package via `snok/container-retention-policy@v3.1.0`, addressing the ~15,000
stale image versions reported in the team's Slack thread. Tagged versions
(including per-commit SHA tags) are never touched in this rollout; only
untagged versions older than the 30-day default cut-off are eligible.

Requires a `CONTAINER_RETENTION_PAT` repository/org secret (classic PAT,
`read:packages` + `delete:packages`) provisioned by an org owner before any
real (non-dry-run) run can succeed — the default `GITHUB_TOKEN` cannot
delete container package versions. Until that secret exists, only dry-run
previews will succeed.

Added the corresponding concept doc at
`docs/knowledge/architecture/ghcr-image-cleanup.md`.
