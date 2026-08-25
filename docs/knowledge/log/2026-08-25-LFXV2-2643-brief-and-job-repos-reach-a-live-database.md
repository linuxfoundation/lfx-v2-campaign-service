# 2026-08-25 — the brief and job repositories reach a live database

**Update** — LFXV2-2643's remaining scope bullet was the brief/campaign/job repositories.
`CreateBrief`, `ReplaceBrief`, `GetBrief`, `Approve`, `ArchiveBrief`, `CreateJob`, `GetJob`,
`UpdateJobStatus` and `FailStuckJobs` had never been executed by PostgreSQL under test — their
only coverage was `brief_repo_test.go` regexing the four statement constants. Campaign
upsert-on-(brief,platform) was already live via `TestLiveClaimThenUpsertPersistsProvenance`, so
it was not redone. 14 tests added; `dbtest/` goes 70 → 84 test functions with none modified.

The ticket had been graded DONE once on a keyword match against filenames. The tests here are
named by behaviour, not by scope bullet, so the check that actually settles it is enumerating
`grep "^func Test"` and mapping each to a bullet — and, for this bullet, `grep -c "\.Method("`
over `dbtest/`, which returned **0 for all nine methods**.

## What only a live database can see here

Every brief write is a guarded UPDATE whose failure mode is invisible in its own statement
text. A gate matching no row returns `pgx.ErrNoRows` whether the brief is MISSING or STALE, and
`classifyNoRowTx` re-reads through the same transaction to decide which. A dropped
`AND version=$n`, a transposed placeholder, or a classifier answering the wrong sentinel all
leave the regexes green — while telling a client holding a stale ETag that their brief was
deleted, which is the difference between a recoverable 412 and a dead end.

`ReplaceBrief` has a SECOND error arm that no version-gate test can reach: renaming a brief onto
a slug another live brief holds raises 23505, exiting through `isUniqueViolation` rather than
the classifier. It was uncovered and is now driven, including that the refused write neither
applied the new slug nor consumed the version.

## Two fixture weaknesses the first pass shipped

Both were caught in review, and both are the same shape — **an assertion that agrees with
itself**.

`created_by` and `updated_by` are adjacent in `briefCols`, so transposing their scan
destinations is a live risk. It was undetectable, because `CreateBrief` stamps the same actor
into both (`$11` twice): the assertions compared both against one value and could not tell which
column they had read. Transposing `&createdBy` and `&updatedBy` in `scanBrief` **passed**. The
test now approves with a second actor first, so the three actor fields hold distinct values, and
the same transposition fails. The round trip had also claimed "every column" while leaving
status, version, the actor blobs and the timestamps unasserted on the read.

The status walk hand-copied the job vocabulary. `model.AllJobStatuses` exists precisely so tests
do not, and its doc says why: a test carrying its own list keeps agreeing with that list while a
status added later goes unexercised — and an unexercised status is exactly one that might be
missing from the CHECK constraint. Dropping `partial` from the constraint now fails the walk;
the hand-copied slice would have stayed green.

## `FailStuckJobs` is table-wide, which makes it a hazard to its neighbours

It carries no tenant or brief predicate, so it rewrites every aged queued/running row in the
shared schema — including the 72-hour rows `TestLivePruneTerminalJobsSparesEveryNonTerminalRow`
seeds and asserts survive its prune. Flipping those to `failed` makes them terminal, and a
terminal aged row is what that test's prune then deletes.

The two do not collide today only because each test seeds and acts within its own body while the
package runs sequentially. That is a property of the current schedule, not a guarantee — this
package already calls `t.Parallel` in `migrate_down_live_test.go`, so the interleaving becomes
reachable the moment the retention tests adopt it. The sweep test now deletes every job it
creates in a `t.Cleanup` registered BEFORE the sweep, so it fires even when an assertion fails
the test early. Verified on a pristine database: zero rows left behind. **Anything added to this
package that ages a non-terminal row owes the same cleanup.**

## Mutation-verified

23 mutations, each turning its covering test red; no survivors. The two that are worth recording
because they nearly escaped:

| mutation | result |
| --- | --- |
| transpose `&createdBy` / `&updatedBy` in `scanBrief` | **PASSED against the original fixture** — the finding |
| same, against the diverged-actor fixture | FAIL, naming both columns |
| drop `'partial'` from the status CHECK constraint | FAIL — the drift the hand-copied list missed |
| `result=COALESCE($2, result)` in `UpdateJobStatus` | FAIL — a nil result must CLEAR the prior document |
| `CreateJob("not-a-uuid")` → SQLSTATE 22P02 | FAIL — proves the 23503 check binds, not just `err != nil` |
| neutralise the `ReplaceBrief` / `Approve` version gates | FAIL — stale replay silently succeeded |
| `classifyNoRowTx` always precondition-failed | FAIL at 4 sites across 3 tests |
| `ReplaceBrief` stops clearing approval | FAIL — edited copy retained its sign-off |
| drop `isUniqueViolation` → `ErrConflict` in `ReplaceBrief` | FAIL — raw 23505 reached the caller |
| `FailStuckJobs` loses its status allow-list / age cutoff | FAIL — swept a succeeded job; swept a live one |

Two mutations were rejected as invalid before counting: replacing a version predicate with
`$12 = $12` and a tenant predicate with `$2 IS NOT NULL` both failed on type inference (encode
plan; SQLSTATE 42P18) rather than on the behaviour under test. A mutation that fails for the
wrong reason is not evidence, so each was reworked to keep the parameter's type
(`version >= $12 - 9223372036854775807`, `$2::text = $2::text`) before being counted.

Live tests confirmed to RUN rather than skip, under `-v` — a non-verbose run prints no SKIP
lines at all, so a "0 skips" reading from one is meaningless: **14 SKIP / 0 PASS** without
`TEST_DATABASE_URL`, **0 SKIP / 14 PASS** with it. Green under `-count=2` and on a pristine
database.
