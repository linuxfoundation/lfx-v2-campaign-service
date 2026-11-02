# 2026-11-02 — 171: creative assets of briefs archived over 90 days ago are pruned

**Update** — `creative_assets` had no prune, so an archived brief kept its images (up to 30 MiB
each) forever, readable by nothing. A new hourly orchestrator sweeper,
`StartArchivedAssetRetentionSweeper`, now deletes the assets of briefs archived more than 90 days
ago (`domain.ArchivedBriefAssetRetention`, a constant). The container binds it to
`CreativeAssetRepo.PruneArchivedBriefAssets` on both wiring paths.

Each pass deletes batches of 100 until one comes back short, capped at 50 batches. Each batch is
one transaction under `pg_try_advisory_xact_lock("CAPR", 0)`, so one replica prunes at a time and
the others skip rather than wait. The archival time is the archived brief's `updated_at`, which
nothing changes after archival. Deleted counts are logged at info, a held lock at debug, and a
failure at error, retried on the next tick. There is no migration and no new index.

The per-brief upload cap shipped in #388 (100 assets / 512 MiB, one 409) is unchanged.
