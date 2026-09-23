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
invoked with `docker pull` / `docker run` against an image digest
(`ghcr.io/snok/container-retention-policy@sha256:...`), not a mutable
release tag and not the `snok/container-retention-policy@<sha>` action alias.

The action alias only pins action metadata; that metadata still points at the
mutable `v3.1.0` container tag. A raw `uses: docker://` step would pin the
executed image, but it has no `action.yaml` outputs and does not leave the
container's stdout in a file a later step can read. This workflow therefore
runs the digest-pinned image itself and tees stdout to `$RUNNER_TEMP` so the
summary step can count unique package versions.

Because there is no `action.yaml` to translate `with:` inputs into CLI flags
or fill in its defaults, any flag this workflow doesn't pass on `docker run`
falls back to whatever default the CLI's own argument parser supplies.
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
and age. The cleanup step tees the container's stdout to `$RUNNER_TEMP`, and
a final step (`if: always()`) writes a summary to `$GITHUB_STEP_SUMMARY`:
trigger, mode, cut-off, the tagged/untagged candidate counts snok logged
before multi-arch filtering, the number of multi-arch children it protected,
and unique package-version counts for deleted (or would-delete, on a dry
run) and failed. Counts are unique `package_version_id` values, not log
lines — one GHCR version that carries both a commit SHA and a branch-name
tag is one deletion and two log lines. snok v3.1.0's `deleted`/`failed`
action outputs are unused: the binary concatenates those values onto the
`GITHUB_OUTPUT` path string and calls `env::set_var`, which the runner never
sees, and a raw docker image has no `action.yaml` to publish them anyway.
The job log remains the per-version record. Accidental deletions are
recovered via GitHub's own package-version restore window, not by this
workflow.
