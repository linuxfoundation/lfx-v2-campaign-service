---
type: "Architecture Doc"
title: "GHCR stale image cleanup"
description: "How the scheduled and on-demand GitHub Actions workflow removes stale, untagged GHCR image versions for the campaign-service container package."
resource: ".github/workflows/ghcr-image-cleanup.yaml"
---

# GHCR stale image cleanup

[`.github/workflows/ghcr-image-cleanup.yaml`](../../../.github/workflows/ghcr-image-cleanup.yaml)
deletes stale, untagged versions of the
`linuxfoundation/lfx-v2-campaign-service/campaign-service` GHCR package using
[`snok/container-retention-policy`](https://github.com/snok/container-retention-policy),
invoked as a direct container reference (`docker://ghcr.io/snok/container-retention-policy@sha256:...`)
pinned by image digest, not by a mutable release tag.

Because this is a raw `docker://` reference rather than the
`snok/container-retention-policy@<sha>` action alias, there is no
`action.yaml` to translate `with:` inputs into CLI flags or fill in its
defaults — any flag this workflow doesn't pass in `with.args` falls back to
whatever default the CLI's own argument parser supplies, not `action.yaml`'s.
`--keep-n-most-recent` and `--timestamp-to-use` are safe to omit because the
CLI's own defaults happen to match what `action.yaml` would have passed
(`0` and `updated_at`). `--image-tags` and `--shas-to-skip` are different: the
CLI's argument parser requires both flags to be present, with no built-in
default, so this workflow must always pass them explicitly, even as empty
strings (`--image-tags=`, `--shas-to-skip=`). Omitting either one causes every
run to fail immediately.

## Triggers

- **Scheduled**: weekly, Sundays at 00:00 UTC. Uses fixed defaults —
  `cut-off: 30d`. `dry-run` currently defaults to `true` (preview only)
  because the package's existing ~15,000-version backlog means the first
  unattended run would otherwise face the whole backlog at once instead of
  a manageable weekly slice. A maintainer flips the fallback to `false` in
  the workflow file after reviewing a manual preview or draining the
  backlog manually.
- **Manual** (`workflow_dispatch`): a maintainer can preview or tune a single
  run via the `dry-run` (default `true`) and `cut-off` (default `30d`)
  inputs, without changing the schedule's defaults.

## Scope

`tag-selection` is hardcoded to `untagged` — tagged versions, including
per-commit SHA tags and any release/production tags, are never deletion
candidates. This is intentionally not exposed as an override: `ko build`
publishes every image with both an immutable SHA tag and a moving
branch-name tag (see `.github/workflows/ko-build-branch.yaml`), so the
untagged versions this workflow reclaims are the orphaned digests left
behind when a branch's moving tag is repointed to a newer build. SHA-tagged
versions keep accumulating and are a known, accepted limitation of this
rollout.

`snok/container-retention-policy` automatically protects multi-arch child
manifests still referenced by a retained parent index, so multi-platform
images are not partially deleted.

## Authentication

The workflow uses the default job `GITHUB_TOKEN` with `permissions: packages:
write` at the job level — no separate PAT is required. This works because
`image-names` names an exact package, not a wildcard; a wildcard target would
need a token with broader package visibility than a single job's
`GITHUB_TOKEN` grants.

## Auditability

Every run's job log lists each considered/deleted image version by digest
and age, and a final step writes a summary (trigger, mode, cut-off,
deleted/failed counts) to the run's `$GITHUB_STEP_SUMMARY`. The
`deleted`/`failed` fields in that summary come from the action's own
outputs and may render `(none)` even on a run that deleted versions, if the
underlying Docker action does not populate `GITHUB_OUTPUT`; the job log
itself is the authoritative record regardless. Accidental deletions are
recovered via GitHub's own package-version restore window, not by this
workflow.
