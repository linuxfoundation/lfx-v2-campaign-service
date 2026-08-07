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

**Picking a version number when several PRs are open at once.** golang-migrate
records the HIGHEST version it has applied and afterwards only applies versions
above it, so a lower-numbered file that lands later is skipped SILENTLY — no
error, permanently unapplied. Two hazards follow, and neither is caught by CI on
either PR:

- A *gap*: numbering above versions that are still unmerged in sibling branches.
  Deploy this PR first and those lower versions never run.
- A *collision*: two branches independently claiming the same number.
  `TestMigrations_UniqueNumbering` globs only its OWN branch's embedded FS, so it
  is green on both PRs and only fails on whichever merges second.

So choose a number by checking `main` AND every open PR branch, not just `main`:

```sh
for b in $(gh pr list --json headRefName -q '.[].headRefName'); do
  echo "== $b"; git ls-tree "origin/$b" \
    internal/infrastructure/postgres/migrations/ --name-only | grep '\.up\.sql'
done
```

Take the next consecutive versions above everything already claimed, and prefer
leaving headroom over reusing a number a sibling branch might renumber into.

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
- `000010` / `000011` — campaign SOFT DELETE. `000002`'s full
  `UNIQUE (brief_id, platform)` made a campaign row occupy its brief's slot for that
  platform PERMANENTLY, so a campaign created with the wrong budget (or one whose
  upstream create failed ambiguously) blocked that pair forever with no recovery.
  `000010` creates the partial unique index `uq_campaigns_brief_platform_live`
  (`(brief_id, platform) WHERE status <> 'deleted'`) and `000011` drops the old
  constraint — mirroring what `000003` did for briefs on archive. Deleting a campaign
  now frees the slot for a re-dispatch while two LIVE campaigns for the pair are still
  rejected.

  The split into two versions is required, not stylistic: `000010` uses
  `CREATE INDEX CONCURRENTLY` (migrations run during a ROLLING startup, so a blocking
  build could stall an in-flight dispatch claim), which cannot share a file with other
  statements — a multi-statement migration is batched, reintroducing the implicit
  transaction CONCURRENTLY forbids. Splitting also gives the required ORDERING for
  free: golang-migrate applies versions ascending, so the replacement index always
  exists before the constraint it replaces is dropped. Dropping first would open a
  window with NO uniqueness, during which two concurrent claims could both win and
  double-create a paid campaign upstream. `000011` is a guarded `DO` block that
  REFUSES to drop the constraint unless the new index is present, `indisvalid`, and
  matches its required DEFINITION, because a failed CONCURRENTLY build does not roll
  back — it leaves an INVALID index that `IF NOT EXISTS` would silently skip rebuilding.
  Failing the migration loudly (leaving the old constraint protecting the table) is the
  correct outcome.

  The guard checks the definition, not just the name, and that distinction is
  load-bearing. Because `000010` builds with `IF NOT EXISTS`, ANY pre-existing index
  carrying the name `uq_campaigns_brief_platform_live` makes `000010` a silent no-op —
  and a name-only guard then accepts it and drops the sole real uniqueness constraint,
  leaving the pair with none: every claim wins and concurrent retries double-create paid
  campaigns, silently. So the guard proves `indrelid = public.campaigns`, `indisunique`,
  `indnkeyatts = 2` with key columns exactly `(brief_id, platform)` in order, and a
  partial predicate deparsing to `(status <> 'deleted'::text)`. Verified on PostgreSQL
  16.10: a non-unique index of the right name PASSED the old name-only guard and FAILS
  this one, as do a superset key list, a reversed column order, a non-partial index, a
  wrong predicate, an index on another table, and an INVALID index; an equivalent
  predicate spelled `!=` or with an explicit `::text` cast still passes, since the
  comparison uses the text Postgres itself deparses. Pinned by
  `TestMigration000011_GuardChecksIndexDefinition`.

  **The two versions cannot be staged apart, and the rollout strategy carries the
  ordering instead.** Deferring `000011`'s drop to a later release (the usual
  expand/contract remedy for a backward-incompatible change) does not work here: the old
  full constraint still covers soft-deleted rows, so a re-dispatch after a delete hits
  `ON CONFLICT ... DO NOTHING` and is SILENTLY swallowed — `RowsAffected` is 0, which
  `ClaimCampaignDispatch` reads back as "already claimed". Verified on PostgreSQL 16.10:
  with the drop deferred the re-dispatch INSERT returns `INSERT 0 0`; after the drop the
  same statement succeeds while a second LIVE claim is still rejected. So staging the drop
  would ship a delete endpoint whose entire purpose — freeing the slot — does not work, and
  fails silently rather than loudly.

  The genuine hazard staging was meant to address is real, though: the PREVIOUS release's
  bare `ON CONFLICT (brief_id, platform)` matches no index once the constraint is gone and
  errors on every dispatch claim (verified: works while the constraint exists, fails with
  "there is no unique or exclusion constraint matching the ON CONFLICT specification" after
  the drop). Since migrations run at pod boot against a shared database, Kubernetes' default
  `RollingUpdate` — which surges the new pod BEFORE terminating the old one — would put the
  migrated schema under the old code. The chart therefore pins `strategy.type: Recreate`, so
  the old pod is gone before the new one migrates. Pinned by
  `TestDeploymentUsesRecreateStrategy`; `replicaCount` is 1, so nothing is lost by dropping
  the surge.

  **Consequence for every `ON CONFLICT (brief_id, platform)`**: PostgreSQL infers the
  arbiter index by matching the conflict target AND its predicate, so once the full
  constraint is gone a BARE conflict target matches no index and fails at runtime with
  "there is no unique or exclusion constraint matching the ON CONFLICT specification".
  `ClaimCampaignDispatch` and `UpsertCampaign` therefore both carry
  `WHERE status <> 'deleted'`, pinned by
  `TestCampaignRepo_OnConflictCarriesLivePredicate`. Every campaigns read
  (`GetCampaign`, `GetCampaignByPlatform`, `ReplaceCampaign`) also filters deleted
  rows — load-bearing for `GetCampaignByPlatform`, which the orchestrator uses to
  decide whether a pair was already dispatched.

## DeleteCampaign's guards

`DeleteCampaign` takes a `SELECT status, version … FOR UPDATE` lock inside one
transaction BEFORE evaluating its guards. The lock is required, not decorative: under
READ COMMITTED each statement takes a fresh snapshot, so a `ClaimCampaignDispatch` that
commits just before a guarded `UPDATE` runs is invisible to its predicate — and because
the claim INSERTs a new row rather than updating this one, there is no row-level conflict
for Postgres to serialize on. Pinned by `TestDeleteCampaign_LocksRowBeforeGuards`.

The row lock alone is NOT sufficient, so `DeleteCampaign` first takes the same campaign
advisory lock `ClaimCampaignVersion` uses. `FOR UPDATE` serializes delete against the
dispatch path, which UPDATEs the row, but not against an in-flight run-state toggle: a
toggle holds its claim ACROSS the platform call, and between `ClaimCampaignVersion` and
`ReplaceCampaign` it holds no row lock at all. A delete committing in that window bumps
`version`, so the toggle's `ReplaceCampaign(expectedVersion)` fails AFTER the paid side
effect already landed upstream. Holding the advisory lock makes delete wait for the
toggle, then observe the bumped version and return an actionable 412.

The delete transaction begins on the connection already holding that advisory lock
(`conn.Begin`), never on the pool (`r.db.Begin`) — beginning on the pool would take a
SECOND connection while the first is held, self-deadlocking on a saturated pool
(`pool_max_conns=1` guarantees it). The unlock is deferred on a context detached from
the request, destroying the connection if the unlock fails, exactly as
`ClaimCampaignVersion` does: a session advisory lock is not released by returning the
connection to the pool, so a failed unlock strands it and blocks every future claim and
delete for that campaign. Pinned by
`TestDeleteCampaign_ParticipatesInAdvisoryLockProtocol`.

The guards, in order: `deleted` → `ErrNotFound` (a second DELETE is a 404, matching
`GetCampaign`, not a silent success); an unresolved reconciliation marker →
`ErrConflict`; then `version != expectedVersion` → `ErrPreconditionFailed` (checked LAST
so a stale ETag on an unresolved campaign yields the actionable conflict rather than a
412 implying a reload would fix it).

**The reconciliation guard covers three statuses, not one.** It keys off
`model.CampaignStatusNeedsReconciliation` — `pending` (a live dispatch claim, or one that
died mid-flight) plus the partial orphans `group_created` and `unconfirmed`. Enumerating
only `pending` was a real defect: soft-deleting overwrites `status` with `'deleted'`,
which erases the only local record that a half-created campaign may exist upstream AND
frees the `(brief, platform)` slot, so the next dispatch creates a fresh campaign with no
sign of the orphan. This is the same doctrine `CampaignStatusToggleable` enforces for the
run-state toggle.

`created_degraded` is deliberately DELETABLE though not TOGGLEABLE — the two predicates
differ on exactly that status, which is why they are separate functions. A degraded
campaign was fully created upstream, so its row is a complete record and retiring it
loses nothing; toggling it would erase a reconciliation marker that still matters.

The two orphan literals are exported from `internal/domain/model` solely so this package
can share them: they originate as unexported constants in `internal/dispatch` and an
unexported map in `internal/service`, neither of which postgres may import. Drift between
the copies is caught by `TestPartialOrphanStatusValues` (`internal/dispatch`), and the
predicate itself is pinned exhaustively over the whole status vocabulary by
`TestCampaignStatusNeedsReconciliation` (`internal/domain/model`) — a new status that
nobody classifies fails that test rather than silently defaulting to deletable.

Note the testing split: this repo has NO DB-backed test harness (no testcontainers,
dockertest or pgxmock; `make test` is a plain `go test`), and `CampaignRepo.db` is a
concrete `*Pool` with no interface seam. So the SQL is pinned as text and the guard
DECISION — the part that actually had the bug — is tested as pure logic in the model
package. A DB-backed test of `DeleteCampaign` would need a docker dependency in CI that
no other test here uses.

See [internal/infrastructure/postgres](../../../internal/infrastructure/postgres).
