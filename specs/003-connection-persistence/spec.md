# Feature Specification: Connection Persistence

**Feature Branch**: `feat/LFXV2-2555-connection-persistence`

**Created**: 2026-07-07

**Status**: Draft

**Input**: Domain models, port interfaces, PostgreSQL pool + migrations, and the SQL repository for singleton per-provider connections, with application-level AES-256-GCM credential encryption. Aligned to `docs/channel-connections-schema.md` (PR #2).

## Scope

This feature is the **persistence layer** for connections. It does not wire the
repository into the Goa service handlers (that is LFXV2-2556) and does not
require a running database to build or unit-test — repository integration tests
need CloudNativePG (LFXV2-2559).

## Requirements *(mandatory)*

- **FR-001**: Define a provider-agnostic `Connection` domain model plus a
  `Provider` enum that maps each provider to its Postgres table.
- **FR-002**: Provide `ConnectionReader` / `ConnectionWriter` port interfaces and
  domain sentinel errors (`ErrNotFound`, `ErrConflict`, `ErrPreconditionFailed`).
- **FR-003**: Ship migrations creating one typed table per provider
  (`google_ads_connections`, …) with the common columns, `UNIQUE(project_id)`
  (singleton), inline `created_by`/`updated_by`, and provider-specific columns.
- **FR-004**: Provide a pgx/v5 connection pool with a readiness check and an
  embedded golang-migrate runner.
- **FR-005**: Implement a pgx-backed `ConnectionRepo` supporting Get, Create
  (409 on singleton violation), Update (optimistic concurrency via `version` +
  `If-Match`; distinguishes missing from stale), SetCredential (isolated from
  config update), and soft Delete.
- **FR-006**: Encrypt credentials at the application layer with AES-256-GCM
  (random nonce per message); the key comes from a k8s secret via
  `CREDENTIAL_ENCRYPTION_KEY` and is never stored in the database.
- **FR-007**: Add `DATABASE_URL` and `CREDENTIAL_ENCRYPTION_KEY` to config.

## Out of Scope

- Wiring the repo/encryptor into the Goa handlers (LFXV2-2556).
- Database provisioning (LFXV2-2559).
- Brief/campaign persistence (LFXV2-2557).

## Testing Notes

- Encryptor round-trip, provider→table mapping, and DSN rewriting are covered by
  unit tests (no DB).
- Repository CRUD + optimistic concurrency require a live Postgres; those
  integration tests land with the database provisioning (LFXV2-2559) and the
  handler wiring (LFXV2-2556).
