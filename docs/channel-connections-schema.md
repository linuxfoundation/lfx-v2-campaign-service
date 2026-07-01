# Campaign Connections — Database Schema

Database schema for storing per-project connections to marketing platforms. Each supported provider is modeled as its **own strongly-typed resource** — both in the API (distinct sub-paths and payload contracts) and in the database (one table per provider). There is intentionally **no generic `connection` abstraction with an untyped `metadata` blob**: credentials and configuration differ materially between providers (an API key is not shaped like `client_id`/`secret`/`refresh_token` is not shaped like OAuth 1.0a consumer/access pairs), and folding that variance into untyped JSON produces a weak contract that is hard to develop and validate against — especially for agents.

One project can have multiple connections of the same provider (e.g. separate LinkedIn ad accounts per sub-brand).

> **Decision, not options.** This document describes the target design. See [architecture.md](architecture.md) for the full ER model and the FGA relations that gate each endpoint; API paths are catalogued in [api-catalog.md](api-catalog.md). To avoid duplication, the endpoint tables live in the API catalog and are linked, not repeated, here.

## Design Principles

1. **One typed table per provider.** `google_ads_connections`, `linkedin_ads_connections`, etc. Provider-specific credential and config fields are first-class columns (or a typed sub-object), not `metadata`.
2. **Optimistic concurrency on every table.** Every table that maps to an LFX platform resource type carries a `version BIGINT` iterator. The DB handle implements optimistic locking: `UPDATE ... WHERE id = $1 AND version = $2`, incrementing `version` on success and returning `ErrPreconditionFailed` on mismatch. This powers the platform-idiomatic **ETag / `If-Match`** concurrency controls on PUTs. (Pattern mirrors committee-service; see [lfx-architecture-scratch/2026-05-CloudNativePG](https://github.com/linuxfoundation/lfx-architecture-scratch/tree/main/2026-05-CloudNativePG).)
3. **No cross-service FK on `project_id`.** Projects are owned by the project-service; like committee-service, `project_id` is a plain `UUID NOT NULL` (indexed), not a foreign key into a table this service owns.
4. **Application-level credential encryption.** Credentials are encrypted by the application (AES-256-GCM) using a key sourced from a Kubernetes secret via an environment variable — *not* by pgcrypto. pgcrypto can do symmetric encryption (`pgp_sym_encrypt`), but doing it in the database would require the key to be present at the DB layer; encrypting in the application keeps the key out of the database entirely (it lives only in the app's k8s secret). That key-custody boundary — not any capability gap in pgcrypto — is the reason for app-level encryption.
5. **Attribution is served by Query Service.** `created_at`/`updated_at`/`created_by` audit and revision history are maintained by the Query Service on each (re)index; this schema does not attempt to reproduce a separate audit/revision store. Where a `created_by` value is retained inline for convenience, it captures the full actor (`name`, `email`, `username`) so historical records remain meaningful even after a user is removed.

## Per-Provider Tables

Each provider gets its own table. To avoid repeating nine identical columns seven times, the shared columns are defined **once** below as a reusable fragment, and each provider table is shown as **the common fragment plus its provider-specific columns**. `account_id` and `label` are common (present on every table); the fragment below is the full base. The `credentials` column holds the app-encrypted blob; the *plaintext* JSON shape it encrypts is documented above each table so the API contract is explicit.

**Common columns** (every connection table begins with these):

```sql
-- Reusable base for every *_connections table.
-- gen_random_uuid() is in PostgreSQL core since v13 (no pgcrypto extension required); target: PG 16.
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL,                 -- owned by project-service; no cross-service FK
    label        TEXT        NOT NULL,                 -- human label, e.g. "TLF Main"
    account_id   TEXT        NOT NULL,                 -- provider account identifier
    credentials  BYTEA,                                -- AES-256-GCM ciphertext (app-encrypted)
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','inactive','error','deleted')),
    version      BIGINT      NOT NULL DEFAULT 1,       -- optimistic lock → ETag/If-Match
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()    -- set by application on UPDATE (no DB trigger)
```

Each table also gets `CREATE INDEX idx_<table>_project_id ON <table> (project_id)` and, where lookups by account are needed, a non-unique `(project_id, account_id)` index (multiple connections per project are allowed). `google_ads_connections` is shown in full as the worked example; the rest list only the provider-specific columns that extend the common fragment.

### google_ads_connections

Encrypted credential shape: `{ refresh_token, client_id, client_secret, developer_token }`

```sql
CREATE TABLE google_ads_connections (
    -- common columns (see above): id, project_id, label, account_id, credentials, status, version, created_at, updated_at
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id         UUID        NOT NULL,
    label              TEXT        NOT NULL,
    account_id         TEXT        NOT NULL,            -- customer ID, e.g. 8666746580
    credentials        BYTEA,
    status             TEXT        NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active','inactive','error','deleted')),
    version            BIGINT      NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- provider-specific:
    login_customer_id  TEXT                             -- manager account for API access
);
CREATE INDEX idx_google_ads_connections_project_id ON google_ads_connections (project_id);
CREATE INDEX idx_google_ads_connections_account    ON google_ads_connections (project_id, account_id);
```

### linkedin_ads_connections

Common columns + provider-specific:

| Column | Type | Notes |
|--------|------|-------|
| `org_id` | `TEXT NOT NULL` | LinkedIn organization URN id, e.g. `208777` |

`account_id` = ad account ID (e.g. `538170226`). Encrypted credential shape: `{ access_token }`.

### meta_ads_connections

Common columns + provider-specific:

| Column | Type | Notes |
|--------|------|-------|
| `page_id` | `TEXT` | Facebook page ID |
| `app_id` | `TEXT` | Meta app ID |

`account_id` = ad account ID (e.g. `act_193556282970417`). Encrypted credential shape: `{ access_token, app_secret }`.

### reddit_ads_connections

Common columns only (no provider-specific columns). `account_id` = advertiser ID (e.g. `t2_gv9wtbfa`). Encrypted credential shape: `{ client_id, client_secret, refresh_token }`.

### twitter_ads_connections

Common columns + provider-specific:

| Column | Type | Notes |
|--------|------|-------|
| `funding_instrument_id` | `TEXT` | Funding instrument for the ad account |

`account_id` = account ID (e.g. `8r7gb`). Encrypted credential shape: `{ consumer_key, consumer_secret, access_token, access_token_secret }` (OAuth 1.0a).

### microsoft_ads_connections

Common columns + provider-specific:

| Column | Type | Notes |
|--------|------|-------|
| `customer_id` | `TEXT` | Microsoft Advertising customer ID |

Encrypted credential shape: `{ client_id, client_secret, refresh_token, developer_token }`.

### hubspot_connections (email)

Common columns + provider-specific:

| Column | Type | Notes |
|--------|------|-------|
| `portal_id` | `TEXT` | HubSpot portal/account ID |
| `sender_email` | `TEXT` | Default sender address |
| `sender_name` | `TEXT` | Default sender name |
| `brand_kit` | `TEXT` | Per-project brand kit selector |

`account_id` = list/audience ID. Encrypted credential shape: `{ private_app_token }`.

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

## Campaign Tables

Beyond connections, the service persists briefs and campaigns. Both carry the `version` iterator (ETag/If-Match). A **brief is the funnel unit** and holds the program type; a **brief is shared across channels**, so one brief has many `campaigns` rows (one per channel/platform), all sharing `brief_id`.

### campaign_briefs

```sql
CREATE TABLE campaign_briefs (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID        NOT NULL,
    program_type  TEXT        NOT NULL                 -- funnel context
                  CHECK (program_type IN ('events','education','membership')),
    event_slug    TEXT        NOT NULL,                -- UNIQUE with project_id
    url           TEXT,
    platforms     JSONB,                               -- selected channels for this brief
    event_details JSONB,
    copy          JSONB,
    keywords      JSONB,
    targeting     JSONB,
    status        TEXT        NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','approved','archived')),
    version       BIGINT      NOT NULL DEFAULT 1,      -- ETag/If-Match
    approved_by   JSONB,                               -- {name,email,username} or null
    approved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, event_slug)
);
CREATE INDEX idx_campaign_briefs_project_id ON campaign_briefs (project_id);
```

### campaigns

```sql
CREATE TABLE campaigns (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           UUID        NOT NULL,
    brief_id             UUID        NOT NULL REFERENCES campaign_briefs(id),  -- many campaigns per brief
    job_id               UUID,                          -- creation job that produced this row
    platform             TEXT        NOT NULL,          -- channel: google-ads / linkedin-ads / ...
    platform_campaign_id TEXT,                          -- ID returned by the ad platform
    campaign_name        TEXT        NOT NULL,
    status               TEXT        NOT NULL,
    budget_amount        NUMERIC(14,2),
    budget_type          TEXT        CHECK (budget_type IN ('daily','lifetime')),
    start_date           DATE,
    end_date             DATE,
    config_snapshot      JSONB,
    result               JSONB,
    version              BIGINT      NOT NULL DEFAULT 1, -- ETag/If-Match
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_campaigns_brief_id   ON campaigns (brief_id);
CREATE INDEX idx_campaigns_project_id ON campaigns (project_id);
```

### campaign_jobs

```sql
CREATE TABLE campaign_jobs (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    brief_id   UUID        NOT NULL REFERENCES campaign_briefs(id),
    status     TEXT        NOT NULL DEFAULT 'pending',  -- pending/running/completed/failed
    result     JSONB,
    error      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX idx_campaign_jobs_brief_id ON campaign_jobs (brief_id);
```

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

### Worked Request/Response Examples

**Create a connection** (credentials in the request body; never echoed back):

```http
POST /projects/7f3.../linkedin-ads-connections
Content-Type: application/json

{ "label": "TLF Main", "account_id": "538170226", "org_id": "208777",
  "credentials": { "access_token": "AQV..." } }
```
```http
201 Created
ETag: "1"

{ "id": "a12...", "project_id": "7f3...", "label": "TLF Main",
  "account_id": "538170226", "org_id": "208777",
  "has_credentials": true, "status": "active", "version": 1 }
```

**Update config with optimistic concurrency** — the caller must present the current version:

```http
PUT /projects/7f3.../linkedin-ads-connections/a12...
If-Match: "1"
Content-Type: application/json

{ "label": "TLF Main (renamed)", "account_id": "538170226", "org_id": "208777" }
```
```http
200 OK
ETag: "2"        # version incremented
```
- Missing `If-Match` → `428 Precondition Required`
- Stale `If-Match` (version moved on) → `412 Precondition Failed` (re-fetch, retry)

**Set credential** — separate from `PUT`; does not touch config, has its own permission/audit:

```http
POST /projects/7f3.../linkedin-ads-connections/a12.../set-credential
Content-Type: application/json

{ "credentials": { "access_token": "AQV...new..." } }
```
```http
204 No Content
```

### set_credential vs. update

`set_credential` is a dedicated action, not a `rotate` endpoint. "Rotate" would imply the service atomically generates a new secret, swaps it upstream at the provider, *and* stores it — which the ad platforms do not support. `set_credential` simply replaces the stored (encrypted) credential and is split out from the generic `PUT` so credential replacement and config edits are independently permissioned and audited.

## Authorization (RuleSet)

Every path in this service — reads and writes — is gated at the gateway by a Heimdall RuleSet referencing the `campaign_manager` relation on the `project` captured from the path. (There is no read-only campaigns audience; `marketing_auditor` applies to the separate Snowflake-backed Marketing Insights dashboard, not this service.) This mirrors the committee-service pattern (`openfga_check` authorizer + `create_jwt` finalizer that mints the service-audience JWT this service then validates). Example rule for creating a connection:

```yaml
- id: "rule:lfx:lfx-v2-campaign-service:linkedin-ads-connections:create"
  match:
    methods: [POST]
    routes:
      - path: /projects/:projectId/linkedin-ads-connections
  execute:
    - authenticator: oidc
    {{- if .Values.openfga.enabled }}
    - authorizer: openfga_check
      config:
        values:
          relation: campaign_manager
          object: "project:{{ "{{- .Request.URL.Captures.projectId -}}" }}"
    {{- end }}
    - finalizer: create_jwt
      config:
        values:
          aud: {{ .Values.app.audience }}   # this service validates this JWT in-app
```

## Security

- Credentials encrypted at rest with **application-level AES-256-GCM**; key sourced from a Kubernetes secret via env var (referenced by the Helm chart). Not pgcrypto.
- Credentials never returned in API responses (replaced with `has_credentials`).
- `test` action verifies credentials against the provider without exposing them.
- Change history and attribution are served by the Query Service, which indexes each connection on write.
