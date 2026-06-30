# Campaign Service — Build Summary

**Status:** Architecture Review — Aligning with platform patterns  
**Date:** 2026-06-30  
**References:** [committee-service](https://github.com/linuxfoundation/lfx-v2-committee-service), [lfx-v2-helm](https://github.com/linuxfoundation/lfx-v2-helm), [Eric's gist](https://gist.github.com/emsearcy/6464a2b87ccb0b5d56c0d96bd1415c8c)

---

## Architecture Options

_Source: [Eric's marketing stack diagram](https://gist.github.com/emsearcy/6464a2b87ccb0b5d56c0d96bd1415c8c) (updated 2026-06-30)_

Two options for how campaigns integrates into the LFX platform. **Orange** (API Gateway brokered) is preferred but requires adopting v2-Platform idioms (OpenFGA, Query Service). **Blue** (NATS RPC from SSR) de-risks by splitting into two phases: first the TypeScript → Go migration, then platform idiom adoption.

```mermaid
flowchart TD
    subgraph LFX Web UI
        frontend["Browser / frontend"]
        ssr["Server-side rendering\n& BFF routes"]
        frontend -->|html page loads| ssr
        frontend -->|partial-page loads / client-side rendering| ssr
    end

    mcp[MCP Server]
    mobile[mobile-app?]
    snowflake[(Snowflake)]

    ssr ----->|less ideal| snowflake

    subgraph Platform[LFX Platform]
        api-gw["API Gateway\n(Authorization)"]
        querysvc[Query Service]
        opensearch[(OpenSearch)]
        api-gw --> querysvc -->|searches| opensearch
        domains[Project domains]
        lists[Mailing lists]
        meetings[Meetings]
        api-gw --> domains
        api-gw --> lists
        api-gw --> meetings

        subgraph campaigns-group["Campaigns"]
            campaigns["Campaigns service\n(Golang)"]
            campaigns-db[("Postgres\n(stores briefs, shared-tenant mappings, etc)")]
            google-ads-helper["Google Ads Typescript<br />helper (optional)"]
            campaigns --> campaigns-db
        end

        style campaigns-group fill:#fff4cc,stroke:#e6ac00,color:#000

        api-gw --> campaigns

        domains & lists & meetings & campaigns -.->|index| opensearch
    end

    ssr & mcp & mobile --> api-gw

    domains ---> DNsimple
    lists ---> GroupsIO
    meetings ---> Zoom

    campaigns -->|NATS RPC| google-ads-helper
    campaigns ---> ads[Ad platforms]
    google-ads-helper --> ads

    lists -->|more ideal| snowflake

    subgraph auth-service
        subsystem[UI subsystem]
    end

    subsystem --> cdp[Crowd.dev CDP]
    ssr -->|NATS RPC| subsystem

    subgraph campaigns-ui-service["Campaigns UI Microservice (alternative)"]
        campaigns-ui-subsystem["UI subsystem\n(Golang)"]
        campaigns-ui-db[("Postgres")]
        campaigns-ui-subsystem --> campaigns-ui-db
        campaigns-ui-ads-helper["Google Ads Typescript<br />helper (optional)"]
        campaigns-ui-subsystem -->|NATS RPC| campaigns-ui-ads-helper
    end

    style campaigns-ui-service fill:#e8f4ff,stroke:#4a90d9,color:#000

    ssr -->|NATS RPC| campaigns-ui-subsystem
    campaigns-ui-ads-helper --> ads
    campaigns-ui-subsystem --> ads
```

### Option comparison

| | Orange (API Gateway brokered) | Blue (NATS RPC from SSR) |
|---|---|---|
| **Auth** | Heimdall / OpenFGA at API Gateway | UI-brokered (SSR passes auth context via NATS) |
| **Routing** | API Gateway → Campaigns service | SSR → NATS RPC → Campaigns UI subsystem |
| **Platform idioms** | Full: OpenFGA rules, Query Service, OpenSearch indexing | Deferred to phase 2 |
| **Risk** | Higher — must adopt platform auth + query patterns upfront | Lower — migration first, platform adoption second |
| **Recommendation** | Preferred (per Eric) | De-risks implementation |

---

## Middleware Pipeline

_Reference: [committee-service/cmd/committee-api/http.go](https://github.com/linuxfoundation/lfx-v2-committee-service)_

```mermaid
flowchart TD
    req[Incoming HTTP Request] --> rid[Request ID middleware]
    rid -->|"X-Request-ID → context + slog"| authz[Authorization propagation]
    authz -->|"Authorization header → context\n(no JWT validation — Heimdall handles that)"| goa[Goa Mux]
    goa -->|"Route match → decode/validate → type-safe payload"| otel[OpenTelemetry]
    otel -->|"Trace spans, metrics\n(excludes /livez, /readyz)"| debug{DEBUG=true?}
    debug -->|yes| dbg[Debug handler\nlog payloads + /debug/pprof]
    debug -->|no| svc[campaign_svc.go\nGoa service handlers]
    dbg --> svc
```

**Not in-app (handled upstream):**

| Concern | Handled by | Reference |
|---------|-----------|-----------|
| CORS | Traefik ingress | Not in committee-service or any resource service |
| JWT validation | Heimdall ForwardAuth | `charts/*/heimdall-middleware.yaml` |
| Authorization (ReBAC) | Heimdall → OpenFGA | URL pattern rulesets at ingress |
| Panic recovery | Go `http.Server` default | Not in committee-service |

Health probes `/livez` and `/readyz` bypass the middleware chain.

---

## What We're Porting

Migrating **~7,300 lines of TypeScript** (Express BFF) → **Go microservice**.

```mermaid
flowchart LR
    subgraph before["BEFORE — Express BFF"]
        proxy["campaign-proxy.service.ts\n1,901 LOC"]
        linkedin["linkedin-ads.service.ts\n1,133 LOC"]
        iface["campaign.interface.ts\n964 LOC"]
        reddit["reddit-ads.service.ts\n698 LOC"]
        ctrl["campaign.controller.ts\n646 LOC"]
        meta["meta-ads.service.ts\n614 LOC"]
        xads["x-ads.service.ts\n620 LOC"]
        metrics["campaign-metrics.service.ts\n450 LOC"]
        consts["campaign.constants.ts\n248 LOC"]
    end

    subgraph after["AFTER — Go Service"]
        go["Hexagonal architecture\nPostgreSQL persistence\nAES-256-GCM encryption\nMulti-platform orchestration\nAsync job tracking"]
    end

    before -->|"~7,300 LOC"| after

    style before fill:#fee,stroke:#c00
    style after fill:#efe,stroke:#0a0
```

**Problems solved:** in-memory job map (3 replicas = lost state), no persistence, no credential encryption, no audit trail.

---

## Campaign Service Architecture

```mermaid
flowchart TD
    subgraph http["HTTP Layer (Goa)"]
        svc["campaign_svc.go — Goa service impl"]
        svc --> conn["Connection CRUD (7 endpoints)"]
        svc --> brief["Brief lifecycle (8 endpoints)"]
        svc --> create["Campaign create (async → jobId)"]
        svc --> monitor["Monitoring (metrics, keywords, audience)"]
        svc --> optim["Optimization (keyword actions, status toggle)"]
    end

    subgraph service["Service Layer"]
        connsvc["ConnectionSvc\n(encrypt / audit)"]
        briefsvc["BriefSvc\n(versions)"]
        execsvc["ExecutionSvc\n(job tracking)"]
        orch["Orchestrator\nerrgroup (cap 5)\ncontext.WithoutCancel"]
        optimsvc["OptimizationSvc"]
        metricssvc["MetricsSvc"]

        orch --> gads["Google Ads\n(gRPC)"]
        orch --> li["LinkedIn\n(REST)"]
        orch --> metaapi["Meta\n(REST)"]
        orch --> redditapi["Reddit\n(REST)"]
        orch --> xapi["X/Twitter\n(OAuth 1.0a)"]
    end

    subgraph infra["Infrastructure Layer"]
        pg[("PostgreSQL\n(CloudNativePG)")]
        aes["AES-256-GCM\nEncryption"]
        nats["NATS\n(platform bus)"]
    end

    conn --> connsvc
    brief --> briefsvc
    create --> execsvc
    monitor --> metricssvc
    optim --> optimsvc

    connsvc & briefsvc & execsvc --> orch
    connsvc --> aes --> pg
    briefsvc & execsvc --> pg
    orch & optimsvc & metricssvc --> nats
```

**Why PostgreSQL instead of NATS KV:** committee-service uses NATS KV as primary store. Campaigns uses PostgreSQL because ad platform credentials require AES-256-GCM encryption at rest + relational queries for briefs and executions. NATS is used for events, inter-service comms, index updates → OpenSearch, and FGA sync → OpenFGA.

Auth (JWT validation, CORS, ReBAC) is handled upstream by Traefik + Heimdall + OpenFGA — not in this service.

---

## File Structure (proposed, aligned with committee-service)

_Reference: [lfx-v2-committee-service](https://github.com/linuxfoundation/lfx-v2-committee-service) directory layout_

```
lfx-v2-campaign-service/
├── cmd/campaign-api/                    — follows committee-api pattern
│   ├── main.go                          — entry point + startup orchestration
│   ├── http.go                          — HTTP server + middleware chain
│   ├── design/                          — Goa design DSL (co-located, not top-level)
│   │   ├── design.go                    — API definition, auth, errors
│   │   ├── types.go                     — shared Goa types
│   │   ├── connections.go               — connection CRUD (7 endpoints)
│   │   ├── briefs.go                    — brief lifecycle (8 endpoints)
│   │   ├── campaigns.go                 — create + job tracking
│   │   ├── monitoring.go                — metrics endpoints
│   │   └── optimization.go             — keyword actions, status toggle
│   └── service/                         — Goa service handlers + providers
│       ├── campaign_svc.go              — Goa service implementation
│       └── providers.go                 — sync.Once singletons for DI
│
├── internal/
│   ├── domain/
│   │   ├── model/                       — ChannelConnection, CampaignBrief, Execution, Job
│   │   └── port/                        — Reader/Writer/Auditor interfaces
│   ├── service/                         — connection, brief, execution, orchestrator, metrics, optimization
│   ├── platform/
│   │   ├── platform.go                  — PlatformClient interface
│   │   ├── googleads/                   — gRPC campaign creation + GAQL metrics
│   │   ├── linkedin/                    — REST + targeting profiles
│   │   ├── meta/                        — Graph API
│   │   ├── reddit/                      — REST + token refresh
│   │   ├── twitter/                     — OAuth 1.0a HMAC-SHA1
│   │   └── hubspot/                     — UTM campaign lookup/create
│   ├── infrastructure/
│   │   ├── config/config.go
│   │   ├── crypto/aes.go               — AES-256-GCM encrypt/decrypt
│   │   ├── nats/                        — pub/sub, request/reply
│   │   └── postgres/                    — pool, migrations, repositories
│   └── middleware/
│       ├── request_id.go                — X-Request-ID propagation
│       └── authorization.go             — Authorization header → context
│
├── gen/                                 — Goa-generated (committed per platform convention)
├── pkg/constants/                       — platforms, char limits, targeting, campaign naming
├── charts/lfx-v2-campaign-service/      — Helm chart (Gateway API, Heimdall)
└── docs/
```

**Key differences from committee-service:**

| Aspect | committee-service | campaign-service | Why |
|--------|------------------|------------------|-----|
| Primary store | NATS KV | PostgreSQL | Credentials require encryption + relational queries |
| External APIs | None | 5 ad platforms + HubSpot | Core purpose is ad platform orchestration |
| NATS usage | Primary data store | Events + inter-service | Index updates, FGA sync, cross-service queries |

---

## Key Design Decisions

| Decision | Why |
|----------|-----|
| Broker pattern (not stateful store) | Go owns platform APIs + persistence. Per Eric/Jordan (2026-06-24). |
| PostgreSQL + NATS (not NATS KV only) | PG for relational data (credentials, briefs, executions). NATS for events + inter-service. Campaigns needs PG for AES-256-GCM encrypted creds. |
| AES-256-GCM | Credentials encrypted at rest. Never returned in API responses. Backwards-compatible passthrough. |
| Auth via Heimdall (not in-app) | JWT validation + ReBAC at Traefik ingress, same as all platform resource services. |
| Hexagonal architecture | domain → ports → adapters. Easy to swap DB or add platforms. |
| Platform-agnostic persistence | No per-platform columns or CHECK constraints. JSONB for platform config. |
| errgroup (cap 5) | Bounded concurrency for multi-platform dispatch. Won't overwhelm APIs. |
| context.WithoutCancel | Async goroutines survive request cancellation (Go 1.21+). |
| Google Ads TS helper (optional) | gRPC client complexity may warrant keeping TypeScript sidecar, connected via NATS RPC. |

---

## Request Flow: Campaign Creation

```mermaid
sequenceDiagram
    participant C as Client
    participant T as Traefik / Heimdall
    participant G as Goa Handler
    participant S as campaign_svc.go
    participant PG as PostgreSQL
    participant O as Orchestrator
    participant P as Ad Platforms

    C->>T: POST /create (JWT)
    T->>T: Heimdall → OpenFGA check
    T->>G: ✓ authorized
    G->>S: decoded + validated payload
    S->>PG: Create job row
    S-->>C: 202 Accepted {jobId}

    Note over S,P: Async goroutine (context.WithoutCancel)

    S->>O: Dispatch(platforms)
    O->>P: errgroup.SetLimit(5)
    P-->>O: results per platform
    O->>PG: Persist executions
    O->>PG: Update job → COMPLETED

    C->>G: GET /jobs/{id}
    G->>PG: Read job status
    PG-->>C: {status: COMPLETED, results: [...]}
```

---

## Data Flow: Credential Security

```mermaid
flowchart TD
    subgraph create["Create Connection"]
        validate["Validate input"] --> encrypt["AES-256-GCM Encrypt(creds)"]
        encrypt -->|'encrypted: base64...'| store["PostgreSQL credentials JSONB"]
        store --> audit["Audit log (no creds)"]
    end

    subgraph read["Read Connection"]
        pgread["PostgreSQL read"] --> decrypt{Decrypt attempt}
        decrypt -->|success| ok["✓ decrypted"]
        decrypt -->|failure| warn["warn log → returns nil"]
        ok & warn --> response["API response:\nhas_credentials: true/false\n(never the actual creds)"]
    end
```

---

## Migration Map: TypeScript → Go

| TypeScript Source | Lines | Go Target | PR |
|------------------|-------|-----------|-----|
| `campaign.interface.ts` | 964 | `design/types.go` + `internal/domain/model/*.go` | 1, 2, 5 |
| `campaign.constants.ts` | 248 | `pkg/constants/*.go` | 4 |
| `campaign.controller.ts` | 646 | `design/*.go` (Goa) + `campaign_svc.go` | 1, 6, 7, 14, 15 |
| `campaign-proxy.service.ts` | 1,901 | `orchestrator.go` + `brief_service.go` | 6, 7, 13 |
| `linkedin-ads.service.ts` | 1,133 | `internal/platform/linkedin/*.go` | 9 |
| `meta-ads.service.ts` | 614 | `internal/platform/meta/*.go` | 10 |
| `reddit-ads.service.ts` | 698 | `internal/platform/reddit/*.go` | 11 |
| `x-ads.service.ts` | 620 | `internal/platform/twitter/*.go` | 12 |
| `campaign-metrics.service.ts` | 450 | `internal/service/metrics_service.go` | 14 |
| Google Ads (in proxy) | ~600 | `internal/platform/googleads/*.go` | 8A, 8B |

---

## Blockers

| Blocker | Impact | Owner | Status |
|---------|--------|-------|--------|
| Architecture alignment | Must match committee-service patterns | Misha + Eric | In progress — Eric reviewing |
| PR #2 arch doc update | Updated diagram needed before code PRs | Misha | This document |
| PostgreSQL provisioning | Can't run migrations in cluster | David (requested) | Pending |
| Antonia — Backstage template | CI fails on main | Antonia + David | In progress |

---

## Next Steps

### Immediate (from architecture review meeting)

1. Update PR #2 with this aligned architecture document
2. Eric reviews and provides line-by-line feedback on the diagram
3. Restructure repo to match committee-service layout:
   - Move `design/` → `cmd/campaign-api/design/`
   - Move `campaign_svc.go` → `cmd/campaign-api/service/`
   - Add `providers.go` with `sync.Once` pattern
   - Rename `server.go` → `http.go`
   - Remove in-app CORS middleware (handled by Traefik)
   - Remove in-app JWT validation (handled by Heimdall)
   - Add NATS client for platform bus integration
   - Add authorization middleware (header propagation only)

### After alignment approval

1. Split into **~10 PRs** (each under 1,000 lines), submit sequentially
2. PR 1: Fix build + Goa design (aligned structure)
3. PR 2: Domain models + PostgreSQL migrations
4. PR 3: Connection service + DI wiring
5. PRs 4-10: Platform clients, orchestration, monitoring, optimization
