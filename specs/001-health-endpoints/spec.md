# Feature Specification: Health Endpoints (Readyz & Livez)

**Feature Branch**: `feat/add-health-endpoints`

**Created**: 2026-07-01

**Status**: Draft

**Input**: User description: "we want to implement the initial Readyz and Livez endpoint. The end-to-end validation will be for me to build, test, format, and test/validate the readyz and livez health endpoints locally. From there, I want to setup the build system to ensure the changes can be deployed into the k8s cluster. Please review the implementation from https://github.com/linuxfoundation/lfx-v2-project-service (local folder ../lfx-v2-project-service.git.main) to understand how the API should be setup, configured, and managed."

## Clarifications

### Session 2026-07-01

- Q: Is making the service actually compile and serve (create the Goa design, generate code, implement the endpoint methods, and wire the container) in scope, given it is currently a non-compiling scaffold? → A: Full bootstrap in scope — create the API design package with `readyz`/`livez`, generate the API code, implement the service methods, and wire the container/service so the app builds and serves the two endpoints end-to-end.
- Q: Which local build-workflow steps must this feature guarantee, given there is no `format` step today? → A: Guarantee `build`, `test`, `format` (add a new formatting target using standard Go formatting), and `lint`; additionally add an explicit production-build target (a static/release binary) distinct from the container image.
- Q: How far should CI build & packaging scope go, given the container-publish workflows are scaffolded but non-functional (`if: false`, empty values)? → A: Enable and fix all container-publish workflows (branch, main, and tag) so branch, main, and release/tag packaging all work, matching the project service.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Liveness signal for the running service (Priority: P1)

As the platform operator (and the orchestration platform acting on my behalf), I need a lightweight endpoint that confirms the campaign service process is alive so that a stuck or non-recoverable instance can be automatically restarted.

**Why this priority**: Liveness is the most fundamental health signal. Without it, a hung process is never recycled and the service silently stops serving traffic. It has no dependencies on any other health logic, so it is the smallest independently shippable slice.

**Independent Test**: Start the service locally, issue a request to the liveness endpoint, and confirm it returns a success response with a plain-text body while the process is running. This alone delivers value: the orchestration platform can restart hung instances.

**Acceptance Scenarios**:

1. **Given** the service process is running, **When** a client requests the liveness endpoint, **Then** the service responds with a success status and a plain-text body indicating it is alive.
2. **Given** the service process is running, **When** the liveness endpoint is requested without any authentication credentials, **Then** the service still responds with success (the endpoint is unauthenticated).
3. **Given** the service is shutting down, **When** the liveness endpoint stops responding, **Then** the orchestration platform can detect the failure and act on it.

---

### User Story 2 - Readiness signal for inbound traffic (Priority: P1)

As the platform operator (and the orchestration platform acting on my behalf), I need an endpoint that reports whether the service is ready to accept inbound requests so that traffic is only routed to instances that can actually serve it, and so that new instances are not added to rotation before startup completes.

**Why this priority**: Readiness gating prevents user-facing errors during rollouts, restarts, and startup. It is separately testable from liveness and is required for safe deployment into the cluster.

**Independent Test**: Start the service locally, request the readiness endpoint, and confirm it returns success once the service has finished initializing; confirm it reports unavailable when the service cannot yet serve requests.

**Acceptance Scenarios**:

1. **Given** the service has completed initialization and its wired dependencies are available, **When** a client requests the readiness endpoint, **Then** the service responds with a success status and a plain-text body indicating it is ready.
2. **Given** the service is not yet able to serve inbound requests, **When** a client requests the readiness endpoint, **Then** the service responds with a "service unavailable" status.
3. **Given** the service is running, **When** the readiness endpoint is requested without any authentication credentials, **Then** the service responds without requiring authentication.

---

### User Story 3 - Build, validate, and deploy into the cluster (Priority: P2)

As the developer, I need to build, format, lint, and test the service locally to validate the two health endpoints, and then produce a deployable artifact whose health probes work inside the Kubernetes cluster, so that the change can be shipped through the normal build system.

**Why this priority**: The endpoints only deliver operational value once they are reachable by the cluster's probes and the artifact can be deployed. This depends on Stories 1 and 2 existing first, hence P2.

**Independent Test**: Run the local build/format/lint/test workflow to completion with passing results, produce the container artifact, deploy it to the local cluster, and confirm the platform's liveness and readiness probes report the instance as healthy and ready.

**Acceptance Scenarios**:

1. **Given** a clean checkout, **When** the developer runs the local build, format, lint, and test workflow, **Then** all steps complete successfully and the health endpoint behavior is validated by automated tests.
2. **Given** a successfully built artifact, **When** it is deployed to the cluster, **Then** the liveness and readiness probes configured for the deployment succeed and the instance becomes ready to receive traffic.
3. **Given** the service is deployed, **When** the readiness probe fails, **Then** the instance is removed from serving rotation until it reports ready again.

---

### Edge Cases

- **Unauthenticated access**: Both endpoints must be reachable without credentials, because probes run without user identity. They must not be gated behind the platform's authentication/authorization layer.
- **Readiness during startup**: Before initialization completes, the readiness endpoint must report unavailable rather than success, so traffic is not routed prematurely.
- **Liveness during unrecoverable failure**: Because liveness is used to trigger restarts, the service is responsible for self-terminating on non-recoverable errors rather than reporting alive indefinitely.
- **Endpoints excluded from public API docs**: Health endpoints are operational, not part of the public product API surface, and should not appear in generated public API documentation.
- **Frequent, low-cost polling**: Probes are called on a short interval, so each response must be cheap and must not perform expensive downstream work.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST expose a liveness endpoint at `GET /livez` that returns a success status with the response body `OK` followed by a newline while the process is running and able to respond.
- **FR-002**: The service MUST expose a readiness endpoint at `GET /readyz` that returns a success status with the response body `OK` followed by a newline when the service is able to accept inbound requests.
- **FR-003**: The readiness endpoint MUST return a "service unavailable" status when the service is not able to accept inbound requests.
- **FR-004**: Both endpoints MUST be accessible without authentication or authorization.
- **FR-005**: Both endpoints MUST respond with a `text/plain` content type and a response body of the literal string `OK\n` (the characters `OK` followed by a single newline), consistent with the reference project service.
- **FR-006**: The endpoints MUST be excluded from the generated public API documentation surface.
- **FR-007**: The liveness endpoint MUST NOT depend on the availability of external dependencies; it reflects only whether the process itself is alive.
- **FR-008**: The readiness check MUST reflect whether the service and its currently wired dependencies are initialized and able to serve requests, and MUST be extensible so future dependencies (e.g., messaging, data stores) can be incorporated into the readiness determination.
- **FR-009**: The service MUST self-terminate (exit with a non-zero status) on non-recoverable errors so that liveness-driven restarts are meaningful. For this initial version — in which no runtime dependencies are wired — the only such condition is a startup failure (e.g., inability to bind the listen port), which MUST cause a non-zero exit. This behavior is retained as an extension point for future runtime dependencies.
- **FR-010**: The endpoint behavior MUST be covered by automated tests that verify the ready, not-ready, and alive responses.
- **FR-011**: The project MUST provide repeatable local workflow steps to build, format, lint, and test the service, and these steps MUST pass for the change. This MUST include a dedicated formatting step using standard Go formatting — `go fmt ./...` together with `gofmt -s -w`, plus a `gofmt -l` verification/check step usable in CI — added as a new target since none exists today, and a dedicated production-build step that produces a static/release binary distinct from the container image.
- **FR-012**: The service MUST be packaged into a deployable artifact and its deployment configuration MUST wire the cluster liveness and readiness probes to the `/livez` and `/readyz` endpoints respectively.
- **FR-013**: The API design, generated code, service implementation, and deployment configuration MUST follow the conventions established by the reference project service so the campaign service remains consistent with the platform.
- **FR-014**: The service MUST be brought from its current non-compiling scaffold state to a compiling, runnable state as part of this feature: the API design package (including the `readyz`/`livez` definitions) MUST be created, the API code MUST be generated, the endpoint methods MUST be implemented, and the dependency container/service MUST be wired so the entry point boots and the HTTP router serves the two endpoints end-to-end.
- **FR-015**: The continuous integration container-publish workflows for branch, main, and release/tag MUST be enabled and corrected (they are currently scaffolded but non-functional — disabled and containing placeholder/empty version, tag, and digest values) so that all three produce working, deployable container images. The release/tag workflow MUST reach full parity with the reference project service, including Cosign artifact signing, SLSA provenance generation, and SBOM (SPDX) production; these steps are already scaffolded with placeholder values and MUST be repaired rather than removed or deferred. All published images MUST target the `linux/amd64` and `linux/arm64` platforms, matching the reference project service.

### Key Entities *(include if feature involves data)*

- **Liveness status**: A binary operational signal — the process is alive and responding. Carries no payload beyond a plain-text acknowledgment.
- **Readiness status**: An operational signal indicating whether the service can currently accept inbound requests, derived from the initialization state of the service and its wired dependencies.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A developer can run the full local build/format/lint/test workflow from a clean checkout and it completes successfully with the health endpoints validated by automated tests.
- **SC-002**: When the service is running, a request to the liveness endpoint returns a success response in under 100 ms without requiring authentication.
- **SC-003**: When the service has completed startup, a request to the readiness endpoint returns success; before startup completes it returns "service unavailable".
- **SC-004**: After the artifact is deployed to the cluster, both the liveness and readiness probes report healthy and the instance reaches a ready/serving state without manual intervention.
- **SC-005**: A newly deployed instance is not added to serving rotation until its readiness endpoint reports ready.

## Assumptions

- The service uses the same API design + code-generation toolchain and layered structure as the reference project service, and the two health endpoints are added to that design rather than mounted as ad-hoc handlers.
- Because the campaign service currently has no external runtime dependencies wired in (the dependency container is a scaffold), the initial readiness check reports ready once the service has initialized; the check is structured so additional dependency checks can be added later without changing the endpoint contract.
- The existing Helm deployment already declares liveness/readiness/startup probes pointing at `/livez` and `/readyz`; the work makes those endpoints exist and respond correctly rather than redefining probe configuration.
- The response body is a defined contract — the literal `OK\n` with a `text/plain` content type (consistent with the reference service, see FR-005) — not merely an example; no structured health payload is required for this initial version.
- The target cluster for validation is the developer's local Kubernetes environment used across LFX platform services.
- "Build system setup" scope covers: the local build/format/lint/test workflow (including the newly added formatting and production-build targets), and enabling/fixing the existing (scaffolded) branch, main, and release/tag container-publish CI workflows so they produce deployable images. The release/tag workflow already scaffolds Cosign signing, SLSA provenance, and SBOM steps (currently with placeholder values); these MUST be repaired to full working parity with the reference project service rather than removed or deferred. No net-new pipeline files beyond the existing scaffolded workflows are required.
