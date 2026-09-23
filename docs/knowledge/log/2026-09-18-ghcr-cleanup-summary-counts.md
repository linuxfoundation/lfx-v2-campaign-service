# 2026-09-18 — GHCR cleanup workflow: summarize unique version counts

**Fix** — the run summary for
`.github/workflows/ghcr-image-cleanup.yaml` reported `Deleted: (none)` /
`Failed: (none)` on a real deletion run. Manual `workflow_dispatch` of
[run 35252765254](https://github.com/linuxfoundation/lfx-v2-campaign-service/actions/runs/35252765254)
(`dry-run: false`, `cut-off: 45d`) deleted 4,575 unique package versions
(4,626 tag-level log lines) with no failed deletes; protected `latest`,
`development`, and `v1.0.13`; and finished successfully. The summary still
printed `(none)` because `DELETED` / `FAILED` were empty.

Two plumbing problems stacked. The job invoked the tool as a raw
`docker://` image, so there was no `action.yaml` to publish
`outputs.deleted` / `outputs.failed`. Independently, snok
`container-retention-policy` v3.1.0 never writes those values to the
`GITHUB_OUTPUT` **file**: it concatenates `deleted=` / `failed=` onto the
path string and calls `env::set_var`, which cannot reach the runner. The
comma-separated image list would also have been the wrong summary for a
run this large.

The cleanup step now `docker pull`s the same digest-pinned image and
`docker run`s it with `tee` into `$RUNNER_TEMP`. The summary step
(`if: always()`) strips ANSI from that log and counts unique
`package_version_id` values for real deletes, unique `package_version`
values for dry-run "would have deleted" lines, and unique IDs from
snok's failed-delete errors. It also surfaces the tagged/untagged
candidate counts and the multi-arch children snok protected. A dry run
reports `Would delete` and `Deleted: 0` so a preview cannot be mistaken
for a live deletion. The job log remains the per-version record.

Updated `docs/knowledge/architecture/ghcr-image-cleanup.md`'s intro and
Auditability sections to match. Companion operational issue:
[linuxfoundation/lfx-self-serve-ops#26](https://github.com/linuxfoundation/lfx-self-serve-ops/issues/26).
