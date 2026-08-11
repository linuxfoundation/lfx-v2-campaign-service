# 2026-08-10 — LFXV2-3059: the lease release must not be version-gated

**Fix** — three review findings on PR #106, all of which end in the same place: a build lease
that nothing automatic will ever release.

## 1. `releaseUnstartedClaim` stranded the lease it exists to release (Copilot,
`internal/service/audience_build.go:446`)

The service layer released an unstarted claim by setting `row.Status = model.AudienceFailed` and
calling `repo.UpdateAudience(relCtx, row, row.Version)`. `UpdateAudience` is **version-gated**,
and the version is exactly the wrong thing to gate a release on. A concurrent `PATCH` that edits
any other column bumps `version` while leaving `status` at `building`, so the release matches
zero rows, returns `ErrPreconditionFailed`, gets logged as a best-effort failure, and the row
stays `building` — blocking every later build of that `(brief_id, platform)` behind a 409 until
an operator PATCHes it by hand.

This is the same defect the reconcile path in `audience_repo.go` had already been corrected for
(see `2026-08-10-lfxv2-3059-ambiguous-commit-reconcile.md`, "the predicate is the status, not the
version"), reintroduced one layer up because the service reached the store through the general
update method rather than a purpose-built one.

**Fix:** a new `AudienceRepository.ReleaseAudienceBuildLease(ctx, projectID, briefID, id)`, sharing
the reconcile's statement — `UPDATE … SET status='failed', version=version+1 WHERE id=$1 AND
brief_id=$2 AND project_id=$3 AND status='building'`. The condition that decides the outcome is
evaluated by the statement that performs it, and the call is idempotent under retry: a row already
`failed`, already `built`, or absent matches nothing and returns `nil`, not an error.
`releaseUnstartedClaim` no longer mutates the caller's struct at all.

Pinned by `TestBuildAudience_ReleasesTheLeaseAfterAConcurrentPatchBumpsTheVersion`, which bumps
every stored row's version from inside the builder and asserts the row still reaches `failed`.
Revert-verified: with `UpdateAudience(relCtx, row, row.Version)` restored it fails with
`expected: "failed" / actual: "building"`.

The fake in `audience_test.go` models the real predicate — tenant-scoped, status-gated, silently
no-op otherwise. A fake that released unconditionally, or that keyed off the version, would make
that test vacuous.

## 2. A confirmed-held lease was reported as a probable rollback (Cursor,
`internal/infrastructure/postgres/audience_repo.go:251`)

`reconcileAmbiguousAudienceCommit` retried while `tryRelease…` returned false, and false meant
two different things: "no row is visible" (the ordinary rolled-back commit) and "the row is
present and still `building`" (a lease this process knows is held). On the LAST attempt — or when
the reconcile context was already done — a row that had just been CONFIRMED holding the lease was
reported with the rollback wording, at warn. The one case that genuinely needs an operator read
as the routine one.

Two changes:

- `tryReleaseAudienceBuildLease` now returns a tri-state (`audienceReconcileUnseen` / `Settled` /
  `Held`), and runs the release **twice** per attempt. Seeing `building` from the classifying
  SELECT after a zero-row UPDATE is not a contradiction: the two statements run on separately
  pooled connections with their own snapshots, so it means the row became visible between them
  and the UPDATE merely ran too early. An immediate second pass settles it. Only `Unseen` is
  worth waiting on.
- `reportUnreconciledAudience` picks the level and the wording from whether any attempt confirmed
  the row held: ERROR and a statement of fact when it did, WARN and the hedged "if the commit did
  land" when it never did. An error on every rolled-back commit is an error nobody reads; that is
  why the hedged case stays a warn, and why it must not swallow the other one.

Tests: `TestReportUnreconciledAudience_SeparatesAConfirmedHeldLeaseFromAProbableRollback` (plain
unit test — reaching `Held` through the database would need the row to become visible in the
microseconds between one attempt's two statements, on the last attempt, which nothing can
schedule into) and `TestTryReleaseAudienceBuildLease_ClassifiesEachOutcome` (live, covering the
outcomes that ARE reachable: absent, building, terminal, and another tenant's row). The report
test is revert-verified: collapsing the two branches fails it on all three assertions.

## 3. `make test` hid every failure behind the first one (Cursor, `Makefile:110`)

Splitting the suite into a `-p 1` live-database half and an everything-else half left them as two
plain recipe lines, and make aborts a target on the first failing line. A red postgres suite meant
the remaining ~40 packages were never built or run — so one live-database failure hid every
unrelated failure until somebody ran the suite again. `go test ./...` did not have that property
and splitting it should not have cost it. Now one compound recipe accumulating `rc`, so both
halves always run and the target's status is the OR of the two.

## Verification

`go build ./...`, `go vet ./...`, `golangci-lint run ./...` clean. Full suite green against a
live database (`TEST_DATABASE_URL` on a per-branch database — branches differ in migration count,
so they cannot share one).
