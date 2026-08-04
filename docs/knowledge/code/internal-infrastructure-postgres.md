---
type: "Go Package"
title: "internal/infrastructure/postgres"
description: "PostgreSQL pool (otelpgx), migrations, repositories, and Ready() for readiness probes."
resource: "internal/infrastructure/postgres"
---

# internal/infrastructure/postgres

Package postgres provides the shared `pgxpool` connection pool (instrumented
with `otelpgx`), migration runner, and repository implementations.

`Pool.Ready` pings the database and is used by `/readyz` via the service
`ReadinessChecker` interface. Pool open fails fast on ping failure so
unreachable databases do not wedge startup.

## Migrations

SQL migrations live under `migrations/` and are embedded (`//go:embed *.sql`)
so golang-migrate's iofs source can run them from the compiled binary. Each
version is a paired `NNNNNN_name.up.sql` / `.down.sql`; applied versions are
never re-run, so a schema change is always a NEW version, never an edit to an
applied file.

- `000001` — connection tables.
- `000002` — brief, campaign, and async-job tables. Indexes: `campaign_jobs`
  on `brief_id`; `campaigns` on `project_id`. `(brief_id, platform)` /
  `(project_id, event_slug)` uniqueness covers those leftmost columns.
- `000003` — brief `project_id` UUID→TEXT and partial-unique
  `(project_id, event_slug)` excluding archived rows.
- `000004` — partial index `idx_campaign_jobs_recovery` on
  `campaign_jobs (updated_at) WHERE status IN ('queued','running')`, supporting
  the periodic stuck-job recovery sweep (`JobRepo.FailStuckJobs`) so it does not
  full-scan campaign_jobs as terminal job history grows.
- `000005` — `campaign_audiences` table (email channel, epic LFXV2-2770): a built
  audience subordinate to a brief (`brief_id` REFERENCES `campaign_briefs(id)`),
  storing a POINTER + provenance (`platform_master_list_id`, `suppression_list_ids`,
  `inclusion_summary`, `status` building/built/failed, `version`) to the audience
  that physically lives in the platform (a HubSpot master list) — NOT its contents.
  Indexed on `brief_id` and `project_id` (a brief may have many audiences, so there
  is no natural uniqueness, hence both get their own index). `AudienceRepo`
  (create/get/list/update, project-scoped, optimistic-concurrency update via
  `ErrPreconditionFailed`) implements `domain.AudienceRepository`.
- `000006` — CHECK constraint `campaign_audiences_platform_valid`
  (`platform IN ('hubspot')`) enforcing the platform enum at the datastore, mirroring
  the `status` CHECK on 000005 — the DSL `Enum("hubspot")` guards it only at request
  time, so a direct/worker write could otherwise persist an unsupported platform. Plus
  the CHECK constraint `campaign_audiences_built_needs_master_list`
  (`status <> 'built' OR (platform_master_list_id IS NOT NULL AND platform_master_list_id ~ '[^[:space:]]')`)
  enforcing the built-audience invariant at the datastore: `built` means the platform
  master list exists, so a built row must carry a genuinely non-blank pointer. The
  service layer already 400s this on create/update, but the constraint also stops the
  platform build worker and direct writes from persisting an inconsistent "built" row.
  The `~ '[^[:space:]]'` test requires at least one non-whitespace character — it rejects
  `''` AND tab/newline-only ids, matching the app's `strings.TrimSpace` (a plain
  `btrim(...) <> ''` would only strip ordinary spaces, letting a tab/newline id through).
- `000007` — composite tenant foreign key on `campaign_audiences`: replaces the
  `brief_id`-only FK with `(brief_id, project_id) REFERENCES campaign_briefs (id,
  project_id)` (and adds the `UNIQUE (id, project_id)` on `campaign_briefs` a composite
  FK requires). The old FK validated only the brief id, so a worker/backfill/direct
  write could persist an audience whose copied `project_id` differed from its brief's —
  and `GetAudience`, which trusts the stored `project_id` for tenant scoping, could then
  expose it under the wrong tenant. The API create path already guards this (`INSERT …
  WHERE EXISTS` an active brief scoped by project+brief); the FK makes the datastore the
  source of truth for all writers.

See [internal/infrastructure/postgres](../../../internal/infrastructure/postgres).

## ReconciliationRepo

`ReconciliationRepo` backs the operator-reconciliation endpoints.

`ListReconciliationItems` classifies non-settled campaign rows IN SQL, next to the
predicates the classification derives from. A row is a releasable BARE claim only when it
is `pending` AND has no `platform_campaign_id` AND no `result` — that combination is the
service's own definition of "carries no evidence of an upstream create" (the orchestrator
persists `Result` precisely so an ambiguous create is not mistaken for a bare claim).
Everything else non-settled is an `unconfirmed_campaign`, never releasable. Settled
statuses (`created`, `created_degraded`, and the run states) are excluded: a degraded
campaign genuinely exists upstream and a re-dispatch cannot repair it, so it is not an
operator action item. `campaign_audiences` rows left `building` are reported alongside.

`ReleaseDispatchClaimByID` is the safety-critical path. It re-verifies EVERY precondition
inside ONE transaction under `SELECT … FOR UPDATE`: still `pending`, still no upstream id
and no result blob, still matching the operator's `expectedVersion`, and still older than
`minAge`.

A plain guarded `DELETE` is NOT sufficient, and this was verified against a real database
rather than by inspection. A claim can be legitimately RE-CLAIMED by a new dispatch
between the operator's read and their write, which bumps `version` and resets
`created_at`; a status-only guard deletes that LIVE claim, freeing the pair while a
provider call is in flight so a concurrent dispatch double-creates a paid campaign. The
version gate refuses it. The age floor is the backstop for the case the version gate
cannot see: a claim DELETEd and re-INSERTed restarts at version 1, which can coincide with
the observed version — but a fresh row is young.

`partialOrphanStatuses` duplicates the literals from `internal/service` (which imports
this package, so a real import would cycle). `TestPartialOrphanStatusesMatchService` is
what keeps them from drifting: an omitted status here would classify a partial orphan as a
releasable bare claim, the exact mistake that authorizes a duplicate paid campaign.

The reconciliation SQL is exercised against a REAL PostgreSQL instance in
`reconciliation_repo_livedb_test.go`, which skips unless `RECON_TEST_DATABASE_URL` is set.
FOR UPDATE locking and READ COMMITTED snapshot semantics cannot be reproduced by a mock,
so a fake-repo test asserting on query strings would pass against a query that deletes
live claims.
