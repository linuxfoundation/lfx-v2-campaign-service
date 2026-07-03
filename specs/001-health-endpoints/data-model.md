# Phase 1 Data Model: Health Endpoints (Readyz & Livez)

This feature introduces no persisted entities and no request/response DTOs beyond a plain-text acknowledgment. The "model" is limited to two transient operational signals and one internal predicate.

## Operational signals

### Liveness signal

- **Meaning**: The process is running and its HTTP handler can respond.
- **Representation on the wire**: `200 OK`, `Content-Type: text/plain`, body = `OK\n`.
- **Inputs**: none.
- **Derivation**: Constant while the process serves requests. Absence of a response (timeout/refused) is itself the negative signal that drives a restart.
- **State transitions**: `alive` (responding) → `absent` (process hung/terminated; no response). There is no in-band "not alive" response body.

### Readiness signal

- **Meaning**: The service can accept inbound requests right now.
- **Representation on the wire**:
  - Ready → `200 OK`, `Content-Type: text/plain`, body = `OK\n`.
  - Not ready → `503 Service Unavailable` (Goa `ServiceUnavailable` error).
- **Inputs**: none (server-side determination only).
- **Derivation**: `ServiceReady()` predicate (see below).
- **State transitions**: `not-ready` (startup/initializing) → `ready` (initialized). May return to `not-ready` in future versions when a wired dependency becomes unavailable; the contract is unchanged when that happens.

## Internal predicate

### `ServiceReady() bool`

- **Location**: `internal/service`.
- **v1 behavior**: returns `true` once the service value is constructed (no external dependencies are wired).
- **Extension point (FR-008)**: future dependency checks (e.g., NATS connection healthy, data store reachable) are combined with logical AND; adding them does not change the endpoint paths, status codes, or body contract.

## Error type

### `ServiceUnavailableError`

- **Purpose**: Goa error result mapped by `readyz` to HTTP `503`.
- **Fields** (mirroring the reference service): `Code` (string), `Message` (string).
- **Usage**: returned by `Readyz` when `ServiceReady()` is false; declared in the `design/` package and referenced by the `readyz` method's `Error(...)`/`Response(...)` mapping.

## Relationships

```text
HTTP GET /livez  ──▶ Livez()  ──▶ constant OK\n
HTTP GET /readyz ──▶ Readyz() ──▶ ServiceReady()? OK\n : 503 ServiceUnavailableError
                                        │
                                        └─(v1) true;  (future) AND of dependency checks
```
