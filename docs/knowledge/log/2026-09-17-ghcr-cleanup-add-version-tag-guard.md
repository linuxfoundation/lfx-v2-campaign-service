# 2026-09-17 — GHCR cleanup workflow: add `!v*` to the tag guard

**Update** — added `!v*` to `.github/workflows/ghcr-image-cleanup.yaml`'s
`--image-tags` filter, changing it from `"!latest !development"` to
`"!v* !latest !development"`:

- `!v*` is the pattern that actually protects every release build: each
  release (`.github/workflows/ko-build-tag.yaml`) is tagged with both a
  `vX.Y.Z` version string and `latest`, so matching on the version string
  protects releases without depending on `latest` also being present.
- `!latest` is kept in the filter as defense in depth: it is redundant today
  since every `latest`-tagged digest in this repo is also `v*`-tagged, but it
  still catches a version if a future workflow change ever tagged something
  `latest` without also giving it a version string.
- `!development` is unchanged and still protects the current main build.

`!v*` matches any tag beginning with `v`, not only `vX.Y.Z` version
strings — a branch name like `validate-something` or `v2-refactor` built by
`ko-build-branch.yaml` would also carry a `v`-prefixed tag and be
permanently protected from cleanup. Accepted as over-protection, the
opposite failure direction from deleting a release; documented as a new
Known tradeoff paragraph rather than narrowed to a more specific pattern.

Updated `docs/knowledge/architecture/ghcr-image-cleanup.md`'s Scope section
and the workflow file's inline comments to describe all three patterns and
which one is doing the actual protecting.
