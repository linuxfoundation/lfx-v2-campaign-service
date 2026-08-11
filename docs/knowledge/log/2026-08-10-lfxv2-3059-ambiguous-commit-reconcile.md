# 2026-08-10 — LFXV2-3059: reconcile an ambiguous audience-build commit error

**Fix** — a lost commit acknowledgement could strand an audience-build lease forever.

Review finding on PR #106 (dealako, CHANGES_REQUESTED): `CreateAudienceForApprovedBrief`
returned a bare error on `tx.Commit(ctx)` failure with no attempt to reconcile state. A
`Commit` error does not prove PostgreSQL rolled back — the server can commit the row before
the client's acknowledgement of the commit is lost. If that happens, the INSERT's `building`
row is real and holds the `(brief_id, platform)` build lease (migration 000018) forever:
`CreateAudienceForApprovedBrief` returns `nil` for the created row on this path, so the
service layer's `releaseUnstartedClaim` has no row to release it through.

## Fix

Added `reconcileAmbiguousAudienceCommit(ctx, pool, row)`, called from the `tx.Commit` error
branch with the row already observed via the INSERT's `RETURNING` clause. It runs a bounded
(5s), detached (`context.WithoutCancel`)
`UPDATE ... WHERE id=$1 AND brief_id=$2 AND project_id=$3 AND status='building'` that moves
the row to `failed`. Genuinely rolled back ⇒ no row at that id ⇒ zero-row UPDATE, a harmless
no-op. Best-effort: logs on failure rather than compounding the caller's already-in-flight
error.

**The predicate is the status, not the version** — the first cut gated on `version=$2` and
that was wrong. A concurrent `PATCH` touching any other field bumps `version` while leaving
the status at `building`, so the version-gated UPDATE matches zero rows, returns no error,
and strands exactly the lease it was added to release. `status='building'` enforces the
do-not-downgrade invariant inside the write itself and is idempotent under retry.

## Testing this required a new live-DB testing seam

The function is unexported, and the obvious test location — the existing `dbtest_test`
package (`audience_lease_live_test.go`) — cannot reach it. Worse, an in-package `postgres`
test file cannot import `dbtest` either: `dbtest.go` (non-test) imports
`internal/infrastructure/postgres` (production) to build its own fixtures, so the reverse
import is a genuine cycle (`go vet` confirms: `import cycle not allowed in test`).

`audience_reconcile_live_test.go` (new, `package postgres`) works around this by not
importing `dbtest` at all — it calls `Migrate` and `NewPool` directly (the same two calls
`dbtest.Pool` makes internally) against `TEST_DATABASE_URL`, and reimplements
`dbtest.UniqueID`'s shape locally. Two tests:

- `TestReconcileAmbiguousAudienceCommit_MovesABuildingRowToFailed` — inserts a real
  `building` row, reconciles it, asserts `status='failed'` and `version` incremented, then
  asserts the `(brief_id, platform)` lease is free again by inserting a fresh `building` row
  for the same pair.
- `TestReconcileAmbiguousAudienceCommit_NoOpWhenTheCommitReallyRolledBack` — calls reconcile
  against an id that was never inserted; must not panic or error.

Mutation-verified: replacing the function body with an early `return` before the UPDATE made
the first test fail with `status = "building", want "failed"` and a duplicate-key error on
the rebuild-lease check; reverted and confirmed byte-identical via `diff`.

Run locally (no CI Postgres service available in this environment): spun up a throwaway
`postgres@16` instance via Homebrew's `initdb`/`pg_ctl` (no Docker available) against
`TEST_DATABASE_URL=postgres://postgres@127.0.0.1:55432/campaign_service_test?sslmode=disable`,
ran the full `internal/...` suite with `-race` — all green — then tore the instance down.

## Round: the retry budget and the error paths that forgot what they had seen

**Kind:** Fix

Two Copilot findings on PR #106, both in `audience_repo.go`, both a case of the code and the
paragraph above it disagreeing.

**The schedule was one attempt short of the one it documents.** N attempts sleep N-1 times, so
`ambiguousCommitReconcileAttempts = 6` spans 25+50+100+200+400 = **775ms**, not the "~1.2s" the
comment claimed — and it never reaches the 500ms cap that same comment describes doubling to. Now
7, spanning 1275ms. `TestAmbiguousCommitReconcileScheduleSpans` derives the total from the three
constants rather than restating it, and separately asserts the cap is reachable and that the whole
schedule stays inside the 5s timeout: the attempt cap is supposed to be what ends the loop
normally, leaving the timeout a ceiling for slow queries.

**An attempt's error paths discarded what that attempt had already observed.** Both returned
`audienceReconcileUnseen` — the zero value. That reintroduced, through the error path, exactly the
collapse the tri-state outcome was added to prevent: once the first pass has read the row as
`building` the lease is CONFIRMED held, and a second-pass query failing afterwards does not
unlearn it. The retry loop's `confirmedHeld` stayed false, so `reportUnreconciledAudience` logged
a known stranded lease at **warn**, in the hedged "if the commit did land" wording meant for the
ordinary rolled-back commit. The attempt now promotes its unsettled outcome to `Held` the moment
it observes `building`, and never demotes — absence is evidence only until the row is seen. A
FIRST-pass failure still reports `Unseen`, which is why this is a promotion rather than a constant.

**Regression Guard** — `tryReleaseAudienceBuildLease` now takes an `audienceReconcileDB`
interface (`Exec`/`QueryRow`, satisfied by `*Pool`). It had to: reaching either error path
requires the row to become visible in the microseconds BETWEEN an attempt's two statements, and
the live tests already record that as unschedulable — which is precisely why the bug survived
three rounds of review on this function. `audience_reconcile_attempt_test.go` drives it with a
fake that models the real contract (a parsed `pgconn.CommandTag`, a `pgx.Row` that can report
`pgx.ErrNoRows`) and covers both second-pass failures, both first-pass failures, the both-passes-
`building` Held case, and the settles-on-the-second-pass case. Revert-verified: restoring
`audienceReconcileUnseen` on the error returns fails with `outcome = 0, want
audienceReconcileHeld (2)`; restoring `= 6` fails with `spans 775ms, want 1.275s` plus `the last
delay is 400ms and never reaches the 500ms cap`.

The generalisation worth keeping: **a comment that states a computed value is a test that never
runs.** Both findings here are the same defect wearing different clothes — a number and a
tri-state, each asserted in prose and neither checked by anything.
