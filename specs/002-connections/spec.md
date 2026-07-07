# Feature Specification: Platform Connections (Singleton per Provider)

**Feature Branch**: `feat/LFXV2-2554-connection-design`

**Created**: 2026-07-07

**Status**: Draft

**Input**: Define the API contract for managing a project's connection to a marketing ad platform. Aligned to the approved architecture docs (PR #2, `docs/api-catalog.md` and `docs/channel-connections-schema.md`): connections are **singleton per provider per project** and strongly typed per provider.

## Clarifications

### Session 2026-07-07

- Q: Can a project hold more than one connection of the same provider? → A: No. A connection is a **singleton per provider per project**. Account multiplicity across the Linux Foundation lives at the project level (CNCF, OpenSearch, TLF are separate projects). The API therefore has no `{id}` in the path and no List endpoint — the provider name is the identity within the project.
- Q: Is credential rotation in scope? → A: No `rotate`. Credential replacement is a dedicated `set-credential` action, split out from the config `PUT` so it can be independently permissioned and audited. The service does not generate or swap secrets upstream.
- Q: Is the persistence/database layer in scope for this feature? → A: No. This feature is the **API contract only** (Goa design + generated code + stub service methods that compile and serve). The database schema, encryption, and repository land in LFXV2-2555/2556.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Establish a platform connection (Priority: P1)

As a campaign manager for a project, I need to register my project's ad-platform account (e.g. Google Ads) with the campaign service so that the service can create and monitor campaigns on my behalf.

**Why this priority**: Without a connection, no downstream campaign work is possible. It is the entry point of the whole domain and the thinnest independently shippable slice.

**Independent Test**: Issue a create request for a provider on a project and confirm the service accepts the typed payload and returns a created resource with an ETag; a second create for the same project+provider is rejected as a conflict.

**Acceptance Scenarios**:

1. **Given** a project with no Google Ads connection, **When** a campaign manager creates one with a valid typed payload, **Then** the service responds created, returns the connection with credentials redacted, and issues an ETag.
2. **Given** a project that already has a Google Ads connection, **When** a campaign manager creates another, **Then** the service responds with a conflict (the connection is a singleton).
3. **Given** a caller without the `campaign_manager` relation on the project, **When** any connection endpoint is requested, **Then** the request is rejected by the gateway before reaching the service.

---

### User Story 2 - Read and safely replace connection config (Priority: P1)

As a campaign manager, I need to read my project's connection and replace its configuration without losing a concurrent update, so that config edits are safe and credentials are never exposed.

**Independent Test**: Read the connection (confirm credentials are redacted and an ETag is returned), then replace the config with the current ETag in `If-Match` and confirm success; replace with a stale ETag and confirm it is rejected.

**Acceptance Scenarios**:

1. **Given** an existing connection, **When** a campaign manager reads it, **Then** credentials are redacted (a `has_credentials` flag is returned instead) and an ETag is present.
2. **Given** a caller holding the current ETag, **When** they replace the config with `If-Match`, **Then** the service applies the change and returns a new ETag.
3. **Given** a caller holding a stale ETag, **When** they replace the config, **Then** the service rejects it (precondition failed); a missing `If-Match` is rejected as precondition required.

---

### User Story 3 - Manage credentials and verify them (Priority: P2)

As a campaign manager, I need to set the stored credential separately from config and verify it against the provider, so credential handling is isolated and I can confirm the connection actually works.

**Independent Test**: Call set-credential with a new credential and confirm success without exposing it; call test and confirm it reports whether the credential authenticates upstream.

**Acceptance Scenarios**:

1. **Given** an existing connection, **When** a campaign manager sets a new credential, **Then** the service stores it (encrypted) and never echoes it back.
2. **Given** an existing connection, **When** a campaign manager triggers a test, **Then** the service reports whether the credential authenticates against the provider, without exposing the credential.
3. **Given** an existing connection, **When** a campaign manager deletes it, **Then** the service soft-deletes it and subsequent reads report it as not found.

## Requirements *(mandatory)*

- **FR-001**: The service MUST expose, per provider, exactly one connection resource per project addressed as `/projects/{projectId}/connection-{provider}` (no service-generated id in the path, no List endpoint).
- **FR-002**: Providers MUST be: `google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`, `hubspot`. Each MUST carry a strongly-typed payload; a generic untyped `metadata` blob MUST NOT be used.
- **FR-003**: Create MUST reject a second connection for the same project+provider with a conflict.
- **FR-004**: Read MUST redact credentials (return a `has_credentials` flag) and return an ETag.
- **FR-005**: Replace (`PUT`) MUST require an `If-Match` header; a mismatch MUST fail precondition, a missing header MUST fail as precondition-required. Replace MUST NOT set credentials.
- **FR-006**: `set-credential` MUST be a separate action that replaces the stored credential and never echoes it.
- **FR-007**: `test` MUST verify the credential against the provider without exposing it.
- **FR-008**: Delete MUST soft-delete.
- **FR-009**: Every endpoint MUST be secured by the Heimdall-issued JWT (audience = this service) and gated on the `campaign_manager` relation at the gateway.
- **FR-010**: This feature delivers the API contract and a compiling stub service only; persistence, encryption, and repository are out of scope (LFXV2-2555/2556).

## Out of Scope

- Database schema, migrations, credential encryption, SQL repository (LFXV2-2555).
- Service business logic wiring to Postgres (LFXV2-2556).
- Brief/campaign endpoints and orchestration (LFXV2-2557 / LFXV2-2626).
