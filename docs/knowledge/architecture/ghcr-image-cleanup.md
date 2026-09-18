---
type: "Architecture Doc"
title: "GHCR stale image cleanup"
description: "How the scheduled and on-demand GitHub Actions workflow removes stale GHCR image versions, tagged and untagged, for the campaign-service container package."
resource: ".github/workflows/ghcr-image-cleanup.yaml"
---

# GHCR stale image cleanup

[`.github/workflows/ghcr-image-cleanup.yaml`](../../../.github/workflows/ghcr-image-cleanup.yaml)
deletes stale versions, tagged and untagged, of the
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
  `cut-off: 30d`, `dry-run: false` — since `github.event.inputs` is undefined
  on a `schedule` trigger, the `dry-run` expression checks `github.event_name`
  directly to give scheduled runs their own default rather than inheriting
  the `workflow_dispatch` input default.
- **Manual** (`workflow_dispatch`): a maintainer can preview or tune a single
  run via the `dry-run` (default `true`) and `cut-off` (default `30d`)
  inputs, without changing the schedule's defaults.

## Scope

`tag-selection` is hardcoded to `both` — untagged versions and tagged
versions are both deletion candidates once past `cut-off`. `--image-tags`
carries the negative filter `"!v* !latest !development"`, which protects any
package version carrying a tag matching `v*`, `latest`, or `development`
regardless of its other tags: release builds
(`.github/workflows/ko-build-tag.yaml`) tag with a `vX.Y.Z` version string
plus `latest`, so `!v*` alone already protects every release; `!latest` is
redundant today but kept as defense in depth in case a future release build
ever tags `latest` without a version string. The current main build
(`.github/workflows/ko-build-main.yaml`) tags with `development`. Neither
workflow's tags ever share a digest with a plain PR/main SHA-tagged build,
so this excludes exactly the versions that must survive.

Everything else tagged — a per-commit SHA plus a branch name from
`.github/workflows/ko-build-branch.yaml`, or a superseded SHA + `development`
pairing from an older main build — is a deletion candidate once past
cut-off. Previously only fully-untagged versions were ever considered, so
per-commit SHA-tagged versions accumulated indefinitely; this widening to
`tag-selection=both` closes that gap. `tag-selection` and the fixed
`account`/`image-names` are not exposed as `workflow_dispatch` overrides.

Known tradeoff: a PR branch whose last push is older than `cut-off` still
carries a live branch-name tag, so its current image is now a deletion
candidate too, not just superseded commits on an active branch. This is
treated as normal cleanup of stale PR images; a maintainer can preview this
wider scope by triggering `workflow_dispatch` with `dry-run: true` (see
Triggers above), but nothing enforces that preview before a scheduled run.

Known tradeoff: `!v*` matches any tag beginning with `v`, not only
`vX.Y.Z` version strings — a branch name like `validate-something` or
`v2-refactor` built by `ko-build-branch.yaml` would also carry a `v`-prefixed
tag and be permanently protected from cleanup. This is accepted as
over-protection, the opposite failure direction from deleting a release.

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
