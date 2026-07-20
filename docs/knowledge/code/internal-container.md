---
type: "Go Package"
title: "internal/container"
description: "Dependency injection: opens the PostgreSQL pool, runs migrations, and wires Readyz to the pool."
resource: "internal/container"
---

# internal/container

Package container provides dependency injection for the application.

When a database URL is configured it validates settings, runs migrations,
opens an instrumented `postgres.Pool`, and passes the pool to
`NewCampaignService` so `/readyz` reflects DB connectivity. Without a
database URL the health service still starts and connection routes return
typed `503` responses.

A database that is unreachable at boot does NOT crash the process. Config
errors that a retry cannot fix fail fast (the process exits): invalid database
settings, a bad credential-encryption key, AND a malformed `DATABASE_URL` (a
keyword DSN migrations can't consume — checked via `postgres.ValidateMigrationDSN`
before the retry path, so a deterministic config error never 503-loops forever).
But a *transient* failure (DB unreachable / migration deadline within
`startupDBTimeout`, 15s per attempt) makes `NewContainer` boot the services in
503 mode instead of returning an error: the health dependency is a `notReady`
placeholder (a non-nil always-false checker — NOT nil, since a nil dep is treated
as ready, so `/readyz` reports 503, distinct from the no-database mode which
reports ready) and the connection service starts with a nil repo. A background
goroutine then retries on `dbRetryInterval`, and once the pool opens it swaps the
live pool/repo into both services (`SetReadinessDep` / `SetBackend`, guarded by a
mutex against concurrent request reads), flipping `/readyz` to healthy and the
connection endpoints live.

`initDatabase` opens the pool FIRST (`NewPool` does a context-bounded `Ping`) and
runs `Migrate` only after a reachable ping. This is deliberate: golang-migrate's
`Up()` takes no context and blocks until the DB responds, so running it against a
DOWN database would hang past the 15s deadline — and because the caller retries,
each hung attempt would leak another migration goroutine and race concurrent
migrations. Gating `Migrate` behind a reachable ping means it only runs when the DB
is up, where it connects immediately.

A migration on a *reachable* DB can still run long (a large or lock-blocked
migration). For that case `Migrate` runs in a goroutine bounded by the startup
deadline: `initDatabase` returns on the deadline, but the migration goroutine may
still be running afterward. Two things keep this safe rather than a leak-and-race:
(1) a package-level `migrateMu` serializes migration runs, so a retry BLOCKS on the
mutex until the prior (deadline-abandoned) migration finishes instead of starting a
second concurrent one; and (2) migrations are idempotent (already-applied steps are
skipped), so a re-run after a partial is harmless. So there is at most one migration
in flight at a time, never overlapping/racing — though a genuinely stuck migration
can still delay readiness (surfaced as `/readyz` 503 during the cold-start window,
which is the intended behavior, not a hang of the whole process).

This is what makes the Deployment's ~90s `startupProbe` budget real: the pod is
kept alive and `/readyz` stays 503 across a DB cold start, rather than the process
exiting at the first 15s attempt and crash-looping. `Close` cancels the retry
goroutine and waits for it before closing the pool.

See [internal/container](../../../internal/container).
