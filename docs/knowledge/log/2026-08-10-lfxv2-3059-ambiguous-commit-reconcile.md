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
