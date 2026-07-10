# Tasks: Database Connection Health Check

**Input**: Design documents from `/specs/002-db-conn-check/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/, quickstart.md

**Tests**: Included — spec FR-010 requires automated coverage for ready-with-DB, not-ready-without-DB, and alive-despite-DB-failure.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- Single Go service at repository root (`cmd/`, `internal/`, `pkg/`, `charts/`)

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Add dependencies and shared constants/package skeleton

- [x] T001 Add `github.com/jackc/pgx/v5` and `github.com/exaring/otelpgx` to `go.mod` / `go.sum` via `go get`
- [x] T002 [P] Add PostgreSQL env constant names (`EnvPGHost`, `EnvPGPort`, `EnvPGUser`, `EnvPGPassword`, `EnvPGDatabase`, optional `EnvPGEngine`) in `pkg/constants/constants.go`
- [x] T003 [P] Create package skeleton `internal/infrastructure/postgres/postgres.go` with license header and package doc (no behavior yet)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Load DB settings and open an instrumentable pool with a ping helper — required before any user story wiring

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T004 Extend `internal/infrastructure/config/config.go` to load `PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`/`PGDATABASE` (and optional engine), compose a DSN in-process, validate required fields are non-empty, and ensure password is never included in any config debug/`String` output
- [x] T005 [P] Add config unit tests for missing/incomplete credentials and successful load (no password assertions in logs) in `internal/infrastructure/config/config_test.go`
- [x] T006 Implement pool factory `NewPool(ctx, cfg)` in `internal/infrastructure/postgres/postgres.go` using `pgxpool`, plus `Ping(ctx)` / health-check helper with a default 2s timeout; fail fast on invalid config / pool create errors
- [x] T007 [P] Add postgres package unit tests for DSN/pool config construction and ping-helper timeout behavior (use interfaces/fakes where a live DB is not required) in `internal/infrastructure/postgres/postgres_test.go`

**Checkpoint**: Foundation ready — config validates credentials; postgres package can open a pool and ping

---

## Phase 3: User Story 1 - Readiness reflects database availability (Priority: P1) 🎯 MVP

**Goal**: `/readyz` returns 200 only when a timed DB connectivity check succeeds; returns 503 when the check fails

**Independent Test**: With mock/real DB reachable → `/readyz` 200 `OK\n`; with ping failure → `/readyz` 503; unauthenticated access preserved

### Tests for User Story 1

> **NOTE: Write these tests FIRST, ensure they FAIL before implementation**

- [x] T008 [P] [US1] Extend `internal/service/service_test.go` with a mock `DBPinger`: Readyz success when ping OK; Readyz returns `ServiceUnavailableError` when ping fails or pinger is nil
- [x] T009 [P] [US1] Add container/startup tests (or config+container focused tests) asserting missing DB credentials cause non-zero/startup failure path in `internal/container/container_test.go` (new) or existing test location matching project patterns

### Implementation for User Story 1

- [x] T010 [US1] Introduce a `DBPinger` (or equivalent) interface and inject it into `CampaignService` in `internal/service/service.go`; update `NewCampaignService` constructor; keep `ServiceReady()` requiring init flag AND non-nil pinger
- [x] T011 [US1] Update `Readyz` in `internal/service/service.go` to run a timed connectivity check via the pinger after `ServiceReady()`; on failure return `ServiceUnavailableError` (503); on success return `OK\n`; do not change response content type/body contract
- [x] T012 [US1] Wire pool open into `internal/container/container.go` (`NewContainer`): create pool from config, inject into service, store pool for `Close()`; return error (fail startup) if config/pool init fails
- [x] T013 [US1] Implement pool close in `internal/container/container.go` `Close()` so connections drain on shutdown
- [x] T014 [US1] Add Helm `app.environment` entries for `PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`/`PGDATABASE` via `valueFrom.secretKeyRef` mapping to secret keys `host`/`port`/`username`/`password`/`dbname` in `charts/lfx-v2-campaign-service/values.yaml` (document optional `PGENGINE`/`engine` if included)
- [x] T015 [US1] Verify `charts/lfx-v2-campaign-service/templates/deployment.yaml` already renders `valueFrom` for `app.environment` (no template change unless a gap is found); confirm probes still target `/readyz` and `/livez`

**Checkpoint**: User Story 1 complete — readiness reflects DB connectivity; Helm injects PG* env; unit tests cover ready/not-ready

---

## Phase 4: User Story 2 - Liveness stays independent of the database (Priority: P1)

**Goal**: `/livez` remains process-only and succeeds even when the DB connectivity check would fail

**Independent Test**: With pinger configured to fail, `/livez` still returns 200 `OK\n` while `/readyz` returns 503

### Tests for User Story 2

- [x] T016 [P] [US2] Extend `internal/service/service_test.go` so `Livez` returns `OK\n` with no error when the mock pinger would fail (and optionally when pinger is nil)

### Implementation for User Story 2

- [x] T017 [US2] Confirm `Livez` in `internal/service/service.go` does not call the DB pinger; add a brief comment documenting intentional independence from database availability (FR-006)
- [x] T018 [US2] Re-run focused tests (`go test -race ./internal/service/`) to confirm Livez/Readyz asymmetry holds after US1 wiring

**Checkpoint**: User Stories 1 AND 2 work — DB down → readyz 503, livez 200

---

## Phase 5: User Story 3 - Operators can observe database connectivity checks (Priority: P2)

**Goal**: DB pool activity during health checks emits observability signals; secrets never appear in telemetry or logs

**Independent Test**: With OTEL enabled (or unit-level tracer/logger assertions), successful and failed connectivity checks are observable without password leakage

### Tests for User Story 3

- [x] T019 [P] [US3] Add tests asserting readiness-failure logging / error paths do not include password or full DSN secrets in `internal/service/service_test.go` and/or `internal/infrastructure/postgres/postgres_test.go`

### Implementation for User Story 3

- [x] T020 [US3] Register `otelpgx` tracer on the pool config in `internal/infrastructure/postgres/postgres.go` so Ping/query activity creates spans when exporters are enabled
- [x] T021 [US3] On Readyz DB check failure in `internal/service/service.go`, emit structured `slog` warning with error message only (no password, no DSN with credentials); keep HTTP `/readyz` otelhttp filter unchanged in `cmd/campaign-service/server.go`
- [x] T022 [US3] Smoke-check against `specs/002-db-conn-check/quickstart.md` section 6 (optional OTEL) and document any required `OTEL_*` notes in that file only if a gap is found

**Checkpoint**: All user stories independently functional — readiness, liveness independence, and safe observability

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: End-to-end validation and cleanup

- [x] T023 [P] Run full `make test` (or `go test -race ./...`) and fix regressions
- [x] T024 [P] Run `make format` / format-check and `make lint` per project Makefile; ensure new files have license headers
- [x] T025 Execute quickstart scenarios 1–4 from `specs/002-db-conn-check/quickstart.md` (unit tests + local healthy/unreachable/missing-creds) and record any doc fixes in `specs/002-db-conn-check/quickstart.md`
- [x] T026 [P] Update `contracts/README.md` or `specs/002-db-conn-check/contracts/health-db.openapi.yaml` only if implementation diverged from the documented env/HTTP contract
- [x] T027 [P] Document required and optional environment variables for starting and running the service in `README.md` (PostgreSQL `PG*`, server, JWT/NATS reserved, and `OTEL_*`)
- [x] T028 [P] Document local development database access in `README.md` and `specs/002-db-conn-check/quickstart.md`: when `kubectl port-forward` is required vs not, how to forward to the Postgres Service, and how to load `PG*` from the secret without printing the password
- [x] T029 Document build-and-run against lfx-v2-dev in `README.md` and `specs/002-db-conn-check/quickstart.md`: jump-pod tunnel to RDS (`ExternalName` `lfx/rds-postgres`), port-forward, load `PG*` from `lfx-v2-campaign-service-secrets`, `make build` / `make run`, and readyz/livez smoke checks; align Helm `secretKeyRef` name with the real secret

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — start immediately
- **Foundational (Phase 2)**: Depends on Setup — **BLOCKS** all user stories
- **User Story 1 (Phase 3)**: Depends on Foundational — MVP
- **User Story 2 (Phase 4)**: Depends on US1 service wiring (pinger injected) so independence can be proven against a failing pinger
- **User Story 3 (Phase 5)**: Depends on Foundational pool + US1 Readyz path (instrument and log the existing check)
- **Polish (Phase 6)**: Depends on desired user stories being complete

### User Story Dependencies

- **User Story 1 (P1)**: After Foundational — no dependency on US2/US3
- **User Story 2 (P1)**: After US1 pinger injection — independently testable via Livez tests with failing mock
- **User Story 3 (P2)**: After US1 Readyz ping path exists — adds otelpgx + safe logging

### Within Each User Story

- Tests (where listed) SHOULD be written and FAIL before implementation
- Interface/service changes before container wiring
- Container wiring before Helm env injection
- Story complete before moving to next priority when working sequentially

### Parallel Opportunities

- T002 / T003 after T001 (or T002∥T003 once module exists)
- T005 ∥ T006 after T004 (tests can start from config shape; pool impl parallel with config tests if careful)
- T007 after T006
- T008 ∥ T009 before or alongside T010–T011
- T016 can start once T010 introduces the pinger field
- T019 ∥ T020 after US1 Readyz path exists
- T023 ∥ T024 ∥ T026 in Polish

---

## Parallel Example: User Story 1

```bash
# Tests in parallel:
Task: "Extend service_test.go with mock DBPinger ready/not-ready cases"
Task: "Add container/startup missing-credentials tests"

# Then sequential implementation:
Task: "Introduce DBPinger + ServiceReady in service.go"
Task: "Update Readyz timed ping in service.go"
Task: "Wire pool in container.go NewContainer + Close"
Task: "Helm PG* secretKeyRef in values.yaml"
```

---

## Parallel Example: User Story 3

```bash
Task: "Secret-leakage tests for failure logs"
Task: "Register otelpgx on pool config in postgres.go"
# Then:
Task: "slog warning on Readyz ping failure in service.go"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational
3. Complete Phase 3: User Story 1 (tests → service → container → Helm)
4. **STOP and VALIDATE**: `make test` + quickstart readyz healthy/unreachable
5. Optionally demo readiness gating before US2/US3

### Incremental Delivery

1. Setup + Foundational → pool + config ready
2. US1 → readiness reflects DB → **MVP**
3. US2 → prove livez independence (mostly tests + guard comment)
4. US3 → otelpgx + safe slog
5. Polish → format/lint/quickstart

### Parallel Team Strategy

With multiple developers:

1. Team completes Setup + Foundational together
2. After Foundational:
   - Dev A: US1 service + container
   - Dev B: US1 Helm values (after env contract confirmed)
3. US2/US3 follow US1 service interface freeze

---

## Notes

- [P] tasks = different files, no dependencies on incomplete sibling tasks
- [Story] label maps task to US1/US2/US3 for traceability
- Do not put DB checks in `Livez`
- Do not log or trace `PGPASSWORD` / credentialized DSNs
- Migrations, repositories, and schema remain out of scope
- Commit after each task or logical group
- Avoid: vague tasks, same-file conflicts without ordering, skipping FR-010 tests
