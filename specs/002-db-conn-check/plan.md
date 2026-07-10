# Implementation Plan: Database Connection Health Check

**Branch**: `feat/LFXV2-2559-add-db-conn-check` | **Date**: 2026-07-09 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-db-conn-check/spec.md`

## Summary

Wire a PostgreSQL connection pool into the campaign service at startup (credentials from the existing ExternalSecret field shape), instrument it with OpenTelemetry (`otelpgx`), and extend `GET /readyz` to fail with 503 when a timed connectivity check (`Ping` / trivial round-trip) fails. `GET /livez` stays process-only. Approach mirrors `lfx-v2-newsletter-service` (pgxpool + PG* env + per-probe ping) while preserving the campaign service’s Goa health contract from `001-health-endpoints`.

## Technical Context

**Language/Version**: Go 1.25.0 (module); local toolchain may be newer

**Primary Dependencies**: Existing — Goa v3, clue, otel SDK + `otelhttp`, testify. New — `github.com/jackc/pgx/v5` (pgxpool), `github.com/exaring/otelpgx`

**Storage**: PostgreSQL (connection pool for health validation; no schema/migrations in this feature)

**Testing**: `go test -race` / `make test`; table-driven unit tests with a mock `DBPinger`; optional local manual probe against a real Postgres

**Target Platform**: Linux containers on Kubernetes (dev/staging/prod)

**Project Type**: Single Go HTTP web service (Goa-generated transport + hand-written service)

**Performance Goals**: Readiness DB check completes within ~2s timeout under failure; successful checks typically well under 1s (SC-006)

**Constraints**: No secrets in logs/traces; livez must not depend on DB; preserve `OK\n` / 503 contracts; fail startup on missing credentials; no migrations/repos in scope

**Scale/Scope**: Config + postgres package + container/service wiring + Helm env injection + unit tests. Small, focused change.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project constitution (`.specify/memory/constitution.md`) is an unpopulated template. Planning applies workspace engineering rules as gates:

- **License headers**: every new source file gets the MIT/LF header. → Satisfied by design.
- **Simplicity / YAGNI**: pool + ping + mockable interface; no migration framework, repository layer, or plugin health system. → Pass.
- **Surgical changes**: extend existing `CampaignService` / container / config / Helm `app.environment`; do not redesign health endpoints or OTEL bootstrap. → Pass.
- **Test-first**: ready/not-ready/alive-despite-DB-down covered by unit tests with mock pinger. → Pass.
- **Platform consistency**: follow newsletter for Postgres wiring and campaign/project-service for Goa health contract. → Pass.
- **Credential safety**: never log/trace passwords (FR-008). → Pass.

No violations → Complexity Tracking not required.

### Post-design re-check

Phase 1 design (data-model, contracts, quickstart) introduces no new constitution pressure: HTTP contract unchanged; entities are operational signals only; Helm wiring is additive. Gate still passes.

## Project Structure

### Documentation (this feature)

```text
specs/002-db-conn-check/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── README.md
│   └── health-db.openapi.yaml
└── checklists/
    └── requirements.md
```

### Source Code (repository root)

```text
internal/
├── container/container.go              # EDIT — open pool at startup; inject into service; close on shutdown
├── infrastructure/
│   ├── config/config.go                # EDIT — load PG* (and optional engine); compose DSN; validate required fields
│   └── postgres/                       # NEW — pool factory, HealthCheck/Ping helper, otelpgx tracer hook
│       ├── postgres.go
│       └── postgres_test.go
├── service/
│   ├── service.go                      # EDIT — DBPinger dependency; Readyz runs timed ping; Livez unchanged
│   └── service_test.go                 # EDIT — mock pinger success/failure cases
pkg/constants/constants.go              # EDIT — PG* / engine env constant names
charts/lfx-v2-campaign-service/
├── values.yaml                         # EDIT — PG* env via secretKeyRef to existing secret keys
└── templates/deployment.yaml           # VERIFY — existing app.environment loop already supports valueFrom
go.mod / go.sum                         # EDIT — add pgx/v5 + otelpgx
```

**Structure Decision**: Single Go service. New code lives under `internal/infrastructure/postgres/` as planned in `docs/build-summary.md`. Health HTTP surface stays in existing Goa design/`CampaignService`; no new endpoints.

## Complexity Tracking

> Not applicable — no constitution violations.
