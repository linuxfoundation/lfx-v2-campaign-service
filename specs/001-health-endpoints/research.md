# Phase 0 Research: Health Endpoints (Readyz & Livez)

All spec clarifications were resolved during `/speckit-clarify` and `/speckit-checklist`, so there are no open `NEEDS CLARIFICATION` items. This document records the technical decisions (derived from the reference `lfx-v2-project-service`, local folder `../lfx-v2-project-service.git.main`) that shape the design.

## D1. Endpoint definition mechanism

- **Decision**: Define `livez` and `readyz` as Goa `Method`s on the service in the `design/` package, and generate the HTTP layer with `make apigen` (`goa gen .../design`). Do not hand-mount ad-hoc `http.HandlerFunc`s.
- **Rationale**: The scaffold's `cmd/campaign-service/server.go` already imports the generated packages and mounts a Goa muxer; the reference service defines the same two endpoints this way. Consistency + the scaffold literally won't compile until `gen/` exists.
- **Alternatives considered**: Mounting plain handlers on the muxer (rejected: diverges from the reference, bypasses generated OpenAPI/type safety, leaves the scaffold's generated imports unsatisfied).

## D2. Goa service & API names (package-name compatibility)

- **Decision**: `API("lfx-v2-campaign-service", …)` and `Service("lfx-v2-campaign-service-svc", …)`.
- **Rationale**: `server.go` imports `gen/lfx_v2_campaign_service_svc` (aliased `svc`) and `gen/http/lfx_v2_campaign_service_svc/server` (aliased `svcsvr`). Goa derives the generated package directory from the service name by lowercasing and replacing non-alphanumerics with underscores, so the service name must be `lfx-v2-campaign-service-svc` to yield `lfx_v2_campaign_service_svc`.
- **Alternatives considered**: A cleaner service name (e.g., `campaign-service`) with an edit to `server.go` imports (rejected: unnecessary churn against the given scaffold; the scaffold's import names are the contract to honor).

## D3. Endpoint contract (matches reference exactly)

- **Decision**:
  - `livez`: `GET /livez`; `Result(Bytes)`; `Response(StatusOK)` with `ContentType("text/plain")`; body `OK\n`; `Meta("swagger:generate", "false")`; no security.
  - `readyz`: `GET /readyz`; `Result(Bytes)`; `Response(StatusOK)` `text/plain`; `Error("ServiceUnavailable", ServiceUnavailableError)` mapped to `Response("ServiceUnavailable", StatusServiceUnavailable)`; `Meta("swagger:generate", "false")`; no security.
- **Rationale**: Directly mirrors `api/project/v1/design/project.go` (`readyz`/`livez`) and `service_endpoint.go` which return `[]byte("OK\n")` / `503`. Satisfies FR-001..FR-006.
- **Alternatives considered**: JSON health payloads (rejected: spec fixes body to `OK\n`, no structured payload for v1).

## D4. Generated server signature / OpenAPI file servers

- **Decision**: The design MUST declare four `Files()` directives serving the generated OpenAPI documents (`openapi.json`, `openapi.yaml`, `openapi3.json`, `openapi3.yaml`).
- **Rationale**: The scaffold's `svcsvr.New(...)` call passes `koHTTPDir` **four** times as trailing `http.FileSystem` arguments. Goa emits one trailing file-system parameter per `Files()` directive, so the design must declare exactly four to match the existing call. The reference declares the same four (under a `/_projects/...` prefix); the campaign service will use an analogous prefix (e.g., `/_campaigns/...`).
- **Alternatives considered**: Zero `Files()` + editing `server.go` to drop the args (rejected: more churn than adding the standard OpenAPI file servers that the platform expects anyway).

## D5. Readiness predicate & service wiring

- **Decision**: Implement `svc.Service` with a concrete type. `Livez` always returns `OK\n`. `Readyz` returns `OK\n` when `ServiceReady()` is true, else the `ServiceUnavailable` error. `ServiceReady()` lives in `internal/service` and, because no external dependencies are wired, returns `true` once the service is constructed — structured so future dependency checks (NATS, stores) can be AND-ed in without changing the endpoint contract (FR-008).
- **Rationale**: Mirrors the reference's `ProjectsAPI.Readyz` + `service.ServiceReady()` split; keeps the extension point explicit while staying trivially simple for v1.
- **Alternatives considered**: Hardcoding `true` inline in `Readyz` (rejected: loses the FR-008 extension seam and the reference's separation of concerns).

## D6. Self-termination on non-recoverable errors (FR-009)

- **Decision**: Rely on the existing `main.go` behavior — startup failures (e.g., `ListenAndServe` error, failed port bind) propagate and cause `os.Exit(1)`. No new runtime self-termination logic is added because no runtime dependencies exist yet.
- **Rationale**: `main.go` already exits non-zero on server error; this is the only non-recoverable condition in v1 per the clarified FR-009.
- **Alternatives considered**: A watchdog/health-manager goroutine (rejected: YAGNI for a dependency-less service).

## D7. Local build workflow (Makefile)

- **Decision**: Add a `fmt` target (`go fmt ./...` + `gofmt -s -w`), a format `check` target (`gofmt -l`, non-zero on diff, for CI), and a production-build target that produces a static/release Linux binary (CGO disabled, `-ldflags` version metadata) distinct from the local `build` target and the container image.
- **Rationale**: FR-011; matches the reference `Makefile` (`fmt`, `check`, build with `LDFLAGS`). The existing `build`, `test`, `lint`, `apigen`, `deps` targets are reused as-is.
- **Alternatives considered**: Relying on `lint` to enforce formatting (rejected by clarification — a dedicated format step was requested).

## D8. CI container-publish parity (FR-015)

- **Decision**: Repair all three scaffolded workflows: `ko-build-branch.yaml` (remove `if: false`; populate `HEAD_REF`, `VERSION`, image tags), `ko-build-main.yaml` (version/tag), and `ko-build-tag.yaml` (fix `COSIGN_VERSION`/`cosign-release`, the `NaN@NaN` sign target, and version/digest wiring). All images build for `linux/amd64,linux/arm64` with `--sbom spdx`; the tag workflow signs with Cosign and generates SLSA provenance.
- **Rationale**: FR-015 (full parity). The campaign `ko-build-tag.yaml` already scaffolds Cosign + SLSA + SBOM steps but with placeholder values (`cosign-release: ""`, `cosign sign --yes 'NaN@NaN'`); repairing is the same work as parity.
- **Alternatives considered**: Image-only (no signing/provenance) — rejected by CHK031 decision (full parity).

## D9. Kubernetes probe configuration

- **Decision**: No chart change expected. `charts/.../templates/deployment.yaml` already declares `livenessProbe → /livez`, `readinessProbe → /readyz`, and `startupProbe → /readyz` against the `web` port.
- **Rationale**: The work makes the endpoints exist and respond; probe wiring pre-exists (spec Assumptions, FR-012). A chart-version bump in `Chart.yaml` applies only if chart manifests change (they are not expected to).
- **Alternatives considered**: Redefining probe thresholds (rejected: out of scope; reuse existing).

## D10. Testing approach

- **Decision**: Table-driven unit tests for `Livez` (always `OK\n`) and `Readyz` (ready → `OK\n`; not-ready → `ServiceUnavailable`), mirroring the reference `service_endpoint_test.go`. Run under `make test` (`go test -race -coverprofile`).
- **Rationale**: FR-010, SC-001. Keeps parity with the reference's proven tests.
- **Alternatives considered**: HTTP-level integration tests through the generated server (optional future add; unit tests on the service methods are sufficient for v1 acceptance).
