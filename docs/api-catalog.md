# Campaign Service — API & Platform Catalog

Reference catalog of all campaign endpoints, platform account attributes, and data structures for the Go service.

## API Design Rules

These rules apply to every endpoint below and reflect platform idioms ([entity-design.md](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/docs/entity-design.md)) rather than the shape of the existing Express BFF:

1. **Everything is nested under a project.** No new top-level FGA types were introduced for this service — only new *relations against `project`*. Per [entity-design.md](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/docs/entity-design.md), a resource may only be a root API path if it is a top-level FGA type. Consequently **every** campaign resource is nested under `/projects/{projectId}/…`. Briefs and campaigns are subordinate to a project (campaigns are further subordinate to a brief).
2. **Every endpoint declares its gating FGA relation.** The service defines no new object types; it relies on the marketing relations on `project` (defined in [`lfx-v2-helm/.../files/model.fga`](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/charts/lfx-platform/files/model.fga#L36-L43)):
   - **`marketing_ops`** — team members with cross-project campaign management.
   - **`campaign_manager`** = `executive_director or marketing_ops` — manages campaigns/briefs/connections for a project. Does *not* cascade from parent; scoped to the project it is granted on.

   **Every endpoint in this service is gated on `campaign_manager`** — both reads and writes. There is no read-only view of campaigns: the Campaigns page is only ever accessed by campaign managers, who both read and write. The `marketing_auditor` relation applies to the separate **Marketing Insights** analytics dashboard (Snowflake-backed), which is not served by this service, so it does **not** appear in any ruleset here.
3. **Reads/lists/history come from the Query Service (briefs and campaigns).** Briefs and campaigns are indexed into the Query Service; consumers (UI, MCP) fetch their **lists** and **revision/audit history** from it, which maintains revision history on each (re)index. This service therefore exposes **no dedicated list endpoints and no bespoke audit endpoints** for them — only the canonical item CRUD needed to mutate state. `GET` on a single item is retained for ETag retrieval prior to a conditional update. **Connections are the exception: they are not indexed** (singleton per project, no listing/inventory consumer — see rule note below and [architecture.md](architecture.md) D5), so a connection is read directly via `GET /projects/{projectId}/connection-{provider}`.
4. **Create and replace are separate; replace requires `If-Match`.** There is no "create-or-update" endpoint. A `PUT` (replace) requires an `If-Match: "<version>"` header carrying the current ETag; the caller must have fetched the current version first. Mismatches return `412 Precondition Failed`; a missing header returns `428 Precondition Required`. (Optimistic-locking pattern per [committee-service / 2026-05-CloudNativePG](https://github.com/linuxfoundation/lfx-architecture-scratch/tree/main/2026-05-CloudNativePG).)
5. **No bulk mutation endpoints.** Bulk status/budget changes are omitted: HTTP cannot cleanly express partial success/failure across a set, and a single bulk call cuts across per-target permission boundaries. Each mutation is scoped to one permission-evaluated target.

Resource pseudotypes declared into the global indexer namespace: `campaign_brief` and `campaign`. **Connections are not indexed** — they are singleton per project with no cross-project listing consumer, so they are read directly (not via the Query Service). See [architecture.md](architecture.md) for the full type-name and relation catalog.

### Brief Lifecycle (Planning Phase)

Briefs are subordinate to a project. AI generation currently runs SSE-streamed in the Express BFF and will migrate later; the persistence/CRUD surface below is what this service owns.

A brief is the funnel unit: it carries the **program** (`program_type` = events / education / membership) that sets the funnel context, and it is **shared across channels** — one brief drives many channel campaigns (see next section), each a row under the brief with the same `brief_id`. Program is a field on the brief, not a separate resource.

**Platform selection lives on the campaign, not the brief.** A brief may carry a *suggested* default set of platforms (a planning hint used to pre-populate the campaign form), but the binding choice of which platforms to launch on — and each platform's configuration — is made at campaign-creation time. The generation strategy is driven by `program_type`, not by which channels a brief was drafted against, so a single approved brief can be launched on any subset of platforms.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs` | `campaign_manager` | JSON | Create a brief. |
| GET | `/projects/{projectId}/briefs/{id}` | `campaign_manager` | JSON | Get a brief (full copy, keywords, targeting); returns ETag. |
| PUT | `/projects/{projectId}/briefs/{id}` | `campaign_manager` | JSON | Replace a brief (requires `If-Match`). |
| POST | `/projects/{projectId}/briefs/{id}/refresh` | `campaign_manager` | JSON | Re-run generation against latest event data, producing a new version. |
| POST | `/projects/{projectId}/briefs/{id}/approve` | `campaign_manager` | JSON | Approve a brief for campaign creation (requires `If-Match`; approval is version-gated so a brief replaced since it was fetched cannot be approved on stale content). |
| DELETE | `/projects/{projectId}/briefs/{id}` | `campaign_manager` | JSON | Archive a brief (soft delete). |

> Listing briefs and viewing a brief's version history are served by the Query Service, not by dedicated endpoints here.

### Campaign Creation (Implementation Phase)

A campaign is subordinate to a brief. This is a **collection** under the brief (a brief may drive multiple campaigns across platforms). The `POST` body carries the **selected platforms** and their per-platform config (see `CampaignCreateRequest`). Creation is **asynchronous**: the upstream ad platforms take seconds-to-minutes to provision, so `POST` returns immediately with a `jobId` (a `JobCreateResponse`), and the caller polls `GET .../jobs/{jobId}` for a `JobPollResponse` until the job is terminal. One execution record is persisted per platform.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns` | `campaign_manager` | JSON | Create campaigns across the platforms selected in the body (async → `JobCreateResponse` with `jobId`). Persists one execution record per platform. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}` | `campaign_manager` | JSON | Get one campaign execution; returns ETag. |
| PUT | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}` | `campaign_manager` | JSON | Replace a campaign execution (requires `If-Match`). |
| GET | `/projects/{projectId}/jobs/{jobId}` | `campaign_manager` | JSON | Poll campaign creation job status (`JobPollResponse`). |

> Listing a project's or brief's campaigns, and per-campaign change history, are served by the Query Service.

### Campaign Audiences (Implementation Phase)

A **built campaign audience** is a pointer + provenance to a platform-side audience (its master-list id, applied suppression lists, and a human-readable inclusion summary) — not the audience's contents. It is a **collection** subordinate to a brief (a brief may drive several audiences over time / per platform). Writes are gated on `campaign_manager` and use optimistic concurrency: reads return an ETag, and `PATCH` requires `If-Match` (`428` when missing, `412` on mismatch). `PATCH` is a load-then-merge — a nil field is left unchanged; an explicit empty list clears it.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs/{briefId}/audiences` | `campaign_manager` | JSON | Create a built audience under the brief; returns ETag. |
| GET | `/projects/{projectId}/briefs/{briefId}/audiences/{audienceId}` | `campaign_manager` | JSON | Get one audience; returns ETag. |
| GET | `/projects/{projectId}/briefs/{briefId}/audiences` | `campaign_manager` | JSON | List a brief's audiences (newest first). |
| PATCH | `/projects/{projectId}/briefs/{briefId}/audiences/{audienceId}` | `campaign_manager` | JSON | Partially update an audience (load-then-merge; requires `If-Match`). |

### Monitoring (Insights Phase)

Metrics are read-through from the ad platforms, scoped by project. There are no per-platform root paths; the provider is a path segment under the project. Because a connection is singleton per project, `/{provider}/metrics` unambiguously means "metrics for **this project's** account on that provider" — there is no per-connection or per-campaign metrics path (a campaign's metrics are a row inside the provider response, keyed by `campaignId`).

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/{provider}/metrics` | `campaign_manager` | JSON | Campaign metrics for this project's account on the provider (`days` param, default 14). `{provider}` ∈ `google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`. |
| GET | `/projects/{projectId}/google-ads/keywords` | `campaign_manager` | JSON | Google Ads keyword performance (top 50 by impressions). |
| GET | `/projects/{projectId}/google-ads/audience` | `campaign_manager` | JSON | Audience demographics (age, gender, device). |

> **Umbrella roll-up (TLF across child foundations).** This service never aggregates across projects: `campaign_manager` does not cascade from parent to child (see rule 2), and each project owns only its own connection. A TLF-wide view of, say, all Google Ads spend is assembled by the **UI backend**, which fans out one `GET /projects/{child}/google-ads/metrics` per child foundation the caller has access to and sums the results. Keeping aggregation out of this service preserves the strict per-project permission boundary and avoids the service resolving project hierarchy.

> There are no `/{provider}/accounts` listing endpoints. A project has at most one connection per provider, read directly via `GET /projects/{projectId}/connection-{provider}` (connections are not indexed into the Query Service — see the Platform Connections section).

### HubSpot UTM Integration

HubSpot campaigns are an **LF-wide, global namespace** — HubSpot is a single foundation-wide instance, not partitioned by project. This service does **not** attempt to scope UTM search to a project: a lookup searches all HubSpot campaigns, and a created UTM is visible to campaign managers on every project. The `{projectId}` in the path exists **only to gate the permission lookup** (the caller must be a `campaign_manager` on *some* project to use the integration at all); it does not filter results.

Lookup is a query by event name, passed as the `q` query parameter (fuzzy name match, scored — see [Platform-Specific Gotchas](#hubspot)). Because the namespace is global and cross-project, the UI **must caveat at create time** that a new UTM will be visible across foundations, so users do not put anything project-sensitive in a UTM name.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/hubspot/utm?q={eventName}` | `campaign_manager` | JSON | Look up a HubSpot campaign by event name across the **entire** LF HubSpot instance (global namespace; not scoped to the project). |
| POST | `/projects/{projectId}/hubspot/utm` | `campaign_manager` | JSON | Create an LF-global HubSpot campaign if not found. **Visible to all projects' campaign managers** — the UI must warn before creating. |

### Optimization

Each optimization action is scoped to a single campaign under its brief and is individually permission-evaluated. Bulk cross-campaign endpoints are intentionally omitted (see rule 5).

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/status` | `campaign_manager` | JSON | Toggle campaign ACTIVE/PAUSED (Meta, Reddit, X). |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions` | `campaign_manager` | JSON | Pause/remove Google Ads keywords for this campaign. |

**Tentative** (later phases, same nesting + `campaign_manager` gating): budget adjust, bid-strategy change, per-keyword bid, ad/creative rotation, ad-copy edit, geo-target edit, audience edit, negative keywords, bid modifiers, scheduling, flight-date change. Cross-platform budget reallocation, if built, is modeled as a first-class per-project resource with its own single-target mutations — not a bulk endpoint.

### Platform Connections (new — typed per provider, singleton per project)

A connection is **singleton per provider per project**: a project holds at most one connection of any given provider (one Google Ads account, one LinkedIn ad account, …). Multiplicity of accounts across the Linux Foundation lives at the **project** level, not inside a project — CNCF, OpenSearch, and TLF are each their own project, each owning its own single connection per provider. (TLF is both an umbrella over child-foundation projects *and* its own project with its own account; it owns only its own connection. Cross-foundation roll-up is a read concern handled by the UI backend — see the Monitoring note below — not by holding multiple connections on one project.)

Because the connection is a singleton, there is **no service-generated `{id}` in the path** — the provider name *is* the identity within the project. The path token is the **same provider key used everywhere else in this service** (`google-ads`, `linkedin-ads`, …, and `hubspot` for the non-ads provider), so the mapping is consistent end-to-end: path `connection-google-ads` → table `google_ads_connections`. Connections are strongly typed per provider (see [channel-connections-schema.md](channel-connections-schema.md)). The table below shows the pattern for `google-ads`; every provider (`linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`, `hubspot`) exposes the identical shape with its own typed payload.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Create the project's Google Ads connection (`409 Conflict` if one already exists). |
| GET | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Get the connection (credentials redacted); returns ETag. |
| PUT | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Replace connection config (requires `If-Match`; does not set credentials). |
| DELETE | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Remove the connection (soft delete). |
| POST | `/projects/{projectId}/connection-google-ads/test` | `campaign_manager` | JSON | Verify credentials against the provider. |
| POST | `/projects/{projectId}/connection-google-ads/set-credential` | `campaign_manager` | JSON | Replace the stored (encrypted) credential. Split out from `PUT` so credential replacement is independently permissioned/audited. Not "rotate" — the service does not generate/swap secrets upstream. |

> Because the connection is a singleton, `GET /projects/{projectId}/connection-google-ads` *is* the read — there is no collection listing and no Query Service index for connections. There is no present use case for a cross-project inventory of connections (the UI reads a project's connection directly), so the connection tables are intentionally not indexed; if such an inventory is ever needed, indexing can be added then.

---

## Platform Account Attributes

Per-provider account identifiers, config fields, and encrypted credential shapes are defined once in [channel-connections-schema.md](channel-connections-schema.md#per-provider-tables) and are not duplicated here.

---

## Campaign Platforms

Status refers to the **existing TypeScript BFF** (the migration source — see [build-summary.md](build-summary.md)); no provider code exists in this repo yet. All "Implemented" providers are migration targets for this service.

| Platform | Key | Status (current TS BFF) | Auth Type |
|----------|-----|-------------------------|-----------|
| Google Ads | `google-ads` | Implemented | OAuth 2.0 |
| LinkedIn Ads | `linkedin-ads` | Implemented | OAuth 2.0 |
| Meta Ads | `meta-ads` | Implemented | Bearer token |
| Reddit Ads | `reddit-ads` | Implemented | OAuth 2.0 |
| X/Twitter Ads | `twitter-ads` | Implemented | OAuth 1.0a (HMAC-SHA1) |
| Microsoft Ads | `microsoft-ads` | Not yet implemented | — |

---

## Campaign Types

### Program Types

| Type | Description |
|------|-------------|
| `events` | Conference/summit campaigns (e.g., KubeCon, All Systems Go) |
| `education` | Training/certification campaigns (e.g., CKA, LFCS) |
| `membership` | Membership recruitment and renewal campaigns |

The program type determines the AI brief generation strategy (copy tone, targeting approach, keywords, UTM structure) and feeds into the campaign naming convention.

### Google Ads Campaign Types

| Type | Description |
|------|-------------|
| `search` | Search (RSA, responsive search ads) |
| `demand-gen` | Display (YouTube, Discover, Gmail) |

### Campaign Goals

| Goal | Description |
|------|-------------|
| `event-registration` | Drive registrations for conferences and summits |
| `training-certification` | Drive enrollment for training courses and certification exams |
| `membership-growth` | Drive new membership sign-ups and renewals |

---

## Character Limits (Per Platform)

### Google Search (RSA)

| Element | Max Chars | Max Count |
|---------|-----------|-----------|
| Headline | 30 | 15 |
| Description | 90 | 4 |

### Google Display (Demand Gen)

| Element | Max Chars | Max Count |
|---------|-----------|-----------|
| Headline | 40 | 5 |
| Description | 90 | 5 |
| Business name | 25 | 1 |

### LinkedIn Sponsored Content

| Element | Max Chars |
|---------|-----------|
| Intro text | 600 |
| Headline | 200 |

### Meta Ads

| Element | Max Chars |
|---------|-----------|
| Primary text | 125 |
| Headline | 40 |
| Description | 30 |

### Reddit Promoted Posts

| Element | Max Chars |
|---------|-----------|
| Headline (post title) | 300 |
| Body (optional) | 500 |

### X/Twitter Promoted Tweets

| Element | Max Chars |
|---------|-----------|
| Tweet text | 280 |

---

## Campaign Naming Convention

Format: `Program | Base Name | Region | Objective | Targeting | Ad Format | Project | Funnel | Date`

Example: `Events | KubeCon NA 2025 | EMEA | Conversions | Intent | Search | cncf | MoFU | 2025-06-01`

The **`Project`** segment must be the project's **canonical LFX slug** (the same value used as `{projectId}`/slug elsewhere in LFX — e.g. `cncf`, `opensearch`, `tlf`), **not** a display name or an ad-hoc abbreviation. This is what the data pipeline joins on to attribute a campaign to the correct foundation, so it must match the LFX project source-of-truth exactly and deterministically.

> **Slug correctness caveat.** The correct slug is not always obvious from the display name — notably the Linux Foundation itself is `tlf`, *not* `LF` or `the-linux-foundation`. Campaigns are named by humans today, so historical/in-flight campaigns may carry an incorrect segment. Two mitigations: (1) when this service creates a campaign it should stamp the `Project` segment from the authenticated `{projectId}` rather than trusting free-text input, and (2) existing campaigns should be audited for slug drift before the naming segment is used as a hard join key. Until (2) is done, treat the segment as best-effort for legacy data.

---

## Data Structures

### CampaignBriefRequest (brief generation input)

```
url: string                     — Event/course page URL
platforms?: CampaignPlatform[]  — ['google-ads', 'linkedin-ads', ...]
programType?: 'events' | 'education' | 'membership'
campaignGoal?: 'event-registration' | 'training-certification' | 'membership-growth'
targetAudience?: string         — User-provided audience description
valueProp?: string              — Key value propositions
totalBudget?: number            — Total campaign budget (USD)
refineFeedback?: string         — For refine endpoint
previousCopy?: object           — For refine endpoint
```

### CampaignCreateRequest (campaign creation input)

```
eventName: string
eventSlug: string
countryCode: string
registrationUrl: string
hsToken?: string                — HubSpot UTM token
campaignTypes: CampaignType[]   — ['search'], ['demand-gen'], or both
budgetUsd: number
searchBudgetPct: number         — 70 = 70%
startDate: string               — YYYY-MM-DD
endDate: string                 — YYYY-MM-DD
keywords: CampaignKeyword[]
headlines: string[]             — Search RSA headlines (15 max)
descriptions: string[]          — Search RSA descriptions (4 max)
displayHeadlines?: string[]
displayDescriptions?: string[]
displayBusinessName?: string
displayCallToAction?: string
geoTargets: string[]            — ISO country codes ['US', 'JP']
project?: string                — Canonical LFX project slug (e.g. 'cncf', 'tlf'); used verbatim in the campaign-name Project segment. Should be derived from the authenticated {projectId}, not free-typed.
driveFolderUrl?: string
platforms?: CampaignPlatform[]
linkedInConfig?: object         — LinkedIn-specific params
redditConfig?: object           — Reddit-specific params
metaConfig?: object             — Meta-specific params
twitterConfig?: object          — X/Twitter-specific params
```

### JobCreateResponse (returned immediately from `POST .../campaigns`)

Campaign creation is asynchronous (see [Campaign Creation](#campaign-creation-implementation-phase)). The `POST` does not return campaign results; it returns a job handle to poll.

```
jobId: string                   — Poll GET /projects/{projectId}/jobs/{jobId}
status: 'queued'                — Initial status; always 'queued' on create
platforms: CampaignPlatform[]   — Platforms this job will create on (echoed from the request)
```

### JobPollResponse (returned from `GET .../jobs/{jobId}`)

```
jobId: string
status: 'queued' | 'running' | 'succeeded' | 'partial' | 'failed'
                                — 'partial' = some platforms succeeded, some failed
result?: PlatformResult[]       — Per-platform results, written once when the job
                                  reaches a terminal state (absent while queued/running)
error?: string                  — Terminal error, if the job failed as a whole
```

### PlatformResult (per-platform outcome, embedded in JobPollResponse.result)

```
platform: string        — Platform this result is for
ok: boolean             — Whether the campaign was created (or reused) successfully
campaignId?: string     — Upstream platform campaign id (present when ok)
error?: string          — Failure reason (present when not ok)
```

Per-platform errors are carried inside each `result` entry rather than in a
separate top-level array; the job's own start/finish times are available from
the job record's timestamps and are not echoed in the poll payload.

### CampaignCreateResult (future, richer per-platform result)

> Not yet emitted. Today the job result carries the minimal `PlatformResult`
> shape above (`platform`/`ok`/`campaignId`/`error`). Once the per-provider
> dispatchers land, each result is expected to grow into the richer shape below
> (counts, creation log, direct UI URL); this section documents that intended
> end-state, not the current payload.

```
platform: CampaignPlatform
type: CampaignType
campaignName: string
campaignId: string
adGroupCount: number
keywordCount: number
adCount: number
campaignUrl: string             — Direct URL to platform UI
steps: string[]                 — Step-by-step creation log
```

### CampaignMonitorResponse (returned from `GET .../{provider}/metrics`)

The response body for a single project's single-provider metrics call. `accountTotals` sums only *this project's* account on this provider — cross-provider and cross-project (TLF umbrella) roll-ups are assembled by the UI backend, not here.

```
campaigns: CampaignMetrics[]    — Per-campaign metrics (one row per campaign on this provider account)
accountTotals: AccountTotals    — Summed metrics across this project's campaigns on this provider
actionItems: ActionItem[]       — Pacing alerts and optimization suggestions
pulledAt: string                — ISO timestamp of data fetch
```

### Per-Campaign Metrics (shared across platforms)

```
campaignName: string
campaignId: string
status: string
impressions: number
clicks: number
ctr: number
spend: number
cpc: number
cpm: number
conversions: number
costPerConversion: number
dailyBudget: number
totalBudget: number
pacingPct: number
pacingLabel: string             — underspending | normal | constrained | overspending | severe
```

---

## SSE Event Types (Brief Generation)

| Event Type | Payload | Description |
|------------|---------|-------------|
| `status` | string | Progress message ("Scraping URL...", "Generating copy...") |
| `event` | object | Extracted event/course details |
| `hubspot_utm` | object | HubSpot UTM token found/created |
| `copy_token` | string | Token-by-token AI output (streaming) |
| `copy_done` | null | Copy generation complete |
| `copy_structured` | object | Parsed, validated ad copy JSON |
| `keywords` | array | Keyword list with match types |
| `linkedin_strategy` | object | LinkedIn targeting recommendation |
| `error` | string | Error message (may appear mid-stream) |
| `done` | null | Stream complete |
| `shutdown` | null | Server shutting down |

---

## Platform-Specific Gotchas

### Google Ads
- Budget is in micros: on **write**, multiply currency → micros (× 1,000,000); on **read**, divide micros → currency (÷ 1,000,000)
- No `campaign.start_date` / `campaign.end_date` in GAQL for API v23+
- Demand Gen campaigns use ad group level geo targeting (not campaign level)
- Duplicate campaign names cause creation failure; retry adds timestamp suffix
- RSA ads pin top 3 headlines for consistency

### LinkedIn Ads
- Images must be owned by org URN, not ad account
- `feedDistribution: NONE` required for dark posts (prevents company page visibility)
- Campaign groups must be ACTIVE status
- Budget as decimal string, not micros
- Timestamps in milliseconds
- Skills + Groups in one `or` block (separate AND blocks = too narrow)
- `callToAction` field not accepted; "Learn More" is automatic for article ads
- Idempotency: search by name across all statuses before creating
- Exclude employers: LF (`urn:li:company:33275771`) + CNCF (`urn:li:company:12893459`)

### Meta Ads
- ISO geo codes for targeting
- Objective-to-parameter mapping varies by campaign type

### Reddit Ads
- Token refresh with expiry buffer (tokens expire; must refresh before expiry)
- Subreddit targeting uses subreddit **names** (the `r/` prefix stripped), not `t5_` IDs — the Ads API `communities` field rejects `t5_` values as "invalid communities" (matches the reference TS implementation, which sends the stripped names directly); if any supplied name is invalid the ad-group create falls back to keyword/geo-only targeting with a warning rather than orphaning the PAUSED campaign
- Account must be whitelisted in runtime config

### X/Twitter Ads
- OAuth 1.0a with HMAC-SHA1 signing (not OAuth 2.0)
- 1 request/second write rate limit
- Exponential backoff retry on 429 responses
- Only "lf-events" account currently supported

### HubSpot
- UTM lookup uses fuzzy name matching with scoring
- 15-second HTTP timeout per call
- If unavailable, falls back to campaign slug for UTM
