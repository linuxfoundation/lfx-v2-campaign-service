# Feature Specification: Database Connection Health Check

**Feature Branch**: `feat/LFXV2-2559-add-db-conn-check`

**Created**: 2026-07-09

**Status**: Draft

**Input**: User description: "We've recently added database credentials to the lfx-v2-campaign-service via the secrets resource (PostgreSQL connection fields: dbname, engine, host, password, port, username). I would like to add a database connection validation check in the livez and/or readyz health endpoints. We need to add the appropriate driver dependency, telemetry on the connection, connection setup, and a sample connection to use during the health check endpoints (such as SELECT 1)."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Readiness reflects database availability (Priority: P1)

As a platform operator (and the orchestration platform acting on my behalf), I need the readiness endpoint to confirm that the campaign service can reach its PostgreSQL database so that traffic is only routed to instances that can actually use the data store.

**Why this priority**: Without a database connectivity check, an instance can report ready while the database is unreachable, causing user-facing failures. This is the core acceptance criterion for the provisioning work and is independently shippable once connection setup exists.

**Independent Test**: Start the service with valid database credentials and a reachable database, request the readiness endpoint, and confirm success; then make the database unreachable (or use invalid credentials) and confirm readiness reports unavailable.

**Acceptance Scenarios**:

1. **Given** the service has completed startup and can successfully reach the configured PostgreSQL database, **When** a client requests the readiness endpoint, **Then** the service responds with a success status and the established plain-text ready body.
2. **Given** the service is running but cannot reach the configured PostgreSQL database (unreachable host, refused connection, authentication failure, or query failure), **When** a client requests the readiness endpoint, **Then** the service responds with a "service unavailable" status so the instance is removed from serving rotation.
3. **Given** the service is running, **When** the readiness endpoint is requested without authentication credentials, **Then** the service still responds (the endpoint remains unauthenticated).

---

### User Story 2 - Liveness stays independent of the database (Priority: P1)

As a platform operator, I need the liveness endpoint to continue reflecting only whether the process itself is alive, so that a temporary database outage does not trigger unnecessary process restarts.

**Why this priority**: Restarting pods during a database outage worsens recovery (thundering herd, longer downtime). Preserving the existing liveness contract is required for safe operations and is independently verifiable.

**Independent Test**: With the service running and the database unreachable, request the liveness endpoint and confirm it still returns success while readiness reports unavailable.

**Acceptance Scenarios**:

1. **Given** the service process is running and the database is unreachable, **When** a client requests the liveness endpoint, **Then** the service still responds with success (liveness does not depend on the database).
2. **Given** the service process is running and the database is healthy, **When** a client requests the liveness endpoint, **Then** the service responds with success as before.

---

### User Story 3 - Operators can observe database connectivity checks (Priority: P2)

As a platform operator or developer investigating readiness failures, I need database connection activity during health checks to produce observable telemetry so I can distinguish "process up but database down" from other failure modes.

**Why this priority**: Correct readiness behavior is valuable without telemetry, but diagnosing production failures requires visibility into connection attempts and failures. This builds on Stories 1 and 2.

**Independent Test**: Run the service with telemetry enabled, exercise readiness when the database is healthy and when it is not, and confirm connection-related telemetry is emitted for the health-check path without exposing credentials.

**Acceptance Scenarios**:

1. **Given** the service is running with observability enabled, **When** readiness successfully validates the database, **Then** operators can observe that a database connectivity check occurred.
2. **Given** the service is running with observability enabled, **When** readiness fails because the database is unreachable or rejects the check, **Then** operators can observe a failed connectivity check without seeing passwords or other secret material in telemetry attributes or logs.

---

### Edge Cases

- **Database unreachable at startup**: When a database URL is
  configured, container init runs migrations and an initial pool
  ping before the HTTP server starts; an unreachable database MUST
  cause startup to fail (non-zero exit) rather than exposing an
  unavailable readiness endpoint. After a successful start, a later
  outage is reported via readiness only (see transient blips).
- **Transient database blips**: A failed check MUST cause readiness to report unavailable for that probe; a subsequent successful check MUST restore ready status without requiring a process restart.
- **Missing or incomplete credentials**: If required connection settings are absent or incomplete at startup, the service MUST fail startup (non-zero exit) rather than report ready without a database.
- **Credential secrecy**: Connection passwords and other secret fields MUST NOT appear in logs, traces, metrics labels, or readiness response bodies.
- **Probe cost**: The connectivity check MUST be a lightweight validation (a simple round-trip query) suitable for frequent readiness polling; it MUST NOT run migrations, heavy queries, or schema validation as part of the probe.
- **Liveness unchanged**: Database failure MUST NOT cause the liveness endpoint to fail.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST establish a PostgreSQL database connection at startup using the connection settings already provisioned for the service (host, port, database name, username, password, and engine/type as supplied by the existing secret/config).
- **FR-002**: The readiness endpoint (`GET /readyz`) MUST include a database connectivity validation as part of determining whether the service can accept inbound requests.
- **FR-003**: The database connectivity validation MUST perform a lightweight round-trip check against PostgreSQL (equivalent in intent to verifying the connection can execute a trivial query such as `SELECT 1`).
- **FR-004**: When the database connectivity validation succeeds and other existing readiness conditions are met, the readiness endpoint MUST continue to return success with the established plain-text ready body (`OK` followed by a newline, `text/plain`).
- **FR-005**: When the database connectivity validation fails for any reason (network failure, authentication failure, timeout, or query error), the readiness endpoint MUST return a "service unavailable" status.
- **FR-006**: The liveness endpoint (`GET /livez`) MUST NOT depend on database availability; its behavior MUST remain process-liveness only, consistent with the existing health-endpoint contract.
- **FR-007**: Database connection activity used by the health check MUST be instrumented with the service's existing observability stack so operators can see successful and failed connectivity checks.
- **FR-008**: Secret connection material (especially passwords) MUST NEVER be logged, traced as attribute values, emitted as metric labels, or returned in health responses.
- **FR-009**: If required database connection settings are *partially* supplied (incomplete PG* set) when the service starts, the service MUST exit with a non-zero status. Fully omitting all database settings remains allowed for unit tests / metadata-only local runs (no-DB mode); production charts inject PG* so in-cluster runs are not optional.
- **FR-010**: The database connectivity check behavior MUST be covered by automated tests for both success (database reachable) and failure (database unreachable or check error) paths affecting readiness, and for liveness remaining successful when the database check would fail.
- **FR-011**: The existing unauthenticated access, response body, content type, and public-docs exclusion contracts for `/livez` and `/readyz` MUST be preserved.

### Key Entities

- **Database connection settings**: The provisioned PostgreSQL connection parameters (host, port, database name, username, password, engine) supplied to the service via its existing secret/config mechanism.
- **Database connectivity status**: A binary operational signal derived from a lightweight round-trip check — the service can or cannot currently talk to PostgreSQL.
- **Readiness status**: The overall operational signal for accepting traffic; now depends on existing initialization readiness plus database connectivity status.
- **Liveness status**: Unchanged process-alive signal; independent of database connectivity status.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: When the database is reachable with valid credentials, readiness reports ready and the instance can enter serving rotation in development, staging, and production.
- **SC-002**: When the database is unreachable or rejects the connectivity check, readiness reports unavailable within one probe interval and the instance is removed from (or never added to) serving rotation.
- **SC-003**: When the database is unreachable, liveness continues to report success so the orchestration platform does not restart the process solely due to database unavailability.
- **SC-004**: A developer can run the existing local build/test workflow and automated tests cover ready-with-database, not-ready-without-database, and alive-despite-database-failure cases.
- **SC-005**: Operators investigating a readiness failure can identify from observability data that the database connectivity check failed, without any secret credentials appearing in that data.
- **SC-006**: Under normal conditions, a successful readiness check that includes the database validation completes quickly enough for standard Kubernetes probe intervals (on the order of hundreds of milliseconds for the check itself, not multi-second blocking work).

## Assumptions

- Database credentials are already provisioned into the service environment via the existing Kubernetes secret / ExternalSecret mechanism; this feature consumes those settings rather than provisioning infrastructure.
- The database is PostgreSQL, as indicated by the provisioned secret (`engine` and connection fields).
- Per the existing health-endpoints specification (FR-007 / FR-008), database validation belongs on **readiness**, not liveness. The user's "livez and/or readyz" ask is resolved by extending readiness only.
- The readiness response contract remains the existing plain-text `OK\n` / service-unavailable behavior; no structured multi-check health payload is introduced in this feature.
- A trivial round-trip query is sufficient validation for this feature; schema migrations, table existence, and application-level data checks are out of scope.
- Connection pooling, migration runners, and repository/data-access layers beyond what is needed for a shared connection used by the health check are out of scope unless required to establish and validate the connection.
- Telemetry follows the service's existing observability setup (including distributed tracing already used by the service); no new observability backend is introduced. Driver choice, connection library, and exact instrumentation hooks are deferred to planning.
- Target validation environments are development, staging, and production, matching Jira LFXV2-2559 acceptance criteria.
