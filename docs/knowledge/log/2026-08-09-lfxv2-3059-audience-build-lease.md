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

## The remedy in the error message was itself a way to lose the index

Both bots flagged the same line independently, which is the strongest signal available, and
they were right about a defect in the fix rather than in the original code.
`checkNoInvalidIndexes` told the operator to drop the invalid index and re-run the
migration. Do exactly that and the service boots green with no uniqueness at all: the drop
clears the scan, but migration 18 is still recorded CLEAN, so `Up()` returns `ErrNoChange`,
nothing rebuilds anything, and the audience-build race is wide open with nothing to report
it. The guard's own advice reproduces the failure the guard was written to catch.

Reverting the new check confirms it exactly — `Migrate` returns `nil` after the index is
dropped.

Worth naming as a class: **a detection whose remedy is a state the detection cannot see.**
The scan asked "is anything invalid", and the operator's action moves the schema from
"invalid" to "absent", which is a different bad state on the far side of the question. The
fix is not better wording — wording is not a control. Detection had to change shape, from
"nothing is invalid" to "the index that enforces the invariant is present and valid", which
is the property the code actually depends on. The message now also says to force the
version back, but the guard no longer relies on anyone reading it.

`requiredIndexes` is a hand-maintained list, and that cost is accepted narrowly: an entry
belongs only where absence is SILENT — a unique index standing in for a constraint, where
every write it serialized still succeeds without it. A performance index going missing is
slow, not wrong. `TestMigrateRefusesADroppedRequiredIndex` drops each name and requires
`Migrate` to notice, so an entry naming an index no migration creates fails the suite
rather than sitting in the list looking like protection.

## Round 2: the required-index check had the same hole it was added to close

Two bots, one class, and it is the class this whole file keeps circling.

**Copilot:** the new required-index check accepts any valid index with the expected NAME —
non-unique, wrong keys, wrong predicate, wrong table. Since migration 000018 uses `IF NOT
EXISTS`, such an index makes the migration skip building the real lease and then satisfies the
check, so startup succeeds while concurrent builds stay unconstrained. It pointed at
`campaign_repo_test.go:157-195`, where the repo already guards this exact failure mode for
migration 000014.

Right, and the precedent is the part that stings: 000014's drop-guard pins uniqueness, key
count, key names, relation and predicate precisely because a PostgreSQL 16.10 run showed a
same-named non-unique index passing the name-only form. I wrote a name-only check one file away
from the test that documents why name-only does not work. Each `requiredIndex` entry now carries
the full definition, compared against Postgres's DEPARSED predicate so an equivalent spelling
does not raise a false alarm, with `indisready` alongside `indisvalid` since a `CONCURRENTLY`
build that dies between phases can leave an index valid but not ready.

Absent and wrong-definition became separate sentinels. Not for taxonomy — the recovery differs
and an operator cannot guess the difference. Absent: force the version so the `CREATE` runs.
Impostor: DROP it first, because forcing alone re-runs a `CREATE` the impostor no-ops, which
lands the operator back where they began having done work. Reverting to the name-only query
makes the new test report `got <nil>` against a non-unique index of the right name.

**Cursor:** the cleanup in the dropped-index test treats a nil `Migrate` result as "already
restored", but a nil return is exactly the open-guard regression under test — so a weakened
guard leaves the index absent for every later lease test in the shared database.

Also right, and it had ALREADY HAPPENED: the live run for this round failed on ten tests because
`audlease106` was sitting there with no lease index, left by an earlier run of the very cleanup
that was supposed to restore it. `restoreLeaseIndex` now drops, re-creates and verifies against
`pg_index`, and asks `Migrate` nothing. It drops rather than using `IF NOT EXISTS` for the same
reason the guard exists — an impostor carries the right name.

The class, stated once more because three rounds have now produced three instances of it: **a
check whose passing condition is also producible by the failure it is checking for.** An invalid
index passes a name lookup. A dropped index passes an invalid-index scan. An impostor passes a
name-and-validity check. A cleanup that asks the code under test whether it worked passes when
the code under test is broken. Each time the fix is the same shape — assert the property you
actually need, against a source that does not depend on the thing you are testing.

## Round 4: the claim was taken after the slowest thing in the function

Copilot, in a suppressed comment: the index is acquired too late to cover all concurrent
`BuildAudience` requests, because `ResolvePastEditions` runs before the `building` insert.

Right, and the reasoning generalises past this function: **a uniqueness constraint serializes
only the interval in which the rows overlap.** Every step ahead of the insert is outside the
lease. Here that step was a warehouse round-trip, so the uncovered interval was the longest one
in the request — long enough for a double-click's second request to be admitted after the first
had finished and to build a second complete set of billable HubSpot lists. The row-level test
that shipped in round 1 could never see it: in the broken ordering the two requests do not reach
the repository at the same time, which is the whole defect.

The reorder was cheaper than it looked because the plan validation that had to stay in front of
the claim does not need the editions. `BuildPlan`'s error set is the country, the event name and
the country-only group-4 filter; the editions-dependent filter's two error branches (no
editions, a blank name) are unreachable from the only point that calls it, since `nonBlank` has
already dropped blanks and the branch is guarded on a non-empty result. So validating with an
empty `PlanInput.PastEditions` is exactly as strong as validating with the real ones. The
post-claim `BuildPlan` keeps an error path anyway — `releaseUnstartedClaim` marks the row failed
— because "unreachable given the check above" is a property of today's code and a held lease is
a stuck brief.

The fake repository now models the partial index itself (an existing `building` row for the same
brief and platform rejects the insert) rather than only honouring a `leaseHeld` flag. The flag
cannot express WHEN the lease is taken, so a fake that only had it would pass under either
ordering. Reverting the reorder makes the new test report `got <nil>` where it required a 409.

Second suppressed comment, same review: the 409's message told an operator to fail a stuck row
without naming the prerequisite documented in `internal-audience.md` — reconcile its HubSpot
lists first. Also right, and worse than a wording slip: failing the row frees the lease at once,
so following the message literally admits the next build while the dead build's lists are still
in the portal. The message, the concept doc and `docs/api-catalog.md`'s remedy column now all
state the reconciliation first.

A third item — that `internal-infrastructure-postgres.md` calls a migration error "permanent"
while the container keeps retrying — was already fixed in `7010eea5`, which added exactly that
distinction ("permanent here means the container stops trying, not that it keeps trying"). No
change.

## Round 5: the reorder was the right move made one step short

Two findings on the same code from two different reviewers, and both are about Round 4's fix
rather than about the original bug.

Copilot: the lease is acquired after `briefs.GetBrief`, so a request delayed inside that read
can still claim cleanly once the first build has finished. Round 4 conceded exactly this shape
for the warehouse call and then stopped one call short of the principle. The tempting reply is
that a database round-trip is a much smaller window than a Snowflake one, and it is — but that
argument was already made once and lost, and it is a claim about latency that no test pins. So
the claim now runs FIRST, immediately after `buildDeps()`: the brief read, plan validation and
the warehouse call all happen under the lease.

Cursor (High): the stale-approval window is reopened. This is the corollary nobody flagged in
Round 4 and it is the more interesting of the two. Moving the claim ahead of the warehouse read
also moves its APPROVAL GATE ahead of the warehouse read — and the gate exists to say something
about the brief at the moment real HubSpot lists are created. Sitting before the slowest call in
the build, it no longer does. A `ReplaceBrief` landing during those seconds builds lists from an
approval the operator has withdrawn.

The two are one fix. `CreateAudienceForApprovedBrief` drops its `expectedVersion` parameter —
running first is precisely what means there is no earlier read to have pinned — and instead
REPORTS the version it observed under its own row lock. `confirmStillApproved` then re-reads the
brief as the last thing before the first upstream call. The two guards are not redundant and it
is worth saying why plainly: the claim's gate serializes builds, and the re-check dates the
approval. Removing either leaves a real hole.

Three consequences, all accepted rather than engineered around:

An unplannable brief now leaves a released `failed` row where it previously left nothing. The
alternative was a delete path on the repository port, which is a larger surface for a row that
is already correct as a record of a build that was attempted and did not start.
`TestBuildAudience_RequiresEventDetails` asserts the row and its status rather than an empty
table.

Every early return between the claim and the first upstream call must release. Nothing exists
upstream on any of those paths, so there is nothing to reconcile first — and a `building` row
left by a request that gave up blocks every later build of that brief behind a 409 until an
operator intervenes.

A brief that was never approved would have regressed from 400 to 409, because the claim now
answers `ErrStaleApproval` for "never approved" and "moved mid-build" alike. `refusedClaimErr`
re-reads the brief on the failure path only and restores the 400. A brief that cannot be re-read
falls through to the generic mapping: guessing 400 would blame the caller for the service's own
inability to look.

One test-harness note worth recording, because the first version of the new concurrency test
failed for a reason that looked like a code bug. The audience fake's claim called
`brepo.GetBrief`, which consumed the one-shot `onGet` hook, so request A blocked inside its own
claim and never inserted. The fix is `fakeBriefRepo.snapshot()`, an unhooked read, and it is not
a convenience: the real claim reads the brief inside its own transaction under `FOR UPDATE`, so
that read is NOT a window a second request can be delayed in. A hook that pretended otherwise
would model a race the database does not have.

Third finding, `deployment.md`: the database-first rollback rule was written for `000018` and
stated for the repo. Confirmed on the merits —
`000005_create_campaign_audiences_table.down.sql` drops the audiences table and
`000015_brief_actor_columns.down.sql` drops `created_by`/`updated_by`, both read and written by
the current binary, and database-first is the ordering that MAXIMISES the window in which that
binary is still serving. The rule is now stated as a property of the individual migration:
safe exactly when the down is benign to the binary still serving, decided per migration, and
for a multi-version `goto` per migration crossed.

## Round 6: the reorder's two remaining debts

Both findings are consequences of moving the claim first, and neither was visible until the
move was made. Worth recording together, because they are the same debt paid in two places:
the claim's gate now answers questions it was never the right instrument for.

**A missing brief came back as a stale-approval 409.** The gate refuses anything not approved,
and a brief that is not there is not approved, so it refuses with `ErrStaleApproval` exactly as
a moved brief does. Before the reorder a brief read ran first and a missing brief was a plain
404; afterwards the caller was told to "refresh and rebuild" a brief that does not exist.
`refusedClaimErr` already re-read the brief to separate the never-approved case (a 400 naming
the status) from the raced case; it now also treats `ErrNotFound` as its own answer. The
distinction that matters is between `ErrNotFound` and a read that FAILED: the first is a
definite answer about the brief, the second is a statement about the service, and only the
first may be reported as a 404.

**The pre-upstream re-check could not see an in-flight withdrawal.** This is the more serious
one. Round 4 and Round 5 justified the re-check by saying the claim's gate no longer dates the
approval; what neither round noticed is that the re-check was built on `GetBrief`, a plain
`SELECT`. The claim's transaction — the one that held `FOR UPDATE` — has long since committed,
so under READ COMMITTED an uncommitted `ReplaceBrief` is simply absent from the re-check's
snapshot. Withdraw approval, and while your transaction is open the build reads "still
approved" and starts creating lists.

So the guard was in the right place and made of the wrong material. It is now
`BriefReader.ConfirmBriefApproved`, a repository operation doing `SELECT ... FOR UPDATE` in
its own transaction: the confirmation queues behind the writer and reads the writer's row.
That is not a closed window — the lock ends with the transaction, and a transaction cannot be
held open across the HubSpot calls — but it removes the already-decided case, which is the
case an operator can actually cause on purpose.

The general rule this adds to the file: **serialization is a property of the READ, not of the
comparison.** Rounds 4 and 5 both moved a check to a better POSITION. This one leaves the
position alone and changes what the check is made of, and no amount of reordering would have
substituted for it.

Verified against live PostgreSQL rather than a fake, because a fake has no locks and would
have passed either implementation. `TestConfirmBriefApprovedWaitsForAnInFlightWithdrawal`
opens a withdrawal, leaves it uncommitted, and requires the confirmation to still be BLOCKED;
dropping `FOR UPDATE` gives `ConfirmBriefApproved returned <nil> while a withdrawal was in
flight`. The fake's `ConfirmBriefApproved` deliberately does NOT fire `onGet`, for the same
reason its claim uses `snapshot()`: a hook there would model a window the lock removes.
