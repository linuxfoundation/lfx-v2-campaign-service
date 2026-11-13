---
type: "Go Package"
title: "cmd/reindex"
description: "Operator CLI that re-enqueues search-index outbox messages for existing briefs and campaigns, built by the service's own payload builders and delivered by the running relay; dry run by default, -apply to write, batched and idempotent (#590)."
resource: "cmd/reindex"
---

# cmd/reindex

The service's only full-reindex path. A document indexed under an old contract is otherwise
corrected only by its resource's next write, and a terminal write such as an archive never
comes again. The motivating case is #590: documents keyed on the project slug before
`INDEXER_RESOLVE_PROJECT_UID` existed. That flag arrives with PR #596, so this command is
useful only once #596 is merged and released, and should merge after it.

## Usage

    go run ./cmd/reindex [-project <slug>] [-kind all|briefs|campaigns] [-batch 200]                       # dry run
    go run ./cmd/reindex [-project <slug>] [-kind all|briefs|campaigns] [-batch 200] -apply -relay-ready   # write

`-project` matches the `project_id` stored on briefs and campaigns, which is the project slug,
not its UID.

It needs the same database environment as the service: `DATABASE_URL` or `PGHOST`, `PGUSER`,
`PGPASSWORD` and `PGDATABASE`, read through `config.ResolveDatabaseURL`. Without `-apply` it
reads every batch and builds every payload, then rolls back and prints only counts.

## What it writes

Only `index_outbox` rows, through `postgres.ReindexRepo`. It never publishes to NATS. The running
service's relay delivers the rows, so the deployment's indexing settings apply: the service
credential, and UID keying when `INDEXER_RESOLVE_PROJECT_UID` is on.

`-apply` refuses to run without `-relay-ready`, the operator's statement that the **target
deployment's** relay is on. The command cannot read that deployment's settings, so it asks:
- `NATS_URL` must be set there. With it empty the service enqueues nothing, and a row written
  here could never be delivered and is never pruned.
- `INDEXER_RESOLVE_PROJECT_UID` must be `"true"` there. Otherwise the re-sent documents are keyed
  on the slug again.

Each payload comes from `service.BriefIndexPayload` or `service.CampaignIndexPayload`, the same
builders every write uses, so a re-sent message is byte-for-byte what a write would produce.

- A live brief or campaign gets `updated`.
- An archived brief or a soft-deleted campaign gets `deleted`.
- A bare dispatch claim (pending, with no upstream id and no result) is skipped and counted.
  `ClaimCampaignDispatch` never indexes it.

## Ordering and safety

- Rows are read in id order, one transaction per batch, `FOR SHARE`. Every write updates its row
  and enqueues its message in one transaction under the row lock.
  - A write in flight blocks the batch until it commits, and the batch then reads the newer row.
  - A later write waits for the batch, and its message comes after.
  - So the outbox's per-resource order always ends on the current state.
- Re-running re-sends current documents and is safe.
- Batches are 1–1000 rows (default 200), and one run is bounded at 100,000 batches. A cursor
  that fails to advance stops the run.
