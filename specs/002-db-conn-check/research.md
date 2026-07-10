# Research: Database Connection Health Check

**Feature**: `002-db-conn-check` | **Date**: 2026-07-09

## R1 — PostgreSQL driver and pool

**Decision**: Use `github.com/jackc/pgx/v5` with `pgxpool.Pool` as the shared connection pool.

**Rationale**: Matches `lfx-v2-newsletter-service` (pgx/v5 + pgxpool). Native PostgreSQL protocol, first-class pool, and `Ping`/`PingContext` for readiness. Aligns with the planned `internal/infrastructure/postgres/` layout in `docs/build-summary.md`.

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| `lib/pq` + `database/sql` | Legacy driver; newsletter/platform direction is pgx |
| `database/sql` only with pgx stdlib | Extra indirection for a health-only pool; can add stdlib later if repositories need it |
| Single `pgx.Conn` (no pool) | Pool is the natural shared handle for future repositories; health check reuses the same object |

## R2 — Connection settings / env var shape

**Decision**: Accept discrete env vars `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` (and optionally validate `PGENGINE` (from secret key `engine`) is postgres when present). Compose a DSN in-process. Do not require a pre-built `DATABASE_URL` in the secret.

**Rationale**: The provisioned Kubernetes secret exposes field keys `host`, `port`, `username`, `password`, `dbname`, `engine` — the same shape newsletter uses with `shape: "fields"`. Composing the URL in-process keeps the password out of shell/Helm string interpolation. Campaign chart already supports per-key `valueFrom.secretKeyRef` under `app.environment`.

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| Single `DATABASE_URL` from secret | Secret does not currently ship a URL key; would require secret-shape change |
| Raw secret key names as env (`host`, `password`) | Collides with HTTP `HOST`; non-idiomatic for Postgres clients |
| Hard-code DSN in values | Secrets must not be committed |

## R3 — Where the check runs (readyz vs livez)

**Decision**: Database connectivity is validated on **every** `GET /readyz` via a timed `Ping` (or equivalent trivial round-trip). `GET /livez` remains process-only and does not touch the database.

**Rationale**: Spec FR-006 and prior health-endpoints FR-007 require liveness independence. Newsletter readiness uses `PingContext` with a 2s timeout per probe — that satisfies transient-blip recovery (failed probe → 503; later success → ready again) without restarting the process.

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| Check DB on livez too | Causes restart storms during DB outages |
| Wire-only check (`pool != nil`) at construction | Misses runtime outages; fails SC-002 / transient-blip edge case |
| Background goroutine updating a ready flag | More moving parts; probe-time ping is simpler and matches newsletter |

## R4 — ServiceReady vs per-request ping

**Decision**:

1. At startup, when a database URL is configured, open the pool; incomplete PG* credentials → non-zero exit (FR-009). Fully omitting all database settings remains allowed (no-DB / metadata-only mode).
2. `ServiceReady()` / `Readyz` require dependency health only when a pool is wired; a nil dep reports ready from the init flag alone (FR-009).
3. When a dep is wired, `Readyz` runs a per-request connectivity check (timed ping). If ping fails → `ServiceUnavailableError` (503). `Livez` unchanged.

**Rationale**: Separates “dependency wired” from “dependency reachable now.” Keeps unit tests mockable via a small `ReadinessChecker` interface injected into `CampaignService`. No-DB mode stays usable for unit tests and metadata-only local runs.

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| Fold ping into `ServiceReady()` only | `ServiceReady()` is currently a pure bool with no context/timeout; ping needs both |
| Change Goa contract / structured health body | Spec explicitly preserves plain-text `OK\n` / 503 |

## R5 — OpenTelemetry on the connection

**Decision**: Register [`github.com/exaring/otelpgx`](https://github.com/exaring/otelpgx) as the pgx tracer on the pool config so repository queries emit spans. Wrap `Pool.Ready` in an explicit `postgres.ready` health span that records ping success/failure — `/readyz` is excluded from `otelhttp` (probe noise), and `pgxpool.Ping` does not go through otelpgx's Query/Exec hooks. On readiness failure, emit a structured `slog` debug line with the error message and **no** connection secrets (host may be logged; password never).

**Rationale**: Service already uses OTEL SDK + `otelhttp`. There is no `go.opentelemetry.io/contrib/.../otelpgx` module path; `exaring/otelpgx` is the community-standard pgx v5 tracer and matches common Go+Postgres+OTEL practice. Keeping `/readyz` out of `otelhttp` avoids steady HTTP span volume; the explicit Ready span still satisfies FR-007 for the health-check path.

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| `go.opentelemetry.io/contrib/.../otelpgx` | Module path does not exist / has no published versions |
| Rely on otelpgx alone for readiness | `Ping` is not traced by otelpgx Query/Exec hooks; `/readyz` has no recording HTTP parent |
| Trace `/readyz` via otelhttp | Creates high-volume probe spans; health check is better as a dedicated DB span |
| Metrics-only | Spec asks for observable connectivity checks; traces + slog cover diagnosis |
| Log DSN on failure | Violates FR-008 |

## R6 — Helm / deployment wiring

**Decision**: Add `app.environment` entries in the campaign Helm chart mapping `PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`/`PGDATABASE` (and optional engine) via `valueFrom.secretKeyRef` to the existing ExternalSecret-managed secret keys (`host`, `port`, `username`, `password`, `dbname`). Probe paths stay `/livez` and `/readyz` (already configured).

**Rationale**: Secret is already provisioned out-of-band; the chart only needs to inject keys into the pod. Matches newsletter field-shape pattern adapted to campaign’s existing `app.environment` map (no new template DSL required).

**Alternatives considered**:

| Option | Why rejected |
|--------|--------------|
| `envFrom: secretRef` whole secret | Would inject raw key names (`host`, `password`) that collide / confuse config |
| Skip chart changes (assume env already present) | Repo chart currently has no DB env; readiness would fail in-cluster without this |

## R7 — Scope boundaries

**Decision**: In scope — driver dependency, config loading, pool open/close, otelpgx, readiness ping, Helm env wiring, unit tests with a mock pinger. Out of scope — migrations, repositories, schema, CloudNativePG/Tofu provisioning, structured multi-check health payloads.

**Rationale**: Spec assumptions and Jira acceptance criteria focus on livez/readyz validating the connection. Broader data-layer work belongs to follow-on tickets.

## R8 — Ping timeout

**Decision**: Use a short per-check timeout (default **2 seconds**, matching newsletter), configurable later if needed. Do not block readiness probes indefinitely.

**Rationale**: SC-006 and Kubernetes probe intervals require bounded checks. 2s is proven in newsletter and leaves headroom under typical `periodSeconds: 10` readiness probes.
