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

   *Note on `GET /briefs?event_slug=`:* this is a **keyed item read, not a list**, so it does not breach the no-list-endpoint rule. `uq_campaign_briefs_project_event` is a UNIQUE index on `(project_id, event_slug) WHERE status <> 'archived'`, so the lookup can match **at most one** brief — the same one-item-plus-ETag shape as `GET /briefs/{id}`, which this rule explicitly retains for conditional updates. It exists because the event slug, not the brief id, is what a caller holds when re-visiting an event page: the UI derives the slug from the pasted URL and must answer "have I already generated a brief for this event?" before generating a new one. It returns no collection, no pagination, and no filtering.
4. **Create and replace are separate; replace requires `If-Match`.** There is no "create-or-update" endpoint. A `PUT` (replace) requires an `If-Match: "<version>"` header carrying the current ETag; the caller must have fetched the current version first. Mismatches return `412 Precondition Failed`; a missing header returns `428 Precondition Required`. (Optimistic-locking pattern per [committee-service / 2026-05-CloudNativePG](https://github.com/linuxfoundation/lfx-architecture-scratch/tree/main/2026-05-CloudNativePG).)
5. **No bulk mutation endpoints.** Bulk status/budget changes are omitted: HTTP cannot cleanly express partial success/failure across a set, and a single bulk call cuts across per-target permission boundaries. Each mutation is scoped to one permission-evaluated target.

Resource pseudotypes declared into the global indexer namespace: `campaign_brief` and `campaign`. **Connections are not indexed** — they are singleton per project with no cross-project listing consumer, so they are read directly (not via the Query Service). See [architecture.md](architecture.md) for the full type-name and relation catalog.

### Brief Lifecycle (Planning Phase)

Briefs are subordinate to a project. AI generation currently runs SSE-streamed in the Express BFF and will migrate later; the persistence/CRUD surface below is what this service owns.

A brief is the funnel unit: it carries the **program** (`program_type` = events / education / membership) that sets the funnel context, and it is **shared across channels** — one brief drives many channel campaigns (see next section), each a row under the brief with the same `brief_id`. Program is a field on the brief, not a separate resource.

**Platform selection lives on the campaign, not the brief.** A brief may carry a *suggested* default set of platforms (a planning hint used to pre-populate the campaign form), but the binding choice of which platforms to launch on — and each platform's configuration — is made at campaign-creation time. The generation strategy is driven by `program_type`, not by which channels a brief was drafted against, so a single approved brief can be launched on any subset of platforms.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/fetch-event-url` | `campaign_manager` | JSON | Fetch an event page and return the metadata extracted from it, for pre-filling a brief form. **Creates and persists nothing** — the caller reviews the result and submits it through `POST /briefs`. `POST` rather than `GET` because the URL is a request *body* parameter: as a query parameter it would be written verbatim into access logs, proxy logs and browser history at every hop. Answers `400` for a URL that is malformed, resolves to an address this service will not connect to, or yields no event name (see the SSRF notes in `docs/knowledge/code/internal-platform-eventurl.md`), and `503` when the origin does not answer. The result names the extraction strategy in `extracted_from` (`jsonld`, `opengraph` or `fallback`) — the whole record comes from exactly one of them. |
| POST | `/projects/{projectId}/briefs` | `campaign_manager` | JSON | Create a brief. |
| GET | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Get a brief (full copy, keywords, targeting); returns ETag. |
| GET | `/projects/{projectId}/briefs?event_slug=` | `campaign_manager` | JSON | Find the saved brief for an event slug; returns ETag. `404` when the event has no brief yet (the ordinary first-generation case). The slug has no upper bound here, matching `BriefWriteInput` (the create/update payload) and the `TEXT` column, so any brief the create contract accepts is recallable; it must be non-empty, as on create. See the D5 note below. |
| PUT | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Replace a brief (requires `If-Match`). |
| POST | `/projects/{projectId}/briefs/{brief_id}/refresh` | `campaign_manager` | JSON | Re-run generation against latest event data, producing a new version. |
| POST | `/projects/{projectId}/briefs/{brief_id}/approve` | `campaign_manager` | JSON | Approve a brief for campaign creation (requires `If-Match`; approval is version-gated so a brief replaced since it was fetched cannot be approved on stale content). |
| POST | `/projects/{projectId}/briefs/{brief_id}/email-copy` | `campaign_manager` | JSON | Generate AI-written email copy (subject, preheader, body, CTA) for the brief. Returns immediately with generated text; does NOT persist to the brief. The AI model is optional — without it configured this endpoint returns 503. Requires valid brief event details (event name). |
| DELETE | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Archive a brief (soft delete). |

> Listing briefs and viewing a brief's version history are served by the Query Service, not by dedicated endpoints here.

### Campaign Creation (Implementation Phase)

A campaign is subordinate to a brief. This is a **collection** under the brief (a brief may drive multiple campaigns across platforms). The `POST` body carries the **selected platforms** and their per-platform config (see `CampaignCreateRequest`). Creation is **asynchronous**: the upstream ad platforms take seconds-to-minutes to provision, so `POST` returns immediately with a `jobId` (a `JobCreateResponse`), and the caller polls `GET .../jobs/{jobId}` for a `JobPollResponse` until the job is terminal. One execution record is persisted per platform.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns` | `campaign_manager` | JSON | Create campaigns across the platforms selected in the body (async → `JobCreateResponse` with `jobId`). Persists one execution record per platform. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/adopt` | `campaign_manager` | JSON | **Bind an ad campaign that already exists upstream** to this brief, without creating anything on the ad platform. Synchronous (no job): the platform is read once, and on success the campaign row is written in the same request. `platform_campaign_id` is verified against the project's own connection before anything is persisted — a `404` means the platform answered and there is no such campaign; a `503` means the campaign could not be **verified** and its existence is **unknown** — the platform may have been unreachable, or it may have answered with something untrustworthy (an unhonoured id filter, an undecodable row, an unrecognised status), which is why the message names verification rather than connectivity. `400` for a platform with no adoption support, an unapproved brief, an unknown platform, or a blank or malformed `platform_campaign_id` (**malformed IDs are validated before the connection state is checked, so a permanent input fault always returns `400` regardless of connection availability**); `409` when the brief already has a live campaign on that platform, when that upstream campaign is already bound to a **different** brief — **in any project**, because Google Ads is one shared upstream account across every foundation, so a project-scoped check would let two projects bind and then fight over one live campaign; the message names the campaign but not the other project, which the caller may not be entitled to see — when the brief lost its approval during the platform read, when the project's connection is unusable, or when the project has **no connection of its own** — adoption is the one path that cannot fall back to the shared LF system account, because many projects share that one ad account and the caller names an arbitrary campaign inside it. There is deliberately **no `500` for an unusable LF system connection** on this endpoint, unlike the metrics and toggle endpoints: adoption resolves the project's own scope only and never loads the LF row, so the answer for a project without its own connection is the same actionable `409` whatever state that row is in. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Get one campaign execution; returns ETag. |
| PUT | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Replace a campaign execution (requires `If-Match`). |
| DELETE | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Delete a campaign (soft delete; requires `If-Match`). **Local only — does NOT touch the ad platform.** Frees the campaign's `(brief, platform)` slot so the brief can be re-dispatched to that platform. `409` if the campaign is mid-dispatch. |
| GET | `/projects/{projectId}/jobs/{jobId}` | `campaign_manager` | JSON | Poll campaign creation job status (`JobPollResponse`). |

**Deleting a campaign.** A campaign row occupies its brief's slot for one platform — the `(brief_id, platform, variant)` uniqueness that makes dispatch idempotent (a retry cannot create a second paid campaign upstream) also means a campaign created with the wrong budget, or one whose upstream create failed ambiguously, would block that pair forever. `DELETE` frees the slot: the row is soft-deleted (`status = 'deleted'`) and excluded from the partial unique index, so a re-dispatch to the same `(brief, platform, variant)` succeeds while two *live* campaigns for the pair are still rejected. The soft-deleted row is retained deliberately — it holds `platform_campaign_id`, the only local pointer to a campaign that may still exist upstream — and becomes invisible to reads (`GET` returns `404`).

> **The ad platform is not touched.** This service has no verified campaign-delete API for any provider, so `DELETE` never deletes, pauses, or modifies the campaign on the ad platform. **A campaign already created upstream keeps running and spending until it is stopped there.** Pause it first via the status-toggle endpoint, or stop it in the platform's own console. Deleting a campaign that is mid-dispatch (`status = 'pending'`, an active dispatch claim) returns `409`: freeing the slot under an in-flight dispatch could let a concurrent claim double-create upstream.

**Adopting a campaign.** Not every campaign a foundation runs was created here: a team that launched in Google Ads' own console before onboarding, or during an outage, still needs the campaign under a brief so metrics, the status toggle and delete reach it. `POST .../campaigns/adopt` is that path, and it is deliberately **not** an upsert. Adoption names an arbitrary upstream campaign, so an updating conflict arm would repoint an existing binding at a different campaign and orphan the one it used to name — which this service cannot stop, because it never deletes or pauses upstream on its own. A pair that is already bound is refused with `409`; free the slot with `DELETE` first if the binding is genuinely wrong. Note also what adoption is *not*: an ownership check. Within a shared ad account a project can name a campaign another project created, and no rule here can prevent that, because that project's stored credential already grants read and pause on everything in the account straight through the provider's API. Account tenancy is where that boundary lives; what this service enforces is its own invariant, one upstream campaign to one brief.

> **Absence and unavailability are different answers, and the distinction is load-bearing.** An operator who is told "no such campaign" reasonably goes and creates one — so a lookup that could not be verified must never be reported as absent. The service returns `404` only when the platform positively answered that the campaign is not there, and `503` for every unverifiable outcome (transport failure, an unhonoured filter, an undecodable or unrecognised response). The id written to the row is the one the **platform echoed back**, not the one requested, so a platform that ever answers with a different campaign cannot have the requested id recorded against it. The stored `status` is this service's own lifecycle value (`created`), never the platform's `ENABLED`/`PAUSED`. **No endpoint here reads the platform's run state back** — the metrics read returns impressions, clicks, cost and CTR and nothing else. Run state is *set* through the status toggle, which persists `active`/`paused` on the row once the platform confirms, so the row reflects the last toggle this service performed; a change made in the platform's own console is visible only there.
>
> **What an adopted campaign supports.** Metrics reads, `DELETE`, and pausing through the status toggle. **Activating** it does not work and is not meant to: on Google Ads the toggle refuses `ACTIVATE` unless the row carries the ad-group, ad and keyword-criterion ids proving targeting was provisioned, and adoption records only the campaign the platform was asked about — it does not walk the campaign's children. An adopted campaign therefore reports `ErrCampaignNotProvisioned` on activate. That is the honest answer rather than a gap: this service has not verified that the campaign can deliver, and the guard exists precisely to stop it reporting a successful activation of something that cannot serve. Activate it in the platform's own console, or dispatch a campaign through `POST .../campaigns` to get one this service provisioned end to end.

> **Adoption support is per-platform and optional.** It is a capability the dispatcher may implement, not part of the dispatch contract, so platforms gain it independently. Google Ads supports it today; every other platform answers `400`.

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
| POST | `/projects/{projectId}/briefs/{briefId}/audiences/build` | `campaign_manager` | JSON | Build the brief's HubSpot audience: derive the regional-expansion inclusion lists, create them, and record the master list (`202`). `400` when the brief is not approved or its details lack an event name/country; `500` when the brief's HubSpot connection is missing; `503` when the audience-build dependencies (brief repository, HubSpot/Snowflake builder) are unconfigured; `409` in two distinct forms, below. The LF system-account fallback does **not** apply here — it is scoped to paid-ads providers, so a project without its own HubSpot connection still fails rather than writing contact lists into the LF portal. Snowflake enrichment is optional; builds proceed country-only if unavailable. Until an audience is `built`, the email channel cannot dispatch. |

**The two 409s on build-audience carry OPPOSITE remedies, so the status code alone is not
enough — a client that keys on it will do the wrong thing for one of them.** Key on the
`reason` field of the error body, which is a stable slug: `stale_approval`,
`audience_build_in_flight`, or `already_exists`. Do NOT key on the message text — it is
reworded whenever an operator finds it unclear, and the wording below has already changed
twice. Today `reason` is populated by the audiences endpoints only — the briefs endpoints
distinguish their conflicts in message prose and set no slug yet. Absent means unspecified:
fall back to the message there.

| `reason` | Message contains | Cause | Remedy |
|---|---|---|---|
| `stale_approval` | "the brief changed while its audience was being built; refresh and rebuild" | The brief was re-edited or re-approved between the build claiming its lease (which locks the brief and records the approved version) and the last check before the first HubSpot call. A brief that simply was NOT approved when the build started is a `400` naming its status, and a brief that is missing or archived is a `404` — neither is this. | Re-read the brief and rebuild. |
| `audience_build_in_flight` | "an audience build for this brief is already in progress" | Another build for this `(brief, platform)` holds the build lease — migration `000018`'s partial unique index over `status = 'building'`. | **Wait**, then re-read the audience list. Do NOT rebuild: the in-flight build is creating real HubSpot lists, and a second one creates a complete duplicate set that nothing downstream can tell apart. If the holding build is genuinely dead, reconcile its lists FIRST and only then `PATCH` its row to `failed` — failing it frees the lease at once, so doing it first admits the next build while the dead build's lists are still in the portal. **Do not treat an empty `inclusion_summary` as "nothing to reconcile":** the claim inserts with an empty summary and ids are recorded only after the lists are created, so the crash-mid-build case is exactly the one that leaves real lists and an empty row. Every list a build creates is suffixed with the first 8 characters of its audience row id in parentheses — search the portal for that prefix, and use `inclusion_summary` as a supplement to it. |

A build that dies mid-flight leaves its row at `building` and keeps holding the lease. That is
intentional — its lists exist upstream, so building again is exactly the duplication being
prevented. There is no automatic takeover. An operator reconciles the portal, then frees the slot
with `PATCH .../audiences/{audienceId}` setting `status` to `failed`; the next build proceeds as a
new row.

### Monitoring (Insights Phase)

Metrics are read-through from the ad platforms, scoped by project. There are no per-platform root paths; the provider is a path segment under the project. Because a connection is singleton per project, `/{provider}/metrics` unambiguously means "metrics for **this project's** account on that provider".

> **Reading ONE campaign's metrics is not here.** It is `GET .../briefs/{briefId}/campaigns/{id}/metrics`, documented once in [Optimization](#optimization) below. It sits on the campaign-scoped path rather than under `{provider}/metrics` because it needs the persisted campaign row (for its platform and `PlatformCampaignID`), not a provider+project pair — so it belongs with the other campaign-scoped actions.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/{provider}/metrics` | `campaign_manager` | JSON | Campaign metrics for this project's account on the provider (`days` param, default 14). `{provider}` ∈ `google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`. |
| GET | `/projects/{projectId}/google-ads/keywords` | `campaign_manager` | JSON | Google Ads keyword performance (top 50 by impressions). |
| GET | `/projects/{projectId}/google-ads/audience` | `campaign_manager` | JSON | Audience demographics (age, gender, device). |

> **Umbrella roll-up (TLF across child foundations).** This service never aggregates across projects: `campaign_manager` does not cascade from parent to child (see rule 2), and each project owns only its own connection. A TLF-wide view of, say, all Google Ads spend is assembled by the **UI backend**, which fans out one `GET /projects/{child}/google-ads/metrics` per child foundation the caller has access to and sums the results. Keeping aggregation out of this service preserves the strict per-project permission boundary and avoids the service resolving project hierarchy.

> There are no `/{provider}/accounts` listing endpoints **under this Monitoring surface**. A project has at most one connection per provider, read directly via `GET /projects/{projectId}/connection-{provider}` (connections are not indexed into the Query Service — see the Platform Connections section). This is a statement about *stored* connections and does not forbid `GET /projects/{projectId}/connection-google-ads/accounts`, which is a different thing: a live, credential-scoped read of the accounts reachable **upstream at the provider**, used to choose which account the single connection should point at. It enumerates nothing this service stores.

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
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/status` | `campaign_manager` | JSON | Toggle campaign ACTIVE/PAUSED (Reddit, Meta, LinkedIn, X/Twitter, Google Ads, Microsoft Ads). **409** when the change is refused before the platform is contacted: the campaign is unprovisioned, or the connection row itself is unusable — no stored credential blob, or one too short for the encryptor to authenticate. Those are non-retryable, which is why none of them is a 503. **Four further 409 reasons come from the adapter's own pre-flight**, each tagged with `ErrConnectionNotUsable`: `reason=connection_inactive` (the row's status is not `active`), `reason=credentials_undecodable` (the decrypted blob is not valid JSON), `reason=credentials_incomplete` (a required credential field is empty) and `reason=account_not_selected` (credentials stored, no ad account chosen yet). **Google Ads, Reddit, X/Twitter and Microsoft Ads emit all four.** **Meta emits the first three** (`resolveMetaCredentials`, `internal/dispatch/meta.go`) **and deliberately not the fourth**: a status update targets the campaign node by platform id and never reads `AccountConfig.AccountID`, so an account cleared after creation must not block pausing or resuming. That guard is `requireMetaAccountID`, and it is reached only from Dispatch. **LinkedIn emits all four as of LFXV2-3196** — `resolveLinkedInCredentials` mirrors `resolveMetaCredentials`, replacing the inline checks that previously fell through to 503. Unlike Meta it DOES emit `account_not_selected` on this path: LinkedIn's client is constructed with a `RuntimeConfig` naming the account, so an empty account id cannot reach the platform at all, whereas Meta targets the campaign node by platform id and never reads the account. **Two reasons are Google-Ads-only on this path**: an account mismatch (the campaign belongs to a different ad account than the connection now resolves to), since Google Ads is the only adapter that verifies account identity, and `reason=provider_config_invalid` (the stored `login_customer_id` is not digits-only, so no manager id can be sent) — Meta raises `ErrProviderConfigInvalid` too, but only from Dispatch, never from a toggle. **500** is reserved for defects nobody but an operator can act on, and there are now two: the project has no connection of its own, fell back to the LF system row, and THAT row is unusable; or the stored credential blob failed GCM authentication (`ErrCredentialDecryptionFailed`), meaning the application's encryption key no longer matches it — a rotated `CREDENTIAL_ENCRYPTION_KEY` or a corrupted row, and this path cannot tell them apart — re-saving credentials repairs the corrupted row, but no reconnect touches a rotated key, so the answer is the conservative one. **404** is the third permanent answer, added alongside them: no connection row exists for this project and provider at all, and the shared system row did not cover it either — there is nothing to repair, so the caller is told to connect rather than to fix. It is deliberately not a 409 — a 409 tells the caller to repair "this project's connection", which is a scope they do not own and cannot address. The `reason` token is logged, never returned. **One request succeeds without persisting anything**: pausing a campaign in `created_degraded` pauses it upstream and returns **200** with the status and ETag UNCHANGED, no version bump and no index event. `created_degraded` records that the campaign's wiring was never verified and the row has a single status column, so writing `paused` would spend the reconciliation marker to record a run state the ad platform already holds authoritatively — and pausing reconciles nothing, it stops spend. Activating such a campaign is refused with **409**. A caller that needs to confirm the pause reads it from the ad platform, not from this row. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/metrics` | `campaign_manager` | JSON | Read live performance metrics (impressions, clicks, cost, CTR) for one campaign directly from the channel that runs it — an ad platform, or HubSpot for the email channel. Pure read — never persisted, unlike `GET .../campaigns/{id}`. `window` query param (`today`, `yesterday`, `last_7_days`, `last_14_days`, `last_30_days`, `this_month`, `last_month`; default `last_30_days`, except X Ads which defaults to `last_7_days` since its stats endpoint caps queryable ranges at 7 days) is a closed, platform-agnostic vocabulary — each dispatcher maps it to its own platform's date-range dialect. A platform with no `MetricsReader` wired returns 400. **409 covers two different moments, and the distinction is operational, not "was the channel ever contacted."** The first group is refused before the TENANT-SCOPED METRICS REQUEST — the read that would actually return numbers — is attempted, and waiting will not change that. Every platform returns it when the campaign is unprovisioned (empty `PlatformCampaignID`), and when the connection row is unusable in one of the two ways the SHARED resolver detects — no stored credential blob (`reason=credentials_absent`) or a blob too short to authenticate (`reason=credential_blob_malformed`). These are genuinely pre-contact: nothing is called at all. **Google Ads, Reddit and X/Twitter** additionally tag the four defects their own pre-flight detects with `ErrConnectionNotUsable`, so those are 409 too: `reason=connection_inactive` (the row's status is not `active`), `reason=credentials_undecodable` (the decrypted blob is not valid JSON), `reason=credentials_incomplete` (a required credential field is empty) and `reason=account_not_selected` (a connection created with credentials only, whose ad account has not been chosen yet). **HubSpot tags the first three but not the fourth** — there is no ad account in an email connection to choose. **Meta tags the first three and not `account_not_selected`** — `ReadMetrics` resolves through `resolveMetaCredentials`, the same helper `ToggleStatus` uses, and like the toggle it targets an existing campaign by platform id via `GET /{campaignID}/insights` without reading `AccountConfig.AccountID`, so an account cleared after creation must not block reading metrics. **LinkedIn emits all four as of LFXV2-3196** — `ReadMetrics` resolves through `resolveLinkedInCredentials`, the same helper `ToggleStatus` uses. It emits `account_not_selected` here too, for the same reason: the client cannot be constructed without an account id. **`reason=provider_config_invalid` remains Google-Ads-only** — the stored `login_customer_id` is not digits-only, so no manager id can be sent. **Account-identity mismatch is emitted by Google Ads and HubSpot, the only two adapters that verify tenant identity, and they verify different things.** For Google Ads the campaign was created under a different ad account than the connection now resolves to; platform campaign ids are account-scoped, so reading one under the wrong account silently yields zeros or another account's numbers, and the fix is to reconnect the original account. That check resolves entirely from locally-stored account ids, so it is pre-contact in the strict sense. HubSpot's is not: a HubSpot email id is a bare numeric unique only within its portal, so the dispatcher records the portal the private-app token authenticates against at create time — read by POSTing the token to `/oauth/v2/private-apps/get/access-token-info`, not from the optional operator-supplied `portal_id` config, which a credential swap leaves untouched — and `ReadMetrics` calls `AuthenticatedPortalID` against that same endpoint to learn the token's CURRENT portal before it can compare. The channel IS reached, just not for the tenant-scoped metrics themselves. **The two HubSpot remedies differ, and the response message is what carries the distinction:** a recorded-but-different portal is `ErrCampaignAccountMismatch` and can be repaired by reconnecting the original portal; a row with no recorded portal at all — which is every campaign staged before this landed — is the narrower `ErrCampaignProvenanceUnknown`, and since there is nothing to reconnect to its message says to re-dispatch instead. That second case IS strictly pre-contact, unlike the mismatch: an absent recorded portal is decided from the row alone, so it is refused before `AuthenticatedPortalID` is called at all. The order matters operationally — checked after the lookup, a legacy row read while token-info was throttled would surface as the transient 503 below rather than this 409, offering "try later" for a row that only re-dispatch can fix. The refusal is deliberate rather than a best guess, because reading across a re-point is wrong in both directions — a same-numeric collision reports another portal's opens and clicks as this campaign's, and no collision reports "not sent yet" for an email that was sent. The second moment belongs to HubSpot alone: `GetEmailMetrics` — the tenant-scoped metrics call itself — succeeds and matches no sent email in the window, which `internal/dispatch/hubspot.go` tags with `domain.ErrNoMetricsInWindow` so it lands on 409 rather than the 503 default. Nothing is broken — a staged draft nobody has sent yet is the ordinary state of this channel between `Dispatch` and the send — so read this 409 as "no data", not as "repair your connection". The response body does not separate these cases (`ConflictError` carries only `code` and `message`), so the message text is what distinguishes them: the first group names the connection, the provisioning state, or (for HubSpot's identity checks) the portal; this one names the window. **500**, as on the status toggle, covers two operator-only defects on a scope the caller cannot address, so neither is a 409: the project has no connection, fell back to the LF system row, and that row is unusable; or the stored credential blob failed GCM authentication (`ErrCredentialDecryptionFailed`) — a rotated `CREDENTIAL_ENCRYPTION_KEY` or a corrupted row, and this path cannot tell them apart — re-saving credentials repairs the corrupted row, but no reconnect touches a rotated key, so the answer is the conservative one. **404**, also as on the toggle, is the third permanent answer: no connection row exists for this project and provider and the shared system row did not cover it, so there is nothing to repair and the caller is told to connect. Both were 503 before LFXV2-3065, which invited a retry that could never succeed. Support is per-platform (see below). |
| GET | `/projects/{projectId}/briefs/{briefId}/metrics` | `campaign_manager` | JSON | **Read every campaign on a brief in one request.** Same read-through as the campaign-scoped row above, fanned out across the brief's campaigns concurrently. `window` accepts the same closed vocabulary and applies to every campaign, with the same per-platform default fallback (X Ads cannot serve `last_30_days`, so with no window named its rows are read over `last_7_days` while the others use 30 — **each row reports the window IT was read over in `metrics.window`; the top-level `window` is the requested one and does not claim to cover every row**). **A per-campaign failure does NOT fail the request.** Every campaign gets a row, including unreadable ones, and each carries its own `status`: `ok` (the only status carrying `metrics`), `unsupported` (no `MetricsReader` for the platform, or the window exceeds what it can serve — the 400s of the campaign-scoped endpoint), `not_ready` (unprovisioned, or the platform reported no data in the window — includes the ordinary state of a staged email draft nobody has sent yet), `connection_problem` (unknown provenance, account mismatch, or an unusable connection — the operator must repair the connection; retrying will not help), and `failed` (the platform read itself failed; transient, retrying may help). **A non-`ok` row omits `metrics` entirely rather than carrying zeroes** — a zero is a measurement, and substituting one for a campaign that could not be read is indistinguishable from a campaign that genuinely served nothing. `reason` carries a fixed, consumer-safe sentence, never the adapter's error text, which can embed a platform response body or an operator-supplied account id. `ok_count` reports how many rows carry a measurement, so a consumer can see that a cross-campaign total covers 2 of 6 campaigns before presenting it. **There is no cross-channel cost total**: `cost_micros` is micro-units of each platform's own native currency and this service performs no FX conversion, so summing them would produce a figure with no currency. Request-level errors remain: `400` for an invalid `window` (refused before any platform is contacted), `404` for a missing or archived brief — a brief with no campaigns is **not** an error and returns an empty `rows` array, since that is what every brief looks like before it is dispatched. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions` | `campaign_manager` | JSON | Pause/remove Google Ads keywords for this campaign. |

**Per-campaign metrics-read support by platform**: this row documents the shared `MetricsReader` capability and endpoint; the remaining per-platform `ReadMetrics` adapters land in their own PRs.

| Platform | Supported windows |
|----------|-------------------|
| Google Ads | All seven. The adapter maps each window to the matching GAQL date literal (`last_30_days` → `LAST_30_DAYS`, and so on) behind an allow-list, so the platform-agnostic value never reaches the query as caller-supplied text. |
| Meta Ads | All seven. Each maps to a Graph Insights `date_preset` (`last_30_days` → `last_30d`, and so on) through a fixed allow-list, so an unrecognized literal fails locally rather than reaching Meta. |
| X (Twitter) Ads | `today`, `yesterday`, `last_7_days` only — the stats endpoint caps a queryable range at 7 days, so the wider windows return `400`. This is why X defaults to `last_7_days` rather than `last_30_days`. |
| LinkedIn Ads | `today`, `last_7_days`, `last_30_days`, `this_month`, `last_month`. `yesterday` and `last_14_days` return `400` — the Ad Analytics finder takes an explicit date range and these two have no mapping today. |
| Reddit Ads | `today`, `last_7_days`, `last_30_days`, `this_month`, `last_month`. `yesterday` and `last_14_days` return `400` — no date-range mapping today. |
| HubSpot (email) | All seven — but the window does NOT scope the counters. HubSpot's statistics span selects WHICH EMAILS are in scope by SEND date; the counters returned are that email's totals to date. `today` and `last_30_days` on an email sent this morning return the SAME numbers, and a window not containing the send date returns nothing at all (a **409**, not zeros). `window` in the response records what was ASKED, not a period the counters are scoped to. |

Reddit Ads is wired but **default-OFF**: its reporting contract is unverified (LFXV2-2995 — see the Reddit Ads notes below), so the adapter returns the same 400 as an unsupported platform unless the deployment sets `REDDIT_METRICS_ENABLED=true`. That is deliberate — a guessed request/response shape and currency unit returning 200 would look authoritative to every consumer, and the caveats are not carried in the response.

The **email channel adds an optional `email` object** to the response, present only for HubSpot campaigns and absent for every ad platform. It carries `sent`, `delivered`, `opens`, `clicks`, `bounces`, `unsubscribes`. The parent object's `impressions`/`clicks` mirror `opens`/`clicks`, and its `cost_micros` is always `0` — HubSpot bills no per-send cost, so that `0` is "not billed here", not "free". **It must never be blended into a cross-channel cost-per-acquisition**, which would divide real ad spend across email conversions and understate CPA.

Email adds one more **409** case, and it is the ORDINARY one rather than an edge: `Dispatch` stages the cloned email as a DRAFT for a human to send, so every read between staging and the send finds no sent email in the window. Three states arrive in that one upstream shape and cannot be told apart — sent outside the window, never sent, or no such email — so the message names all three instead of guessing. A 503 here would report an outage on a healthy integration.

Microsoft Ads is **not supported** — its only reporting surface is the Reporting API v13 (SOAP, async submit-then-poll-then-download), incompatible with this endpoint's single bounded synchronous call; see `docs/knowledge/code/internal-dispatch.md`.

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
| GET | `/projects/{projectId}/connection-{provider}/accounts` | `campaign_manager` | JSON | **This row stands for all four `AccountLister` providers** — `google-ads`, `meta-ads`, `linkedin-ads` and `microsoft-ads`; the Meta row below records only where Meta's own behaviour differs. Enumerate the ad accounts **reachable upstream with the stored credential**, so an operator can pick which one the connection should point at. Live read against the provider (Google Ads `customers:listAccessibleCustomers`); never persisted. Available for every provider whose dispatcher implements `AccountLister` — Google Ads, Meta Ads, LinkedIn Ads and Microsoft Ads today. The capability is optional per dispatcher, like `MetricsReader`, and **all of them share one handler implementation**, so the status mapping below cannot drift apart between them. Stated as the shape rather than a fixed list: the membership grows, and an enumeration is falsified by the next provider added without anything failing. **Status codes** (all four published by the Goa design): **400** when the platform has no account-discovery capability wired, **or** the stored connection exists but is not usable as it stands (inactive, incomplete credential blob, malformed stored config, or a credential blob too short to be valid ciphertext — none improve with time, so a 503 would be a false promise). **404 in two setup states, neither of them an outage.** The first is that NEITHER the project nor the LF system account has a connection — a project with no connection of its own falls back to the system row and gets a `200` listing the accounts the LF credential reaches, which is deliberate: those are the accounts its campaigns would actually run on. The second is that the project once connected and **explicitly disconnected**: `Delete` soft-deletes and `Get` filters the tombstone out, so a disconnect would otherwise be indistinguishable from never having connected and would silently earn the project the LF credential. `credsSource.systemConn` therefore probes `ConnectionRepo.Disconnected` first and refuses the fallback, so a disconnected project gets 404 **even when the LF system row exists and is perfectly usable**. That is the intended answer, not a gap: absence of a statement is what the fallback is for, and a statement to the contrary is not absence. A 503 in either case would tell the caller to retry something that cannot succeed until a connection exists. **500 in two cases**: a well-formed credential blob that fails authenticated decryption (a wrong/rotated application key, or a corrupted row — indistinguishable to GCM, and neither is something the caller can edit), and a fallback onto a system row that is itself unusable. The second is 500 rather than the 400 above because the 400 tells the caller to fix "the stored connection" and this caller HAS none — the reserved scope is unaddressable, so only an operator can act, and the failure is deployment-wide rather than about their project. **503** when the provider call fails — and also **before Google is called at all**, when the disconnect probe or the system-row fallback read fails against the database. That probe fails CLOSED: an unanswered "did this project disconnect?" is not a no, so the request is refused rather than resolved onto the LF credential. Those are genuine retryables, which is why they are 503 rather than the 404 above. **Serves first-time setup as well as re-pointing.** `GoogleAdsConnectionConfig` no longer declares `Required("account_id")` (nor does `MetaAdsConnectionConfig`, as of LFXV2-3061 — Google Ads and Meta are the two providers whose connection can be created credentials-first. LinkedIn and Microsoft gained the discovery endpoint in LFXV2-3064 but are NOT yet credentials-first, and it takes MORE than one change to make them so — naming only the bootstrap map would send the next change to do half the work. Both still declare `Required("account_id")` on their PUBLIC payloads (`LinkedInAdsConnectionConfig`, `MicrosoftAdsConnectionConfig` in `design/connection.go`), so an account-less connection cannot even be created over HTTP; `accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` separately gates whether an account-less SYSTEM row is installable; and LinkedIn additionally needs its create path to tag the missing choice, which `Dispatch` does not do today. All three gates, not just the map. Reddit and X have neither half), so the bootstrap is `POST .../connection-{provider}` with credentials only → `GET .../accounts` → `PUT .../connection-{provider}` with the chosen id. A connection between those steps stays `status=active` (discovery refuses a non-active connection, so any other status would dead-end the flow) and reports `account_id` as `""`. Operations that actually need an account report it non-retryably rather than as a 503, because waiting cannot fix a choice only a human can make — but the shape differs by endpoint, and **this paragraph describes Google Ads; Meta differs, see the row below**. For Google Ads the **status toggle** and **metrics read** need the account id (they share `validateGoogleAdsConnection` with create) and answer a synchronous **409** whose message names the missing account specifically (matched ahead of the general unusable-connection arm). **Campaign create** is asynchronous for both providers: it answers **202**, and the failure is NOT rendered into the job result — `dispatchPlatform` collapses every dispatcher error into the same `"platform campaign creation failed"` — so on the create path the reason token reaches an operator through the dispatch-failure **log line**, not through polling. No endpoint exposes a machine-readable `reason` field — `ConflictError` carries only `code` and `message` — so `account_not_selected` is a log/reason token, not part of the HTTP contract. The two mechanisms compose: a project that has connected nothing at all falls back to the LF system row, while a project that HAS connected credentials but has selected no account is served by its own row and never falls back — its connection exists, so there is nothing to fall back from. |
| GET | `/projects/{projectId}/connection-meta-ads/accounts` | `campaign_manager` | JSON | The same thing for Meta: enumerate the ad accounts reachable with the stored credential, so an operator can pick which one the connection points at. Live read against Graph `GET /me/adaccounts`; never persisted. Ids come back in `act_<digits>` form, ready to store as the connection's `account_id` verbatim. **Status mapping is literally the same code** — both handlers call one `listAccounts` helper parameterized by provider, so 400/404/500/503 cannot drift apart between them; only the caller-facing remedy text differs (Meta's names `access_token`, the field its set-credential payload carries, not `login_customer_id`). Two Meta-specific behaviours: (1) accounts Meta reports as **disabled, unsettled, pending review/settlement, in grace period or closed are RETURNED**, with the reason appended to the label (`"LF Events (disabled)"`), not filtered out — dropping them would answer "your token reaches no ad accounts" about an account sitting right there and send the operator hunting a permissions problem that does not exist; refusing to *create a campaign* on such an account stays where it already is, in the client's preflight. (2) An **incomplete enumeration is an error, never a short list**: a 2xx body with no `data` field, a `next` link with no cursor, a repeated cursor, or more than 2000 accounts all return a failure rather than what was collected, because a truncated list is indistinguishable from a complete one at the boundary and the caller acts on the absence. Meta's `paging.next` is never followed — it carries `access_token` and `appsecret_proof` as query parameters, so each page's path is rebuilt from the opaque `after` cursor instead, keeping the credential out of request URLs and error text. **Serves first-time setup as well as re-pointing, as of LFXV2-3061**: `MetaAdsConnectionConfig` no longer declares `Required("account_id")`, so the same `POST` (credentials + `page_id`) → `GET .../accounts` → `PUT` bootstrap Google Ads has is available here. What gated that was not the discovery endpoint but the other half — Meta's **campaign create**, the one Meta path that requires an account id (the status toggle and metrics read target the campaign node by id and need none), used to answer an empty account id with a generic error. `requireMetaAccountID` now tags it `ErrAccountNotSelected` + `ErrConnectionNotUsable`, so `unusableConnectionReason` names the missing choice as `account_not_selected`. **Where that reaches an operator is the log, not the job result** — create is queued work and `dispatchPlatform` collapses every dispatcher error into `"platform campaign creation failed"`. Meta gets no synchronous 409 from this sentinel the way Google Ads does, because Meta's status toggle and metrics read do not need an account id at all (they target the campaign node by id), so create is Meta's only account-needing path and it is the asynchronous one. |
| GET | `/projects/{projectId}/connection-hubspot/emails` | `campaign_manager` | JSON | Search the marketing emails reachable through the stored HubSpot connection, most-recently-updated first, so a caller can choose which one an email campaign will CLONE. Optional `q` matches name OR subject case-insensitively; **omitting it lists rather than fails**, which is the useful first screen for a picker nobody has typed into yet. Live read against HubSpot; never persisted. **This is a TEMPLATE picker, not an account picker** — and that is the one way it departs from the two rows above. A HubSpot connection is already scoped to the portal its private-app token authenticates against (`Client.AuthenticatedPortalID`), so there is no account to choose; what has no default is `hubspotConfig.SourceEmailID`, which campaign create REQUIRES, so without this endpoint the email channel cannot be driven from the UI at all. **Status mapping is the same code** — `classifyDiscoveryError`, lifted out of `listAccounts` unchanged, so 400/404/500/503 cannot drift between account discovery and this; only the operation noun differs ("email search could not be completed", not "account discovery"), and the 400's remedy names `private_app_token`, the PUBLISHED wire field of the set-credential payload — not `privateAppToken`, the Go/JSON shape the blob is persisted under, which no caller can send. One arm is NOT shared: a dispatcher with no `EmailSearcher` yields `ErrEmailSearchUnsupported` → 400, a separate sentinel from `ErrAccountsUnsupported` because the two capabilities are independent — HubSpot searches emails and has no ad accounts to enumerate, while the ad platforms are the reverse (they implement `AccountLister` and search no emails; Reddit and X implement neither, having no `ListAdAccounts` in their clients). **Draft emails are RETURNED**, with `state` on each row, for the same reason Meta returns disabled accounts: hiding the row the user is looking for answers "your portal has no such email" about an email sitting right there. **Archived emails are a different case and are simply absent** — HubSpot models archival as a separate `archived` flag rather than a lifecycle state, and the search does not request archived rows, so no `state` value can describe them. `state` is REQUESTED via `includedProperties`; the list endpoint does not return it by default, so a consumer promised a lifecycle state would otherwise have received an empty string from every row. The caller sees the state and decides. An empty result marshals as `[]`, never `null`, and a searcher returning `(nil, nil)` is rejected as a contract violation rather than reported as an empty portal. **A FILTERED search is a PAGINATED WALK** — the client follows `paging.next.after` so a match beyond the first page is not missed, up to `maxListPages` (200) sequential upstream requests, and the whole walk shares one 20s deadline. It is deliberately NOT truncated: a short list would answer "no such email" about an email on a later page, and the caller cannot tell a missing template from an absent one, so a 503 (recoverable) is preferred. **An UNFILTERED listing — an omitted or blank `q` — is BOUNDED to at most 500 rows.** An empty query matches every row, which makes the picker's default screen the walk's worst case, so it stops once it has collected the cap. Those 500 are taken in SERVER order (`sort=-updatedAt` is requested as a hint) and then sorted client-side, so the response is correctly ordered within itself but is NOT a guarantee of the newest 500 in the portal — under a bound the two cannot both hold. There are no pagination fields on this endpoint: a caller that needs an older template must SEARCH for it rather than page to it. |

> **Create requires a canonical slug `projectId`.** The connection is stored keyed by `project_id`, which is the EXACT-MATCH key for the dispatch lookup, and brief/campaign create already require a canonical slug — so a UUID-keyed connection could never be joined to a dispatched campaign. `POST` (create) therefore rejects a UUID `projectId` with `400` (Pattern `^[a-z0-9]+(-[a-z0-9]+)*$`, MaxLength 35). The generated HTTP request decoder validates the pattern/length for these create routes, and the service applies the same guard for direct/non-HTTP callers (belt-and-suspenders). `GET`/`PUT`/`DELETE`/`test`/`set-credential` stay permissive (UUID-or-slug) to keep historical UUID-keyed rows reachable.
>
> **The reserved `system:linuxfoundation` scope is not addressable.** It holds the LF-owned credentials a project falls back to when it has connected no account of its own, and every one of the **seven** endpoints taking a caller-supplied `projectId` refuses it: `POST` with `400` (the colon cannot satisfy the slug pattern above), and `GET`/`PUT`/`DELETE`/`test`/`set-credential`/`accounts` with `404`. `404` rather than `403` — the reserved scope is not a project this API exposes, and answering "forbidden" would confirm to an unauthorized caller that something is there. Account discovery is the seventh and the only one that does not reach storage through a shared helper, so it needs its own guard: left open, `GET /projects/system:linuxfoundation/connection-google-ads/accounts` would decrypt the LF credential and enumerate the Linux Foundation's own ad accounts. A project that has no connection of its own still sees the system account's accounts through *its own* `projectId`, deliberately — it is shown the accounts its campaigns would actually run on. Dispatch reaches the reserved scope internally; nothing reaches it over HTTP.
>
> **Installing the system account is out-of-band, by construction.** Because no request can address the reserved scope, the credentials are installed with the service binary's `bootstrap-system-account` subcommand, which speaks to the repository and encryptor directly: `DATABASE_URL=… CREDENTIAL_ENCRYPTION_KEY=… campaign-service bootstrap-system-account -provider google-ads [-account-id …] [-config login_customer_id=…] < creds.json`. Keys use the documented snake_case form, and `-provider` accepts the PAID-ADS providers only — Google Ads, LinkedIn, Meta, Reddit, X and Microsoft Ads. HubSpot is refused even though it is a valid provider elsewhere: the reserved-scope fallback is classification-gated to paid ads (a fallback on the audience path would write one project's contact lists into the LF's own portal), so a HubSpot system row would be installable, would report success, and would then be reachable by nothing. `-config` accepts only the keys the SELECTED provider stores (`login_customer_id` for Google Ads, `org_id` for LinkedIn, `page_id`/`app_id` for Meta, `funding_instrument_id` for X, `customer_id` for Microsoft, none for Reddit — `model.Provider.ConfigKeys`). Anything else is refused rather than accepted: storage has one column per key, so an unrecognised one has nowhere to go, and the earlier behaviour dropped it while still exiting 0 — telling the operator a setting was installed that nothing held. It is idempotent (a second run rotates the credential rather than violating the singleton index), reads the credential from stdin rather than a flag, requires the `-config` keys an adapter refuses to create without (LinkedIn `org_id`, Meta `page_id`, X `funding_instrument_id`), and `-account-id` may be omitted **for Google Ads and Meta**, leaving the row credentials-only. The other four require it, and the reason is worth stating precisely, because **discovery capability and credentials-first bootstrap are not the same thing** and conflating them hides which half is actually missing. Two halves are needed: an endpoint that can enumerate the accounts a credential reaches, AND a failure on the path that needs the id which NAMES the missing choice, so an operator is told to go and use that endpoint. The four still require it, but for DIFFERENT reasons, and the difference is what tells you how far each is from eligibility. **Reddit and X lack the first half**: no account-discovery endpoint exists for either, because neither platform client has a `ListAdAccounts`, so nothing inside this API could tell an operator what to put there. **Microsoft now has BOTH halves** as of LFXV2-3064, which added its discovery endpoint; `MicrosoftDispatcher.Dispatch` resolves through `validateMicrosoftConnection`, which tags a missing account with `domain.ErrAccountNotSelected`. It is therefore eligible to join `accountDiscoveryProviders` and has deliberately NOT been added yet — that is a change to what the bootstrap CLI accepts, and it belongs in its own change rather than riding along with the endpoints. **LinkedIn gained only the FIRST half** in the same ticket, and this document previously claimed otherwise by naming `resolveLinkedInCredentials` — a function that does tag `ErrAccountNotSelected`, but which the CREATE path never reaches. `LinkedInDispatcher.Dispatch` resolves the connection inline and answers a missing account id with a bare `notCreated`, so adding LinkedIn to `accountDiscoveryProviders` today would still produce an unclassified create failure that never names the missing choice. Routing `Dispatch` through the shared resolver — preserving its `notCreated` semantics — is the remaining work, and it is what earns LinkedIn the second half. Stating which half is missing matters because the halves are earned separately. Meta is the one provider where the halves ever came apart: it gained enumeration in LFXV2-3062 (the row above) and was still refused here, because its `Dispatch` answered an empty account id with a generic error. LFXV2-3061 supplied the second half — `requireMetaAccountID` tags it `ErrAccountNotSelected` + `ErrConnectionNotUsable`, which `unusableConnectionReason` reports as `account_not_selected`, so the dispatch-failure log line names the missing choice from a fixed vocabulary instead of carrying an unclassified error. State that precisely rather than as "the job result says so": create is queued work and `dispatchPlatform` collapses every dispatcher error into `"platform campaign creation failed"`, so the reason token is log-only on this path — for Google Ads too. LFXV2-3061 also dropped `Required("account_id")` from `MetaAdsConnectionConfig` to match. Meta is in `accountDiscoveryProviders` as of that ticket. Both halves, not either alone, are the bar for adding the next provider. The account-discovery endpoint above then LISTS the accounts those credentials can reach, but it is a GET and assigns nothing: selecting one means re-running `bootstrap-system-account` with `-account-id`, which rotates onto the same row. On that rotation an omitted flag means KEEP, not clear — restating the whole row should not be required — so removals are said explicitly: `-clear-account-id` returns a Google Ads or Meta row to credentials-only, and `-config login_customer_id=` (empty value) drops a config column. Both are refused where the state they would produce is one the installer already refuses to create: clearing LinkedIn's `org_id` or its account id fails exactly as omitting them at install time does, and a clear issued before the row exists is refused rather than dropped. Finally, an UNRECOGNISED first argument now exits 2 instead of falling through to server startup — `campaign-service bootstrap-system-acount …` used to parse cleanly and bring up an idle HTTP server, leaving the Job green with nothing installed.
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
googleAdsConfig?: object        — Google Ads-specific params (see GoogleAdsConfig below)
linkedInConfig?: object         — LinkedIn-specific params
redditConfig?: object           — Reddit-specific params
metaConfig?: object             — Meta-specific params (see MetaConfig below)
twitterConfig?: object          — X/Twitter-specific params (see TwitterConfig below)
microsoftConfig?: object        — Microsoft Ads-specific params (see MicrosoftConfig below)
hubspotConfig?: object          — HubSpot (email channel) params (see HubSpotConfig below)
```

#### MicrosoftConfig (the `microsoftConfig` object)

Microsoft Advertising (Bing) per-platform config. The dispatcher creates a PAUSED Search
campaign with an ad group + a responsive search ad (auto-composed copy), then attaches the
`keywords` supplied here — without them the ad group has nothing to match a query against and the
campaign can never serve, even once a human enables it (which is why `ToggleStatus` refuses to
activate a campaign whose keywords were never provisioned). **Budget is in whole units of the ad
ACCOUNT's currency**, not USD — the client does no FX conversion (mirroring `metaConfig`).

```
budget: number                  — Whole units of the account currency (e.g. 2500 = 2500 USD/JPY/…),
                                  applied as the campaign's DAILY budget. Must be a finite, POSITIVE
                                  number; NaN/Inf or a non-positive value is rejected by the client
                                  during dispatch (a pre-create job failure, since CreateCampaigns is
                                  async). Omitting it fails the platform job — supply it explicitly.
timeZone?: string               — OPTIONAL Microsoft Campaign.TimeZone enum value. Microsoft marks
                                  the field deprecated but still requires it on Add; when omitted the
                                  client uses its default.
keywords?: [{text, matchType}]  — Positive Search keywords attached to the created ad group. text is
                                  capped at 100 characters (Microsoft's limit; Google Ads caps the
                                  same field at 80) and matchType is Exact | Phrase | Broad —
                                  Microsoft's PascalCase spelling, though the SCREAMING_CASE Google
                                  Ads spelling is accepted and canonicalized. At most 60; duplicates
                                  (case-insensitive, per match type) are dropped; an empty text, an
                                  over-long one, a control character, or an unrecognized match type
                                  is REJECTED before anything is created, not silently dropped.
                                  Keywords are created PAUSED and enabled by the status toggle.
                                  OMITTING THIS CREATES A CAMPAIGN THAT CAN NEVER SERVE and that
                                  the status toggle will refuse to activate (409).
cpcBid?: number                 — OPTIONAL ad-group max cost-per-click, in whole units of the account
                                  currency (no micros, no FX). Omitted means UNSET, and Microsoft then
                                  applies the account-currency minimum — a documented, serve-capable
                                  floor, so omitting it is safe and the service invents no default. A
                                  supplied value must be within [0.01, 1000]. A REUSED ad group keeps
                                  its existing bid rather than being re-bid on a retry.
```

There is deliberately **no `geoTargets`** here, unlike `redditConfig`/`metaConfig`/`linkedInConfig`.
Microsoft's location targeting takes its own numeric `LocationId` values (from Microsoft's
downloadable geographical-locations file) and accepts ISO 3166 country codes for targeting
*nowhere* — the ISO table in Microsoft's Geographical Location Codes guide is scoped to account
business addresses, not targeting. A `geoTargets: ["US"]` could therefore only be honoured via an
invented ISO→LocationId mapping, so the field is not offered rather than being accepted and
silently dropped. Tracked separately; it needs the locations file as a real input.

The connection supplies the ad account id (`account_id`, the digits-only `CustomerAccountId`) and
an OPTIONAL manager/MCC id (`customer_id`, the `CustomerId` header) via the Microsoft connection
config — not this campaign config.

#### GoogleAdsConfig (the `googleAdsConfig` object)

Google Ads per-platform config. The dispatcher creates a PAUSED search campaign with an ad
group + a Responsive Search Ad (GA-3), then attaches keyword/audience targeting to that ad
group (GA-4) — without it, the ad group has zero criteria and the campaign can never serve,
even once a human enables it. **Budget is in whole units of the ad ACCOUNT's currency**, not
USD — the service does no FX conversion (mirroring `metaConfig`).

```
budget: number                  — Whole units of the account currency (e.g. 2500 = 2500 USD/JPY/…),
                                  applied as the campaign's DAILY budget. Must be a finite, POSITIVE
                                  number; NaN/Inf or a non-positive value is rejected by the client
                                  during dispatch (a pre-create job failure, since CreateCampaigns is
                                  async). Omitting it leaves the shell with no budget, which fails the
                                  platform job asynchronously — supply it explicitly.
headlines?: string[]            — Optional Responsive Search Ad headlines (≤30 WEIGHTED chars
                                  each, 3-15 after padding). Trimmed, truncated, and de-duplicated;
                                  caller-supplied entries are accepted up to 15 (later entries
                                  beyond that are silently dropped). Padded with deterministic
                                  eventName-derived placeholders up to the minimum of 3 when fewer
                                  are supplied (or omitted entirely).
descriptions?: string[]         — Optional Responsive Search Ad descriptions (≤90 WEIGHTED chars
                                  each, 2-4 after padding). Same trim/truncate/dedupe/pad rules as
                                  headlines, with caller-supplied entries accepted up to 4 (later
                                  entries beyond that are silently dropped).

                                  WEIGHTED, not plain runes: matching Google Ads' own counting,
                                  CJK and full-width characters (Hangul, Kana, CJK ideographs,
                                  Fullwidth Forms) each count as TWO. All-wide-character copy
                                  therefore fits 15 headline / 45 description characters, not
                                  30 / 90, and is truncated at that point. Latin text is
                                  unaffected — one character, one unit.
keywords?: {text, matchType}[]  — OPTIONAL positive Search keyword criteria (GA-4), attached to the
                                  ad group created above. `text` ≤80 runes; `matchType` one of EXACT,
                                  PHRASE, BROAD (case-insensitive). At most 20 entries; duplicates
                                  (same matchType+text) are silently deduped, but an empty text or
                                  unsupported matchType fails the job BEFORE any Google Ads request is
                                  made. Left empty/omitted, the ad group has no criteria and can never
                                  serve — supply at least one for a campaign that should actually run.
audienceSegments?: string[]     — OPTIONAL Google Ads resource names of EXISTING audiences to attach
                                  to the ad group (GA-4) as observation-only criteria — bid/report on
                                  the segment without narrowing delivery to it. This client does not
                                  create audiences; each entry must be a Customer Match user list
                                  (`.../userLists/{id}`) the caller already built elsewhere. Custom
                                  audiences are not supported (limited to Display/Demand Gen/Gmail/Video/
                                  Performance Max by Google; this client creates SEARCH campaigns only).
                                  Any other resource-name shape (customAudiences, userInterest,
                                  combinedAudience, etc.) is rejected. At most 20 entries; duplicates are
                                  deduped. When non-empty, the client sets the ad group's
                                  `targetingSetting.targetRestrictions` (AUDIENCE, bidOnly) on the ad group
                                  create so these segments stay observation-only rather than Google's
                                  default of restricting delivery to the audience alone.
adoptExisting?: boolean         — OPTIONAL, default FALSE (LFXV2-3042). When true, the dispatcher first
                                  looks the composed campaign name up on the account and, if a single
                                  live campaign already carries it, ADOPTS that campaign instead of
                                  creating one: the row is persisted with the existing
                                  platform_campaign_id and status `created_degraded`, no budget/ad
                                  group/ad is created, and this request's budget and config are recorded
                                  on the row but NOT pushed upstream. Use it to bind a campaign that
                                  already exists on the account to a brief. Leave it off otherwise: the
                                  composed name is deterministic and survives a campaign DELETE, so an
                                  unconditional lookup would silently re-attach a re-dispatch to the
                                  still-live campaign the delete walked away from. With the flag off,
                                  that dispatch creates, and Google's duplicate-name response surfaces
                                  as a job failure requiring reconciliation.
```

#### HubSpotConfig (the `hubspotConfig` object)

HubSpot (email channel) per-platform config. Unlike the ad platforms (which CREATE a campaign),
the HubSpot dispatcher STAGES a marketing email: it CLONES a template email as a DRAFT and points
its send list at the brief's already-**built** audience (the `campaign_audiences` resource,
populated by the audience-building step). No budget/schedule — email has none.

```
sourceEmailId: string           — REQUIRED. The HubSpot marketing-email id to CLONE as this
                                  campaign's email. There is no default template. The clone is
                                  created as a DRAFT (a human reviews and sends it), so staging is
                                  safe. The AI body content (subject/preheader/body) is applied by
                                  a separate content-generation step.
utmCampaign: string             — OPTIONAL. Overrides the utm_campaign applied to every ELIGIBLE
                                  link in the staged email — that is, every untagged web link.
                                  Links that already carry a non-empty utm_campaign keep it (an
                                  author's deliberate campaign is never overwritten), and
                                  mailto:/tel:/#anchor targets are left alone entirely.
                                  When unset the campaign is DERIVED from the
                                  deterministic email name, so links are always attributable —
                                  set this only to make several briefs' emails roll up to one
                                  campaign in reporting. utm_source is always `email` and
                                  utm_medium always `LF-Events`.
```

The connection supplies the HubSpot private-app token (credentials) and `portal_id` (provider
config); the send-list audience comes from the built `campaign_audiences` row for the brief, not
this config.

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

Connection prerequisites (from the Meta connection, not this config): `page_id` is REQUIRED,
format-validated (numeric), and length-bounded (`MaxLength 64`) at connection creation (a
missing/malformed/over-long value is a 4xx there, not a runtime dispatch failure). `account_id`
(`act_<digits>`, same format/length rules when present) is OPTIONAL at connection creation —
mirroring the Google Ads bootstrap (see the Platform Connections section) — so a connection can
be created with credentials only, then have an account chosen via `GET
.../connection-meta-ads/accounts` and set with `PUT`. Only the campaign-create job (async — 202,
then the polled result) checks account selection: it fails pre-create when none is chosen, never a
503, since waiting cannot fix a choice only a human can make. **The polled result does not say
which fault it was** — `dispatchPlatform` collapses every dispatcher error into the single string
`platform campaign creation failed`, so a client sees only that the platform create failed. The
classification lives in the service LOG, as a `reason=account_not_selected` field on the
"platform dispatch failed before upstream create" line; that is what an operator reads to tell a
missing account selection apart from a bad credential. A UI that wants to name the fault to a user
must read the connection's state, not the job. The status toggle and metrics read do NOT require an account id — both
target an existing campaign by its platform id and never read `AccountConfig.AccountID` — so an
account selection cleared after the campaign was created does not block pausing/resuming it or
reading its metrics. All three do share the credential-state checks (active connection, decodable
credentials, non-empty access token), tagged the same way as Dispatch's.

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
- **Metrics reads are UNVERIFIED (LFXV2-2995) and therefore DEFAULT-OFF**: the `ReadMetrics` adapter is wired but gated on `REDDIT_METRICS_ENABLED=true` (any other value, including unset, fails closed). With the gate closed, `GET .../campaigns/{id}/metrics` answers 400 for a Reddit campaign. Reddit's v3 reporting endpoint has no public documentation (unlike Google/Meta/LinkedIn/X, which have public specs). The `POST /ad_accounts/{account_id}/reports` request/response shape is inferred from this client's own proven `{"data": ...}` conventions, not from Reddit's real (gated) contract — treat every field name as a placeholder pending official API access. See the [internal/platform/reddit knowledge doc](knowledge/code/internal-platform-reddit.md).

### X/Twitter Ads
- OAuth 1.0a with HMAC-SHA1 signing (not OAuth 2.0)
- 1 request/second write rate limit
- Exponential backoff retry on 429 responses
- Only "lf-events" account currently supported

### HubSpot
- UTM lookup uses fuzzy name matching with scoring
- 15-second HTTP timeout per call
- If unavailable, falls back to campaign slug for UTM
