---
type: "Go Package"
title: "internal/container"
description: "Dependency injection: opens the PostgreSQL pool, verifies the schema, and wires Readyz to the pool."
resource: "internal/container"
---

# internal/container

Package container provides dependency injection for the application.

When a database URL is configured it validates settings, verifies the schema,
opens an instrumented `postgres.Pool`, and wires the services against it: the
connection service (with its repo and the AES-GCM credential encryptor), the
brief service and its async orchestrator (brief/campaign/job repos), the
audiences service (its audience repo — the campaign_audiences resource), and the
campaign/health service so `/readyz` reflects DB connectivity. The per-provider
PlatformDispatcher adapters are registered via `registerDispatchers` (see
internal/dispatch), so campaign creation actually dispatches to the ad platforms;
a provider without an adapter yet is logged at startup (`logMissingDispatchers`) and
its jobs report "no dispatcher registered". Without a database URL the
health service still starts and the connection, brief, and audiences routes
return typed `503` responses rather than unmounted `404`s.

A database that is unreachable at boot does NOT crash the process. Config
errors that a retry cannot fix fail fast (the process exits): invalid database
settings, a bad credential-encryption key, a malformed `DATABASE_URL` (a
keyword DSN migrations can't consume — checked via `postgres.ValidateMigrationDSN`
before the retry path, so a deterministic config error never 503-loops forever),
AND a **permanent schema-verification failure** — a missing or invalid required index
(`ErrMissingRequiredIndex` / `ErrInvalidIndex` / `ErrRequiredIndexMismatch`). Boot only
VERIFIES the schema now (migrations run in the PreSync Job), and such a verdict can never
clear by retrying — the operator must run the rebuild DDL the error carries — so
`postgres.IsPermanentMigrationErr` classifies it and BOTH the synchronous fast path
(returns an error → process exits) and the background retry loop (logs ERROR and stops
looping) refuse to 503-loop on it. (`migrate.ErrDirty` is now reachable only in the migrate
Job, not at boot.) But a *transient* failure (DB unreachable / migration deadline within
`startupDBTimeout`, 15s per attempt) makes `NewContainer` boot the services in
503 mode instead of returning an error: the health dependency is a `notReady`
placeholder (a non-nil always-false checker — NOT nil, since a nil dep is treated
as ready, so `/readyz` reports 503, distinct from the no-database mode which
reports ready), and the connection, brief, AND audiences services start with nil
repos (their routes stay mounted and return the typed 503). A background goroutine
then retries on `dbRetryInterval`, and once the pool opens it LATE-BINDS the live
pool/repos into ALL the mounted services — the connection service (`SetBackend`), the
brief service + orchestrator (`BriefService.SetBackend`, guarded by an RWMutex;
handlers snapshot their collaborators via `ready()` so a swap can't race a request),
the audiences service (`AudienceService.SetBackend`, same RWMutex/`ready()` pattern),
and health readiness (`SetReadinessDep`) — and runs the same stuck-job recovery +
starts the periodic sweeper the fast path does. Readiness is flipped LAST, so
`/readyz` never reports OK while brief/job routes still 503. So after a cold-start
retry succeeds, the connection, brief/job, AND audiences endpoints go live WITHOUT a
pod restart.

The orchestrator is late-bound the same way and on BOTH paths: `SetOrchestrator` wires it into
the connection service on the fast path and again after a cold-start retry succeeds. Until it
runs, account discovery returns its own typed 503 ("account discovery service is unavailable"),
distinct from the storage-unavailable one — the connection service can have a live repo and no
dispatchers yet, and the two states are reported separately so an operator can tell which
dependency is still coming up.

`initDatabase` opens the pool FIRST (`NewPool` does a context-bounded `Ping`) and then
VERIFIES the schema (`postgres.VerifySchema`) — it no longer migrates. Schema MUTATION moved
into the `migrate` subcommand, run as an ArgoCD PreSync Job before the rollout (see the
*Migrate Job* concept), so boot only reads the catalog to confirm the schema is one this
binary can serve. Three states are rejected, and their remedies differ:

- an index this binary relies on is missing or INVALID — rebuild DDL;
- the schema is OLDER than the latest embedded migration — run the migrate Job;
- `schema_migrations` is DIRTY, meaning a migration failed partway and the schema matches no
  release — inspect and repair the migration state.

Documenting all three as index drift would send an operator down the wrong recovery path, which
is why the error carries the state and the log defers to it. A NEWER schema is accepted
deliberately: during a rollout the Job has already run while the previous release is still
serving, and expand/contract means the older binary works against it.

`VerifySchema` is a bounded, read-only, idempotent catalog read, so — unlike the old
migration path — overlapping boot retries are harmless and no serialization (`migrateMu` is
gone) is needed. It still runs in a goroutine bounded by the startup deadline so a hung read
cannot wedge boot; a genuinely stuck read delays readiness (surfaced as `/readyz` 503 during
the cold-start window, the intended behavior, not a hang of the whole process). A slow or
lock-blocked MIGRATION is now the PreSync Job's concern, not boot's.

This is what makes the Deployment's ~90s `startupProbe` budget real: the pod is
kept alive and `/readyz` stays 503 across a DB cold start, rather than the process
exiting at the first 15s attempt and crash-looping. `Close` cancels the retry
goroutine and waits for it before closing the pool.

**Close joins the init goroutine BEFORE it reads `indexRelay`.** That goroutine is what installs
a relay on the cold-start path, so reading the field first loses the race: a retry succeeding in
the gap starts a relay nothing stops, which then reads the outbox straight through `pool.Close`.
`indexRelay` is mutex-guarded like `pool` for the same reason — it is written from the init
goroutine and read by `Close`. Every timeout `Close` actually spends is a term of
`ContainerCloseTimeout` (`sweeperStopTimeout` + `relayStopTimeout` + `dispatchDrainTimeout` +
`service.CancelGracePeriod` + `indexer.DrainTimeout` + `cooldownStopTimeout` — all six, in the
order the constant declares them); the container test
asserts the full sum, so a timeout added to `Close` but not the budget fails a test rather
than shipping a shutdown that can overrun `DefaultShutdownTimeout` and get SIGKILLed
mid-drain.

**Cooldown stop, then pool close — in that order.** An UNCONFIRMED status toggle keeps its
campaign advisory lock past the request via `postgres.ReleaseCampaignLockAfterCooldown`, which
holds a checked-out connection for up to `unconfirmedLockCooldown` (30s). `pgxpool.Close`
blocks until every checked-out connection is returned, so `Close` calls
`postgres.StopCooldownsForShutdown(cooldownStopTimeout)` first to make each pending release
happen immediately. That budget bounds the CONNECTION, not just the wait:
`StopCooldownsForShutdown` passes it down as the `pg_advisory_unlock` round-trip's own
deadline, so an unlock that cannot finish in time fails and its connection is DESTROYED rather
than left checked out. Without that hand-off the release would run on under its ordinary
`lockReleaseTimeout` (5s) while `Close` had already moved on, and `pool.Close` would absorb
the difference outside `ContainerCloseTimeout`.

## The event fetcher is built from config, not constructed inline

`Container.eventFetcher` exists solely so `EVENT_URL_NAT64_PREFIXES` reaches
`eventurl.WithNAT64Prefixes`. The well-known `64:ff9b::/96` is decoded unconditionally
inside `eventurl`; RFC 6052 §2.2 network-specific prefixes are the operator's own global
unicast space, undiscoverable in-process and indistinguishable from any other public
prefix, so config is the only place that fact can come from. On a cluster that uses one,
an unlisted prefix is a live SSRF hole — the translator, not this process, makes the IPv4
connection.

`WithNAT64Prefixes` panics on a malformed or non-RFC-6052 length, which is the wanted
behaviour at this call site specifically: composition runs at startup, so a prefix typed
wrong stops the pod instead of silently decoding at the wrong offset for its lifetime.

See [internal/container](../../../internal/container).

## A failed `NewContainer` must not leave a live NATS connection behind

`newIndexPublisher` runs before every wiring branch, and against a configured `NATS_URL` it
returns a LIVE `*NATSPublisher` with `MaxReconnects(-1)` behind it — a goroutine that dials
forever. Every failure after that point returns a NIL container, so the caller has no handle
to `Close`, and nothing else stops that goroutine. In a long-lived caller (a test binary is
where this shows up first) each failed construction leaks a connection and its background work.

`NewContainer` therefore names its results and closes the publisher in a `defer` keyed on the
error, rather than closing it at each `return`. The difference matters: this function grows a
new failure path roughly every time the container gains a dependency, and a per-return close is
a rule the next one has to remember. A deferred one is the default.
