# Data Model: Database Connection Health Check

**Feature**: `002-db-conn-check` | **Date**: 2026-07-09

This feature does not introduce persistent domain entities or schema. It defines operational configuration and runtime signals used by readiness.

## Entities

### Database Connection Settings

Provisioned connection parameters consumed at process startup.

| Field | Source (secret key → env) | Required | Notes |
|-------|---------------------------|----------|-------|
| Host | `host` → `PGHOST` | Yes | PostgreSQL hostname |
| Port | `port` → `PGPORT` | No | Defaults to `5432` when unset |
| Username | `username` → `PGUSER` | Yes | DB role |
| Password | `password` → `PGPASSWORD` | Yes | Secret; never logged/traced |
| Database name | `dbname` → `PGDATABASE` | Yes | Target database |
| Engine | `engine` → optional `PGENGINE` / `ENGINE` | No* | When present, must indicate PostgreSQL |

\*If engine is supplied and is not PostgreSQL, startup MUST fail. If absent, assume PostgreSQL (secret is known to be Postgres).

**Validation rules**:

- All required fields non-empty after config load, or process exits non-zero (FR-009).
- Password MUST NOT appear in `String()` helpers, logs, trace attributes, or metric labels (FR-008).
- DSN is composed in-process from the fields above (not read as a single URL from the secret).

**Relationships**: Settings → open one shared connection pool for the process lifetime.

### Database Connectivity Status

Binary runtime signal derived from a lightweight round-trip check against the pool.

| State | Meaning | Effect on readiness |
|-------|---------|---------------------|
| Reachable | Timed ping/query succeeds | Contributes to ready (AND with other readiness conditions) |
| Unreachable | Timeout, network error, auth error, or query error | Readiness returns unavailable (503) |

**Transitions**:

```text
[unknown at startup]
        │
        ▼
   pool opened ──fail──► process exit (non-zero)
        │
        ▼
   Reachable ◄──ping ok──► Unreachable
        │                      │
        └── /readyz 200        └── /readyz 503
```

Transient failures flip to Unreachable for that probe; a later successful probe returns to Reachable without restart.

### Readiness Status

Overall signal for accepting traffic.

| Inputs | Ready when |
|--------|------------|
| Service initialized (`ready` flag / wired deps) | true |
| Pool present (non-nil) | true |
| Database Connectivity Status | Reachable |

All must hold for `/readyz` → 200 `OK\n`. Any failure → 503 `ServiceUnavailableError`.

### Liveness Status

Process-alive signal only. Independent of Database Connectivity Status. `/livez` → 200 `OK\n` while the process can respond.

## Out of scope (not modeled here)

- Application tables, migrations, repositories
- Connection pool sizing policy beyond library defaults (may use sensible pgxpool defaults)
- Structured multi-check health response bodies
