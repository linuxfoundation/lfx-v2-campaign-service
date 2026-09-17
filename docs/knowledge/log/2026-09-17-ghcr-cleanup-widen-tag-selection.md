# 2026-09-17 — GHCR cleanup workflow: widen scope to tagged versions

**Update** — widened `.github/workflows/ghcr-image-cleanup.yaml` from
`--tag-selection=untagged` to `--tag-selection=both`, adding
`--image-tags="!latest !development"`:

- Per-commit SHA-tagged versions from PR/main builds
  (`.github/workflows/ko-build-branch.yaml`) and superseded
  SHA + `development` pairings from older main builds
  (`.github/workflows/ko-build-main.yaml`) are now deletion candidates once
  past `cut-off`, instead of accumulating indefinitely as untagged-only
  scope allowed.
- The negative `--image-tags` filter protects any package version carrying
  a `latest` or `development` tag regardless of its other tags, so release
  builds (`.github/workflows/ko-build-tag.yaml`, tagged `latest` plus
  version strings) and the current main build (tagged `development`)
  always survive.
- `--shas-to-skip=` is passed empty rather than omitted: the underlying
  CLI's arg parser requires the flag present even when unset, confirmed by
  a live dry-run failure listing both `--image-tags` and `--shas-to-skip`
  as required when omitted.
- `dry-run` still defaults to `true` for both triggers, called out in a new
  workflow comment: this is the first run widening scope into tagged
  versions, on top of the existing ~15,000-version untagged backlog.

Updated `docs/knowledge/architecture/ghcr-image-cleanup.md`'s Triggers and
Scope sections to match, including the known tradeoff that an idle PR
branch's still-tagged image becomes a deletion candidate once its last
push is older than `cut-off`.
