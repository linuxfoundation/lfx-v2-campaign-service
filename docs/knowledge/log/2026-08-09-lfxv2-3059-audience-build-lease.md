# 2026-08-09 — A build lease for audiences, because duplicate HubSpot lists are invisible

**Update** — Migration `000018` adds the partial unique index
`uq_campaign_audiences_brief_platform_building` on
`campaign_audiences (brief_id, platform) WHERE status = 'building'`. The repository maps the
resulting 23505 to a new `domain.ErrAudienceBuildInFlight`, and `mapAudienceErr` gives it a 409
with its own message. No Goa change was needed: `build-audience` already declares Conflict.

**Why this exists.** `BuildAudience` creates real, billable HubSpot lists and then records them.
Two calls for the same brief could run that whole sequence concurrently, and nothing stopped
them — `campaign_audiences` carried no uniqueness at all, only the tenant FK from `000007`. The
usual second line of defence was absent too: the builds cannot collide by list NAME, because the
plan's `BuildRef` is the audience row's own id. That was a deliberate choice, so a later build
never adopts an earlier one's lists, and it is exactly what makes the duplication silent. Two
complete sets of groups appear in the portal, indistinguishable from one another, and every
downstream reader sees a perfectly ordinary audience.

**Why the predicate is `'building'` and not the campaigns shape.** `000010` constrains campaigns
with `WHERE status <> 'deleted'`, and copying that here would have been wrong. A brief has
exactly one live campaign per platform; `000005` records that a brief may have MANY audiences
over time, and `BuildRef = created.ID` exists in `BuildAudience` precisely because a later build
for the same brief is expected. Constraining `'built'` rows would make the first successful build
permanent and turn every rebuild into a 409. The lease is about concurrency, not history —
`TestAudienceBuildLeaseFreesOnCompletion` is the test that would have caught the stricter version.

**The stuck lease is the intended behaviour, not a gap.** A build that dies mid-flight leaves its
row at `'building'`, and rebuilds stay refused. That is the correct outcome: its lists exist
upstream, and the old answer — build again — is what duplicated them. The escape hatch already
existed: `PATCH update-audience` moving the row to `failed` frees the slot, and an operator uses
it after reconciling the portal. Automatic stale-lease takeover is deliberately out of scope,
because taking over a row that may own real lists re-creates the very duplication being closed.

**A separate sentinel rather than `ErrConflict`, on the strength of what the message says.**
`ErrConflict` renders as "the resource already exists", which instructs the caller to stop asking
for something that exists — and nothing it asked for exists yet. The remedy is to wait. It also
had to stay distinguishable from the stale-approval 409 on the same call, whose remedy is the
opposite: that one says refresh and rebuild, and rebuilding is exactly what duplicates the
in-flight build's lists. `TestBuildAudience_ConcurrentBuildIsRefusedWithItsOwnMessage` asserts
against both wrong messages by text, not just the status code.

**The tests are live-Postgres, because the arbitration IS the index.** No fake has one. Eight
goroutines call `CreateAudienceForApprovedBrief` together; exactly one may get a row. Dropping
the index makes all eight succeed, which is the revert-check. They also needed a new
`insertApprovedBrief` helper: the existing `insertBrief` creates a DRAFT, and
`CreateAudienceForApprovedBrief` rejects a draft with `ErrStaleApproval` before the lease is ever
consulted — a test built on it would have passed for the wrong reason.
`TestAudienceBuildLeaseIndexIsValid` asserts `indisvalid`/`indisready` rather than existence,
because a failed `CONCURRENTLY` build leaves the NAME in place and the migration's
`IF NOT EXISTS` would then skip the rebuild while reporting success.

**Why the index and the code that maps it ship in one commit.** Adding a constraint while a
binary that does not understand it is still serving is the usual reason to split a migration
across two releases. It does not apply here: the chart pins `strategy: type: Recreate` at
`replicaCount: 1`, so the old pod is gone before the new one starts and migrates. Recorded in the
migration header as a condition, not a fact — if the chart ever moves to `RollingUpdate`, the
`isUniqueViolation` mappings have to ship a release EARLIER than a migration like this one, or the
surviving old pods answer a lost lease with a bare 500 for the length of the rollout.

**The two 409s are documented in `docs/api-catalog.md`, not just mapped.** They have opposite
remedies — stale approval says rebuild, the lease says do not — so a client keying on the status
code alone gets one of them wrong, and the wrong one duplicates portal lists. The catalog table
gives the exact message text for each, plus the operator procedure for a stuck lease.

**Merge ordering.** `allowedVersionGaps` in `outbox_repo_test.go` records `000016` (PR #95) and
`000017` (PR #93) as open. golang-migrate stores only the HIGHEST applied version, so if this
tree deploys first those two are skipped silently and permanently. **#95 and #93 must both land
before this branch.** `TestMigrations_AllowedVersionGapsAreStillOpen` fails once they do, which
is what forces the entries to be deleted rather than left to rot.

## `IF NOT EXISTS` cannot recover from its own failure — so the runner checks

Raised by Copilot on #106, and the mechanism holds. The migration's comment already said
that a failed `CREATE INDEX CONCURRENTLY` leaves the index INVALID under the intended name
and that a bare re-run would skip it while reporting success. What it then claimed as the
defence was `TestAudienceBuildLeaseIndexIsValid` — and a test cannot be the defence here,
because the state it would have to observe is production's:

1. The concurrent build fails. Index present, `indisvalid = false`; golang-migrate marks
   the version dirty.
2. An operator reconciles the duplicate data and forces the version back to 17.
3. The re-run reaches `IF NOT EXISTS`, finds the NAME, does nothing, and succeeds.
4. Version 18 is now CLEAN over an index that enforces nothing. The planner will not use
   it, and the unique constraint the lease depends on is gone — silently, since every
   lookup by name still finds it.

Migration tests run on a fresh database, where none of that debris exists. So the check
moved onto the path production takes: `Migrate` now ends with `checkNoInvalidIndexes`,
returning `ErrInvalidIndex` — permanent, via `IsPermanentMigrationErr` — while the schema
carries any invalid index.

Two deliberate choices. It is **schema-wide**, not scoped to this index's name: an invalid
index is never an intended state, and every future `CONCURRENTLY` migration inherits the
guard rather than writing its own assertion. And it runs on the **`ErrNoChange`** path too,
so a pod booting against an already-damaged schema refuses rather than accepting the
duplicate builds the index exists to prevent.

The alternative Copilot offered first — dropping `IF NOT EXISTS` — was rejected: it makes
the ordinary re-run fail on `42P07` while doing nothing about the invalid-index case, since
a re-run over invalid debris still cannot rebuild. The problem was never the `IF NOT
EXISTS`; it was that nothing production runs ever looked at `indisvalid`.

One bug was written and caught while building this: the check was first handed
`migrateURL`, which `pgxURL` has rewritten to golang-migrate's INTERNAL `pgx5://` scheme.
pgx cannot parse it, so every boot would have reported a connect failure from the check —
retryable, so not even loudly. It takes the original `dsn`.

`TestMigrateRefusesAnInvalidIndex` produces a real invalid index rather than simulating
one: a unique `CONCURRENTLY` build over duplicate rows is exactly how Postgres makes them.
It also revealed that Postgres truncates identifiers at 63 bytes — the first version named
its index after the test and asserted the error quoted that name, which failed on the
truncated tail.

## Two conflict-mapping branches had no live coverage

Also Copilot, also real. `CreateAudienceForApprovedBrief` was the only path the live tests
exercised, but the lease has three doors and each maps its own 23505:

- `CreateAudience` — the plain `POST create-audience`. It defaults to `building`, so it
  takes the same lease and can lose it.
- `UpdateAudience` — a PATCH moving a `failed`/`built` row BACK to `building`. This is the
  rarest way in and the reason it earns a branch: it is the retry an operator reaches for
  after reconciling a stuck build. The completion test cannot reach it, because it moves
  `building` → `built`, LEAVING the index predicate rather than entering it.

Both are now covered by live tests that hold the lease with a real build first, so the
path under test is genuinely second. Revert-verified: removing either mapping surfaces the
raw `duplicate key value violates unique constraint ... (SQLSTATE 23505)` — which is a 500,
reading as a broken service rather than an occupied slot.
