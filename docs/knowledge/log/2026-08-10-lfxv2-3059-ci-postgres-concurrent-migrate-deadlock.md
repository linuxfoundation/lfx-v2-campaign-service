# 2026-08-10 — LFXV2-3059: CI deadlock from two packages migrating the same live database

**Fix** — two test packages ran `Migrate` concurrently against one database and deadlocked CI.

`make test`'s Build and Test check failed on PR #106 with a real Postgres error, not a
flaky one:

```
audience_lease_live_test.go:33: live-database harness: migrate TEST_DATABASE_URL: apply
migrations: try lock failed in line 0: SELECT pg_advisory_lock($1)
(details: ERROR: deadlock detected (SQLSTATE 40P01))
```

## Root cause

This PR added the first live-database test file OUTSIDE `internal/infrastructure/postgres/dbtest`:
`audience_reconcile_live_test.go` lives in `internal/infrastructure/postgres` itself, because
`reconcileAmbiguousAudienceCommit` is unexported and importing `dbtest` back into `postgres`
would be a cycle. It carries its own package-scoped `sync.Once` + `Migrate(dsn)` call, mirroring
`dbtest.Pool` "minimally, for THIS file only" (see its comment).

`go test ./...` builds and runs each package as a separate binary/process, with the default
parallelism running several at once. `internal/infrastructure/postgres` and
`internal/infrastructure/postgres/dbtest` are now TWO independent processes, each with its own
`sync.Once`, both capable of calling `postgres.Migrate()` against the same
`TEST_DATABASE_URL` at the same moment. golang-migrate's advisory lock serializes the two
`m.Up()` calls, but that is not enough here: migration 000018's
`CREATE INDEX CONCURRENTLY IF NOT EXISTS uq_campaign_audiences_brief_platform_building` still
has to wait for every OTHER session's open transaction against `campaign_audiences` to finish —
including one held by the other package's own live tests — which is exactly the shape of
Postgres's documented CONCURRENTLY-vs-concurrent-transaction deadlock. Postgres kills one side
and CI reports it as a genuine test failure, not a skip.

Retrying past the deadlock was rejected: a killed `CREATE INDEX CONCURRENTLY` build can leave
the index present and INVALID under the same name, so a blind retry's `IF NOT EXISTS` would
silently skip it — and `checkNoInvalidIndexes` (pool.go) would then correctly, but confusingly,
refuse the schema as permanently broken for what was actually a transient CI race.

## Fix

`Makefile`'s `test` target now runs `internal/infrastructure/postgres/...` (all three
subpackages: `postgres`, `dbtest`, `migrations`) in its own `go test -p 1` invocation, forcing
them to execute strictly one at a time. Every other package still runs with full default
parallelism in a second invocation. This closes the race at its source — no two live-database
migrate calls can ever be in flight together — rather than papering over an occasional CI
failure with a retry.

`coverage.out` was never consumed downstream (no codecov/upload step), so splitting it into
`coverage.out` + `coverage-postgres.out` needed no other change; both are already covered by
the repo's `*.out` gitignore rule.
