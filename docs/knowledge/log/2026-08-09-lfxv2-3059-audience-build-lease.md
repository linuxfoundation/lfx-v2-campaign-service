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

**Merge ordering — discharged.** This branch carried an ordering obligation on `000016`
(PR #95) and `000017` (PR #93): golang-migrate stores only the HIGHEST applied version, so a
tree deploying above an unfilled gap skips those migrations silently and permanently. Both
have merged, and `allowedVersionGaps` in `outbox_repo_test.go` now holds no live entries —
its body is the comments recording why each was removed, plus `000018`, which is this
branch's own migration and is deliberately kept rather than renumbered (pool.go's
version-forcing recovery path and its tests name the number). **There is no remaining merge
order this branch depends on.** Deleting each entry was not a thing anyone had to remember:
`TestMigrations_AllowedVersionGapsAreStillOpen` fails the moment the sibling lands, which is
why the list is trustworthy enough to read a merge decision off.

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

### Round 6b: the remedy pointed at the record least likely to exist

From the same review's suppressed block, and it is a better finding than its placement suggests.
The in-flight 409 told an operator to "reconcile the HubSpot lists recorded on its audience row"
— but the row that is genuinely stuck is precisely the one that recorded nothing. The claim
inserts with an empty `inclusion_summary`, and ids are written only after `createPlanLists`
returns, so a crash mid-build leaves real lists upstream and an empty row. An operator reads the
summary, finds it empty, concludes there is nothing to reconcile, fails the row — and the next
build duplicates lists that are sitting in the portal. The duplicate set arrived at by following
the remedy that exists to prevent it, which is the second time in this file that has happened.

The durable handle was already there and unnamed: every list a build creates carries the first 8
characters of its audience row id in parentheses, the same `Plan.BuildRef` suffix that stops a
rebuild adopting an earlier build's lists. That is true of a list whether or not the row ever
recorded its id. The 409 and the api-catalog remedy now name the prefix first and treat
`inclusion_summary` as a supplement, and the message says WHY — a prefix offered without the
reason reads as a redundant second option and gets skipped.

The rule: **a recovery instruction has to name evidence that exists in the FAILURE it is written
for, not in the success case.** Close kin to Round 3 and to #101's "a guard must run where the
evidence still exists" — same mistake, pointed at an operator instead of a decoder.

## Round N: recovery advice that is only right for some of what the scan finds

Copilot on `pool.go:247`: the invalid-index error tells the operator to
`migrate force <version-1>`, but the scan producing the names is schema-wide. An invalid
index may have been created by hand — the live test does exactly that — and two hits may
come from two different migrations, so there is no single version to force, and forcing an
unrelated one replays unrelated DDL.

Right, and the shape of the mistake is worth naming: the breadth of the scan was a
deliberate decision (an invalid index is never intended, and every future CONCURRENTLY
migration gets the check for free), while the remedy sentence was written for the ONE index
this migration adds. The two were correct separately.

`describeInvalid` now annotates per name rather than per sentence — each name matched
against `requiredIndexes` gets `(migration 000018: force 17)`, and anything else gets
"no migration creates this; drop it, leave the schema version alone". `requiredIndex` grew
a `migration` field to carry it; it is reported, never checked.

A number that exists only to be printed is the kind that rots, so it is pinned twice.
`TestRequiredIndexMigrationsExistAndCreateTheirIndex` fails if a version names no
`*.up.sql`, and — the half that survives a rename — if the file it names does not mention
the index. `TestDescribeInvalid_AnnotatesOnlyMigrationOwnedIndexes` covers both branches
without a database. The live test now also asserts that its hand-built index is NOT offered
a version, which is the case that motivated the finding. Both new unit tests
revert-verified.

**When a check is deliberately broader than the case it was written for, its ERROR MESSAGE
inherits that breadth and usually does not deserve it.** The scan was general; the remedy
was specific; nothing connected the two until an operator followed it.

## Round N+1: the registry I used for ownership was the wrong registry

Both bots flagged the same thing about last round's fix, independently, which is very strong
signal — and they were right. `describeInvalid` derived "which migration creates this index"
from `requiredIndexes`, and treated every name absent from that list as one no migration
creates: *"drop it, leave the schema version alone."*

`requiredIndexes` cannot serve as that registry, and the reason is written into its own
doc comment: membership is **deliberately narrow**, restricted to indexes whose ABSENCE is
silent. Most migration-created indexes are correctly excluded — `idx_campaigns_stuck_claims`
(000008) among them, and worse, `uq_campaigns_brief_platform_live` (000013), which after
000014 drops the old constraint is the ONLY thing enforcing `(brief_id, platform)`
uniqueness. An operator with an invalid copy of that index was being told to drop it and
leave the version alone. Follow that and the uniqueness is gone permanently, the next boot
succeeds clean, and concurrent `ClaimCampaignDispatch` calls double-create PAID campaigns.

The fix is not to add the two missing entries. Adding entries fixes today's list and leaves
the mechanism that produced the bug: the next `CREATE INDEX CONCURRENTLY` migration written
by someone who has never read this file inherits the same wrong advice. Ownership now comes
from the migrations themselves — `migrationIndexOwners` parses `CREATE … INDEX` out of every
embedded `*.up.sql`. The CREATE specifically, not the name anywhere in the file, so a
migration that DROPs an index is not offered as the version to force back to.

The general rule, and it is the one I should have applied a round earlier: **a list curated
for one predicate is not a registry for a different one.** `requiredIndexes` answers "whose
absence is silent?". `describeInvalid` asks "who creates this?". The two sets overlap, which
is exactly why the substitution looked fine and why it produced a High-severity answer for
the most important index in the schema. When a lookup needs a set, derive it from the thing
that defines it, or state in the doc why the narrower set is complete — and if you cannot,
it is not.

`TestMigrationIndexOwners_FindsEveryCreatedIndex` re-derives the index names by a different
parse and requires the map to know each one, so the parser cannot be checked against itself.
`TestDescribeInvalid_AnnotatesIndexesOutsideRequiredIndexes` pins the specific case,
asserting first that the index is still outside `requiredIndexes` so the test cannot quietly
stop testing anything. Revert-verified: restoring the `requiredIndexes`-derived map fails it
with the literal wrong advice in the diagnostic.

## Round N: the recovery advice destroyed its own precondition

Two findings on the invalid-index remedy, one stale and one real.

**Copilot's is stale.** It said `requiredIndexes` omits `uq_campaigns_brief_platform_live`, so an
invalid copy gets the "no migration creates this; drop it, leave the schema version alone"
advice. True of the commit it reviewed; `a1266307` had already moved ownership off
`requiredIndexes` and onto the migrations for exactly that reason, and the index is annotated
`(migration 000013: force 12)`. Replied with the diff rather than editing anything.

**Cursor's is real, and it is a nastier shape than it first reads.** `migrationIndexOwners`
attributed `idx_campaigns_stuck_claims` to 000009 rather than 000008, because 000009 rebuilds
it inside a `DO` block and the highest CREATE wins. On its face "force 8" looks fine — 000009
IS the recovery migration for this index. The problem is what the operator does first. The
error message says "DROP each index listed", then force. 000009's rebuild is guarded on
`NOT indisvalid`, so after the drop there is no invalid copy, the block no-ops, and the index
is gone permanently. Boot succeeds. The stuck-claim scan full-scans forever with nothing
reporting it — precisely the silent failure 000008 and 000009 were written to prevent.

The remedy destroyed its own precondition. That is the generalisable part, and it is not
specific to indexes: **an instruction with two steps where step one invalidates the guard step
two depends on is a bug in the instruction, not in either step.** Neither half is wrong on its
own, which is why it survived review.

The fix names the property the map actually needs, rather than patching the one case:
ownership means "re-running this migration against a schema missing the index rebuilds it".
A conditional create cannot promise that, so `executableSQL` strips dollar-quoted bodies before
the scan and `idx_campaigns_stuck_claims` resolves to 000008, whose plain
`CREATE … IF NOT EXISTS` does fire against a missing name.

Two things fell out of writing it:

- The scan was also reading index names out of PROSE. `createIndexRe` matched the phrase "a
  failed CREATE INDEX CONCURRENTLY" in 000009's header and recorded an index called `does`.
  Inert, because nothing is named that — but a parser whose output depends on comment wording
  is one edit away from a wrong attribution, and this one feeds an operator instruction. `--`
  comments are stripped too now.
- Pairing the dollar-quote delimiters had to happen in Go, not in the pattern. Requiring the
  closing tag to match the opening one needs a BACKREFERENCE, and RE2 has none. The
  tag-agnostic `\$\w*\$.*?\$\w*\$` that compiles is actively wrong: given two adjacent blocks
  it closes the first on the SECOND's opening delimiter and deletes the executable statements
  between them. A test case pins that.

`TestMigrationIndexOwners_IgnoresAConditionalRebuild` was revert-verified against the
unstripped scan and fails naming both the attributed version and the rendered advice.

## Round 7: the second registry entry made the recovery advice wrong

Copilot's suppressed finding on #106 was that `uq_campaigns_brief_platform_live` belongs in
`requiredIndexes`. Verified on the merits and correct: 000013 creates it, 000014 pins that exact
definition and then drops `campaigns_brief_id_platform_key`, and after that nothing re-checks
dispatch uniqueness ever again. 000014's guard is not a substitute — it runs once, at migration
time, and cannot speak for the schema a year of operations later. Two checks on one definition
is the point.

Adding it exposed a defect the finding did not mention, and the new live test
`TestMigrateRefusesADroppedDispatchIndex` caught it on its first run. The
`ErrMissingRequiredIndex` message ended in a generic ``migrate force `<version-1>` ``. That was
correct while the registry held exactly one entry and became wrong the instant it held two,
because 000013 and 000018 create them. An operator missing the campaigns index who follows the
message to force 17 replays 000018, rebuilds the AUDIENCE index, and boots against a campaigns
table that still enforces nothing — having done precisely what the error told them to do.

The fix reuses machinery that already existed for exactly this reason: `describeInvalid` had
been annotating per NAME since the invalid-index scan went in, for the same argument. Extracted
`indexRecovery(name)` from it; both required-index messages now build from it, so each name
carries the version that rebuilds THAT index.
`TestRequiredIndexes_AnnotateToDifferentMigrations` fails if two entries ever collide on one
version, and was revert-verified by making `indexRecovery` return the old generic string.

Two doc claims went stale in the same move and were corrected rather than left: both the concept
doc and `describeInvalid`'s godoc used `uq_campaigns_brief_platform_live` as the example of an
index legitimately ABSENT from `requiredIndexes`. It is in the list now.
`idx_campaigns_stuck_claims` (000008) carries the argument instead — a performance index, which
is the class that genuinely does not qualify.

The general shape, worth keeping: **advice generated from a one-element set can be indefensibly
generic and still read as correct.** The second element is what makes the message wrong, and
nothing about adding it looks like an advice change.

## Round 8: advice that names an annotation the message does not carry

Three findings this round, from Cursor Bugbot (one unresolved thread) and Copilot (three
suppressed comments). Two bots reported the first one independently, which is usually the
signal that it is real, and it was.

**The recovery clause reached one of the two paths that reference it.** Round 7 added
`indexRecovery(name)` so each reported index carries the migration to force, and wired it into
the missing-index path via `describeInvalid`. The wrong-definition path a hundred lines down
kept building its entries as `name (defects)` — while the sentence closing that same message
told the operator to "force the version annotated against it". The annotation was not there.
This is my own previous round's fix leaving its sibling behind, which is the commonest way a
partial fix ships: the path I was looking at got the improvement, the path I was not looking at
got a message that now referred to something it did not have.

Worth separating the two failure modes, because the second is the one that survives review.
Wrong advice is caught by anyone who follows it once. Advice that points at an *absent*
annotation reads as perfectly coherent in the diff — the sentence is well-formed and the
mechanism it describes is real — and is discovered only by an operator at 2am who scans the
error for a version number that is not in it. `defects = append(defects, indexRecovery(want.name))`
before the join fixes it, and
`TestMigrateRefusesARequiredIndexWithTheWrongDefinition` now asserts the literal
`migration 000018: force 17`. Revert-verified against the live cluster: removing the append
fails with the diagnostic naming the index that carries no annotation.

**A status seen after a successful claim is a retraction, not a refusal.** The post-claim
brief re-read returned `audienceValidationErr` — a 400 saying "approve the brief first" — for
any non-approved status. But reaching that line means the claim succeeded, and the claim gates
on approval; the brief WAS approved when the row was locked. The only thing a non-approved
status can mean there is that somebody withdrew the approval in the interval, which is a
mid-build change: 409, `ErrStaleApproval`. The 400 accused the caller of an input error they
had not made, and sent them to re-approve a brief while the real event went unnamed. The two
branches are complements — `refusedClaimErr` owns the genuinely-pre-claim case and keeps its
400; this one is what is left over.

**A 409 may only be made from a read that succeeded.** `refusedClaimErr` handled `ErrNotFound`
from its diagnostic re-read and let every other read failure fall through to mapping the claim
error, i.e. to `ErrStaleApproval`. So a pool exhaustion or an expired deadline between the two
calls answered "the brief changed; refresh and rebuild" — a factual assertion about what a
third party did, made on the strength of a read that failed to observe anything. The switch now
returns `mapAudienceErr(berr)` first for any `berr != nil`. The rule generalises: a status code
that describes someone else's action is evidence-bearing, and an error path that cannot produce
the evidence must not produce the code.

**Neither of the two audience fixes was bound by a test.** After making both changes
`go test ./internal/service/` stayed green, which proved the branches were uncovered rather
than that the fixes were right. Two hooks were needed to reach them: `afterClaim` on
`fakeAudienceRepo`, which fires once after a SUCCESSFUL claim (the existing `onGet` cannot model
this — it fires after the read and hands back the pre-mutation snapshot, so the service still
sees an approved brief), and `getErr` on `fakeBriefRepo`, deliberately not `ErrNotFound`.
`TestBuildAudience_RetractionAfterTheClaimIsA409` and
`TestBuildAudience_UnreadableBriefIsNotAStaleApproval` were both revert-verified.

## Round N+1: the guard covered one of ten identically-exposed indexes

Copilot, on `pool.go`: `requiredIndexes` is incomplete under its own rule. The seven partial
UNIQUE connection indexes in 000001 and 000003's `uq_campaign_briefs_project_event` are each
the sole enforcement behind their invariant, and dropping any of them leaves both scans
passing and `Migrate` returning success.

Verified rather than assumed, because the finding turns entirely on there being no other
enforcement:

- `grep -n UNIQUE 000001_create_connection_tables.up.sql` outside the `CREATE UNIQUE INDEX`
  lines returns only the file header's prose. No table-level constraint exists.
- 000003 opens with `ALTER TABLE campaign_briefs DROP CONSTRAINT IF EXISTS
  campaign_briefs_project_id_event_slug_key;` and then creates the partial index. The
  constraint is deliberately gone.

So all eight qualify under the rule the list itself states, and the list held neither. Added
them: the brief index written out, the seven connection indexes GENERATED from the table
list, since seven near-identical literals are how the eighth provider gets added to the
schema and forgotten here.

Predicates confirmed by deparsing rather than transcribing — `WHERE status <> 'deleted'`
comes back as `(status <> 'deleted'::text)`, `<> 'archived'` likewise. Comparing against the
source text would false-alarm on an equivalent spelling.

### The test that had to change, and why its old invariant was wrong

`TestRequiredIndexes_AnnotateToDifferentMigrations` asserted every entry annotated to a
DISTINCT migration, and would have failed outright on seven entries all owned by 000001.

The temptation is to read that as the test catching something. It is not. The hazard the
test was built for is an annotation that sends the operator to a version whose migration
does not rebuild the index they are missing. Shared owners are the case where that hazard
cannot arise: all seven connection indexes annotate to "force 0", and forcing 0 replays
000001 and rebuilds every one of them. Distinctness was a proxy for the real property, and
the proxy failed on the first input that separated them.

Renamed to `TestRequiredIndexes_AnnotateToTheMigrationThatRebuildsThem` and made to assert
the property directly: every entry resolves to an owning migration, and the annotation names
that migration's version minus one. That also closes a hole the generated names opened —
deriving `uq_<table>_project` by convention is exactly how an entry acquires a plausible
name no migration creates.

### Revert checks

Coverage claimed in a doc comment is the failure this round is an instance of, so both
additions were verified by removing them:

- `requiredIndexes` without the seven generated entries → all seven subtests fail with
  `got <nil>, want ErrMissingRequiredIndex`.
- `requiredIndexes` without the brief entry → that subtest fails the same way.

One probe was discarded as non-binding before it could be trusted. Renaming the brief entry
to a `_DISABLED` name looks like a revert and is not: the renamed index never exists, so
`Migrate` reports it missing on every run and the subtest passes for the wrong reason. **A
revert that makes the guard fire unconditionally proves nothing** — the entry has to be
removed from the list, not made unsatisfiable.

## Round N+2: the recovery advice was unsafe, and I had argued it was safe

Cursor Bugbot filed two findings on the previous round. Both were correct, and the first
falsifies a claim I made in that round's own test comment and in this log.

### High — `force 0` is not a safe recovery

I wrote, in `TestRequiredIndexes_AnnotateToTheMigrationThatRebuildsThem` and here, that
"forcing 0 really does replay 000001 and rebuild every one of them", using it to justify
retargeting the test away from its distinctness assertion. That is false.

`migrate force N` followed by `Up()` replays **every migration above N**, not the one that
owns the index. Verified against the tree:

```
$ grep -ln "ADD CONSTRAINT" internal/infrastructure/postgres/migrations/*.up.sql
000006_campaign_audiences_built_check.up.sql
000007_campaign_audiences_tenant_fk.up.sql
```

Both use the bare form. PostgreSQL has no `ADD CONSTRAINT IF NOT EXISTS`, so replaying
either against a schema that already carries the constraint fails with SQLSTATE 42710 and
leaves the version DIRTY. An operator recovering ONE missing connection index by following
"force 0" takes the whole schema down.

The advice was not newly broken by the previous round — it was newly REACHABLE. While every
annotated index came from 000013 or later, the replayed range was all `IF NOT EXISTS` DDL
and the force worked. Adding the 000001 and 000003 entries moved the annotation to "force 0"
and "force 2", and the range those imply is the whole chain.

**The hazard is a property of the RANGE replayed, not of the index.** That is why annotating
more carefully — which is exactly what the previous round did, and congratulated itself for —
could not have found it. The fix is to stop naming a version: `requiredIndex` already carries
uniqueness, table, key order and deparsed predicate, so `createSQL()` emits the index's own
DDL. `indexRecovery` prefers it for any registered name; both error messages print it.

Two things follow that are worth more than the fix:

- **A version number is not executable, so no test could ever confirm it recovered
  anything.** The advice survived two rounds of review on plausibility alone. The DDL is
  checkable, so it is checked: `TestRequiredIndexCreateSQL_RebuildsAnIndexTheCheckAccepts`
  drops each required index on a live database, runs the exact statement the error prints,
  and requires the next `Migrate` to succeed.
- **The justification I gave was the failure, not the code.** I replaced a proxy invariant
  (distinctness) with a real one and defended the swap with a claim about `Up()` semantics I
  had not checked. The replacement assertion was fine; the reason was wrong, and the wrong
  reason is what would have kept the force advice alive.

Revert check: setting any entry's predicate to a logically-equivalent-but-not-deparsed
spelling (`status != 'deleted'`) makes the new live test fail on the rebuilt index, while
every other test in the package still passes. Confirmed.

### Medium — the live test's doc comment claimed coverage it did not have

`TestMigrateRefusesEachDroppedSingletonIndex` said "driving it off requiredIndexes rather
than a hand-written list is deliberate: a ninth provider … gets this coverage without anyone
remembering to extend a test". The loop iterated a hardcoded list of eight names.

This is the same failure class the whole change exists to close — a claim of coverage that
reads, from the file, exactly like the coverage itself — reintroduced in the test written to
close it. `postgres.RequiredIndexNames()` and `RequiredIndexRebuildSQL()` are exported for
the `dbtest` package (a different package, so it cannot reach the unexported list), and both
live tests now iterate the real registry with a length floor so a shrunken list fails loudly
instead of passing vacuously.

### Also in this round

- `allowedVersionGaps[16]` deleted: PR #95 merged, discharging the ordering obligation.
  `TestMigrations_AllowedVersionGapsAreStillOpen` is what forced the deletion on the merge
  that brought 000016 in — the entry did not have to be remembered.
- Merged `origin/main` (#95, #101, #105, #107). One conflict, in
  `docs/knowledge/code/internal-infrastructure-postgres.md`: main added the `000016` bullet
  where this branch had a sentence promising it. Kept main's bullet, kept this branch's
  `000018` section.

## Round N+3: the three findings that were never a thread

Copilot filed these inside a `<details><summary>Suppressed comments</summary>` block in the
review BODY. `unresolved=0` on the PR was true and meaningless — there were no threads to
resolve because suppressed findings do not create any. All three were real.

### The oracle cannot report that it has shrunk

Every index check in `invalid_index_live_test.go` iterates `requiredIndexes` and asks whether
the schema honours it. That direction catches an index dropped from the DATABASE and is
structurally incapable of catching one dropped from the REGISTRY: delete an entry and each
test loops one fewer name, passes, and the index it used to cover is now unguarded. The
`len(names) < 8` floor was doing the whole job of noticing, and only until the eighth
deletion. Two rounds ago this same file was corrected for a loop over a hardcoded list that
*claimed* to be driven off the registry; driving it off the registry fixed the claim and left
the registry itself unaudited.

`TestEveryUniquePartialIndexIsRequired` runs the other direction: enumerate the migrated
schema and require every unique PARTIAL index to be registered. The predicate is the argument,
not a filter of convenience — a unique constraint declared with `ADD CONSTRAINT` has a
`pg_constraint` row and Postgres refuses to drop its index, so partial unique indexes are
exactly the class whose invariant lives only in an object any `DROP INDEX` removes without
complaint. That is the class `requiredIndexes` exists for, which makes membership checkable
rather than a matter of taste. The live schema has ten and all ten were already registered, so
this passes today and buys nothing except every future one.

### Three 409s separated only by English

`mapAudienceErr` returned `Code: "409"` three times with three different messages carrying
three different instructions — refresh and retry, wait and do NOT retry, stop. A client can
only act on that by matching message text, and this repo rewrites those messages freely: the
in-flight one has been reworded twice in this branch alone, for operator clarity, with no
version bump and nothing that would fail. `ConflictError` now carries an optional `reason`
slug (`stale_approval`, `audience_build_in_flight`, `already_exists`), enum-constrained in the
design so a typo fails response validation. Optional is what made it a two-line change: the
other eighteen construction sites of the shared type compile and behave unchanged, and the
connection-usability 409s deliberately stay unslugged because their remedy is identical in
every case. The test asserts the slugs are pairwise distinct as well as correct — one
copy-pasted slug would re-merge the cases the field exists to separate while every individual
assertion still passed.

### A merge-order instruction outliving its merge

The head summary told a reader that #95 and #93 must land before this branch. Both had, in
this branch's own history, and the `allowedVersionGaps` entries recording the obligation were
deleted by the test that watches them. The paragraph was left behind.

The class: **operational instructions in a document whose other sections are append-only.**
Round sections are history and correcting them would be a lie about what was known when. The
summary is not history — it is what someone reads to decide whether to merge — and the
machinery that discharged the obligation (`TestMigrations_AllowedVersionGapsAreStillOpen`
fails the moment a sibling lands) has no reach into prose. Anything stated as a current
constraint has to be re-derived from the thing that enforces it, and a document mixing the two
kinds of section makes it easy to inherit the append-only habit for both.

## The lease-release assertions were tautologies

`fakeAudienceRepo.CreateAudience` stored the caller's pointer and returned that same pointer, so
the service and the store shared one struct. `releaseUnstartedClaim` sets `row.Status = FAILED`
BEFORE it calls `UpdateAudience` — with a shared pointer that assignment alone made
`rows()[0].Status == AudienceFailed` true, and every "the lease must be released" assertion in
`audience_build_test.go` passed whether the persist happened or not. Four sub-tests plus three
top-level tests were pinning nothing.

`GetAudience` already returned a copy, with a comment explaining exactly this hazard for the
load-then-merge path. The same discipline now applies to `CreateAudience`, `UpdateAudience`,
`ListAudiences` and the `rows()` helper: store a copy, return a distinct copy, which is what
PostgreSQL does and the only shape a fake can honestly claim to model. Verified by stubbing out
the `UpdateAudience` call inside `releaseUnstartedClaim` — the assertions now fail with
`expected "failed", actual "building"`, and before the fix they stayed green.

The class: **a fake that aliases the row under test cannot observe whether a write happened.**
It is not enough for one accessor to copy; any accessor that hands back a stored pointer
reintroduces the alias for every test downstream of it.

## The shared 409's example was one endpoint's vocabulary on 29 responses

`ConflictError` is deliberately shared by every 409 in the API, and its `reason` attribute
carried `Example("audience_build_in_flight")`. Goa copies an attribute-level example into the
schema of every response that uses the type, so an audience-build value appeared on the 409 for
"a connection already exists for this provider" and 12 others it has nothing to do with — which
reads as a contract rather than an illustration. The `Enum` already publishes the whole
vocabulary, which is the part clients may rely on; the example is dropped. A per-endpoint
example would need a per-endpoint type, and the type is shared on purpose.

Removing an explicit example makes Goa draw one more value from its example RNG, which shifts
every generated placeholder after it — hence the wide `gen/` diff. It is deterministic
(`make apigen` twice is byte-identical) and confined to generated files.
