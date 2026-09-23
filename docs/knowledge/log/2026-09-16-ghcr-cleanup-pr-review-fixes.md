# 2026-09-16 — GHCR cleanup workflow: PR review fixes

**Fix** — addressed PR #212 review feedback on
`.github/workflows/ghcr-image-cleanup.yaml`:

- Dropped `CONTAINER_RETENTION_PAT` entirely. The workflow now authenticates
  with the default job `GITHUB_TOKEN` (`permissions: packages: write`),
  which can delete package versions because `image-names` names an exact
  package rather than a wildcard (per review feedback from a maintainer).
  This also resolves a related finding that the dry-run preview couldn't
  work before the PAT secret existed, since an empty token failed before
  `dry-run` was ever evaluated.
- Pinned the executed container by image digest
  (`docker://ghcr.io/snok/container-retention-policy@sha256:...`) instead of
  via the `snok/container-retention-policy@<sha>` action alias, whose pin
  only covered the action's metadata commit while the metadata itself still
  pointed at the mutable `v3.1.0` container tag.
- Fixed the run-summary step's `DRY_RUN` fallback, which still read `'false'`
  while the cleanup step's fallback had already been changed to `'true'`,
  causing scheduled dry-run previews to report `Dry run: false`.

Updated `docs/knowledge/architecture/ghcr-image-cleanup.md`'s Authentication
and intro sections to match.
