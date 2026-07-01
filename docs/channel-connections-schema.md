# Campaign Connections — Database Schema

Database schema for storing per-project connections to marketing platforms. Each supported provider is modeled as its **own strongly-typed resource** — both in the API (distinct sub-paths and payload contracts) and in the database (one table per provider). There is intentionally **no generic `connection` abstraction with an untyped `metadata` blob**: credentials and configuration differ materially between providers (an API key is not shaped like `client_id`/`secret`/`refresh_token` is not shaped like OAuth 1.0a consumer/access pairs), and folding that variance into untyped JSON produces a weak contract that is hard to develop and validate against — especially for agents.

One project can have multiple connections of the same provider (e.g. separate LinkedIn ad accounts per sub-brand).

> **Decision, not options.** This document describes the target design. See [architecture.md](architecture.md) for the full ER model and the FGA relations that gate each endpoint; API paths are catalogued in [api-catalog.md](api-catalog.md). To avoid duplication, the endpoint tables live in the API catalog and are linked, not repeated, here.

## Design Principles

1. **One typed table per provider.** `google_ads_connections`, `linkedin_ads_connections`, etc. Provider-specific credential and config fields are first-class columns (or a typed sub-object), not `metadata`.
2. **Optimistic concurrency on every table.** Every table that maps to an LFX platform resource type carries a `version BIGINT` iterator. The DB handle implements optimistic locking: `UPDATE ... WHERE id = $1 AND version = $2`, incrementing `version` on success and returning `ErrPreconditionFailed` on mismatch. This powers the platform-idiomatic **ETag / `If-Match`** concurrency controls on PUTs. (Pattern mirrors committee-service; see [lfx-architecture-scratch/2026-05-CloudNativePG](https://github.com/linuxfoundation/lfx-architecture-scratch/tree/main/2026-05-CloudNativePG).)
3. **No cross-service FK on `project_id`.** Projects are owned by the project-service; like committee-service, `project_id` is a plain `UUID NOT NULL` (indexed), not a foreign key into a table this service owns.
4. **Application-level credential encryption.** Credentials are encrypted by the application (AES-256-GCM) using a key sourced from a Kubernetes secret via an environment variable — *not* by pgcrypto. pgcrypto is oriented toward digests/hashes; symmetric credential encryption is an application concern so the key never lives in the database.
5. **Attribution is served by Query Service.** `created_at`/`updated_at`/`created_by` audit and revision history are maintained by the Query Service on each (re)index; this schema does not attempt to reproduce a separate audit/revision store. Where a `created_by` value is retained inline for convenience, it captures the full actor (`name`, `email`, `username`) so historical records remain meaningful even after a user is removed.

## Common Columns

Every provider connection table shares this shape. Provider-specific credential/config columns are added per table (see below).

```sql
-- gen_random_uuid() is built in on PostgreSQL 13+; deployment target is PostgreSQL 16.
id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
project_id   UUID        NOT NULL,                    -- owned by project-service; no cross-service FK
label        TEXT        NOT NULL,                    -- human label, e.g. "TLF Main"
account_id   TEXT,                                    -- provider account identifier
credentials  BYTEA,                                   -- AES-256-GCM ciphertext (app-encrypted)
status       TEXT        NOT NULL DEFAULT 'active'
             CHECK (status IN ('active', 'inactive', 'error', 'deleted')),
version      BIGINT      NOT NULL DEFAULT 1,          -- optimistic-lock iterator → ETag/If-Match
created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()       -- set by application on UPDATE (no DB trigger)
```

Each table carries `idx_<provider>_project_id ON (project_id)` and, where lookups by account are needed, a non-unique `(project_id, account_id)` index (multiple connections per project are allowed).

## Per-Provider Tables

Provider-specific fields are shown as the columns that supplement the common shape. The `credentials` column always holds the encrypted blob; the *plaintext* credential shape it encrypts is documented per provider so the API contract is explicit.

### google_ads_connections
- Config columns: `login_customer_id TEXT`
- `account_id` = customer ID (e.g. `8666746580`)
- Encrypted credential shape: `{ refresh_token, client_id, client_secret, developer_token }`

### linkedin_ads_connections
- Config columns: `org_id TEXT NOT NULL`
- `account_id` = ad account ID (e.g. `538170226`)
- Encrypted credential shape: `{ access_token }`

### meta_ads_connections
- Config columns: `page_id TEXT`, `app_id TEXT`
- `account_id` = ad account ID (e.g. `act_193556282970417`)
- Encrypted credential shape: `{ access_token, app_secret }`

### reddit_ads_connections
- `account_id` = advertiser ID (e.g. `t2_gv9wtbfa`)
- Encrypted credential shape: `{ client_id, client_secret, refresh_token }`

### twitter_ads_connections
- Config columns: `funding_instrument_id TEXT`
- `account_id` = account ID (e.g. `8r7gb`)
- Encrypted credential shape: `{ consumer_key, consumer_secret, access_token, access_token_secret }`

### microsoft_ads_connections
- Config columns: `customer_id TEXT`
- `account_id` = account ID
- Encrypted credential shape: `{ client_id, client_secret, refresh_token, developer_token }`

### hubspot_connections (email)
- Config columns: `portal_id TEXT`, `sender_email TEXT`, `sender_name TEXT`, `brand_kit TEXT`
- `account_id` = list/audience ID
- Encrypted credential shape: `{ private_app_token }`

> Organic/community channels (LinkedIn organic, X organic, Bluesky, Mastodon, YouTube, Slack, Discord) follow the same one-table-per-provider pattern and will be added as those integrations land. They are out of scope for the initial paid-platform migration and are not detailed here to keep this document focused on the decided target.

## Multiple Connections Per Project

A project can hold many connections of the same provider — e.g. TLF running two LinkedIn ad accounts:

```
project: TLF
  linkedin_ads | account 538170226 | "TLF Main"  | active
  linkedin_ads | account 509430019 | "LF Events" | active
```

## Current Account Inventory

Non-secret platform account IDs currently configured in the Express BFF, retained here as a migration reference. Each provider has a different tenancy model — some share one account across foundations (campaigns separated by naming convention), others use separate accounts per foundation.

**Single shared account (all foundations):**
- **Google Ads** — Manager `9746983954`, Customer `8666746580`; projects distinguished by campaign naming convention, not separate accounts.
- **HubSpot** — single org-wide private app token; campaigns matched to projects by name.

**Separate accounts per foundation:**
- **LinkedIn Ads** — TLF `538170226` (org `208777`), LF Events `509430019`
- **Meta Ads** — LF Core `act_193556282970417` (page `41911143546`)
- **Reddit Ads** — TLF `t2_gv9wtbfa`
- **X/Twitter Ads** — LF Events `8r7gb`

## API

Connection endpoints are nested under `/projects/{projectId}/…` and are strongly typed per provider (e.g. `POST /projects/{projectId}/google-ads-connections`). The authoritative endpoint list, gating FGA relations, and payload shapes are in [api-catalog.md](api-catalog.md).

### Response Shape

Credentials are never returned. The response exposes `has_credentials: boolean` in place of the encrypted column, and carries the `version` as an ETag.

```json
{
  "id": "uuid",
  "project_id": "uuid",
  "label": "TLF Main",
  "account_id": "538170226",
  "org_id": "208777",
  "has_credentials": true,
  "status": "active",
  "version": 3
}
```

### set_credential vs. update

Rotating a credential is exposed as a dedicated `set_credential` action, not a `rotate` endpoint. "Rotate" would imply the service atomically generates a new secret, swaps it upstream at the provider, *and* stores it — which the ad platforms do not support. `set_credential` simply replaces the stored (encrypted) credential and is split out from the generic `PUT` update so that credential replacement and metadata edits are independently permissioned and audited.

## Security

- Credentials encrypted at rest with **application-level AES-256-GCM**; key sourced from a Kubernetes secret via env var (referenced by the Helm chart). Not pgcrypto.
- Credentials never returned in API responses (replaced with `has_credentials`).
- `test` action verifies credentials against the provider without exposing them.
- Change history and attribution are served by the Query Service, which indexes each connection on write.
