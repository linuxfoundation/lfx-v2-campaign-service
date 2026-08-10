# 2026-08-09 — An audience edit records who made it (LFXV2-3055)

**Update** — `campaign_audiences` gained `updated_by` (migration `000019`; it was drafted as
`000017`, then `000018`, as siblings claimed those numbers — see the renumbering below), both inserts
now stamp it alongside `created_by`, and `UpdateAudience` stamps the editor.

**The gap.** `campaign_audiences` has carried `created_by` since `000005` and nothing else.
`000015` recorded why that was a gap rather than a design choice: `update-audience` is a
published PATCH backed by an in-place `UPDATE`, so an audience edit recorded no actor at
all — the row kept naming whoever created it however many times somebody else rewrote its
suppression lists or flipped its status. Audiences are built through **system accounts**;
HubSpot reports one shared LF-owned identity for everyone, so if this service does not
record who narrowed a list, the information exists nowhere.

**Where the stamp goes.** On the row the handler **loaded**, not on the incoming patch.
`UpdateAudience` is read-modify-write: the loaded row already carries the *previous*
editor, so writing it back unchanged would silently re-assert them as the author of
somebody else's edit. An audit trail that names the wrong person is worse than one that
names nobody, because it reads as evidence. A single-edit test cannot see this — a
fill-only-if-empty stamp passes it and is wrong from the second edit onwards, which is why
`TestAudienceActor_UpdateStampsTheEditorNotTheCreator` runs three actors through the row.

**Both inserts stamp, including the build path.** This follows the brief precedent
(`000015`): leaving `updated_by` NULL until the first edit makes "who touched this last"
unanswerable without also reading `created_by`. `CreateAudienceForApprovedBrief` is the
`BuildAudience` insert, and it is stamped too — that request creates real HubSpot lists and
spends money, and it runs under a human's HTTP request, so the initiator is a fact the
statement has. The build's later progress writes carry that actor forward rather than
restamping; if the build ever moves off the request goroutine, they must go NULL instead,
because a scheduled retry has no principal.

**What is deliberately NOT done.** Existing rows are not backfilled from `created_by`. The
migration cannot know whether anyone edited them, so a backfill would manufacture
attribution for edits it has no evidence of. NULL means *not recorded*, never *nobody*.

**Migration numbering is a merge-order constraint, not a formality.** This is `000017`
while `000016` is claimed by the campaigns actor columns on an unmerged branch.
golang-migrate records only the HIGHEST applied version and never applies a lower one, so
if this merges first, `000016` becomes unapplicable **forever** — and silently, since the
tool reports success. `TestMigrations_NoVersionGaps` caught the gap; `allowedVersionGaps`
records it as transitional, and `TestMigrations_AllowedVersionGapsAreStillOpen` stops that
entry outliving the branch that justified it.

**Three query constants.** `createAudienceQuery`, `createAudienceForApprovedBriefQuery` and
`updateAudienceQuery` moved to package level for the same reason the brief statements did:
the invariant is that the stamp happens in the *same statement* as the write, and that can
only be asserted without a live database if the SQL is reachable from a test. A follow-up
`UPDATE` compiles and passes everything else while leaving a committed window in which the
row had changed and the attribution had not.

## Round N+1: the merge-order obligation came due, and the number had to move

`#93` merged first, carrying `000017_index_disconnected_probe`. This branch's
`000017_audience_actor_columns` then became a duplicate version the moment `origin/main`
was merged in, and `TestMigrations_UniqueNumbering` caught it — which is the whole reason
that test exists. Renumbered to `000018`.

Worth stating plainly, because the earlier round in this file predicted the wrong failure:
the obligation recorded here was "do not merge before the PR that fills the gap." That is
the obligation for a GAP. A DUPLICATE is a different failure with a different remedy, and
which of the two you get is decided by the merge order you do not control. Both branches
picked the next free number against the `main` they could see; whichever merged second was
always going to collide. Choosing a migration version by reading `main` is choosing against
a snapshot that a sibling PR can invalidate between the choice and the merge — the number is
only settled at merge time, so the test, not the reasoning, is what holds.

The gap at `000016` (PR #95's campaigns actor columns) is now a live risk rather than a
scheduled one: `main` already carries `000017`, so the first environment to apply it records
`000017` as the highest applied version and `000016` is thereafter skipped silently and
forever. `#95` must merge before the next deploy, or be renumbered above `000017`. That is
`#95`'s obligation; nothing this branch does can discharge it, and the
`allowedVersionGaps[16]` entry now says so in those terms.

## Round N+2: 000018 was taken too — and the number moved again, on purpose

The renumber above landed on `000018`, which PR #106 (LFXV2-3059, the audience build lease)
already holds. Moved again, to `000019`.

The choice of WHICH branch moves is not arbitrary and is worth stating, because "renumber the
one you happen to be looking at" is the wrong rule. #106's number is named in `pool.go`'s
version-forcing recovery path and in three of its tests (`migration 000018: force 17`), so
moving it means rewriting recovery logic and its assertions. This branch names its number in
one test and two documentation lines. **Renumber the branch with the fewest references to the
number, not the branch you are in** — a migration version is a value that leaks into prose and
recovery code, and the leak, not the file name, is the cost.

Three numbers in one branch also says something about the numbering scheme itself: picking
"next free against `main`" is picking against a snapshot every sibling PR can invalidate, and
the number is only settled at merge. The `allowedVersionGaps` map plus
`TestMigrations_UniqueNumbering` is what makes each invalidation loud instead of silent, which
is the only property that actually matters here.

## Round N+3: the right fix, arrived at through the wrong mechanism

The review finding was that `CreateAudienceForApprovedBrief` binds `SuppressionListIDs` and
`CreatedBy` raw where `CreateAudience` wraps both in `nullJSON`, and that the consequence is a
JSONB literal `null` where SQL NULL belongs — the exact distinction this branch treats as
load-bearing. The asymmetry is real and the wrapper belongs there. **The mechanism is not.**

`pgx` v5 tests nil-ness before its JSON codec runs, so a nil `json.RawMessage` binds as SQL
NULL with or without the wrapper. Probe, run against a live database rather than reasoned
about:

```
untyped-nil  -> sql-null
typed-nil    -> sql-null
typed-empty  -> ERROR: invalid input syntax for type json (SQLSTATE 22P02)
```

So the nil case — `marshalActor(actorFromCtx(ctx))` on an unauthenticated build, the one the
finding traced — was never at risk. What `nullJSON` actually catches is the third row: an
EMPTY but non-nil `json.RawMessage` is sent as zero bytes and PostgreSQL rejects the statement.
The failure mode is a FAILED INSERT, not a wrong row. No caller produces one today, so this is
a guard rather than a live defect.

The first test written for this passed with the fix REVERTED, which is what exposed the wrong
mechanism. That is the whole value of the revert check and the reason it is not optional: a
test written from a finding's stated cause inherits that cause's errors, and a green revert is
the only signal that distinguishes "the fix works" from "the test cannot see the fix." Had it
been committed as-is, the branch would carry a fix, a test, and a comment all asserting a
mechanism that does not exist — three mutually corroborating wrong statements, each looking
like evidence for the others.

Generalised: **a fix and its justification are separately checkable, and reviewers supply both
while only the first gets verified.** Accepting a correct patch does not accept its reasoning.
Here the reasoning would have been the durable artefact — it was headed for a code comment,
where the next reader takes it as established.

Fix kept, comment and test retargeted at the empty-value case. Revert check now fails on the
`empty` subtest with the 22P02 error quoted; the `nil` subtest passes either way on purpose, to
record that the nil case is safe on its own so nobody re-derives the wrong reason.

## Round N+1: the merge, and a test that claimed a path it never reached

Merged `origin/main` (which brought #95's `000016` and #93's `000017`). One conflict, in
`docs/channel-connections-schema.md`: main added `campaigns` to the attribution bullet and a
paragraph on why its write path needed its own migration, while this branch rewrote the same
bullet to say `campaign_audiences` now carries `updated_by`. Both are true; resolved as the
union. `allowedVersionGaps[16]` was deleted — #95 merged, so
`TestMigrations_AllowedVersionGapsAreStillOpen` fails while the excuse survives. The `18`
entry stays: #106 is still open.

Copilot's suppressed findings, all three confirmed on the merits:

**`TestAudienceActor_SystemUpdateRecordsNoActor` did not cover what it said it did.** Its
comment claimed the BUILD path. It called the public PATCH handler with a bare context.
`BuildAudience` never reaches that handler — it writes progress with
`repo.UpdateAudience(ctx, created, ...)`, passing the model its own attributed insert
returned, so the initiator stays in `updated_by`. The two paths therefore give OPPOSITE
answers, and the test asserted only one of them while its name promised the other. Renamed
to `..._UnattributedUpdateRecordsNoActor` and added
`TestAudienceActor_BuildCarriesTheInitiatorForward` for the path that was actually
uncovered; a rename alone would have left the gap. Verified binding by nil-ing
`created.UpdatedBy` before the build's persist — it fails naming ada.

**Two numbering statements had gone stale as the migration moved 000017 → 000018 → 000019.**
The log's opening summary still said `000017`, and the concept file listed `000016` and
`000017` as skipped in a sentence whose next clause said `000017` had merged. Both corrected;
after this merge the only genuine gap is `000018`.

**The generalisation: a test's name and comment are the only part of it a reviewer reads
first, and they are not checked by anything.** Both this round's real findings were
statements about coverage that no build could contradict — one in a test comment, two in
docs. The mechanical fixes were minutes; noticing that the claim and the code had drifted is
the whole cost.

## Round N+2: a compile error neither branch can see

Copilot noticed that PR #106 declares a package-level `insertApprovedBrief` in
`internal/infrastructure/postgres/dbtest/audience_lease_live_test.go`, and this branch declares
one of the same name — different signature — in `audience_actor_live_test.go`. Same
`dbtest_test` package. This branch merges after #106, so the second one in is a redeclaration
and the package stops compiling.

Confirmed by grepping both worktrees; the two differ only in whether they return the brief's
version. Renamed this branch's to `insertApprovedBriefVersioned`.

**What makes this worth recording is that no test could have caught it.** Every gate on both
branches is green, because neither tree contains both files — the defect exists only in a
merged state that does not exist anywhere yet. It is the same class as the migration-number
collisions this branch already hit three times, and the same lesson in a second form: a
package-level name chosen against the `main` you can see is chosen against a snapshot a
sibling PR can invalidate. Migration numbers at least have `TestMigrations_UniqueNumbering`
watching them. Test-helper names have nothing, so the only defence is reading the sibling
branch before adding one — which is what a bot did here and I did not.

Folding the two into one belongs on whichever branch merges second; the rename is deliberately
the smaller move, so the merge stays mechanical.

## Two documents still described the state this branch changed

`docs/architecture.md`'s D5 row listed `campaign_audiences` as carrying `created_by` only and
called it "a GAP rather than a design choice" — the exact gap `000019` closes here. The sibling
statement in `docs/channel-connections-schema.md` had already been updated, so the two disagreed
about the same table. D5 now matches, including the no-backfill decision and what a NULL means.

`docs/knowledge/code/internal-infrastructure-postgres.md` explained the second renumber by saying
#106's number "is named only in its own migration". That is the opposite of the reason recorded
above: #106's version appears in `pool.go`'s version-forcing recovery path and in three of its
tests, which is precisely why THIS branch moved instead. Left standing, the note taught the wrong
rule for the next collision — renumber whichever branch you are in — rather than the one that was
actually applied: renumber the branch with the fewest references to the number.
