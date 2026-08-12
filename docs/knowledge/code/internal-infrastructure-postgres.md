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
- `000013` / `000014` — campaign SOFT DELETE. `000002`'s full
  `UNIQUE (brief_id, platform)` made a campaign row occupy its brief's slot for that
  platform PERMANENTLY, so a campaign created with the wrong budget (or one whose
  upstream create failed ambiguously) blocked that pair forever with no recovery.
  `000013` creates the partial unique index `uq_campaigns_brief_platform_live`
  (`(brief_id, platform) WHERE status <> 'deleted'`) and `000014` drops the old
  constraint — mirroring what `000003` did for briefs on archive. Deleting a campaign
  now frees the slot for a re-dispatch while two LIVE campaigns for the pair are still
  rejected.

  The split into two versions is required, not stylistic: `000013` uses
  `CREATE INDEX CONCURRENTLY` (migrations run during a ROLLING startup, so a blocking
  build could stall an in-flight dispatch claim), which cannot share a file with other
  statements — a multi-statement migration is batched, reintroducing the implicit
  transaction CONCURRENTLY forbids. Splitting also gives the required ORDERING for
  free: golang-migrate applies versions ascending, so the replacement index always
  exists before the constraint it replaces is dropped. Dropping first would open a
  window with NO uniqueness, during which two concurrent claims could both win and
  double-create a paid campaign upstream. `000014` is a guarded `DO` block that
  REFUSES to drop the constraint unless the new index is present, `indisvalid`, and
  matches its required DEFINITION, because a failed CONCURRENTLY build does not roll
  back — it leaves an INVALID index that `IF NOT EXISTS` would silently skip rebuilding.
  Failing the migration loudly (leaving the old constraint protecting the table) is the
  correct outcome.

  The guard checks the definition, not just the name, and that distinction is
  load-bearing. Because `000013` builds with `IF NOT EXISTS`, ANY pre-existing index
  carrying the name `uq_campaigns_brief_platform_live` makes `000013` a silent no-op —
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
  `TestMigration000014_GuardChecksIndexDefinition`.

  **The two versions cannot be staged apart, and the rollout strategy carries the
  ordering instead.** Deferring `000014`'s drop to a later release (the usual
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

- `000015` — `created_by` / `updated_by` JSONB on `campaign_briefs` (see *Actor
  attribution* below).

- `000016` — the same two columns on `campaigns`. Deliberately a separate version, not
  a second `ALTER` inside `000015`: the write paths differ in KIND, not just in table
  (see *Async attribution on campaigns* below), and that difference is a signature change
  across the dispatch path. Ordering matters — `000016` must reach `main` AFTER `000015`,
  because golang-migrate tracks a single version integer and would step straight past a
  lower version that appeared later.

- `000018` — the AUDIENCE BUILD LEASE: partial unique index
  `uq_campaign_audiences_brief_platform_building` on `(brief_id, platform)
  WHERE status = 'building'`. `BuildAudience` creates real HubSpot lists before it can
  possibly know a sibling build is doing the same, and the two cannot collide by list
  NAME because the plan's `BuildRef` is the audience row's own id — chosen precisely so
  a later build does not adopt an earlier one's lists. So two concurrent builds for one
  brief produced two complete, indistinguishable sets of lists in the portal, and
  nothing downstream noticed. The index makes the second insert fail with SQLSTATE
  23505, which `audience_repo.go` maps to `domain.ErrAudienceBuildInFlight` — matching
  the index BY NAME (`isUniqueViolationOn`, not the SQLSTATE-only `isUniqueViolation`),
  so the next unique index added to `campaign_audiences` does not inherit a sentinel
  that means "a build is already running".

  The predicate covers `'building'` ONLY, and deliberately not the `status <> 'deleted'`
  shape `000013` uses for campaigns. A brief has exactly one LIVE campaign per platform,
  but `000005` records that it may have MANY audiences over time, and constraining
  `'built'` rows would make the first successful build permanent and every rebuild a
  409. The lease is about concurrency, not history.

  A build that DIES holding the lease keeps blocking rebuilds, and that is the intended
  outcome rather than a gap: its HubSpot lists exist, so the old "just build again"
  answer is what duplicated them. `PATCH update-audience` moving the row to `failed`
  frees the slot, and is the escape hatch an operator uses AFTER reconciling the portal.
  Automatic stale-lease takeover is deliberately absent — taking over a row that may own
  real lists re-creates exactly the duplication the lease closes.

  Single statement per file for the same reason `000013` is: `CREATE INDEX
  CONCURRENTLY` cannot run inside a transaction, and the pgx/v5 golang-migrate driver
  only avoids one because it issues a bare `ExecContext`; a second statement would be
  batched and reintroduce it. Unlike `000013` there is no constraint to drop, so no
  guarded `DO` block follows — but the INVALID-index hazard is the same, and
  `TestAudienceBuildLeaseIndexIsValid` in `dbtest` asserts `indisvalid`/`indisready`
  rather than mere existence, because a failed concurrent build leaves the NAME in place
  and `IF NOT EXISTS` would then skip the rebuild while reporting success.

- `000019` — `updated_by` JSONB on `campaign_audiences`. Only the one column: that table
  has carried `created_by` since `000005`, and `000015` recorded the missing half as a
  known gap. This version moved twice before landing, and the moves are the lesson.
  Migrations are numbered against a snapshot of `main` that a concurrent branch can
  invalidate, and golang-migrate records only the HIGHEST applied version — a lower number
  arriving later is skipped silently and permanently, so a collision surfaces not as an
  error but as a missing column. It was `000017` until #93 merged and took that number,
  then `000018` until #106 (the lease above) was found to hold it. It moved rather than
  #106's on the REFERENCE COUNT: #106's number is named in `pool.go`'s version-forcing
  recovery path and in three of its tests (`migration 000018: force 17`), whereas this one
  was named in a single test and two documentation lines. **Renumber the branch with the
  fewest references to the number, not the branch you happen to be in** — a migration
  version leaks into prose and recovery code, and the leak, not the file name, is the cost.
  See *Migration numbering* below.

### `Migrate` refuses to succeed over an INVALID index

The hazard above cannot be closed by a test alone, because it is a SEQUENCE that ends in
production catalog state: a `CREATE INDEX CONCURRENTLY` fails, leaving the index present
and invalid while golang-migrate marks the version dirty; an operator reconciles the data
and forces the version back; the re-run finds the NAME, does nothing, reports success. The
version is then clean over an index that enforces nothing — and every assertion that looks
the index up by name still passes. Migration tests run on a fresh database and can never
see it.

So `Migrate` ends with `checkNoInvalidIndexes`, a catalog read for
`pg_index.indisvalid = false` in the current schema. The scan is schema-wide, not scoped
to names this repo knows: an invalid index is never an intended state, and every future
CONCURRENTLY migration gets the check for free. That breadth is also why the RECOVERY
advice is attached per NAME rather than to the sentence. Dropping the debris is always
step one, but "then force `<version-1>`" is only right for an index a migration creates —
a hand-built index, another tool's, or two hits from different migrations each make one
blanket version wrong, and forcing an unrelated version replays unrelated DDL.
`describeInvalid` therefore annotates each name with its owning migration and tells the
operator to leave the version alone for anything else.

Ownership comes from the MIGRATIONS themselves — `migrationIndexOwners` parses every
`*.up.sql` in the embedded FS for its `CREATE … INDEX` statements — and not from
`requiredIndexes`. That distinction is the whole safety of the advice, not a style
preference. `requiredIndexes` is deliberately narrow: it lists only indexes whose ABSENCE
is silent, so most migration-created indexes are legitimately absent from it —
`idx_campaigns_stuck_claims` (000008) among them, a performance index whose loss makes the
stuck-claim scan full-scan forever. Derive ownership from that list and an operator holding
an invalid copy of it is told to drop it and leave the schema version alone, which deletes
the index permanently and boots clean. **Any list narrower than "every index a migration
creates" produces that class of answer; the migrations are the only set that is not.** The
narrowness is not incidental either: `requiredIndexes` grows only when an index's absence
would be silent, and the invalid-index scan must keep annotating correctly for every index
it does not list, both today's and the ones added after this code was written.

**Narrow is not the same as short, and the first draft of the list confused the two.** It
held one entry — the index the change at hand created — while the rule it stated ("a unique
index standing in for a constraint") admits ten. The seven per-provider connection indexes
from 000001 and `uq_campaign_briefs_project_event` from 000003 are exposed identically:
000001 declares no table-level UNIQUE constraint at all, and 000003 DROPs
`campaign_briefs_project_id_event_slug_key` before creating its replacement, so in each case
the partial index IS the constraint. The failure mode this produces is worth naming, because
it is invisible from inside the change that produces it: **a guard that covers one of ten
identically-exposed cases reads, from the boot log, exactly like a guard that covers all
ten.** The other nine were never judged less important — they were just not what anyone was
looking at. The seven connection entries are therefore GENERATED from the table list rather
than written out, so an eighth provider added to the schema and the list is covered without
anyone remembering, and `TestMigrateRefusesEachDroppedSingletonIndex` drops each one against
a live database and requires `Migrate` to refuse. The counts above are the ten that existed
when the list was written; 000020's `uq_campaigns_platform_campaign_live` is the eleventh,
and it arrived by exactly the route this paragraph warns about — the index was created on one
branch while the list was introduced on another, so neither tree was wrong on its own and the
schema-wide check in `TestEveryUniquePartialIndexIsRequired` is what caught it. That test
enumerates unique PARTIAL indexes from `pg_index` rather than from any hand-written list,
which is why a count stated in prose can go stale here without the guard going stale with it.
The parser matches the CREATE, not the name anywhere in the file, so a migration that
DROPs an index is not reported as the version to force back to; where two migrations
create one name, the highest wins. `TestMigrationIndexOwners_FindsEveryCreatedIndex`
re-derives the names a different way and fails if the map misses any.

**"Creates" means creates UNCONDITIONALLY, which is why `executableSQL` strips the body of
every dollar-quoted block before the scan.** The remedy this map feeds is "DROP the index,
then force back so the migration RUNS again" — and that only recovers the index if the
CREATE fires against a schema where the index is ABSENT. A `DO $$ … $$` block exists
precisely to make DDL conditional, and 000009's condition is "an INVALID copy is present":
the operator's drop, the first half of the remedy, is exactly what makes it false. Count
that rebuild as ownership and an operator is told to force 8, watches 000009 no-op, and
boots clean with `idx_campaigns_stuck_claims` gone for good — the stuck-claim scan silently
full-scanning forever, the very failure 000008 and 000009 exist to prevent. Skipping the
block hands them 000008, whose plain `CREATE INDEX CONCURRENTLY … IF NOT EXISTS` does fire
once the name is gone; forcing one version further back replays 000009 too, which then
correctly no-ops. The general rule: **ownership means "re-running this migration against a
schema missing the index rebuilds it". A conditional create cannot promise that — it is
repair, and repair is not a recovery target.**

`executableSQL` also strips `--` comments, because these migrations DISCUSS the statements
they avoid: 000009's header contains the phrase "a failed CREATE INDEX CONCURRENTLY", from
which the regexp reads the index name `does`. Inert — nothing is called that — but a scan
whose output depends on prose is one comment edit away from claiming a migration owns an
index it never touches. The block stripping pairs delimiters in CODE rather than in one
pattern because a `$tag$…$tag$` regexp needs a BACKREFERENCE and RE2 has none; a
tag-agnostic pattern would close one block on the next block's OPENING delimiter and delete
the executable statements between them.

Any hit returns `ErrInvalidIndex`,
which `IsPermanentMigrationErr` reports as permanent — and "permanent" here means the
container stops trying, not that it keeps trying. Retrying rebuilds nothing, so both
startup paths refuse rather than loop: `NewContainer` returns the error, which crashes the
pod loudly on initial startup, and the background initializer that runs after a TRANSIENT
failure logs at ERROR and RETURNS, leaving `/readyz` at 503 with no live pool and no
further attempts. Either outcome is better than serving over a lost UNIQUE constraint, and
both are better than a boot-loop: an operator has to force the migration version by hand,
and a restart cycle would only bury that fact under repeated startup noise. The check is
schema-wide rather than scoped to one index name — an invalid index is never an intended
state, and every future `CONCURRENTLY` migration inherits the guard instead of needing its
own assertion. It also runs on the `ErrNoChange` path, so a pod booting against an already
damaged schema refuses rather than quietly accepting what the index was added to prevent.
A connect/query failure inside the check is NOT wrapped in the sentinel, so ordinary
unreachability stays retryable. `TestMigrateRefusesAnInvalidIndex` provokes a genuine
invalid index (a unique `CONCURRENTLY` build over duplicate rows) and asserts the refusal.

### …and refuses to succeed with the index MISSING, which is where its own remedy leads

The instruction "drop the invalid index and re-run" is only half a recovery, and the other
half is not optional. Once the drop is done, migration 18 is still recorded CLEAN, so
`Up()` returns `ErrNoChange`, nothing rebuilds the index, the invalid-index scan finds
nothing wrong, and boot succeeds against a schema with **no uniqueness at all** — the same
silent loss the scan exists to prevent, arrived at by following the scan's own advice. The
index has to be rebuilt, which the error message now says by emitting the index's own
`CREATE ... INDEX` statement per name — see *A required index recovers by REBUILD* below for
why that superseded the "force the version back" advice this paragraph used to carry.

Instruction alone is not a control, so detection changed shape: not "nothing is invalid"
but "the index that enforces the invariant is PRESENT and valid". `requiredIndexes` names
them and `ErrMissingRequiredIndex` reports a gap, permanent for the same reason — the
recorded version is already correct, so `Up()` returns `ErrNoChange` forever and no retry
rebuilds anything until an operator runs the emitted DDL.

Membership in that list is deliberately narrow: an index belongs only if its absence is
SILENT, meaning it stands in for a constraint and every write it was serializing succeeds
without it. A performance index going missing makes the service slow, not wrong, and does
not qualify. The hand-maintained list is kept honest by
`TestMigrateRefusesADroppedRequiredIndex`, which drops each name and requires `Migrate` to
notice — an entry naming an index no migration creates fails there rather than sitting in
the list as decoration.

Ten indexes qualify, and the count is worth stating because the list started at one. Three
are written out by hand and the other seven are generated by `connectionSingletonIndexes` —
one per provider connection table, all identical but for the table and the name, generated
precisely so the eighth provider cannot be added to the schema and forgotten here.
`uq_campaign_briefs_project_event` (000003) keeps one live brief per (project, event slug).
`uq_campaign_audiences_brief_platform_building` (000018) is the
audience-build lease. `uq_campaigns_brief_platform_live` (000013) is the arbiter of
`ClaimCampaignDispatch`, and since 000014 dropped `campaigns_brief_id_platform_key` it is
the **only** thing enforcing at most one live campaign per `(brief_id, platform)`. 000014's
drop-guard pins that same definition — deliberately, not redundantly: the guard runs once,
at migration time, and cannot speak for the schema a year of operations later. Two checks
on one definition is the design.

### A required index recovers by REBUILD, never by `migrate force`

The recovery advice in these errors has been wrong twice, in two different ways, and the
second correction is the one that matters because it changed the KIND of advice rather than
its accuracy.

**Round one — per message.** The missing-index error ended in a single
`migrate force <version-1>`. Correct while the registry held one entry, wrong the moment it
held two, because the two are created by different migrations: an operator missing
`uq_campaigns_brief_platform_live` who follows that sentence to force 17 replays 000018,
rebuilds the *audience* index, and boots against a campaigns table that still enforces
nothing — having done exactly what the message said. Advice that is right for the first name
in a list and wrong for the second is worse than no advice, because it gets followed.

**Round two — per name, and still unsafe.** Annotating each name with its own owning
migration fixed the mis-targeting and left the real hazard untouched: `migrate force N`
followed by `Up()` replays **every migration above N**, not just the one that owns the index.
This chain is not uniformly replay-safe. `000006` and `000007` carry bare
`ALTER TABLE … ADD CONSTRAINT`, and PostgreSQL has no `ADD CONSTRAINT IF NOT EXISTS`, so
replaying them against a schema that already has those constraints fails with **42710** and
leaves the version DIRTY. The hazard therefore scales with the DISTANCE forced back — which
is precisely why it stayed invisible: while every annotated index came from 000013 or later,
the replayed range was all `IF NOT EXISTS` DDL and the advice happened to work. It became
unsound the moment the seven `000001` connection indexes and `000003`'s brief index joined
the registry, because their annotation is "force 0" — replay everything.

That is a property of the RANGE, not of the index, so no amount of more careful annotation
fixes it.

**What it is now.** Every `requiredIndex` carries uniqueness, table, key order and deparsed
predicate — enough to emit the index's own DDL. `createSQL` does, `indexRecovery` prefers it
for any name in the registry, and both messages print it: the missing-index error says "run
each statement below", the wrong-definition error says "DROP each index listed and then run
the statement beside it". A rebuild restores exactly the missing thing, replays nothing, and
needs no version change — the recorded version was never wrong, the schema drifted underneath
it. The force branch survives only for a name the registry has no DDL for, which is what
`describeInvalid` still needs: the invalid-index scan is schema-wide and turns up indexes like
`idx_campaigns_stuck_claims` that no `requiredIndex` describes.

**The advice is now checkable, and that is the actual win.** A version number is not
executable, so nothing could ever confirm that following it recovered anything — and it did
not. `TestRequiredIndexCreateSQL_RebuildsAnIndexTheCheckAccepts` drops each required index
against a live database, runs the exact statement the error prints, and requires the next
`Migrate` to succeed. `TestRequiredIndexes_RecoverByRebuildNotByForce` fails if any entry's
recovery ever contains the word `force` again.

Three generalisations worth keeping:

- **Advice that names a version is advice about a RANGE.** Ask what else replays before
  concluding a force is safe, and re-ask it whenever an entry from an older migration joins
  the list — the answer is not a property of the entry.
- **Prefer remedies that can be executed by a test.** The per-name annotation was defensible
  in review for two rounds precisely because nothing could run it.
- **Any sentence that refers the reader to an annotation must be tested against a message
  that carries one.** The wrong-definition path spent a round telling operators to force "the
  version annotated against it" while printing no annotation at all — advice that cannot be
  followed, which reads as correct in review and is useless in an incident.

### The check is on the DEFINITION, because the name is what IF NOT EXISTS matches

"Present and valid" under the right name is still not enough, and the reason is the same
`IF NOT EXISTS` that produced the invalid-index case. Any index carrying the name makes
migration 000018's `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS` a silent no-op — so a
non-unique index, one on a superset of the keys, or one with a different predicate leaves
the real constraint unbuilt and then *satisfies* a name-only check. Boot succeeds,
concurrent builds are unconstrained, nothing reports it.

This is not a hypothetical: migration 000014's drop-guard was written to close exactly this
hole, and `TestMigration000014_GuardChecksIndexDefinition` records the PostgreSQL 16.10 run
where a same-named NON-unique index passed the name-only form. Each `requiredIndex` entry
therefore carries uniqueness, relation, key columns in order, and the predicate **as
Postgres deparses it** — comparing against the deparsed form is what lets an equivalent
spelling (an explicit `::text` cast, different whitespace) compare equal instead of raising
a false alarm. `indisready` is checked alongside `indisvalid` because the two fail apart: a
`CONCURRENTLY` build that dies between phases can leave an index valid but not ready.

Absent and wrong-definition are **separate sentinels** (`ErrMissingRequiredIndex`,
`ErrRequiredIndexMismatch`) because their recovery differs in a way an operator cannot
guess. An absent index needs only its rebuild statement run. An impostor must be DROPPED
*first* — rebuild without dropping and `CREATE` finds the name taken, leaving the operator
exactly where they started. The message names every
defect it found, not the first: told only "wrong definition", an operator rebuilds
something that is still wrong.

One consequence worth stating for anyone writing a live test against this schema: a
cleanup that restores the lease index must **not** use `Migrate`'s return value as its
success signal. `Migrate` answering "clean" is precisely the regression these tests exist
to catch, so a cleanup that trusts it restores nothing when the guard is weakened, and
every later lease test in that database then passes against an unconstrained table.
`restoreLeaseIndex` drops, re-creates, and verifies against `pg_index` — and drops rather
than using `IF NOT EXISTS`, since an impostor carries the right name.

### Version gaps are exempted, then the exemption is retired

`TestMigrations_NoVersionGaps` refuses a migration numbered above a version that does not
exist in the tree, for the reason just given: a tree that deploys with a gap skips whatever
later fills it, silently and permanently. `allowedVersionGaps` (in `outbox_repo_test.go`)
suspends that guard at one version so a branch can stay green while a sibling PR that claims
the number is still open — a merge-ORDERING obligation recorded in code, not a numbering
opinion.

The obligation is discharged when the sibling lands, and the entry must come out with it:
left behind, it is a hole in the guard at exactly the version most likely to be reused next.
`TestMigrations_AllowedVersionGapsAreStillOpen` enforces that, failing as soon as an entry
names a version that now exists. `000016`'s exemption (for PR #95) was retired that way in
LFXV2-3068; the map is left declared and empty, with the retired entry's rationale as a
comment, so the next PR to need a gap finds the mechanism rather than re-inventing it.

## Actor attribution

Campaigns execute under **system accounts** — shared, LF-owned platform credentials —
so every ad platform reports one identity no matter who acted. The platform can
therefore never answer "who did this", and if this service does not record it, the
information exists nowhere. `campaign_briefs.created_by` / `updated_by` are that record.

### Briefs

Both are JSONB holding a `model.Actor` (`{name, email, username}`), marshalled by the
same `marshalActor`/`unmarshalActor` pair the connection tables use (`connection_repo.go`),
and populated from `actorFromCtx` — the principal `JWTAuth` decodes out of the bearer token.

**Trust boundary.** `JWTAuth` does NOT verify the token signature or audience; it decodes
the payload and takes the claims at face value. Signature and audience validation happen
UPSTREAM, in Heimdall/OpenFGA at the gateway, before the request reaches this service (see
`ConnectionService.JWTAuth`). The integrity of this audit trail therefore rests entirely on
that gateway: a request that reached the service with a forged token would produce a forged
attribution row, and nothing here would detect it. In-app JWKS verification is a follow-up.

`scanBrief` surfaces corrupt actor JSON as an error rather than returning a nil audit
trail, so data corruption fails loudly instead of looking like "not recorded".

**Retention.** These columns hold personal data — a name and an email address — and
nothing prunes them. The rows live as long as the brief does, by design: an audit trail
that expires answers "who did this" only for recent writes. There is no deletion path
today; adding one is a compliance decision, not a schema one.

Three properties are load-bearing, and each is pinned by a test in `brief_repo_test.go`:

- **The stamp is in the SAME statement as the write.** `createBriefQuery`,
  `replaceBriefQuery`, `approveBriefQuery` and `archiveBriefQuery` are package constants
  precisely so this can be asserted. A follow-up `UPDATE` would compile and pass every
  other test while leaving a committed window in which the row had changed and the
  attribution had not; a crash inside that window loses the actor for a change that
  stuck. (`TestBriefWrites_StampTheActorInTheSameStatement`, which strips the `RETURNING`
  clause first — every statement returns `briefCols`, which NAMES both actor columns, so
  an unstripped match passes for a statement that writes neither.)
- **`created_by` is written once.** No `UPDATE` assigns it, or every edit would look
  like authorship and the row would end up claiming its last editor wrote it.
  (`TestBriefWrites_UpdatesNeverTouchCreatedBy`.)
- **Insert stamps BOTH** (`VALUES … ,$11,$11`). Leaving `updated_by` NULL until the
  first edit makes "who touched this last" unanswerable without also consulting
  `created_by`; the two diverge from the first update onwards, which is when it matters.

### Async attribution on campaigns

Everything above describes a write on the REQUEST goroutine, where reading the actor at the
point of the write is enough. Campaign creation is not one. `Orchestrator.Start` returns as
soon as the job row exists; the dispatch runs on `o.rootCtx` in a goroutine that outlives the
request. No context reachable from inside `dispatchPlatform` carries an actor, so an
`actorFromCtx` at the INSERT would return nil for every campaign ever created — silently, with
no error and no log line.

So `Start` captures it while the request context is still in hand and threads it down through
`run` → `dispatchPlatform` → `ClaimCampaignDispatch`. `by` is therefore a PARAMETER on that
repository method rather than something the repo reads from its ctx. What is captured is the
DECODED actor value, never the bearer token: a token captured for asynchronous use may be
expired by the time the work runs and there is no retry, while a decoded value has no expiry
and is the exact thing being recorded.

Three consequences follow, and they are what the SQL encodes:

- **The claim INSERT is the row's first INSERT.** `claimCampaignDispatchQuery` is where
  `created_by` is stamped, and `upsertCampaignQuery`'s conflict arm then FINALIZES that same
  claim rather than revisiting some earlier dispatch's row. `dispatchPlatform` reaches an
  upsert only when the claim was WON: every `!claimed` branch returns first — a reusable
  campaign is reported as a reuse, a retained partial as a reconcile, a bare pending claim as
  a skip — so a later dispatch of the pair never reaches the upsert at all. A retry is not the
  exception it looks like either: it re-claims first, and since the released row is gone the
  INSERT wins and re-stamps `created_by` with the retrying actor. A
  re-dispatch after a soft delete is NOT the exception it looks like: the deleted row falls
  outside the partial unique index, so it is the CLAIM that inserts the fresh campaign and
  stamps its `created_by`, and the upsert conflicts with that. (A `pending` row cannot be
  soft-deleted in the first place — `CampaignStatusDeletable` is a whitelist of settled
  statuses.) The upsert's INSERT arm stamps `created_by` too, but only for the case where the
  claim row is gone by the time the upsert runs: an operator clearing an apparently-stuck
  claim, or a concurrent `DeleteDispatchClaim`. Both arms setting it is what keeps the column
  populated on every path that can create the row.
- **The claim stamps BOTH actor columns from one placeholder** (`$5, $5`), matching
  `createBriefQuery`. At creation the author IS the last mover, and leaving `updated_by` NULL
  would make "untouched since it was made" indistinguishable from "we never recorded who" —
  which the conflict arm cannot repair later, since it only moves `updated_by` when it has a
  non-NULL actor to move. Pinned by `TestClaimCampaignDispatchStampsBothActorColumns`.
- **`created_by` is absent from that conflict arm's SET list.** Given the reachability above,
  this is not stopping an overwrite today's orchestrator would otherwise perform — it is
  REPOSITORY SEMANTICS. `UpsertCampaign` is a general-purpose method, not a private half of
  `dispatchPlatform`, and its conflict arm has to be safe for a caller that reaches it without
  a claim in front of it. Such a caller, with `created_by` in the SET list, would rewrite the
  original author with whoever triggered the latest write; under shared system accounts the ad
  platform cannot supply that author again, so once gone it is gone. Being a property of the
  SQL rather than of any Go path, no service-layer test can reach it, and it is asserted
  against the statement text (`TestUpsertCampaignDoesNotRewriteCreatedBy`).
- **`updated_by` moves via `COALESCE(EXCLUDED.updated_by, campaigns.updated_by)`**, not a
  bare assignment. The actor threaded into a re-dispatch is whatever `attributedActor`
  produced back in `Orchestrator.Start` — nil whenever that request carried no authenticated
  principal — and letting that NULL land would turn "we know who" into "we do not".
  `replaceCampaignQuery` and `deleteCampaignQuery` do the same for the update and delete paths.
- **The soft delete stamps `updated_by`** (`TestDeleteCampaignStampsTheDeletingActor`). The
  row is kept precisely because it may still point at a campaign spending upstream, so "who
  retired this" is a question actually asked of it later; leaving the column alone would
  answer with whoever last EDITED the campaign — worse than NULL, because it reads as
  knowledge and names the wrong person.

A campaign row therefore attributes to whoever asked for the DISPATCH: the person who
authorized the spend, which is the question `created_by` exists to answer, and not the same
question as "who was authenticated when some later goroutine got around to writing the row".
A NULL is correct on an unattributed write, not a lost attribution.

`scanCampaign` is covered directly (`TestScanCampaign_MapsEachColumnToItsField`,
`TestCampaignCols_MatchesTheDeclaredOrder`) rather than only through the queries that call it.
Both actor columns are JSONB and both timestamps are `time.Time`, so a swap in its destination
list cannot fail at the type level: `created_by` and `updated_by` would simply trade places and
every other test would stay green while the audit trail named the wrong person on every row.
Distinct per-column values are what make the swap visible. A NULL actor decodes to nil
(ordinary — every row predating `000016` has both NULL); actor JSON of the wrong SHAPE fails
the scan with the column named, since a silently-nil actor is indistinguishable from "not
recorded", which is the confusion the NULL semantics exist to avoid.

`Approve` moves `updated_by` alongside `approved_by`, and the two then diverge:
`ReplaceBrief` CLEARS `approved_by` (a modified brief must not stay approved) while
`updated_by` survives — which is exactly the difference between "who signed off on this
content" and "who touched this row last".

NULL means **not recorded**, never "nobody": a request with no bearer token, or one whose
claims could not be decoded, still writes. Losing the attribution is bad; refusing the
write over it would turn a token-decoding regression into a total outage of brief
creation. Neither column is exposed on the Goa surface or in the index payload, matching
the existing `approved_by` precedent.

### Audiences

`campaign_audiences` carries the same pair and the same three properties, pinned by the
same shape of test in `audience_repo_test.go`. `createAudienceQuery`,
`createAudienceForApprovedBriefQuery` and `updateAudienceQuery` are package constants for
exactly that reason.

Two things are specific to this table:

- **Both inserts stamp an actor, including the BUILD path.** `BuildAudience` runs under a
  human's HTTP request, so the person who started a build that creates real HubSpot lists
  — and spends money — is recorded. Treating it as a system write because a *background*
  step follows would lose the only record of who started it.
- **The build's progress writes carry the actor FORWARD rather than restamping.**
  `BuildAudience` passes the row returned by the insert straight back to `UpdateAudience`,
  so `updated_by` keeps naming the initiator. Moving the build off the request goroutine
  would NOT by itself change that — campaign creation is already asynchronous and still
  attributes correctly, by capturing the decoded actor while the request context is in hand
  and threading it down (see the campaign pattern above). Detachment is not what breaks
  attribution; absence of an initiator is. The column has to go NULL only where there was no
  human request to capture from in the first place — a separately scheduled retry, a cron
  sweep, a startup reconciliation — because there the alternative is asserting a person who
  was not there.

The service handler stamps the editor onto the row it LOADED, not onto the incoming patch:
the loaded row already carries the PREVIOUS editor, so writing it back unchanged would
silently re-assert them as the author of somebody else's edit. That is the failure mode the
second edit in `TestAudienceActor_UpdateStampsTheEditorNotTheCreator` exists to catch —
a fill-only-if-empty stamp passes a single-edit test and is wrong from the second edit on.

## Migration numbering

golang-migrate records only the HIGHEST applied version and never applies a lower one, so a
branch that claims `000017` while an unmerged branch holds `000016` makes that migration
unapplicable forever if it merges second — silently, since the tool reports success. The
numbering guards live in `outbox_repo_test.go` (`TestMigrations_NoVersionGaps`,
`TestMigrations_UniqueNumbering`), with `allowedVersionGaps` as the documented escape hatch
for a transitional gap and `TestMigrations_AllowedVersionGapsAreStillOpen` to stop an entry
outliving the branch that justified it. Choosing a version means checking every OPEN branch,
not just `main`.

## Live-database tests (`dbtest/`)

Almost every test in this package asserts over SQL **source text** — `campaign_repo_test.go`
regexes the `ON CONFLICT` clauses, `campaign_repo_test.go` regexes the claim query. Those
assertions are worth having, but they can only check that a string still looks the way
someone decided it should look. They cannot check whether PostgreSQL accepts the statement,
whether an index the statement depends on still exists, or whether a fix changed anything
observable. The cost is on the record: the `UPDATE ... RETURNING` fix on the connection repo
could not be revert-checked, because reverting it produced source text no assertion here
disagreed with, and a test that cannot fail when the fix is removed is not evidence.

`dbtest` closes that gap. `dbtest.Pool(t)` returns a pool against `TEST_DATABASE_URL` with
the migrations applied; with the variable unset every helper calls `t.Skip`, so `go test
./...` still works on a laptop with no database. CI supplies a `postgres:16-alpine` service
container, and `Pool` **fails** rather than skips when the variable is empty while `CI` is
set — otherwise deleting the service block from the workflow would make the live suite skip
forever while CI kept reporting green. That decision lives in `verdict`, which takes both
values as arguments so it can be tested from a laptop: the case that matters most, "on CI
with no database", is by definition one no live test could ever run.

Two properties are worth knowing before adding a test here.

**The schema is migrated once per package run, and rows are never cleaned up.** Migrating is
the slow part. Isolation therefore comes from unique keys, not from a fresh schema — which is
also what production does, since one schema serves every project. Use `dbtest.UniqueID` for
every identifier a test writes. It appends random bytes for exactly this reason: a purely
name-derived id collides with the row the PREVIOUS run inserted against
`uq_campaign_briefs_project_event`, which breaks `go test -count=2` and, worse, turns a
failure at setup into a test that never reaches its own assertion.

**What belongs here is a claim about the SERVER, not about the code.** Two of the tests pin
migration 000013/000014: that the bare `ON CONFLICT (brief_id, platform)` raises SQLSTATE
`42P10` now that the full unique constraint is gone, and that a `'deleted'` row stops
occupying its `(brief_id, platform)` slot. Restore the dropped constraint and both fail — that
is the check the regex test cannot perform, and it is the reason to reach for this package
rather than another source-text assertion.

`adopt_binding_live_test.go` pins migration 000020 the same way, and is the sharpest case in
the package because the guard it covers has no runtime symptom. Adoption lets a caller name an
arbitrary upstream campaign, so two briefs in one project can each bind the SAME paid campaign;
from then on each brief's toggle and metrics reader act on it independently, and both rows stay
individually well-formed, so nothing in the service can detect the collision afterwards. The
only thing standing between that and production is one index definition — and a wrong one still
applies cleanly, still satisfies the fake repository, and still matches the SQL-text assertions.
Each of the four sub-tests is bound to a different way the definition can be wrong: putting
`brief_id` in the key (the shape 000013 uses, and so the mistake most likely to be copied) lets
the second brief bind; dropping `status <> 'deleted'` makes deletion permanently reserve the
upstream campaign; dropping `platform_campaign_id IS NOT NULL` makes unprovisioned dispatch
claims collide with each other; and ADDING `project_id` to the key lets a second project bind a
campaign the first already holds. That last one is the edit that reads as the careful choice — a
bare platform id is unique only within the account that minted it — and it is wrong for exactly the
provider adoption supports: Google Ads is ONE shared customer across every foundation, with a
connection row per project pointing at it, so a project-scoped key inserts both rows cleanly and
they then toggle the same live campaign against each other. 000020 keys globally on purpose. All
four were verified by making each of those edits to the migration and watching the corresponding
sub-test fail.

`ConnectionRepo.Disconnected` is here for a sharper version of the same reason. Its whole job
is to tell a deliberate disconnect apart from never having connected, and the two are
distinguished by ONE clause — `status = 'deleted'` — in one statement. Every other test of
that distinction runs against a fake reader that answers the question by construction, so all
of them stay green against a predicate that lost the clause and started reporting every
project as disconnected. `TestDisconnectedTellsADeliberateDisconnectApartFromNeverConnected`
writes the three real rows (never connected, tombstoned, live) and asserts the answer for
each, so the clause has to survive in the SQL and not merely in the fake. It also asserts an
unknown provider ERRORS rather than answering `false`: `false` here means "no deliberate
disconnect", which would hand a typo'd provider the system-account fallback.

**That probe also needed an index of its own, and the reason it did not have one is worth
recording.** Every connection table indexes `project_id` — but under
`WHERE status <> 'deleted'` (migration 000001), the exact complement of the rows this query
reads. The index was present, named for the column, and covered none of the rows in question.
Migration 000017 adds the mirror-image partial index on the six paid-ads tables, so the two
partition the table between them and neither pays for the other's rows.

Only paid-ads tables are indexed, because `credsSource` gates the probe behind
`provider.IsPaidAds()` — the system account is an ad-ACCOUNT fallback and HubSpot never
reaches it, so an index on `hubspot_connections` would be write cost for a query never issued.

`TestDisconnectedProbeIsIndexed` binds it, and it is a PLAN assertion rather than a timing
one: the query returns the same answer indexed or not, so no correctness test can see the
difference. It runs `EXPLAIN` with `enable_seqscan = off` and fails on a surviving `Seq Scan`.
Turning seqscan off does not force an index to be used — it cannot be, if none applies — it
only removes the reason a usable index would be passed over on a table this small. Dropping the
six indexes turns all six sub-tests red with the plan printed, which is the revert-check.

The build-lease tests (`audience_lease_live_test.go`) are the same kind of claim and show why
a fake cannot substitute. The arbitration IS the index: eight goroutines call
`CreateAudienceForApprovedBrief` together and exactly one may get a row, which is only true
because PostgreSQL serialises them. They also need an APPROVED parent — `insertBrief` creates
a DRAFT, which `CreateAudienceForApprovedBrief` rejects with `ErrStaleApproval` before the
lease is ever consulted, so a test built on it would pass for the wrong reason and
`insertApprovedBrief` exists for that alone.

**`reconcileAmbiguousAudienceCommit` cannot be tested from `dbtest_test`, and its own live
test cannot import `dbtest` either — both for the same import cycle.** `dbtest.go` is a
non-test file that imports `internal/infrastructure/postgres` (production) to build its
fixtures, so `package postgres`'s own test files cannot import `dbtest` back (`go vet`
reports `import cycle not allowed in test` if one tries). The function is also unexported,
which rules out testing it from the external `dbtest_test` package regardless.
`audience_reconcile_live_test.go` (`package postgres`) works around both constraints at
once: it calls `Migrate` and `NewPool` directly — the same two calls `dbtest.Pool` makes
internally — rather than going through `dbtest`, giving it its own live pool with no import
of the `dbtest` package at all. It duplicates `dbtest.UniqueID`'s shape (name prefix + a
`crypto/rand` suffix) for the same reason `dbtest` uses that shape: the schema is shared and
never dropped between runs.

## `CreateAudienceForApprovedBrief`'s ambiguous-commit path

A `tx.Commit(ctx)` failure does not prove PostgreSQL rolled back — the server can commit the
row before the client's acknowledgement of the commit itself is lost (a dropped connection,
a timeout on the ack). If that happens here, the INSERT's `building` row is real and holds
the `(brief_id, platform)` build lease (migration 000018) — but `CreateAudienceForApprovedBrief`
returns `nil` for the created row on this path, so the service layer's `releaseUnstartedClaim`
(`internal/service/audience_build.go`) has no row to call `UpdateAudience` on. Nothing would
ever release that lease.

`reconcileAmbiguousAudienceCommit` closes the gap directly in the repo, using the row the
INSERT's `RETURNING` clause already observed inside the (possibly-rolled-back) transaction:
a bounded (`ambiguousCommitReconcileTimeout = 5s`), detached (`context.WithoutCancel`)
`UPDATE ... WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND status='building'` that moves
the row to `failed`.

**The predicate is the status, NOT the version.** A version predicate does not enforce the
do-not-downgrade invariant it looks like it enforces: a concurrent `PATCH` that edits some
other field while leaving the status at `building` still bumps `version`, so the UPDATE
would match zero rows, report no error, and abandon the row holding the lease forever.
Predicating on `status='building'` closes that hole and makes the write idempotent under
retry. The *write* is one statement rather than a SELECT-then-UPDATE for the same reason — a
status read taken before the write is a TOCTOU window, and it could only add a failure mode on
a path that runs precisely when the connection is already known to be unreliable. (The
classification read described below is not that read: it runs only after the write declined to
match, it decides nothing about what to write, and every one of its failure modes falls through
to another attempt rather than to an early return.) The tenant
columns match `UpdateAudience`'s predicate; `id` alone would be correct (it is the primary
key) but every other write to this table carries the tenant scope.

**A zero-row UPDATE does not settle the question, so it retries.** Three of the four outcomes
are self-evident from the write alone: `building` ⇒ 1 row (released, the point of the
function); `built`/`failed` ⇒ 0 rows plus a visible row (a stable end state, left untouched).
The fourth — 0 rows and no visible row — has two readings, and the write cannot tell them
apart. The commit whose result was never received may still be IN PROGRESS: the reconcile runs
on a different pooled connection, so it takes its own snapshot, and under READ COMMITTED a row
inserted by a not-yet-committed transaction is *invisible*, not locked. The UPDATE does not
block on it; it matches nothing and returns. Milliseconds later the commit lands and the row
is there in `building`, holding the lease — the exact case the function exists for, missed by
the function. So that one outcome is retried (`ambiguousCommitReconcileAttempts = 7`, 25ms
doubling to a 500ms cap: 25+50+100+200+400+500 = 1275ms in total) while a row observed in a
terminal state settles immediately. N attempts sleep N-1 times, so the count is one more than
the number of delays — it was 6, which spans 775ms and never reaches the documented cap at all.
`TestAmbiguousCommitReconcileScheduleSpans` now derives the sum from the three constants so the
prose cannot drift from them again. The attempt cap rather than the 5s timeout is what normally ends the loop, and it
is there to bound the cost on the path where nothing is wrong: retrying to the timeout would
add five seconds to every genuinely-rolled-back commit, which during a database outage means
every failing request holding its goroutine five times longer.

**Each attempt runs the release TWICE before reporting the row absent.** Observing `building`
from the classifying SELECT after a zero-row UPDATE is not a contradiction — the two statements
run on separately-pooled connections with their own snapshots, so it means the row became
visible *between* them and the UPDATE merely ran too early. An immediate second pass settles it.
Without that pass the attempt returns "not seen", which is harmless on any attempt that has a
successor and is not harmless on the LAST one (or when the reconcile context is already done):
there a row this process had just CONFIRMED holding the lease got abandoned to the manual
escape hatch. `tryReleaseAudienceBuildLease` therefore returns a tri-state —
`audienceReconcileUnseen` / `Settled` / `Held` — rather than a bool, and only `Unseen` is worth
waiting on.

**An attempt's ERROR paths carry what that attempt already observed.** Both error returns handed
back `audienceReconcileUnseen` — the zero value — which reintroduced the same collapse through
the back door: once the first pass has read the row as `building`, this process KNOWS the lease
is held, and a second-pass query failing afterwards does not unlearn it. The retry loop's
`confirmedHeld` stayed false, so a confirmed stranded lease was logged at warn in the hedged
"if the commit did land" wording. The attempt now promotes its unsettled outcome to `Held` the
moment it observes `building`, and never demotes: absence is evidence only until the row is
seen. A FIRST-pass failure still reports `Unseen`, because it observed nothing.

`tryReleaseAudienceBuildLease` takes an `audienceReconcileDB` interface (`Exec`/`QueryRow`,
satisfied by `*Pool`) so both of those paths are unit-testable. They have to be: reaching them
requires the row to become visible in the microseconds *between* an attempt's two statements,
which nothing can schedule into against a live database — the live tests say as much.

Giving up after the last attempt logs at **warn** *when no attempt ever saw the row*, and at
**error** when one did. The two endings are different events and collapsing them was the defect:
"never saw a row" is the overwhelmingly likely reading — the commit really did roll back, which
is what "no row" means every other time — and an error on every rolled-back commit is an error
nobody reads. "Saw the row, still `building`" is a lease this process knows is held, that no
automatic path will release (migration 000018's escape hatch is deliberately manual, since the
row may own real HubSpot lists), so it is an error and its wording states the fact instead of
hedging with the other case's "if the commit did land". `reportUnreconciledAudience` owns that
choice and is pinned by
`TestReportUnreconciledAudience_SeparatesAConfirmedHeldLeaseFromAProbableRollback` — a plain
unit test, because reaching `Held` through the database would need the row to become visible in
the microseconds between one attempt's two statements, on the last attempt, which nothing can
schedule into. The outcomes that *are* reachable are pinned live by
`TestTryReleaseAudienceBuildLease_ClassifiesEachOutcome`.

The whole path is otherwise best-effort and log-only, matching `releaseUnstartedClaim`'s error
handling for the same reason: the caller is already returning an error from
`CreateAudienceForApprovedBrief` and has no better path to report a second failure on top of the
first.

## ReleaseAudienceBuildLease is unversioned on purpose

`AudienceRepo.ReleaseAudienceBuildLease` shares its statement with the reconcile above and
exists so the service layer's `releaseUnstartedClaim` stops routing through `UpdateAudience`.
`UpdateAudience` is version-gated, and the version is exactly the wrong thing to gate a release
on: a concurrent `PATCH` that leaves the row `building` while changing another field still bumps
it, so the release returns `ErrPreconditionFailed` and the row stays `building` **forever** —
the identical bug the reconcile's status predicate was written to avoid, reintroduced one layer
up. Gating on `status='building'` instead makes the condition that decides the outcome the same
statement that performs it, and makes the call idempotent under retry: a row already `failed`,
already `built`, or gone matches nothing and returns `nil` rather than an error.

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
effect already landed upstream. Taking the same advisory lock keeps delete out of that
window entirely: it does not wait for the toggle, it is refused with
`ErrCampaignWriteInProgress` and returns a retryable 409, so a delete never commits between
a toggle's claim and its persist. A delete issued after the toggle releases sees the bumped
version through the ordinary optimistic check.

The delete transaction begins on the connection already holding that advisory lock
(`conn.Begin`), never on the pool (`r.db.Begin`) — beginning on the pool would take a
SECOND connection while the first is held, self-deadlocking on a saturated pool
(`pool_max_conns=1` guarantees it). The unlock is deferred on a context detached from
the request, destroying the connection if the unlock fails, exactly as
`ClaimCampaignVersion` does: a session advisory lock is not released by returning the
connection to the pool, so a failed unlock strands it and blocks every future claim and
delete for that campaign. Pinned by
`TestDeleteCampaign_ParticipatesInAdvisoryLockProtocol`.

**Every release path spends a budget that is a term of the shutdown budget.** A campaign
lock holds a pooled connection, and `pgxpool.Close` blocks until every checked-out
connection is returned — so any unlock that outlives `Container.Close`'s wait pushes
shutdown past `ContainerCloseTimeout` by the difference. `releaseCampaignLock` therefore
takes its bound from the caller rather than fixing it at `lockReleaseTimeout`, and EVERY
path that can run during shutdown passes `shutdownReleaseBound()` — which returns whatever
`StopCooldownsForShutdown` published, falling back to the ordinary timeout before shutdown,
so nothing is tightened outside shutdown. Two of those paths do not look like shutdown
paths: the straggler branch of `ReleaseCampaignLockAfterCooldown` (reached when
`cooldownStopped` is already set), and its cooldown-ELAPSED branch, because when the timer
fires at the same instant `cooldownShutdown` closes both select cases are ready and Go
picks one at random. The same rule governs the connection-destroying fallback: `Close` is
given the already-bounded `releaseCtx`, not `context.WithoutCancel(ctx)` — pgx uses that
context to bound its wait, and on cooldown paths `ctx` is `context.Background()`, so
`WithoutCancel` would supply no deadline at all. Pinned by
`TestReleaseCampaignLockAfterCooldown_EveryShutdownPathUsesTheShutdownBound` and
`TestReleaseCampaignLock_OrdinaryPathKeepsTheGenerousBound`.

That `Close` rule is not confined to the release paths. `closeLockConn` applies it to every
site on the CLAIM path that destroys a possibly-lock-bearing connection — a failed
`pg_try_advisory_lock`, a failed unlock after a failed guarded read, and the delete path's two
equivalents. Each is reached precisely BECAUSE something already failed or was cancelled,
so the caller's `ctx` is routinely dead; `context.WithoutCancel` alone strips the deadline
along with the cancellation and leaves the wait unbounded exactly where it is most likely
to be taken. The pool slot is not returned until `Close` returns, so an unbounded `Close`
here is a held slot, not just a slow log line.

**The shutdown budget is an absolute deadline, not a duration.**
`StopCooldownsForShutdown` stamps `time.Now().Add(timeout)` where the wait STARTS, and
`shutdownReleaseBound` returns `time.Until` that instant. A stored duration would hand
every straggler that woke afterwards a fresh full-length allowance — N stragglers costing
N × timeout, from the one call whose purpose is to cap the wait — and only an absolute
instant composes across an unknown number of wake-ups. A non-positive remainder is returned
as is rather than floored: the resulting already-expired context destroys the connection,
and destroying the connection is what actually frees the slot, so it is the FASTER answer
on the out-of-budget path. The tests assert `got <= budget` rather than equality, which is
the property itself and not a CI tolerance — equality cannot tell a shared deadline from a
per-goroutine allowance.

**Releasing a superseded token is a no-op for the SUCCESSOR, not for the caller's own
connection.** When a delayed release finds that its `campaignID` slot now holds a different
`*campaignLock`, `CompareAndDelete` deliberately fails and the successor's lock and
connection are left alone — but the stale token still owns a checked-out connection that
nothing else references, so the unlock and `Release` must still run. An early return there
leaks that pool slot for the life of the process.

**Every `campaigns` read excludes soft-deleted rows, the claim pair included.** Soft
delete is a status value (`status = 'deleted'`), not a column, so the exclusion has to be
written into each statement: `getCampaignQuery`, `getCampaignByPlatformQuery`,
`replaceCampaignQuery`, `claimCampaignVersionQuery`, and `claimCampaignExistsQuery` all
carry `status <> 'deleted'`. The claim is the one that must not be missed — it gates the
run-status toggle, which makes a PAID platform call between claiming and replacing. A
claim that admitted a deleted row at a matching version would succeed, mutate the
campaign upstream, and only then fail in `ReplaceCampaign`, which does filter. The EXISTS
probe needs the same predicate for its own reason: disagreeing with the read turns a
correct 404 into a 412, telling the caller to retry a campaign that is gone for good. All
five are package-level constants specifically so
`TestCampaignRepo_ReadsExcludeSoftDeleted` can inspect their source; inlined SQL is
invisible to it, which is how the claim originally slipped through.

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

## `AdoptCampaign` — why it is not `UpsertCampaign`

`adoptCampaignQuery` inserts a campaigns row with `ON CONFLICT (brief_id, platform) WHERE
status <> 'deleted' DO NOTHING ... RETURNING`, and `AdoptCampaign` classifies `pgx.ErrNoRows`
as `domain.ErrConflict`.

The predicate is mandatory for the same reason it is on every other statement targeting this
pair: migration 000014 drops the full `UNIQUE (brief_id, platform)` constraint, leaving only
000013's PARTIAL unique index, so a bare conflict target infers no arbiter index and fails at
runtime. `TestCampaignRepo_OnConflictCarriesLivePredicate` covers this statement too.

`DO NOTHING` is what distinguishes adoption from an upsert. `UpsertCampaign`'s `DO UPDATE`
arm is correct where it is used, because it overwrites a row describing THE SAME campaign
this service is provisioning — that is how a retried dispatch converges. Adoption's caller
names an ARBITRARY upstream campaign, so an updating arm would repoint a live binding at a
different campaign and orphan the one it used to name; this service never deletes or pauses
upstream, so that orphan keeps spending with nothing here pointing at it. `RETURNING` is
load-bearing for the same reason the classification is: "no rows came back" IS the conflict
signal, so dropping it makes a refused adoption indistinguishable from a successful one.

The statement also names `result`, `created_by` and `updated_by` (both actor columns from one
parameter — adoption creates the row and is the last thing to have touched it). Omitting them
would leave an adopted row with no audit trail and, because `result` is where the Google Ads
customer id lives, no account provenance for the mismatch guards to check.

Two guards live in the same transaction as the insert, and neither can be enforced in Go:

- **A locked re-read of the brief.** `lockAdoptBriefQuery` takes the same `SELECT … FOR UPDATE`
  as `CreateJobForApprovedBrief` and re-checks `status`/`version` against the caller's
  `expectedVersion`, returning `domain.ErrStaleApproval` on a mismatch. The service reads
  approval BEFORE a platform lookup bounded at 20 seconds, and a `ReplaceBrief` or
  `ArchiveBrief` committing inside that window would otherwise leave paid spend bound to an
  unapproved brief — the approval gate defeated by latency alone.
- **`uq_campaigns_platform_campaign_live`** (migration 000020), keyed
  `(platform, platform_campaign_id)` over live rows **restricted to
  `platform = 'google-ads'`**. It is registered in `requiredIndexes`, so its definition —
  uniqueness, both keys and the three-conjunct predicate — is re-asserted at every boot and
  not only at migration time. That matters more here than for the other entries: 000020
  deliberately omits `IF NOT EXISTS`, which protects the FIRST build from a same-named
  leftover but says nothing about a later `DROP INDEX`. 000013's index answers only
  "does this BRIEF have a campaign here"; adoption names an arbitrary upstream campaign, so
  without this a second brief can bind the same one and the two rows toggle it against each
  other. The index is deliberately not scoped by project and equally deliberately scoped to one
  provider: Google Ads campaign IDs are globally unique within Google's shared customer account
  across every foundation, while Microsoft campaign IDs are account-scoped. This service
  supports separate per-project Microsoft connections, so a global uniqueness index would
  false-reject a perfectly legitimate dispatch from Microsoft account B because account A had
  already minted the same numeric ID. The `platform = 'google-ads'` predicate was added in
  commit `1ca63e97` to prevent the dispatch path from ever touching this index when using
  non-Google providers, keeping a normal dispatch from triggering the wrong conflict error. When
  adoption gains a second provider, that provider needs its own uniqueness handling in a
  separate constraint — do not widen this one, because whether a global key is even correct
  depends on whether that provider's IDs are account-scoped or globally unique like Google Ads.

  Note what this is NOT: an ownership check. A project connected to the shared customer
  can already read and pause anything in it through Google's API, so adoption cannot be more
  restrictive than the credential it uses; the index enforces the service's own invariant, one
  upstream campaign to one brief. The `ON CONFLICT` clause names 000013's index, so this one raises an ordinary
  unique violation — classified separately as `domain.ErrPlatformCampaignAlreadyBound`, because
  a 409 naming the wrong brief sends the operator to inspect one that has no campaign. It is
  also the ONLY index in the chain built without `IF NOT EXISTS`, which is a correctness
  choice rather than an oversight: a failed `CONCURRENTLY` build leaves an INVALID index
  holding the name, and the recovery for the resulting dirty version — force back and re-run —
  would then skip the build and record the version clean over an index that enforces nothing.
  000013 carries the clause and is safe only because 000014 follows it with an explicit
  `indisvalid` guard; nothing follows 000020, so the absence of the clause is the guard.
  `TestMigration000020_HasNoIfNotExists` keeps it absent.

The insert and its outbox index row are co-committed in one transaction, as every campaign
write is — see `enqueueCampaignIndex`.
