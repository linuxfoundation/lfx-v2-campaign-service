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

A **built campaign audience** is a pointer + provenance to a platform-side audience (its master-list id, applied suppression lists, and a human-readable inclusion summary) — not the audience's contents. It is a **collection** subordinate to a brief (a brief may drive several audiences over time / per platform). Writes are gated on `campaign_manager` and use optimistic concurrency: reads return an ETag, and `PATCH` requires `If-Match` (`428` when missing, `412` on mismatch). `PATCH` is a load-then-merge — a nil field is left unchanged; a non-empty `suppression_list_ids` replaces the set, and the explicit `clear_suppression_lists` boolean removes all (an empty array can't round-trip through the generated client's `omitempty` tag, hence the flag).

Because these paths nest under `/briefs/{briefId}/`, they inherit the gateway wiring already in place for briefs: the HTTPRoute `briefs(/.*)?` path match forwards them, and the single Heimdall `project-api` rule (`/projects/:projectId/briefs/**`) authorizes them on `campaign_manager` — no separate route or rule entry is needed (LFXV2-2783). The route/rule parity test pins explicit audiences paths so a future narrowing of the briefs match/rule can't silently unroute or de-authorize them.

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
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/status` | `campaign_manager` | JSON | Toggle campaign ACTIVE/PAUSED (Reddit, Meta, LinkedIn; X/Twitter + Google Ads follow once their dispatchers land). |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions` | `campaign_manager` | JSON | Pause/remove Google Ads keywords for this campaign. |

**Tentative** (later phases, same nesting + `campaign_manager` gating): budget adjust, bid-strategy change, per-keyword bid, ad/creative rotation, ad-copy edit, geo-target edit, audience edit, negative keywords, bid modifiers, scheduling, flight-date change. Cross-platform budget reallocation, if built, is modeled as a first-class per-project resource with its own single-target mutations — not a bulk endpoint.

### Platform Connections (new — typed per provider, singleton per project)

A connection is **singleton per provider per project**: a project holds at most one connection of any given provider (one Google Ads account, one LinkedIn ad account, …). Multiplicity of accounts across the Linux Foundation lives at the **project** level, not inside a project — CNCF, OpenSearch, and TLF are each their own project, each owning its own single connection per provider. (TLF is both an umbrella over child-foundation projects *and* its own project with its own account; it owns only its own connection. Cross-foundation roll-up is a read concern handled by the UI backend — see the Monitoring note below — not by holding multiple connections on one project.)

Because the connection is a singleton, there is **no service-generated `{id}` in the path** — the provider name *is* the identity within the project. The path token is the **same provider key used everywhere else in this service** (`google-ads`, `linkedin-ads`, …, and `hubspot` for the non-ads provider), so the mapping is consistent end-to-end: path `connection-google-ads` → table `google_ads_connections`. Connections are strongly typed per provider (see [channel-connections-schema.md](channel-connections-schema.md)). The table below shows the pattern for `google-ads`; every provider (`linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`, `hubspot`) exposes the identical shape with its own typed payload.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Create the project's Google Ads connection (`409 Conflict` if one already exists). `projectId` MUST be a canonical slug, not a UUID (see the slug note below). |
| GET | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Get the connection (credentials redacted); returns ETag. |
| PUT | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Replace connection config (requires `If-Match`; does not set credentials). |
| DELETE | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Remove the connection (soft delete). |
| POST | `/projects/{projectId}/connection-google-ads/test` | `campaign_manager` | JSON | Verify credentials against the provider. |
| POST | `/projects/{projectId}/connection-google-ads/set-credential` | `campaign_manager` | JSON | Replace the stored (encrypted) credential. Split out from `PUT` so credential replacement is independently permissioned/audited. Not "rotate" — the service does not generate/swap secrets upstream. |

> **Create requires a canonical slug `projectId`.** The connection is stored keyed by `project_id`, which is the EXACT-MATCH key for the dispatch lookup, and brief/campaign create already require a canonical slug — so a UUID-keyed connection could never be joined to a dispatched campaign. `POST` (create) therefore rejects a UUID `projectId` with `400` (Pattern `^[a-z0-9]+(-[a-z0-9]+)*$`, MaxLength 35). The generated HTTP request decoder validates the pattern/length for these create routes, and the service applies the same guard for direct/non-HTTP callers (belt-and-suspenders). `GET`/`PUT`/`DELETE`/`test`/`set-credential` stay permissive (UUID-or-slug) to keep historical UUID-keyed rows reachable.
>
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
metaConfig?: object             — Meta-specific params (see MetaConfig below)
twitterConfig?: object          — X/Twitter-specific params (see TwitterConfig below)
```

#### MetaConfig (the `metaConfig` object)

Meta (Facebook/Instagram) per-platform config. **Budget is in the ad ACCOUNT's currency**, not USD — the service does no FX conversion.

```
budget: number                  — Whole units of the account currency (e.g. 2500 = 2500 USD/JPY/…).
                                  Must be POSITIVE and round to at least one minor unit; a budget
                                  that fails this is rejected by the client during dispatch (a
                                  pre-create job failure, since CreateCampaigns is async).
lifetimeBudget?: boolean        — true → lifetime budget over the flight; false/absent → daily budget
startDate: string               — YYYY-MM-DD. Must NOT be before today (UTC).
endDate: string                 — YYYY-MM-DD. Must be STRICTLY AFTER startDate. (Both date rules are
                                  enforced by the client during dispatch — a violation fails the
                                  platform job pre-create, not a synchronous 4xx.)
objective?: string              — awareness | traffic | engagement | leads | conversions.
                                  Omitted or blank → defaults to `traffic`.
                                  NOTE: `leads` is INTERIM — it runs a website-traffic campaign
                                  (OUTCOME_TRAFFIC optimizing for LINK_CLICKS to the registration
                                  URL); it does NOT create an on-Facebook instant lead form. Full
                                  LEAD_GENERATION parity is deferred (LFXV2-2665).
geoTargets?: string[]           — ISO country codes, e.g. ['US', 'JP']. Optional: omitted or an
                                  empty list defaults to ['US']. Supplied entries are uppercased,
                                  trimmed, and filtered to valid ISO-2 codes; if entries were
                                  supplied but NONE survive validation the request is REJECTED
                                  (it does not silently fall back to US). The client also DROPS
                                  Meta-ineligible countries: comprehensively sanctioned ones (IR,
                                  CU, KP, RU, …) are removed by validation, and regulated markets
                                  (SG, TW, KR) are filtered out during dispatch with a note — so a
                                  request naming only ineligible/regulated countries is rejected,
                                  and a mixed list proceeds with just the eligible entries.
pixelId?: string                — Meta pixel id. REQUIRED (non-empty, NUMERIC) for the
                                  `conversions` objective — it becomes the promoted-object pixel; a
                                  missing or non-numeric pixelId fails the dispatch job pre-create.
                                  Ignored by the other objectives.
currencyOffset?: number         — Account minor-unit scale (1 for zero-decimal currencies like JPY,
                                  100 for most). Must be a NON-NEGATIVE INTEGER: it is decoded as an
                                  int64, so a fractional value fails config decoding and a negative
                                  value is rejected as malformed. 0/omitted → derived by the client.
                                  This is a FALLBACK, not an unconditional override:
                                  the client's preflight derives the offset from the account's ISO
                                  currency and that is AUTHORITATIVE — a supplied value is used only
                                  when the currency can't be determined, and a supplied value that
                                  CONFLICTS with a recognized account currency is REJECTED by the
                                  client during dispatch rather than trusted. Since CreateCampaigns
                                  is async (a 202 is returned first), that rejection fails the
                                  platform job BEFORE any mutating Meta call — a pre-create dispatch
                                  failure, not a synchronous 4xx on the campaign request. Omit it
                                  unless the account currency is unrecognized.
placements?: object             — Which feeds to run on; ALL keys optional booleans. Keys are the
                                  Go field NAMES (no lowercase json aliases): FacebookFeed,
                                  InstagramFeed, Stories, Reels, AudienceNetwork, MessengerInbox.
                                  Omitted → the client's default (both feeds enabled).
                                  At least ONE supported placement must remain enabled after your
                                  overrides — e.g. `{FacebookFeed:false, InstagramFeed:false}` with
                                  nothing else enabled is REJECTED (the dispatch job fails pre-create).
                                  NOTE: `MessengerInbox: true` is REJECTED — Meta removed the
                                  Messenger Inbox placement (Nov 2025), so the client fails the
                                  dispatch job pre-create if it is enabled. Leave it false/omitted.
variants: AdVariant[]           — One ad per variant; at least one is required.
```

`AdVariant` (an entry in `variants`):

```
primaryText: string             — Required; non-empty; at most 125 runes
headline: string                — Required; non-empty; at most 40 runes
description?: string             — At most 30 runes
```

Copy limits are enforced by the client before any upstream call, so a variant that
exceeds them fails the platform job pre-create (async — not a synchronous 4xx). The
composed ad-creative NAME (`<eventName> - Variant N`) is also capped at 255 runes and
rejected pre-create, so keep `eventName` well short of that so the suffix fits.

Connection prerequisites (from the Meta connection, not this config): a valid `account_id`
(`act_<digits>`) and a numeric `page_id` — both REQUIRED, format-validated, and length-bounded
(`MaxLength 64`) at connection creation (a missing/malformed/over-long value is a 4xx there, not
a runtime dispatch failure).

Destination URL: the ad points at the brief's registration URL. The Meta client validates it
before any upstream create — it must be an absolute **HTTPS** URL with a real hostname, carry NO
embedded userinfo/credentials, and have a cleanly parseable query. A URL that violates these
fails the dispatch job pre-create (the brief endpoint accepts any string; this is enforced at
dispatch, not at brief creation).

#### TwitterConfig (the `twitterConfig` object)

X (Twitter) per-platform config. **Budget is in the ad ACCOUNT's currency**, not USD — X
serializes it as `daily_budget_amount_local_micro`, interpreted in the account's local currency;
the service does no FX conversion.

```
budgetAmount: number            — DAILY budget in whole units of the account currency (e.g. 500 =
                                  500 USD/JPY/…). Must be POSITIVE; a non-positive or non-finite
                                  value is rejected by the client during dispatch (a pre-create job
                                  failure, since campaign creation is async).
startDate: string               — YYYY-MM-DD. Must be in the future by at least a few minutes
                                  (a start too close to now can cross UTC midnight before the
                                  line-item POST and orphan the campaign, so it is rejected).
endDate: string                 — YYYY-MM-DD. Must be STRICTLY AFTER startDate. (Both date rules
                                  are enforced by the client during dispatch — a violation fails the
                                  platform job pre-create, not a synchronous 4xx.)
tweetId?: string                — An existing promotable tweet id to promote. Omitted → the
                                  manual-tweet workflow: the campaign + line item are created and
                                  the operator attaches the promoted tweet manually (the result
                                  carries a warning + the sanitized destination URL). A create that
                                  can't confirm the promoted-tweet association is reported as an
                                  UNCONFIRMED degraded outcome, not a clean success.
```

Connection prerequisites (from the X connection, not this config): the OAuth1 4-tuple (consumer
key/secret + access token/secret), plus an `account_id` AND a `funding_instrument_id` — both
REQUIRED, both ALPHANUMERIC (`^[A-Za-z0-9]+$`, e.g. `account_id` `8r7gb`), and both
pattern/length-validated (`MaxLength 64`) at connection creation. The X client requires both and
interpolates them into the account-scoped request path, so a missing/malformed value is rejected as
a 4xx at connection creation rather than surfacing as an asynchronous dispatch failure.

Destination URL: the ad points at the brief's registration URL. The X client validates it before any
upstream create — it must be an absolute **http/https** URL with a real hostname and carry NO
embedded userinfo/credentials; a violation fails the dispatch job pre-create. Validation errors
redact the URL (scheme+host+path only) so a persisted error can't leak a userinfo/query secret.

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
