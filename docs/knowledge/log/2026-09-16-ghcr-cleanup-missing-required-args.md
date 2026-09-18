# 2026-09-16 — GHCR cleanup workflow: missing required CLI args

**Fix** — a manual `workflow_dispatch` dry-run of
`.github/workflows/ghcr-image-cleanup.yaml` failed immediately with:

```text
error: the following required arguments were not provided:
  --image-tags <IMAGE_TAGS>
  --shas-to-skip <SHAS_TO_SKIP>
```

Omitting a flag entirely from `with.args` is not equivalent to upstream
`action.yaml`'s behavior of always forwarding it (empty string when its own
input is unset). `snok/container-retention-policy`'s `clap`-based CLI treats
`--image-tags` and `--shas-to-skip` as required-to-be-present flags, even
though their values may be empty — unlike `--keep-n-most-recent` and
`--timestamp-to-use`, which have real non-empty defaults baked into
`action.yaml` (`'0'` and `'updated_at'`) and were never missing.

Added `--image-tags=` and `--shas-to-skip=` (both empty) to the `args` block,
matching what `action.yaml` would have forwarded had this been invoked
through the action wrapper instead of a raw `docker://` reference.
