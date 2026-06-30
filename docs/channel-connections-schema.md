# Channel Connections — Database Schema

Database schema for storing platform and channel connections. Supports paid ad platforms (current) and organic/communication channels (future). One project can have multiple connections of the same channel type.

## Tables

### channel_connections

Primary table for all platform and channel connections. Credentials stored as encrypted JSONB.

```sql
CREATE TABLE channel_connections (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id        UUID        NOT NULL,
    channel_type      VARCHAR(50) NOT NULL,
    channel_category  VARCHAR(20) NOT NULL,
    label             VARCHAR(255) NOT NULL,
    account_id        VARCHAR(255),
    credentials       JSONB,          -- encrypted at rest
    metadata          JSONB,          -- non-secret config
    status            VARCHAR(20) NOT NULL DEFAULT 'active',
    created_by        UUID        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_channel_type CHECK (channel_type IN (
        -- Paid advertising
        'google-ads', 'linkedin-ads', 'meta-ads', 'reddit-ads', 'twitter-ads',
        'microsoft-ads',
        -- Email
        'email',
        -- Organic social
        'linkedin-organic', 'twitter-organic', 'bluesky', 'mastodon', 'youtube',
        -- Community
        'slack', 'discord'
    )),
    CONSTRAINT chk_channel_category CHECK (channel_category IN (
        'paid', 'email', 'organic', 'community'
    )),
    CONSTRAINT chk_status CHECK (status IN (
        'active', 'inactive', 'error'
    ))
);

CREATE INDEX idx_connections_project_type ON channel_connections (project_id, channel_type);
CREATE INDEX idx_connections_project_status ON channel_connections (project_id, status);
CREATE INDEX idx_connections_type_account ON channel_connections (channel_type, account_id);
```

### channel_connection_audit

Audit trail for connection changes. Never stores credentials.

```sql
CREATE TABLE channel_connection_audit (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id     UUID        NOT NULL REFERENCES channel_connections(id) ON DELETE CASCADE,
    action            VARCHAR(30) NOT NULL,
    changed_by        UUID        NOT NULL,
    changed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    previous_values   JSONB,          -- snapshot of changed fields (never includes credentials)

    CONSTRAINT chk_action CHECK (action IN (
        'created', 'updated', 'deleted',
        'credentials_rotated', 'status_changed'
    ))
);

CREATE INDEX idx_audit_connection ON channel_connection_audit (connection_id);
CREATE INDEX idx_audit_changed_at ON channel_connection_audit (changed_at);
```

## Channel Type Reference

### Paid Advertising

| channel_type | channel_category | account_id | credentials (encrypted) | metadata |
|-------------|-----------------|------------|------------------------|----------|
| `google-ads` | `paid` | Customer ID (e.g. `8666746580`) | `{ refresh_token, client_id, client_secret, developer_token }` | `{ login_customer_id }` |
| `linkedin-ads` | `paid` | Ad account ID (e.g. `538170226`) | `{ access_token }` | `{ org_id }` |
| `meta-ads` | `paid` | Ad account ID (e.g. `act_193556282970417`) | `{ access_token, app_secret }` | `{ page_id, app_id }` |
| `reddit-ads` | `paid` | Advertiser ID (e.g. `t2_gv9wtbfa`) | `{ client_id, client_secret, refresh_token }` | `{}` |
| `twitter-ads` | `paid` | Account ID (e.g. `8r7gb`) | `{ consumer_key, consumer_secret, access_token, access_token_secret }` | `{ funding_instrument_id }` |
| `microsoft-ads` | `paid` | Account ID | `{ client_id, client_secret, refresh_token, developer_token }` | `{ customer_id }` |

### Organic Social

| channel_type | channel_category | account_id | credentials (encrypted) | metadata |
|-------------|-----------------|------------|------------------------|----------|
| `linkedin-organic` | `organic` | Org ID (e.g. `208777`) | `{ access_token }` | `{ org_name }` |
| `twitter-organic` | `organic` | User ID | `{ consumer_key, consumer_secret, access_token, access_token_secret, bearer_token }` | `{ username, handle }` |
| `bluesky` | `organic` | DID | `{ app_password }` | `{ handle }` |
| `mastodon` | `organic` | Username | `{ access_token }` | `{ instance_url }` |
| `youtube` | `organic` | Channel ID | `{ refresh_token, client_id, client_secret }` | `{ channel_name }` |

### Email

| channel_type | channel_category | account_id | credentials (encrypted) | metadata |
|-------------|-----------------|------------|------------------------|----------|
| `email` | `email` | List/audience ID | `{ private_app_token }` | `{ portal_id, list_name, sender_email, sender_name, brand_kit }` |

HubSpot is the first email integration. Each project has its own brand kit, footer, and email templates, selected based on the request.

### Community (Slack / Discord)

| channel_type | channel_category | account_id | credentials (encrypted) | metadata |
|-------------|-----------------|------------|------------------------|----------|
| `slack` | `community` | Workspace ID | `{ bot_token, signing_secret }` | `{ workspace_name, channel_ids[], default_channel_id }` |
| `discord` | `community` | Server (guild) ID | `{ bot_token }` | `{ server_name, channel_ids[], default_channel_id }` |

## Multiple Connections Per Project

A single project can have many connections of the same type. Examples:

**Slack** — a foundation may have separate workspaces or channels for different events:
```
project: CNCF
  slack | workspace: T01234 | #cncf-marketing       | active
  slack | workspace: T01234 | #kubecon-na-2025       | active
  slack | workspace: T01234 | #kubecon-eu-2025       | active
  slack | workspace: T56789 | #cncf-partners         | active
```

**Discord** — a foundation may participate in multiple community servers:
```
project: CNCF
  discord | server: 123456 | CNCF Community         | active
  discord | server: 789012 | Kubernetes              | active
```

**Email (HubSpot)** — different lists for different audiences:
```
project: CNCF
  email | list: abc123 | CNCF Newsletter           | active
  email | list: def456 | KubeCon Updates            | active
  email | list: ghi789 | CNCF Certification News   | active
```

**LinkedIn Ads** — different ad accounts for different sub-brands:
```
project: TLF
  linkedin-ads | account: 538170226 | TLF Main       | active
  linkedin-ads | account: 509430019 | LF Events      | active
```

## Current Account Inventory

Accounts currently configured in the Express BFF (to be migrated to this service).

Each platform has a different tenancy model. Some use a single shared account across all foundations (campaigns separated by naming convention). Others have separate accounts per foundation/project.

### Single Shared Account (all foundations use one account)

**Google Ads**
- Manager Account: 9746983954
- Customer Account: 8666746580
- ALL foundations run campaigns under this single account
- Projects distinguished by campaign naming convention, not by separate accounts

**HubSpot**
- Single private app token (org-wide)
- Campaigns matched to projects by name
- Each project has its own brand kit, footer, and email templates, selected based on the request

### Separate Accounts Per Foundation/Project

**LinkedIn Ads** — each foundation has its own ad account
- TLF: Account 538170226, Org 208777
- LF Events: Account 509430019
- More accounts added per foundation as needed
- Loaded from ConfigMap at runtime

**Meta Ads** — separate ad accounts per foundation
- LF Core: Account act_193556282970417, Page 41911143546
- More accounts added per foundation as needed

**Reddit Ads** — per foundation
- TLF: Account t2_gv9wtbfa

**X/Twitter Ads** — per foundation
- LF Events: Account 8r7gb, Funding Instrument (pending)

## API Endpoints

### Connection CRUD

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/projects/{project_id}/connections` | Create a new connection |
| `GET` | `/projects/{project_id}/connections` | List all connections (filterable by `channel_type`, `channel_category`, `status`) |
| `GET` | `/projects/{project_id}/connections/{id}` | Get a specific connection (credentials redacted in response) |
| `PUT` | `/projects/{project_id}/connections/{id}` | Update a connection |
| `DELETE` | `/projects/{project_id}/connections/{id}` | Remove a connection (soft delete, audit logged) |
| `POST` | `/projects/{project_id}/connections/{id}/test` | Test connection (verify credentials are valid) |
| `POST` | `/projects/{project_id}/connections/{id}/rotate` | Rotate credentials (for token refresh) |

### Response Shape

Credentials are never returned in API responses. The `credentials` field is replaced with a `has_credentials: boolean` flag.

```json
{
  "id": "uuid",
  "project_id": "uuid",
  "channel_type": "slack",
  "channel_category": "communication",
  "label": "#kubecon-na-2025",
  "account_id": "T01234",
  "has_credentials": true,
  "metadata": {
    "workspace_name": "CNCF",
    "channel_ids": ["C01234", "C56789"],
    "default_channel_id": "C01234"
  },
  "status": "active",
  "created_by": "uuid",
  "created_at": "2026-06-29T00:00:00Z",
  "updated_at": "2026-06-29T00:00:00Z"
}
```

## Security

- Credentials column encrypted at rest using PostgreSQL pgcrypto or application-level encryption
- Credentials never returned in API responses (replaced with `has_credentials` boolean)
- Credential changes logged in audit table without storing the actual credential values
- Connection test endpoint verifies credentials without exposing them
- Rotation endpoint allows credential refresh without full connection update
