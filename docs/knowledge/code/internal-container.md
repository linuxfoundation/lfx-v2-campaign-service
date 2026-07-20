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
errors that a retry cannot fix — invalid database settings or a bad
credential-encryption key — still fail fast (the process exits). But a
*transient* failure (DB unreachable / migration deadline within
`startupDBTimeout`, 15s per attempt) makes `NewContainer` boot the services in
503 mode instead of returning an error: the health dependency is a `notReady`
placeholder (so `/readyz` reports 503, distinct from the no-database mode which
reports ready) and the connection service starts with a nil repo. A background
goroutine then retries migration+pool-open on `dbRetryInterval`, and once the
pool opens it swaps the live pool/repo into both services (`SetReadinessDep` /
`SetBackend`, guarded by a mutex against concurrent request reads), flipping
`/readyz` to healthy and the connection endpoints live. This is what makes the
Deployment's ~90s `startupProbe` budget real: the pod is kept alive and `/readyz`
stays 503 across a DB cold start, rather than the process exiting at the first
15s attempt and crash-looping. `Close` cancels the retry goroutine and waits for
it before closing the pool.

See [internal/container](../../../internal/container).
