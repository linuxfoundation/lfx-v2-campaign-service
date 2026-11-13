# 2026-11-13 — #590 cmd/reindex re-enqueues index messages for existing resources

**Creation** — The service had no full-reindex path. Documents indexed under an old contract (keyed
on the project slug before `INDEXER_RESOLVE_PROJECT_UID`) were corrected only by a later write.

- New `cmd/reindex` and `postgres.ReindexRepo`.
  - It enqueues one outbox row per brief and campaign: `updated` for live ones, `deleted` for
    archived or soft-deleted ones. Bare dispatch claims are skipped.
  - It works in id-ordered batches, `FOR SHARE`, one transaction each.
  - It is a dry run by default; `-apply` writes. `-project` and `-kind` narrow the run.
- `service.BriefIndexPayload` and `service.CampaignIndexPayload` are now the shared payload
  builders. The service's three closures call them too, so the command cannot drift from a write.
- Run it after `INDEXER_RESOLVE_PROJECT_UID` is on in the environment.
- Tests:
  - `cmd/reindex/main_test.go`: arguments, paging, dry run and apply, the payload matching the
    service builder, and the cursor guard.
  - `TestLiveReindexEnqueuesEveryResourceOnce`.
