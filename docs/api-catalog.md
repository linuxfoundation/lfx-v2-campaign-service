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

   *Note on `GET /briefs?event_slug=`:* this is a **keyed item read, not a list**, so it does not breach the no-list-endpoint rule. `uq_campaign_briefs_project_event_delivery_stage` is a UNIQUE index on `(project_id, event_slug, delivery_type, stage) WHERE status <> 'archived'`, and the endpoint takes all four parts of that key (`delivery_type` and `stage` default to `paid-marketing` and `""`, the pre-000030 identity), so the lookup can match **at most one** brief — the same one-item-plus-ETag shape as `GET /briefs/{id}`, which this rule explicitly retains for conditional updates. It exists because the event slug, not the brief id, is what a caller holds when re-visiting an event page: the UI derives the slug from the pasted URL and must answer "have I already generated a brief for this event, on this surface, for this send?" before generating a new one. Since 000030 an event carries a paid brief AND an email series at once, so the slug alone no longer names one brief — which is why the delivery type and stage are part of the query rather than filters over a result set. It returns no collection, no pagination, and no filtering.
4. **Create and replace are separate; replace requires `If-Match`.** There is no "create-or-update" endpoint. A `PUT` (replace) requires an `If-Match: "<version>"` header carrying the current ETag; the caller must have fetched the current version first. Mismatches return `412 Precondition Failed`; a missing header returns `428 Precondition Required`. (Optimistic-locking pattern per [committee-service / 2026-05-CloudNativePG](https://github.com/linuxfoundation/lfx-architecture-scratch/tree/main/2026-05-CloudNativePG).)
5. **No bulk mutation endpoints.** Bulk status/budget changes are omitted: HTTP cannot cleanly express partial success/failure across a set, and a single bulk call cuts across per-target permission boundaries. Each mutation is scoped to one permission-evaluated target.
6. **A refused bearer token is `401`, and the `400`s below are never about authentication.** Every endpoint in this service carries `Authorization: Bearer <jwt>`, and the token is verified in-process against Heimdall's JWKS (signature, issuer, audience, expiry, non-empty principal) in addition to the gateway's own check — see [architecture.md](architecture.md) and `internal/infrastructure/auth`. A token that is absent, expired, wrongly signed, or accepted without naming a principal answers **`401 Unauthorized`** with a **`WWW-Authenticate: Bearer`** challenge (RFC 9110 §15.5.2). This is a per-endpoint contract, not a gateway convention: every method declares `Unauthorized`, because Goa builds each method's error encoder from its declared list and an undeclared error is encoded as a `500`.

   The split from `400` is what a client acts on. `401` says the request was well-formed and the **credential** must be replaced — refresh the token and retry the identical request. `400` says the **request** was wrong (a payload that failed validation, a `project_id` that is a UUID where the create routes require a canonical slug, a malformed campaign id) and retrying it unchanged will fail again. Every `400` documented in the tables below is of the second kind. Note also what `401` is *not*: authorization. The `campaign_manager` relation is enforced at the gateway, so a caller with a valid token but no grant is refused there and never reaches this service — no endpoint here answers `403`.

   The rejection **message is deliberately opaque and identical for every reason** a token is refused; only the status distinguishes the credential from the payload. Naming which check failed tells an attacker what to fix next, so a client must branch on the status, never on the prose. Distinct from both: when the service cannot *perform* the check — no verifier wired, or Heimdall's JWKS unreachable — nothing was established about the token, so that answers **`503`** (retry when the dependency recovers), not `401`.

Resource pseudotypes declared into the global indexer namespace: `campaign_brief` and `campaign`. **Connections are not indexed** — they are singleton per project with no cross-project listing consumer, so they are read directly (not via the Query Service). See [architecture.md](architecture.md) for the full type-name and relation catalog.

### Brief Lifecycle (Planning Phase)

Briefs are subordinate to a project. AI generation runs HERE: `generate-email-copy` answers one-shot, and the eight **email-creation wizard** routes below drive a multi-turn session. The sentence that used to sit here said generation was still in the Express BFF and would migrate later — that stopped being true when the wizard landed, and a consumer reading it would look for these routes in the wrong service.

A brief is the funnel unit: it carries the **program** (`program_type` = events / education / membership) that sets the funnel context, and one brief drives many channel campaigns (see next section), each a row under the brief with the same `brief_id`. It is shared across the **paid** platforms, not across delivery surfaces: since migration `000030`, `delivery_type` and `stage` are part of a brief's identity, so an event carries a paid brief plus one brief per email send in its series. Program is a field on the brief, not a separate resource.

**Platform selection lives on the campaign, not the brief — within one channel.** A brief may carry a *suggested* default set of platforms (a planning hint used to pre-populate the campaign form), but the binding choice of which platforms to launch on — and each platform's configuration — is made at campaign-creation time. The generation strategy is driven by `program_type`, not by which platform a brief was drafted against, so an approved brief can be launched on any subset of the platforms **belonging to its own channel**.

The CHANNEL itself is not a free choice. Since migration `000030` a brief's `delivery_type` is part of its identity, and `create-campaigns` and `adopt-campaign` both reject a platform whose channel differs from it with a `400`: an email brief cannot launch paid ads, and a paid brief cannot stage a HubSpot send. The two carry different content — RSA headlines and a keyword list on one side, a subject and preheader on the other — so a cross-channel dispatch would send a payload the platform has no field for and record it against a brief that never meant it.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/fetch-event-url` | `campaign_manager` | JSON | Fetch an event page and return the metadata extracted from it, for pre-filling a brief form. **Creates and persists nothing** — the caller reviews the result and submits it through `POST /briefs`. `POST` rather than `GET` because the URL is a request *body* parameter: as a query parameter it would be written verbatim into access logs, proxy logs and browser history at every hop. Answers `400` for a URL that is malformed, resolves to an address this service will not connect to, or yields no event name (see the SSRF notes in `docs/knowledge/code/internal-platform-eventurl.md`), and `503` when the origin does not answer. The result names the extraction strategy in `extracted_from` (`jsonld`, `opengraph` or `fallback`) — the whole record comes from exactly one of them. |
| POST | `/projects/{projectId}/briefs` | `campaign_manager` | JSON | Create a brief. |
| GET | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Get a brief (full copy, keywords, targeting); returns ETag. |
| GET | `/projects/{projectId}/briefs?event_slug=&delivery_type=&stage=` | `campaign_manager` | JSON | Find the saved brief for one event slug **on one delivery surface, for one send**; returns ETag. `delivery_type` (`paid-marketing` \| `email`) and `stage` (`""` for paid, else one of the six email stages) default to `paid-marketing` and `""` — the identity every brief written before `000030` carries, so a caller predating them addresses exactly the row it always did. All four together are the unique key, so the lookup matches at most one brief. `404` when that brief does not exist yet (the ordinary first-generation case) — note this now means *that send* has no brief, not that the event has none. The slug has no upper bound here, matching `BriefWriteInput` (the create/update payload) and the `TEXT` column, so any brief the create contract accepts is recallable; it must be non-empty, as on create. See the D5 note below. |
| PUT | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Replace a brief (requires `If-Match`). |
| POST | `/projects/{projectId}/briefs/{brief_id}/refresh` | `campaign_manager` | JSON | Re-run generation against latest event data, producing a new version. |
| POST | `/projects/{projectId}/briefs/{brief_id}/approve` | `campaign_manager` | JSON | Approve a brief for campaign creation (requires `If-Match`; approval is version-gated so a brief replaced since it was fetched cannot be approved on stale content). |
| POST | `/projects/{projectId}/briefs/{brief_id}/email-copy` | `campaign_manager` | JSON | Generate AI-written email copy (`subject`, `preheader`, and ordered `sections`) for the brief. **The response carries `sections`, not flat `body`/`cta` fields** — each entry is a `rich_text` (inline HTML), `button` (label plus destination) or `divider`, in display order, so a consumer assembles the body from the rich-text entries and renders the button as its own element rather than expecting it inline. Returns immediately with generated text; does NOT persist to the brief. The AI model is optional — without it configured this endpoint returns 503. **Two size bounds, two different status codes**: event details over 2400 runes are a `400` (the caller can edit them; the count covers the event name, location, dates and the brief's `url`), while a composed prompt over 20400 is a `503` — that branch is unreachable by caller input, so it can only mean a service-owned stage template has outgrown its budget, and a 400 would tell the caller to fix a brief that is not the problem. Requires valid brief event details (event name). **The model is INSTRUCTED that every `href` in the generated body must be the brief's `url`** — or a `registrationUrl` inside `event_details` when the top-level column is empty — and that with no usable URL the call to action be PLAIN TEXT with no link, rather than the `href="#"` placeholder the model invented when it was given no destination at all. The URL is supplied only to stages whose call to action is a registration ask; **CFP Launch, Post-Event and Final Countdown WITHHOLD it** (they render "Submit Your Proposal", "Share Feedback" and "See You There"), because the brief carries one `url` column and no CFP-form or survey field, so there is no correct destination to give them — those stages get the plain-text call to action instead of a button pointing at the wrong page. That is a prompt instruction, NOT an enforced guarantee: the response is validated for JSON shape and body length only, so a model that ignores it can still return an invented address or `href="#"` under a `200`, and a caller needing certainty must check the returned body itself. What IS enforced is the value that reaches the prompt: a candidate that is not an absolute `http(s)` URL counts as none, as do embedded credentials, a missing host, a malformed query escape, and anything over 2400 runes. The accepted value is NORMALISED rather than copied byte-for-byte — delimiters in the path, query and fragment are percent-encoded so it is safe inside an `href`, and the query is re-encoded (which SORTS its parameters) — so the URL in the prompt can differ from the stored column in escaping and parameter order while addressing the same destination. This is part of the stage-aware prompt only, so an absent `stage` still returns copy written without a destination. **Optional `stage` QUERY parameter** selects the event-lifecycle template: `CFP Launch`, `Schedule Announcement`, `Registration Push`, `Discount Offer`, `Final Countdown`, `Post-Event`. It is a query parameter rather than a body field deliberately — declared in the body it made the BODY itself required (Goa emits `requestBody.required: true` and the decoder answers `MissingPayloadError` on EOF), so every existing body-less POST began failing with a 400. **Matching is case-sensitive and there is no enum**: an UNRECOGNISED value resolves to `Registration Push` rather than erroring, per LFXV2-1940, so a misspelling returns registration copy under a `200` rather than telling the caller. An ABSENT (or blank) stage is a distinct case: it returns the exact prompt this endpoint emitted before stages existed, byte for byte, so an existing caller that never sends `stage` sees no change **in the prompt**. The RESPONSE shape did change for every caller (`sections` replaced the flat `body`/`cta` pair), and the legacy repackaging is applied only on this no-stage path — a stage-aware request whose model output regresses to the flat shape is refused rather than silently converted. The response does not report which stage was used, so a caller that needs certainty must send an exact value from the list above. **Optional `variant` QUERY parameter** requests a differently-styled draft of the same stage's copy: the only recognised value is `urgency-fomo`, which adds an urgency/FOMO-forward structure (deadline framing, social proof, a secondary CTA) to the stage-aware prompt — genuine urgency only, never an invented deadline, capacity number, attendee count or price. It follows the same lenient shape as `stage`: any other value, or absence, produces the normal stage-based copy under a `200` rather than an error, and it is consulted only on the stage-aware path, so an absent `stage` still returns the LFXV2-1940-frozen legacy prompt unaffected by `variant`. The schema has no image or card section type, so speaker photos, logos and highlight cards the variant prompt asks for are approximated as text/emoji structure inside `rich_text`, not literal images. **Optional `segment` QUERY parameter** narrows which content blocks the stage-aware prompt asks the model to keep or drop for a named audience, without changing the underlying facts, stage or variant framing: `developer` (keep session/track detail, drop sponsorship framing), `business-decision-maker` (keep ROI/sponsorship framing, drop session-level detail), `alumni` (lead with what's new since a past edition) or `prospect` (lead with what the event is, assuming no prior familiarity). Same lenient shape as `stage` and `variant`: any other value, or absence, produces the normal stage-based copy under a `200` rather than an error, it is consulted only on the stage-aware path, and it composes ADDITIVELY alongside `variant` — both may be set together, either alone, or neither. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/plan-start` | `campaign_manager` | JSON | Opens a wizard SESSION over the brief and returns its `session_id` plus a `progress_token`. Every later turn needs what the earlier ones decided, and sessions are a TABLE rather than process memory because this service runs multiple replicas with no session affinity — the pod answering `generate-content` is routinely not the one that answered `plan`. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/plan` | `campaign_manager` | JSON | Decides the approach: picks a past email to clone when one matches (`mode: reference`) or writes from the stage guide (`mode: stage`). Answers 200 with a warning rather than an error on a thin brief — a brief with no event name yields thin copy, and saying so beats refusing. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/generate-content` | `campaign_manager` | JSON | The AI turn. **503 when no model is configured** (`AI_PROXY_URL` + `AI_API_KEY`), which is a deployment fact, not a caller error. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/update-sections` | `campaign_manager` | JSON | Replaces the content blocks and re-renders. Button hrefs and image sources go through `httpURL`, so a `javascript:` URL supplied by a model or a scraped page cannot reach an anchor. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/clone` | `campaign_manager` | JSON | **Creates a real HubSpot draft and is NOT idempotent.** 409 when the plan chose no source email. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/set-send-list` | `campaign_manager` | JSON | Sets the draft's recipients. Acts ONLY on the draft this session created: a supplied `email_id` that does not match the session's is a 400, because HubSpot credentials resolve through the shared LF portal and authorization here is scoped to the brief. |
| POST | `/projects/{projectId}/briefs/{brief_id}/wizard/chat` | `campaign_manager` | JSON | One conversational turn about the email being built. Prose only — it cannot edit the draft, and says which wizard step applies a change. Oldest history is dropped to stay inside the composed prompt budget. |
| GET | `/projects/{projectId}/briefs/{brief_id}/wizard/session/{session_id}` | `campaign_manager` | JSON | Reads a session back, so a reload can resume a run. |
| GET | `/projects/{projectId}/briefs/{brief_id}/wizard/progress/{token}` | `campaign_manager` | **SSE** | Progress frames for a run. The ONE route here that is not a Goa method — Goa v3 has no SSE encoding — so it is mounted by hand in `cmd/campaign-service/server.go` after the generated routes, on a path no generated route claims. |
| POST | `/projects/{projectId}/briefs/{brief_id}/creative-assets` | `campaign_manager` | JSON | Upload an image asset for a brief so a Meta ad creative can reference it by id. Synchronous: the image is validated and stored in Postgres (`bytea`), returning a `CreativeAsset` (`id`, verified `mime_type`, `byte_size`, SHA-256 `checksum`). The bytes are base64-encoded in the JSON body, so a request body is roughly 4/3 the image size. The field is declared a **`String`**, not a Goa `Bytes` attribute, so the published schema says `type: string` rather than `format: binary` (which in OAS3 means raw octets and would describe a wire this endpoint does not accept); the service decodes it at the boundary and answers **`400`** for malformed base64. The published `maxLength` on that field is the **encoded** ceiling (41,943,040 base64 characters = 30 MiB decoded), because OpenAPI `maxLength` counts characters of the JSON string rather than decoded bytes; the 30 MiB decoded ceiling itself is enforced in the handler and answers `400` (and is carried as a `byte_size` table CHECK by migration `000029`, for writers that never reach the handler). Bodies above `constants.MaxRequestBodyBytes` (42 MiB) are refused with `413`, by one of two arms depending on whether the size is DECLARED. A request whose `Content-Length` exceeds the cap is refused before any of the body is read. A chunked/undeclared body cannot be measured up front, so it is wrapped in `http.MaxBytesReader` and the overflow is only discovered by reading up to the cap; the decoder then fails with a generic `400` about malformed JSON, which the middleware replaces with the `413`. Both answer `413` and neither buffers more than the cap, but only the declared arm avoids reading the body at all. Re-uploading identical bytes to the same brief returns the existing asset (idempotent on `(brief_id, checksum)`), and the STATUS distinguishes the two outcomes: **`201`** when this request stored the asset, **`200`** when an identical upload already existed and the stored row was returned unchanged. The body is the same `CreativeAsset` either way — it also carries `created` (`"true"`/`"false"`), the field the status is derived from. An unconditional `201` would tell a retrying client it had created a resource when nothing was created. Touches **no ad platform** — the account-scoped Meta `image_hash` is resolved later, at campaign dispatch. `400` for an empty body, a `bytes` value that is not valid base64, bytes that are not a decodable image, a format outside the PNG/JPEG allow-list, a declared `content_type` that disagrees with the sniffed bytes, an image whose decoded pixel buffer would exceed 80 MiB or whose sides exceed 10,000 (the decompression-bomb gate, priced from the header's colour model before any decode, so a 16-bit PNG is charged the 8-bytes-per-pixel it really costs), or image data that is truncated or corrupt (proven by a full decode, so a header-only PNG cannot be stored); `413` if the request body exceeds the cap — including when the DECLARED `Content-Length` already exceeds it, which is refused before any admission permit is sought so a plainly-oversized request is never answered with a retryable `503` it can never retry past; `503` in three distinct cases: while the database is still binding (cold start) **or, in the supported no-database mode, permanently** — the repository is never bound there, so that arm is NOT a transient state a client can retry past and its wording is deliberately availability-neutral rather than promising recovery; when `UploadAdmission` cannot get wire-memory capacity within 250 ms; and when `DecodeReserver` cannot get decoded-pixel capacity within the same window. Only the last two are transient — they are CAPACITY, not failure — the request was refused without being attempted, so retrying after a short backoff is the correct client behaviour, and none of the three should be diagnosed as a database problem. Only the `UploadAdmission` case carries a `Retry-After` header: it is written by the middleware, which sits outside the mux and controls its own response. The cold-start and `DecodeReserver` cases are typed Goa errors whose generated encoder emits status and body only, so a client must not condition its retry on the header being present. |
| DELETE | `/projects/{projectId}/briefs/{brief_id}` | `campaign_manager` | JSON | Archive a brief (soft delete). |

> Listing briefs and viewing a brief's version history are served by the Query Service, not by dedicated endpoints here.

### Campaign Creation (Implementation Phase)

A campaign is subordinate to a brief. This is a **collection** under the brief (a brief may drive multiple campaigns across platforms). The `POST` body carries the **selected platforms** and their per-platform config (see `CampaignCreateRequest`). Creation is **asynchronous**: the upstream ad platforms take seconds-to-minutes to provision, so `POST` returns immediately with a `jobId` (a `JobCreateResponse`), and the caller polls `GET .../jobs/{jobId}` for a `JobPollResponse` until the job is terminal. One execution record is persisted per platform.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns` | `campaign_manager` | JSON | Create campaigns across the platforms selected in the body (async → `JobCreateResponse` with `jobId`). Persists one execution record per platform. Repeating the create is an idempotent **retry** — a platform whose latest campaign is complete returns that campaign. Set `new_version: true` to create **another** campaign alongside it instead — **`microsoft-ads` only for now**; a request naming any other platform with `new_version` is refused with `400` before a job exists, because those platforms reuse campaigns by a name that does not yet differ per version (a platform with none gets its first; one whose latest campaign is still in flight or needs reconciliation gets the retry answer). The version is internal — returned as `slot_version` on the campaign — and never appears on the ad platform as a label. Until the release after migration `000037` drops the one-campaign-per-slot index, a `new_version` request on an occupied slot fails that platform with "not available yet" and creates nothing. `409` with `reason=ab_test_unsupported_send_type` — **synchronous, before any job exists** — when a HubSpot email asks for an A/B test (`hubspotConfig.abTestEnabled`) but its source email (`hubspotConfig.sourceEmailId`) is set to send based on recipients' time zones, which HubSpot does not allow together. Nothing is created; choose a different source email or turn the A/B test off. The check reads the source email once and **fails open**: if HubSpot cannot be reached or does not report a type, the create proceeds as it did before the check existed. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/adopt` | `campaign_manager` | JSON | **Bind an ad campaign that already exists upstream** to this brief, without creating anything on the ad platform. Synchronous (no job): the platform is read for the campaign once — plus, on Reddit and X only, one read of the connection's own ad account after a campaign `404` (see below) — and on success the campaign row is written in the same request. `platform_campaign_id` is verified against the project's own connection before anything is persisted — a `404` means the platform PROVED there is no such campaign (an answer that could equally be an access, auth or account problem is never a `404`); a `503` means the campaign could not be **verified** and its existence is **unknown** — the platform may have been unreachable, or it may have answered with something untrustworthy (an unhonoured id filter, an undecodable row, an unrecognised status), which is why the message names verification rather than connectivity. `400` for a platform with no adoption support, an unapproved brief, an unknown platform, or a blank or malformed `platform_campaign_id` (**malformed IDs are validated before the connection state is checked, so a permanent input fault always returns `400` regardless of connection availability**); `409` when the brief already has a live campaign on that platform, when the campaign is of a type the platform's slot cannot hold (a non-Search Microsoft campaign), when the platform reports the campaign under a **different ad account** than the project's connection (Meta, whose node read is by id alone, and Reddit/X when their answer names its account — Google and Microsoft scope the read to the account, so a foreign campaign is simply absent there), when that upstream campaign is already bound to a **different** brief — **in any project**, because Google Ads is one shared upstream account across every foundation, so a project-scoped check would let two projects bind and then fight over one live campaign; the message names the campaign but not the other project, which the caller may not be entitled to see — when the brief lost its approval during the platform read, when the project's connection is unusable, or when the project has **no connection of its own** — adoption is the one path that cannot fall back to the shared LF system account, because many projects share that one ad account and the caller names an arbitrary campaign inside it. There is deliberately **no `500` for an unusable LF system connection** on this endpoint, unlike the metrics and toggle endpoints: adoption resolves the project's own scope only and never loads the LF row, so the answer for a project without its own connection is the same actionable `409` whatever state that row is in. An adopted campaign records **provenance only** — no ad group, ad or asset group ids — so the status-toggle endpoint **refuses to ACTIVATE it on every platform channel** and says so in those words; un-pause an adopted campaign in the ad platform's own UI. PAUSE is unaffected, because pausing the campaign resource alone is always safe. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Get one campaign execution; returns ETag. |
| PUT | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Replace a campaign execution (requires `If-Match`). |
| DELETE | `/projects/{projectId}/briefs/{briefId}/campaigns/{campaign_id}` | `campaign_manager` | JSON | Delete a campaign (soft delete; requires `If-Match`). **Local only — does NOT touch the ad platform.** Frees the campaign's `(brief, platform)` slot so the brief can be re-dispatched to that platform. `409` if the campaign is mid-dispatch. |
| GET | `/projects/{projectId}/jobs/{jobId}` | `campaign_manager` | JSON | Poll campaign creation job status (`JobPollResponse`). |

**Deleting a campaign.** A campaign row occupies its brief's slot for one platform — the `(brief_id, platform, variant)` uniqueness that makes dispatch idempotent (a retry cannot create a second paid campaign upstream) also means a campaign created with the wrong budget, or one whose upstream create failed ambiguously, would block that pair forever. `DELETE` frees the slot: the row is soft-deleted (`status = 'deleted'`) and excluded from the partial unique index, so a re-dispatch to the same `(brief, platform, variant)` succeeds while two *live* campaigns for the pair are still rejected. The soft-deleted row is retained deliberately — it holds `platform_campaign_id`, the only local pointer to a campaign that may still exist upstream — and becomes invisible to reads (`GET` returns `404`).

> **The ad platform is not touched.** This service has no verified campaign-delete API for any provider, so `DELETE` never deletes, pauses, or modifies the campaign on the ad platform. **A campaign already created upstream keeps running and spending until it is stopped there.** Pause it first via the status-toggle endpoint, or stop it in the platform's own console. Deleting a campaign that is mid-dispatch (`status = 'pending'`, an active dispatch claim) returns `409`: freeing the slot under an in-flight dispatch could let a concurrent claim double-create upstream.

**Adopting a campaign.** Not every campaign a foundation runs was created here: a team that launched in Google Ads' own console before onboarding, or during an outage, still needs the campaign under a brief so the per-campaign endpoints reach it: the metrics read, the settings readback, the status toggle and delete. `POST .../campaigns/adopt` is that path, and it is deliberately **not** an upsert. Adoption names an arbitrary upstream campaign, so an updating conflict arm would repoint an existing binding at a different campaign and orphan the one it used to name — which this service cannot stop, because it never deletes or pauses upstream on its own. A pair that is already bound is refused with `409`; free the slot with `DELETE` first if the binding is genuinely wrong. Note also what adoption is *not*: an ownership check. Within a shared ad account a project can name a campaign another project created, and no rule here can prevent that, because that project's stored credential already grants read and pause on everything in the account straight through the provider's API. Account tenancy is where that boundary lives; what this service enforces is its own invariant, one upstream campaign to one brief.

> **Absence and unavailability are different answers, and the distinction is load-bearing.** An operator who is told "no such campaign" reasonably goes and creates one — so a lookup that could not be verified must never be reported as absent. The service returns `404` only when the platform positively answered that the campaign is not there, and `503` for every unverifiable outcome (transport failure, an unhonoured filter, an undecodable or unrecognised response). The id written to the row is the one the **platform echoed back**, not the one requested, so a platform that ever answers with a different campaign cannot have the requested id recorded against it. The stored `status` is this service's own lifecycle value (`created`), never the platform's `ENABLED`/`PAUSED`. **The metrics read does not report the platform's run state** — it returns impressions, clicks, cost and CTR and nothing else. **The `/settings` readback is the one endpoint that does read it back**: it reports the platform's `ENABLED`/`PAUSED`/`REMOVED` as an upstream-only observation, deliberately never compared against the row's `status`, since the two are different axes. Run state is *set* through the status toggle, which persists `active`/`paused` on the row once the platform confirms, so the row reflects the last toggle this service performed; a change made in the platform's own console is **never written back to the row** and is visible only there and through the `/settings` readback.
>
> **What an adopted campaign supports.** Every per-campaign read the platform offers, plus `DELETE` and pausing through the status toggle — an adopted row is an ordinary campaign row to those endpoints, so the metrics read and the `/settings` readback work on it where the platform supports them. It is NOT true that everything else works, because adoption records only the campaign the platform was asked about (its id, name and account) and never walks the campaign's children:
>
> - **`ACTIVATE` is refused on every adopted row, on every platform** (`ErrCampaignNotProvisioned`, `409`, the message naming adoption). On Google Ads the toggle requires the ad-group, ad and keyword-criterion ids proving targeting was provisioned; on Microsoft (ad group + ad), Meta (ad set), Reddit (ad group + ad) and X (line item) it requires the child ids its cascade flips. This service has not verified that the campaign can deliver, and the guard exists precisely to stop it reporting a successful activation of something that cannot serve. Activate it in the platform's own console, or dispatch a campaign through `POST .../campaigns` to get one this service provisioned end to end.
> - **The bid lever (`PATCH .../bid`) is refused on adopted Microsoft, Meta, Reddit and X rows** (`ErrBidUnwritable`, `409`): the manual bid lives on the ad group, ad set or line item, and the row records none. Change the bid in the platform's own UI.
> - **The budget lever (`PATCH .../budget`) is refused on adopted Meta rows** (`409`), because a Meta budget lives on the ad set. **On Microsoft, Reddit and X it works**: those platforms keep the budget on the campaign itself, so the write reads and changes the adopted campaign's LIVE budget, behind the same provenance guard (the recorded account must match the connection) and the same shared-budget/pacing refusals as on a dispatched campaign.
> - **The Meta ad-set toggle (`POST .../meta-ad-sets/{ad_set_id}/status`) refuses `ACTIVE` on an adopted row** (`409`) for the campaign toggle's reason; `PAUSED` works, and the ad-set read (`GET .../meta-ad-sets`) works with no ad set marked `recorded`.

> **Adoption support is per-platform and optional.** It is a capability the dispatcher may implement, not part of the dispatch contract, so platforms gain it independently. **Google Ads, Microsoft Advertising, Meta, Reddit and X support it** (the last four since LFXV2-2665); LinkedIn and HubSpot answer `400`. Each reads the campaign ONCE by id under the project's own connection:
>
> | Platform | Read | Id accepted (else `400`, before any connection work) | Absent (`404`) | Adoptable status | Provenance |
> |---|---|---|---|---|---|
> | `google-ads` | GAQL `campaign.id = …` | canonical positive int64 | no row, or `REMOVED` | `ENABLED`, `PAUSED` | query scoped to the customer |
> | `microsoft-ads` | `GetCampaignsByIds` (every documented campaign type, the eight-value space-delimited v13 set incl. `ObjectiveBased`; only a **Search** campaign is adopted — any other live type is a `409` "adoption supports Microsoft Search campaigns only", never a `404`) | canonical positive int64 | `CampaignServiceInvalidCampaignId`, or `Deleted` | `Active`, `Paused`, `BudgetPaused`, `BudgetAndManualPaused`, `Suspended` | request scoped to the account (body + `CustomerAccountId`) |
> | `meta-ads` | `GET /{id}?fields=id,name,status,effective_status,account_id,objective,daily_budget,lifetime_budget,bid_strategy` | decimal node id, no leading zero, ≤32 digits | only `DELETED`/`ARCHIVED` — Graph code 100/subcode 33 is "does not exist OR cannot be loaded due to missing permissions", so it is `503`, never `404` | `ACTIVE`, `PAUSED` | answer's `account_id` must equal the connection's (`act_` normalised); absent `account_id` is unverifiable |
> | `reddit-ads` | `GET /ad_accounts/{account}/campaigns/{id}` | letters/digits/underscore, ≤64 | `404` **confirmed by a readable ad account** (`GET /ad_accounts/{account}`), or `DELETED`/`ARCHIVED` | `ACTIVE`, `PAUSED` | path-scoped; `ad_account_id` compared when reported |
> | `twitter-ads` | `GET accounts/:account_id/campaigns/:id` | alphanumeric, ≤64 | `404` **confirmed by a readable account** (`GET accounts/:account_id`), or `deleted: true` | `ACTIVE`, `PAUSED`, `DRAFT` | path-scoped; `account_id` compared when reported |
>
> **`404` only when the platform has PROVEN absence.** A terminal state (removed, deleted, archived) reads as absent because it cannot spend — "absent" licenses no duplicate of anything serving. A not-found answer that could equally mean an access, auth or account problem is never a `404`: Meta's 100/33 is `503`, and a Reddit or X campaign `404` is followed by one read of the connection's own ad account, and is a `404` only if that account answers and is the connection's own — any other answer (account `404`, `401`/`403`, `5xx`, throttle, a body naming another account) is `503`. Any other status, a missing name, an id the platform did not echo, a body that would decode with silent substitution (malformed UTF-8, an unpaired surrogate escape, a duplicated key), a throttle that outlasted the retries, an auth failure and every transport or `5xx` failure are `503` "could not be verified".

> Listing a project's or brief's campaigns, and per-campaign change history, are served by the Query Service.

### Campaign Audiences (Implementation Phase)

A **built campaign audience** is a pointer + provenance to a platform-side audience (its master-list id, applied suppression lists, and a human-readable inclusion summary) — not the audience's contents. It is a **collection** subordinate to a brief (a brief may drive several audiences over time / per platform). Writes are gated on `campaign_manager` and use optimistic concurrency: reads return an ETag, and `PATCH` requires `If-Match` (`428` when missing, `412` on mismatch). `PATCH` is a load-then-merge — a nil field is left unchanged; a non-empty `suppression_list_ids` replaces the set, and the explicit `clear_suppression_lists` boolean removes all (an empty array can't round-trip through the generated client's `omitempty` tag, hence the flag).

**The platform list ids become immutable once the audience records the portal it was built in.** A `PATCH` that would change `platform_master_list_id` or the suppression set on such a row is refused with **409**; the remedy named in the message is to rebuild the audience. Re-sending the values a read returned is not a change and is never refused, and `status` / `inclusion_summary` stay patchable throughout.

An audience attached from several existing lists carries `include_list_ids` on the audience view (response-only — no `PATCH` can set it). Those lists, not `platform_master_list_id`, are what an email send targets; the master holds only the first of them, so a `PATCH` that would change it on such a row is refused with **409** whether or not the row records its portal. Absent `include_list_ids` means the master alone is the send list.

The reason is not a policy choice and cannot be relaxed per-caller: `built_in_portal_id` records the HubSpot portal the row's EXISTING ids were created in, and a `PATCH` carries ids rather than a credential, so the service cannot ask HubSpot which portal new ids belong to. Applying such a patch would leave the stamp vouching for ids nothing verified — and because dispatch compares the stamp against the currently resolved portal rather than against the ids, that send would be **approved**. Refusing the write is what keeps the guarantee true.

Audiences written before provenance existed record no portal and are deliberately not backfilled; their ids remain patchable, and they fail closed at dispatch instead (see `channel-connections-schema.md`). The deciding field is not exposed on the audience view, so a client that needs to know whether a row is stamped should treat the 409 as the answer rather than trying to predict it.

Because these paths nest under `/briefs/{briefId}/`, they inherit the gateway wiring already in place for briefs: the HTTPRoute `briefs(/.*)?` path match forwards them, and the single Heimdall `project-api` rule (`/projects/:projectId/briefs/**`) authorizes them on `campaign_manager` — no separate route or rule entry is needed (LFXV2-2783). The route/rule parity test pins explicit audiences paths so a future narrowing of the briefs match/rule can't silently unroute or de-authorize them.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/briefs/{briefId}/audiences` | `campaign_manager` | JSON | Create a built audience under the brief; returns ETag. |
| GET | `/projects/{projectId}/briefs/{briefId}/audiences/{audienceId}` | `campaign_manager` | JSON | Get one audience; returns ETag. |
| GET | `/projects/{projectId}/briefs/{briefId}/audiences` | `campaign_manager` | JSON | List a brief's audiences (newest first). |
| PATCH | `/projects/{projectId}/briefs/{briefId}/audiences/{audienceId}` | `campaign_manager` | JSON | Partially update an audience (load-then-merge; requires `If-Match`). |
| POST | `/projects/{projectId}/briefs/{briefId}/audiences/build` | `campaign_manager` | JSON | Build the brief's HubSpot audience: derive the regional-expansion inclusion lists, create them, and record the master list (`202`). `400` when the brief is not approved or its details lack an event name/country; `500` when the brief's HubSpot connection is missing; `503` when the audience-build dependencies (brief repository, HubSpot/Snowflake builder) are unconfigured; `409` in two distinct forms, below. A project without its own HubSpot connection resolves the **LF system connection** (`system:linuxfoundation`), installed by `bootstrap-system-account -provider hubspot`. That fallback was once refused for HubSpot on the reasoning that it would write one tenant's contact lists into another's portal; every LF foundation shares the one LF portal, and list names are portal-global and disambiguated by event name plus build ref, so there is no second tenant for a list to land in. A project WITH its own connection is still served that connection, never the LF row. Snowflake enrichment is optional; builds proceed country-only if unavailable. Until an audience is `built`, the email channel cannot dispatch. |

**The two 409s on build-audience carry OPPOSITE remedies, so the status code alone is not
enough — a client that keys on it will do the wrong thing for one of them.** Key on the
`reason` field of the error body, which is a stable slug: `stale_approval`,
`audience_build_in_flight`, or `already_exists`. Do NOT key on the message text — it is
reworded whenever an operator finds it unclear, and the wording below has already changed
twice. `reason` is populated by the audiences endpoints, and by `create-campaigns` for
`ab_test_unsupported_send_type` (see its row above); every other briefs-endpoint conflict is still
distinguished in message prose and sets no slug. Absent means unspecified:
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

### Audience Builder (Pre-commit exploration)

The nine `audience-builder` endpoints are what an operator drives **before** a brief commits to an audience: find the event's candidate HubSpot lists, choose suppressions, preview how many distinct people the selection reaches, compose a master list, and QA it. They are NOT subordinate to a brief — nothing here writes a `campaign_audiences` row, and nothing the Campaign Audiences endpoints above return is produced by this family. The two surfaces are deliberately separate: `POST .../audiences/build` materialises the audience a brief has committed to, and a list created here is invisible to the HubSpot dispatcher, which resolves a send's audience by brief id.

**Almost every endpoint here degrades rather than fails.** A suppression term that resolved to no list, a list id that no longer exists, a discovery run that hit its inspection cap — each is reported as itself, with the gap named, because an operator exploring an audience is better served by a partial answer they can see than by a 500. `compose-master` is the exception, and it is the exception because it WRITES.

Unlike the discovery flow in the LFX One UI, `POST .../discover` is **synchronous**. Goa v3 has no SSE encoding, so this service answers once and any progress rendering is the caller's own concern.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/audience-builder/capabilities` | `campaign_manager` | JSON | Whether the builder can be used at all, and if not, why. Reports `hubspot_configured` with a human-readable detail when it is false. **This endpoint returns no error for an unusable connection** — an unusable connection is its ANSWER, which is what lets a client render one explanatory banner with actions disabled instead of nine broken buttons. |
| POST | `/projects/{projectId}/audience-builder/discover` | `campaign_manager` | JSON | Fetch the event page behind the SSRF guard, extract the event's identity, search the portal for candidate lists, and classify each into a signal bucket. Classification is entirely DETERMINISTIC; the model is used only to read the event's name, brand and dates off the page. Returns the inspected count so a capped run is visible AS capped, and `uncertain` is a reported bucket rather than a silent drop. `400` for a URL the guard refuses. |
| GET | `/projects/{projectId}/audience-builder/lists/search` | `campaign_manager` | JSON | Typeahead over portal list names. `q` is required and must be non-empty — an empty term matches everything in the portal, so it is a `400` rather than an answer. |
| GET | `/projects/{projectId}/audience-builder/suppression-lists` | `campaign_manager` | JSON | Resolve the standard suppression terms (plus brand- and event-specific probes) to actual lists. Each row is keyed by the ROW, not the list, so a term that resolved to nothing is reported as unavailable instead of vanishing from the response — a missing GDPR row that silently disappeared is how a send goes out without it. |
| GET | `/projects/{projectId}/audience-builder/last-sent` | `campaign_manager` | JSON | The lists a prior edition's emails actually targeted, as precedent. `event_name` is required; `limit` defaults to 3 and is capped at **10** — the endpoint fans out per email against a rate-limited API, so the ceiling is a budget, not a preference. A list id still referenced by an email whose list was deleted is returned marked missing, not omitted. Rows are ordered by SEND date, newest first; a row whose send date the portal does not report sorts last, and `sent_at` is normalised to RFC 3339 or omitted. Candidates are matched on the event's distinctive tokens across an email's name AND subject rather than on one contiguous phrase, and `brand_short` is a fallback only when nothing matched the event itself. Only emails that actually went out count — a draft and an in-flight send are excluded, and so is a send whose REPORTED date is in the future. A send the portal reports no date for cannot be distinguished from one that went out, so it is returned with `sent_at` omitted rather than dropped. A bounded sweep that yields no rows is a recoverable failure, not an empty history -- whether nothing matched, or everything that matched was a draft or a scheduled send: an operator reads an empty panel as "this event has never been emailed" — but a request carrying no searchable tokens at all — an `event_name` that is a bare year or only tokens short enough to be dropped, AND no `brand_short` that survives the same filter — is answered as an empty history without a request, because there is nothing to search on. A degenerate `event_name` with a usable `brand_short` still sweeps, and may answer from the brand fallback. The send date is read authoritatively for a shortlist of `limit + 12` candidates (capped at 22) and the per-list fan-out runs only for the `limit` survivors, so ordering holds for any portal whose most recent sends fall inside that shortlist rather than depending on the projected date arriving. The headroom is a fixed 12 rows at every `limit` rather than a multiple of it — `3*limit` capped at 12 left only two rows of slack at `limit=10`, which is where the protection is needed most. |
| GET | `/projects/{projectId}/audience-builder/existing-master-lists` | `campaign_manager` | JSON | Master lists already in the portal for this event, newest quarter first. An undated list ranks below every dated one rather than being treated as recent. |
| POST | `/projects/{projectId}/audience-builder/preview-count` | `campaign_manager` | JSON | How many DISTINCT people the selection reaches, by unioning list memberships — summing list sizes over-counts, because registrant/speaker overlap is the normal case. `list_ids` must be non-empty and at most 50 (`400`) — the sweep is two sequential HubSpot round-trips per id, so the bound is a fan-out budget, not a form limit. **Above the 25,000-record paging cap the membership sweep is SKIPPED and the response returns the summed list sizes as an inexact estimate, never an exact number**: a truncated membership makes the union an UNDER-count, and understating how many people an email reaches is the one error direction that must never be presented as exact. When `exact` is false, `estimate` is the SUM of the selected lists' sizes — an UPPER bound on the union, since every contact in more than one list is counted once per list. It is not a floor. **If any selected list did not report a size at all, no number is returned** (`exact:false`, `count:0`, `estimate:0`, with a reason naming the cause): HubSpot omits the size property on some list shapes, and summing an absent size as zero would leave the total short by that entire list — the same under-count, arriving silently. |
| POST | `/projects/{projectId}/audience-builder/compose-master` | `campaign_manager` | JSON | **Creates real contact lists in the production HubSpot portal and is NOT idempotent.** Creates the combined suppression list first, then the master list that excludes it, and responds `201`. Takes no `event_url` — composition works from the lists and names the operator already reviewed, so a page edited since discovery cannot change what gets created. **With an optional `brief_id`, the composed master is ALSO recorded as that brief's built audience in the same call**, stamped with the portal it was composed in, and the response carries `recorded: true` plus a slim `audience` object (`id`, `status`, `version`, `platform_master_list_id`). Recording happens here, and nowhere else, because the portal must be read from the same build-scoped client that creates the lists — a portal resolved from a second credential would vouch for list ids it never saw, and `assertAudiencePortal` refuses to dispatch an audience whose portal is blank or mismatched while `refuseProvenanceBreakingPatch` makes the stamp unrepairable afterwards. Everything that can refuse a recording compose is checked BEFORE the first create: no audience/brief repository is a `503`, an unknown brief a `404`, and an unreadable brief a `503` — each with zero HubSpot lists created. `brief_id` is optional, and omitting it leaves the endpoint byte-identical to the exploratory behaviour: no portal lookup, no database write, `recorded: false`. An optional `inclusion_summary` supplies operator-written provenance for the recorded row; it is derived from the source lists when omitted. See the partial-failure contract below. |
| POST | `/projects/{projectId}/audience-builder/attach-existing` | `campaign_manager` | JSON | Record lists that ALREADY exist in the portal as a brief's built audience — creates nothing in HubSpot. Body: `{"attach": {"brief_id", "master_list_id"?, "include_list_ids"?, "suppression_list_ids"?, "inclusion_summary"?}}`. Send **exactly one** of `master_list_id` (one composed list) or `include_list_ids` (1–200 existing lists sent to directly, with no composed master — HubSpot's `contactIlsLists.include` is an array). Both, neither, a blank include entry, or an include id that is also in `suppression_list_ids` is a `400`; an id the portal does not have (or that is not a contact list) is a `404`. Include ids are trimmed and de-duplicated in order; the row records `platform_master_list_id` = the first and `include_list_ids` = all of them, and every email send targets all of them. Responds with `{master, suppression_list_ids, include_list_ids?, audience: {id, status, version, platform_master_list_id, include_list_ids?}}`. |
| POST | `/projects/{projectId}/audience-builder/qa/run` | `campaign_manager` | JSON | Pre-send QA on a composed list: four checks inferred from the list's own filter branch plus the NAMES of the lists it references (the portal carries no machine-readable marker for "this is the GDPR list"). **`NEEDS VERIFY` is the honest and most common verdict, and no caller may treat a `PASS` as authorization to send.** Every finding carries a fix; severity ranks by legal exposure, not tidiness. Check 4 (`current_registrants`) asks whether THIS edition's own registration list is being INCLUDED rather than suppressed — a registration-push send reaches people who have NOT registered, so its own registrants belong in the combined suppression. It needs the optional `event_name` to tell this edition from a past one or a sibling region, and **is omitted from the response entirely when that is absent or cannot decide** (an event name with no year, or one made entirely of portfolio-common words, where no token rule can separate a sibling). Absent means the audit did not run, which a caller must not read as a pass. |

**`compose-master` has TWO distinct `500`s and the status code cannot tell them apart.** Goa maps both the declared `ComposePartial` error and an ordinary internal error to `500`, discriminating with a `goa-error` response header — which a proxying BFF does not see. **The body is therefore the discriminator:** a `ComposePartial` body carries `code` and `message` plus whichever of `suppression` / `suppression_name` / `master_name` apply, while an internal error carries `code` and `message` only.

The distinction is the whole point of declaring the error, and it is never a bare retry: whichever list(s) the message names may already exist in the portal, and retrying either collides on a duplicate name or leaves a second list behind. Five shapes are reachable, and the field that is set tells the caller which:

- **`suppression` set, `master_name` unset** — the combined suppression list was definitely created (it has a real `list_id`) and the master create definitely failed. Surface the suppression list, with a link into the portal, and let the operator decide.
- **`suppression_name` set (no `suppression`, no `master_name`)** — the suppression *create itself* is unconfirmed: HubSpot may have created it under this deterministic name, but no id came back to confirm it. Search HubSpot by name before composing again; do not assume nothing happened.
- **`master_name` set, `suppression` and `suppression_name` unset** — no exclusions were requested (or the suppression create itself failed outright), and the master create is unconfirmed. Search HubSpot for the master by name.
- **`suppression` and `master_name` both set** — the suppression list was definitely created and the master create is unconfirmed. Verify both before retrying.

- **`master` set** — the RECORDING shape, reachable only on a compose that carried a `brief_id`. Both lists were definitely created and only the attachment to the brief failed, so this is the one shape carrying a CONFIRMED master object rather than a `master_name`. The lists are usable: attach the master to the campaign's audience by hand. Do not compose again — composing again mints a second master list for the same send.

`suppression` and `suppression_name` are never both set — an id is only ever reported once a create is confirmed, at which point there is no "unconfirmed name" left to report.

**`POST .../signal-list` is specified but deliberately NOT implemented, and is NOT routed.** Auto-creating a list for a missing signal needs portal-specific facts this service does not have — the internal name of the per-project subscription-type property, the `hosted_events` property, and the page-view filter shape. Guessing would create real, wrongly-filtered contact lists in a production portal. A read-only property-discovery pass against the live portal unblocks it.

Unlike the audiences endpoints above, these paths are **not** nested under `/briefs/`, so they inherit no gateway wiring: the HTTPRoute regex and the Heimdall `project-api` RuleSet each enumerate the nine leaves individually. That is deliberate rather than a `**` wildcard — the base path serves nothing, `signal-list` is unimplemented, and two leaves (`lists/search`, `qa/run`) are two segments deep, which a single-segment capture would miss. Heimdall is default-deny, so adding an endpoint to this family without editing both chart files makes it UNREACHABLE through the gateway (a 404 from the edge, not a permission error). `charts/lfx-v2-campaign-service/parity_test.go` carries positive rows for all nine leaves plus negative rows for the base path, an unknown leaf, and `signal-list`, so a one-sided chart edit fails the build.

### Monitoring (Insights Phase)

Metrics are read-through from the ad platforms, scoped by project. There are no per-platform root paths; the provider is a path segment under the project. The `/{provider}/metrics` shape below was designed so that, because a connection is singleton per project, it would unambiguously mean "metrics for **this project's** account on that provider" — but it was never built, and the two warnings that follow are the operative statement of what this section actually offers. Everything real under Monitoring is the two Google Ads rows.

> ⚠️ **The chart already routes and authorizes these five paths.** `charts/.../templates/ruleset.yaml` grants `campaign_manager` on `/projects/:projectId/{google-ads,linkedin-ads,meta-ads,reddit-ads,twitter-ads}/metrics`, and the HTTPRoute regex admits the same segment — so a request reaching them is authorized by Heimdall and forwarded to a service that serves no such route. The chart's `parity_test.go` cannot catch this: it checks the RuleSet against the HTTPRoute regex, and reads neither `design/` nor `gen/`, so chart routes are never compared against implemented endpoints. Whoever picks this up should decide whether to implement the route or withdraw the five chart entries; leaving both is the state that produced this note.

> ⚠️ **`GET /projects/{projectId}/{provider}/metrics` IS NOT IMPLEMENTED.** It is declared by no `design/` file on any branch, and appears in no generated mux; only the two Google Ads rows below and the campaign- and brief-scoped reads under [Optimization](#optimization) are real. (A comment on the abandoned `feat/LFXV2-campaign-metrics` branch refers to it as an existing read — that is a stale mention, not a declaration.) The entry is kept as a design sketch, not a contract — do not code against it, and treat its `days` parameter and its `CampaignMonitorResponse` body as unratified (both disagree with every shipped endpoint; see the notes on each). LFXV2-2641 was closed by `aef68385`, which shipped `/google-ads/keywords`, `/google-ads/audience` and `keyword-actions` and stated verbatim: *"Completes LFXV2-2641. The ticket's metrics endpoints and status toggle already shipped; this adds the last three, with paths taken verbatim from docs/api-catalog.md."* The metrics endpoints that had shipped are the brief- and campaign-scoped reads, not this route — and "paths taken verbatim from docs/api-catalog.md" is why this row mattered: the catalog was being read as a contract.

> **Reading ONE campaign's metrics is not here.** It is `GET .../briefs/{briefId}/campaigns/{id}/metrics`, documented once in [Optimization](#optimization) below. It sits on the campaign-scoped path rather than under `{provider}/metrics` because it needs the persisted campaign row (for its platform and `PlatformCampaignID`), not a provider+project pair — so it belongs with the other campaign-scoped actions.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/{provider}/metrics` | `campaign_manager` | JSON | **NOT IMPLEMENTED — design sketch only.** Intended as campaign metrics for this project's account on the provider, `{provider}` ∈ `google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`. The `days` param this row used to specify is **wrong for this codebase**: every shipped metrics endpoint takes a closed `window` enum (`today` … `last_month`, see `metricsWindowEnum` in `design/brief.go`). An explicit `window` always wins; a per-platform default applies only when the caller OMITS it (`defaultMetricsWindowFor`). On X Ads that default is `last_7_days`, and a wider explicit window is **rejected** rather than narrowed — the stats endpoint caps queryable ranges at 7 days per request, so `last_30_days` returns an error there. Anyone building this should take `window`, not `days`. Reading one campaign's metrics, or a whole brief's, is available today under [Optimization](#optimization). |
| GET | `/projects/{projectId}/google-ads/keywords` | `campaign_manager` | JSON | Google Ads keyword performance (top 50 by impressions over `window`, default `last_30_days`), **across the campaigns THIS PROJECT owns**. A live read-through: nothing is persisted, and this service stores no keyword — it enumerates nothing this service holds, which is why it is not a list endpoint under rule 3 (same reasoning as `/connection-google-ads/accounts` below). **Scoped to the project's own campaigns, NOT to the connected account.** Google Ads is one customer shared across every foundation (see [architecture.md](architecture.md), "Account Tenancy"), so a read scoped only by the connection would return every other project's keyword text, campaign ids and spend. The query is confined to the `platform_campaign_id`s this service holds for the project, read with `project_id` in the SQL WHERE clause. **A project that has dispatched no Google Ads campaigns receives an empty `rows` array and `row_count: 0`, not an error** — and no upstream query is issued at all, since an empty scope is exactly when an unscoped read would expose everyone else's data. **Scope is every live campaign row this service holds for the project that carries a `platform_campaign_id`** — the scope query filters on `project_id`, `platform` and `status <> 'deleted'`, and has NO dispatch-origin predicate. **ADOPTED campaigns are therefore IN scope**: `AdoptCampaign` persists the externally-created campaign's `platform_campaign_id`, which is exactly what the query selects. What is NOT in scope is a campaign this service has no row for at all, or one claimed but never given a `platform_campaign_id` (the column is required non-empty), or a soft-deleted one. **If any campaign in scope was created under a different ad account than the project's connection now resolves to, the whole read fails with 409 rather than returning the rest** — these responses carry no omitted-campaign signal, so a filtered subset would be indistinguishable from a complete answer. The status predicate is an ALLOW-LIST (`ENABLED`, `PAUSED`) rather than an exclusion of `REMOVED` — Google's enum also carries `UNSPECIFIED`/`UNKNOWN` and an omitted field decodes to `""`, all of which an exclusion would offer as actionable rows. `status` and `match_type` are normalised onto their declared enums, with **`UNKNOWN` meaning Google reported a value this service does not recognise** — never that the field was absent; the row keeps its ids and counters either way. **The rows are capped and `truncated` reports it** — they are the TOP keywords by impressions, not the full set held by **the project's own campaigns**, so a total over them is not even this project's whole spend — and it is never the account's, since the read never covered the campaigns this project does not own. **Only POSITIVE keywords are returned.** `keyword_view` carries both polarities, so the query restricts to `ad_group_criterion.negative = FALSE` and the response is re-checked before rows are published — a NEGATIVE keyword (an exclusion) is never listed. That is what keeps the handle contract true: every row carries the `criterion_id` + `ad_group_id` pair that `keyword-actions` takes, and `keyword-actions` REFUSES a negative criterion (pausing or removing an exclusion widens delivery and spend), so publishing one would offer a handle whose only advertised use is guaranteed to fail — and it would consume one of the capped rows, making `truncated` describe a set containing unactionable entries. An absent `negative` field means POSITIVE and is kept: protobuf JSON omits a false bool, so omission is the shape of the ordinary keyword, not a missing answer. Each row also carries the ad group and campaign **display names** alongside their ids — the names are for display only and are NOT identifiers, since two ad groups in different campaigns may share a name; address a criterion by `ad_group_id` + `criterion_id`. **`conversions` is a float and keeps its fraction** (Google credits fractional conversions under data-driven and position-based attribution); an absent upstream field is a measured `0`, since the field is always selected. **`quality_score` is OPTIONAL and its absence is not a zero**: Google withholds the 1-10 rating until a keyword has accrued enough impressions, so an unrated keyword omits the field entirely — `0` is off the scale, so a consumer must render absence as unknown and never as a low score. A score outside 1-10 is likewise omitted rather than published: the response type declares the bound, so publishing an out-of-range value would fail validation for the WHOLE response rather than one row. |
| GET | `/projects/{projectId}/google-ads/audience` | `campaign_manager` | JSON | Audience demographics — age, gender and device — over `window` (default `last_30_days`), **across the campaigns THIS PROJECT owns**. A live read-through; nothing is persisted. **Scoped to the project's own campaigns, NOT to the connected account**, for the same reason and by the same mechanism as `/google-ads/keywords` above: the shared Google Ads customer means a connection-scoped read would aggregate every project's targeting and performance distribution. **A project with no dispatched Google Ads campaigns receives an empty `buckets` array and no upstream query is issued**; scope is every live project row carrying a `platform_campaign_id`, which **includes ADOPTED (externally created) campaigns** — the scope query has no dispatch-origin predicate — and excludes only rows this service does not hold, rows with no `platform_campaign_id`, and soft-deleted ones. **A partial account mismatch fails the whole read with 409** rather than returning the matching subset, for the same reason a partial breakdown fails below. All three breakdowns arrive in ONE array discriminated by `dimension`. **Each dimension independently covers the same traffic, so every counter — impressions, clicks, `cost_micros` and `conversions` alike — totals within a dimension and never across them** — summing all three triple-counts. That matters most for `conversions`, which feeds CPA and ROAS: a triple-counted conversion count is a third of the true cost per acquisition. Google's `UNDETERMINED`/`UNKNOWN` buckets are returned as-is rather than dropped: they are real unattributed traffic, and hiding them would make the buckets silently under-sum. A failure in any one breakdown fails the whole request, because a partial demographic picture presented as a whole one is how a campaign gets re-targeted on the half of the data that loaded. Each bucket carries `conversions` alongside its other counters, as a float keeping its fraction. |
| GET | `/projects/{projectId}/microsoft-ads/keywords` | `campaign_manager` | JSON | Microsoft Advertising keyword performance (top 50 by impressions over `window`, default `last_30_days`; `yesterday` and `last_14_days` are not offered), **across the campaigns THIS PROJECT owns**, in the SAME row shape as `/google-ads/keywords` (`criterion_id` = Microsoft `KeywordId`, `ad_group_id` = `AdGroupId` — the handle pair a Microsoft keyword action takes; `cost_micros` = `Spend` × 10⁶ in the account currency; `ctr` a fraction). **Served from a SAVED asynchronous report, not live**: Microsoft builds keyword reports in minutes against a 20s request, so the response carries `metrics_as_of` (when the served report was requested; absent until one finishes) and `metrics_pending` (a newer report is building), exactly as the report-backed account monitors do; the first read returns no rows with `metrics_pending: true`. A saved report is served **only while it covers every campaign the project now owns** — after a new campaign is dispatched the read returns no rows until a report over the new scope finishes, rather than a partial table. `data_incomplete: true` means Microsoft flagged the served report's last day as still aggregating. `conversions_complete: false` means Microsoft left at least one returned row's conversion count blank (published as `0`, not a measurement). **Own connection only** (no LF system-account fallback → 404 when the project has none), bound account only; the report scope is the project's campaign ids (`Campaigns`, never `AccountIds`). **409** when any campaign in scope was created under a different ad account than the connection is bound to, the project owns more than 300 distinct Microsoft campaigns (the report-scope ceiling; duplicate rows count once), or a stored campaign id is not a valid Microsoft id. **500** when Microsoft rejects the campaign-only report scope itself (error 2027) — the request shape is ours, so it is not retried as a transient failure. **400** while `MICROSOFT_METRICS_ENABLED` is off. Saved reports live in `keyword_insight_reports` (cached platform data). Age/gender demographics are the sibling `/microsoft-ads/audience` below. See [microsoft-keyword-insights](knowledge/architecture/microsoft-keyword-insights.md). |
| GET | `/projects/{projectId}/microsoft-ads/audience` | `campaign_manager` | JSON | Microsoft Advertising age/gender audience demographics (`get-microsoft-ads-audience`, LFXV2-2665) over `window` (the keyword read's five windows, default `last_30_days`), **across the campaigns THIS PROJECT owns**: one bucket per (`age_group`, `gender`) — Microsoft's values verbatim — summed over the project's campaigns, with `impressions`, `clicks`, `cost_micros` (`Spend` × 10⁶ in the account currency, no FX; the currency is not reported) and `ctr` (a fraction), ordered by impressions; buckets are disjoint, so counters total across them. **NO device breakdown**: Microsoft's `AgeGenderAudienceReportRequest` has no device dimension (a device split would need a second report and is out of scope); no conversions. **Served from a SAVED asynchronous report, exactly as `/microsoft-ads/keywords`**: `metrics_as_of`, `metrics_pending` and `data_incomplete` with the keyword read's meaning; the first read returns no buckets with `metrics_pending: true`; a saved report is served only while it covers every campaign the project now owns. A project with no Microsoft campaigns gets `buckets: []` and Microsoft is not contacted. Same trust boundary and errors as the keyword read: own connection only (**404** when the project has none), bound account only, `Campaigns`-only scope; **409** for a campaign created under another account, more than 300 distinct campaigns, or a malformed stored id (the audience read's own messages); **500** when Microsoft rejects the campaign-only scope (2027); **503** for anything unverifiable (no upstream text in the body); **400** `keyword and audience insights are not supported for this platform` while `MICROSOFT_METRICS_ENABLED` is off. Saved reports are the `age_gender` kind of `keyword_insight_reports` (migration 000041). See [microsoft-keyword-insights](knowledge/architecture/microsoft-keyword-insights.md). |
| GET | `/projects/{projectId}/google-ads/campaign-ref` | `campaign_manager` | JSON | Resolve ONE Google Ads campaign id to this service's own campaign and brief. A caller holding keyword rows has the PLATFORM's numeric campaign id (`GoogleAdsKeyword.campaign_id` is Google's, not ours), while every mutation route here is keyed by this service's campaign UUID under its brief — nothing else bridges the two, so without this a keyword table cannot act on the rows it just displayed. A pure read: it contacts no ad platform and mutates nothing, so unlike the reads above it has **no 409** — there is no connection to be unusable and no ad account to mismatch. It does declare a **503** whenever `resolveBackendWithOrch` refuses because storage and the orchestrator are not wired. That is USUALLY cold start, and retrying is then the right answer — **but 503 here is not a promise that waiting clears it**, and the endpoint cannot tell the two apart: in the supported no-database mode `NewContainer` leaves the repository and orchestrator nil deliberately, so these routes stay mounted and answer this same 503 for the life of the process. Read it as "not available", not "not available yet". A third source produces the same status on this route without being about storage at all: a JWKS outage takes the 503 disposition rather than answering 401, so an authentication-key failure is indistinguishable here from an unwired backend (`internal/service/auth_test.go` pins that disposition). All three are "cannot answer right now"; none of them licenses a client to assume a retry will succeed. A storage FAULT is a **500** instead: that is a failure in a service already up, and retrying does not help. The two are deliberately distinct, and a caller should branch on them differently. **Scoped to the project's own campaigns by the same `project_id` predicate**, which is what stops it answering whether ANOTHER foundation owns a given id on the shared Google Ads customer. **An unowned id is `200` with an empty `matches`, NOT `404`** — "this project owns no such campaign" is an answer the caller acts on by refusing the action, and it must be distinguishable from the route or project being wrong. **`matches` is an array, but a valid database can never return more than one**: migration 000020's `uq_campaigns_platform_campaign_live` is a UNIQUE index on `(platform, platform_campaign_id)` over every live Google Ads row, and it is global rather than per-project, so scoping to a project can only narrow one row to zero or one. The array is DEFENSIVE against that invariant lapsing — a dropped index, a narrowed predicate, a platform added to this read but not to the index — not a claim that duplicates occur. A single-ref contract would force some layer to pick a row, and picking would mutate a campaign nobody named; a caller receiving more than one must refuse rather than choose. Soft-deleted campaigns are invisible, matching every other read. Not a list endpoint under rule 3: a keyed lookup for one supplied id, with no collection, pagination or filtering. |
| GET | `/projects/{projectId}/microsoft-ads/campaign-ref` | `campaign_manager` | JSON | Resolve ONE Microsoft Advertising campaign id to this service's own campaign and brief (`resolve-microsoft-ads-campaign`, LFXV2-2665) — the Microsoft twin of `/google-ads/campaign-ref` above, so a row from `/microsoft-ads/keywords` (whose `campaign_id` is Microsoft's CampaignId) can address `keyword-actions` and `negative-keywords`, which are keyed by this service's campaign UUID under its brief. Same `platform_campaign_id` query parameter (digits only, no leading zero, within int64; a malformed id is **400**), same `platform-campaign-resolution` result, and the same statuses and meanings as the Google row: a pure read of this service's tables that contacts no platform (so no **409**, and not gated on `MICROSOFT_METRICS_ENABLED`), **scoped to the project's own campaigns** by the same `project_id` predicate, an unowned id is **`200` with an empty `matches`**, a storage fault is **500**, and an unwired backend (or a JWKS outage) is **503**, which is "not available", not "not available yet". The platform is fixed by the route: a Google campaign with the same digits never matches here. **One difference: more than one match is REACHABLE.** Migration 000020's unique index is scoped to `google-ads` because Microsoft mints campaign ids per ad account, so a project whose connection was re-pointed between accounts can hold two live rows for the same id; every match is returned and a caller must refuse rather than choose. Soft-deleted campaigns are invisible. Not a list endpoint under rule 3. |
| GET | `/projects/{projectId}/meta-ads/audience` | `campaign_manager` | JSON | `get-meta-ads-audience` (LFXV2-2665): Meta audience insights over `window` (all seven, default `last_30_days`) **across the campaigns THIS PROJECT owns**, live from `GET /act_{id}/insights` on the connection's ad account with `level=campaign` and a `filtering` of `campaign.id IN` the project's own ids — the account is shared, so every returned row is ALSO checked against that scope and a foreign row fails the read. Two breakdowns in one `buckets` array discriminated by `dimension`: `age_gender` (Meta's combined `age,gender`; `age` and `gender` set) and `placement` (`publisher_platform,platform_position`; both set). Each bucket: `impressions`, `clicks`, `cost_micros` (micros of `account_currency`, no FX), `ctr`. **No conversions** — Meta reports them only per action type. Each dimension covers the same traffic: total within one, never across. Breakdown values are returned verbatim when they match a safe charset; any other value, a malformed or duplicated row, a key repeated anywhere in a response page (exact or case-folded — e.g. `data` twice, `data` and `Data`), an explicit `null` counter (an OMITTED counter is a measured 0), a currency disagreement, more than 20 pages per breakdown, or any upstream failure (transport, 5xx, 429 after retry, 401/403) is **503 with no partial rows**. No Meta campaigns of its own → **200 with empty `buckets`**, Meta not contacted. System scope → **404**; no connection (neither the project's own nor the LF system fallback) → **404**; the project's OWN connection inactive, its credential incomplete or no `act_<digits>` account selected → **400**, but the same defects on the **LF system fallback** connection (a project with no connection of its own) → **500** (`ErrSystemConnectionNotUsable` — only an operator can fix it); a campaign created under another ad account (any, not just all) → **409**; a stored non-canonical campaign id or more than 250 campaigns → **409**. The whole read — both breakdowns, up to 2×20 sequential pages, plus any 429 backoff — runs under the orchestrator's `metricsCallTimeout` (20s), the same budget as the Google audience read, so a very large project can **503 consistently** on timeout rather than intermittently. Reddit, LinkedIn: no audience route, and the read is `400 not supported` at the orchestrator (X has its own read, below). Microsoft serves age/gender buckets only, through its own report-backed `/microsoft-ads/audience` (below the keyword row); it has no placement breakdown. Not a list endpoint under rule 3. |
| GET | `/projects/{projectId}/twitter-ads/audience` | `campaign_manager` | JSON | `get-twitter-ads-audience` (LFXV2-2665): X Ads audience insights **across the campaigns THIS PROJECT owns** — three segmentations in one `buckets` array discriminated by `dimension`: `age`, `gender`, `platform` (X `segmentation_type` AGE, GENDER, PLATFORMS), each bucket `value` (X's segment name, verbatim when it matches a safe charset), `impressions`, `clicks`, `cost_micros` (X `billed_charge_local_micro`, micros of `account_currency`, no FX) and `ctr` (after summing). **No conversions.** Each dimension covers the same traffic: total within one, never across. **Window**: `today`, `yesterday` or `last_7_days` (default) — the X metrics read's window NAMES, but NOT its instants: this read takes the days on the ad **account's** calendar (its `timezone`), `[start of first day, start of the day after the last)`, while the X campaign metrics read takes them as UTC days, so on a non-UTC account the two cover windows offset by the zone's UTC offset and their totals are not directly comparable; any other window is **400** (`window must be one of: last_7_days, today, yesterday (X Ads reads cover at most 7 days)`; the design enum declares the same subset). An account whose timezone does not start its days on a whole UTC hour (Asia/Kolkata, …) is **409** (X takes whole hours only). **Upstream**: X serves segmented stats ONLY through its asynchronous stats-jobs API (the synchronous `GET stats/accounts/:account_id` takes no `segmentation_type`), so ONE read does `GET accounts/:account_id` (timezone, currency), then one `POST stats/jobs/accounts/:account_id` per segmentation per batch of ≤20 own campaign ids (`entity=CAMPAIGN`, `entity_ids`, `granularity=TOTAL`, `placement=ALL_ON_TWITTER`, `metric_groups=ENGAGEMENT,BILLING`, `segmentation_type`), polls `GET stats/jobs/accounts/:account_id?job_ids=…` and downloads each results file — all inside the orchestrator's 20s `metricsCallTimeout`, nothing persisted. Jobs not finished in time are **503** (retry later; the abandoned jobs expire on X). **Load bound** — each read holds stats-job slots on an ad account that every foundation on it, and the X account monitor, share (X allows 100 concurrent jobs per account): identical concurrent reads (same account, window and scope) share one set of jobs; a successful result is reused for 5 minutes while its window still names the same account-local instants; at most one audience read per ad account runs at a time, and a read waiting behind another past its deadline is **503**; a read refuses with **503** before its first job POST when the client's write-pacer backlog plus its own paced POSTs would not fit the budget. Every response is checked with `identityjson` before decoding and every file row must name a campaign of the job that produced it; a foreign row, duplicate key (any level, case-folded), repeated segment, out-of-charset value, malformed counter, int64 overflow, failed job, or any upstream failure (transport, 5xx, 429, 401/403) is **503 with no partial buckets**. Absent/null counters read 0 (X's "no activity"); `all_counters_null: true` flags a dimension whose rows carried no measured counter at all — idle delivery or X's reported all-null segmented-stats defect, which cannot be told apart — so those zeros are not a measurement. No X campaigns of its own → **200 with empty `buckets`** and no `account_currency`, X not contacted. System scope → **404**; no connection (own or LF fallback) → **404**; own connection inactive / credential incomplete / no usable `account_id` → **400**, the same on the **LF system fallback** → **500**; a campaign created under another X account (any) → **409**; a stored id that is not a valid X campaign id, or more than 40 campaigns (a local bound: at most six jobs per read) → **409**. **400 not supported** unless `TWITTER_METRICS_ENABLED` is `"true"` — the flag that gates the X account monitor, which shares this unverified stats-jobs contract (the synchronous X campaign metrics read is not gated). Microsoft, Reddit, LinkedIn: no audience route. Verified only against X's published documentation, not a live account. Not a list endpoint under rule 3. |
| GET | `/projects/{projectId}/meta-ads/campaign-ref`, `/projects/{projectId}/reddit-ads/campaign-ref`, `/projects/{projectId}/twitter-ads/campaign-ref` | `campaign_manager` | JSON | **This row stands for three endpoints** — `resolve-meta-ads-campaign`, `resolve-reddit-ads-campaign` and `resolve-twitter-ads-campaign` (LFXV2-2665), the Meta, Reddit and X twins of `/microsoft-ads/campaign-ref` above. Same `platform_campaign_id` query parameter, same `platform-campaign-resolution` result, same statuses and meanings as the Microsoft row: a pure read of this service's tables that contacts no platform (no **409**), **scoped to the project's own campaigns** by the `project_id` predicate, an unowned id is **`200` with an empty `matches`**, the reserved system scope is **404**, a storage fault is **500**, and an unwired backend (or a JWKS outage) is **503**. The platform is fixed by the route. **More than one match is reachable on all three** (000020's unique index is Google-only); a caller must refuse rather than choose. What differs is the id rule, checked by the Goa decoder AND again in the service before any lookup (each platform package's `ValidateCampaignID`), so a malformed id is **400** whichever door it came in by: Meta — digits only, no leading zero, at most 32; Reddit — letters, digits and underscores, at most 64; X — letters and digits, at most 64. Not a list endpoint under rule 3. |

> **Umbrella roll-up (TLF across child foundations).** This service never aggregates across projects: `campaign_manager` does not cascade from parent to child (see rule 2), and each project owns only its own connection. A TLF-wide view of, say, all Google Ads spend would be assembled by the **UI backend**. Note this is harder than the sentence here used to imply: it named a per-provider read that is not implemented (see the warning above), and **no project-wide metrics read exists either** — both shipped reads require a `brief_id` (`GET .../briefs/{briefId}/metrics` and `GET .../briefs/{briefId}/campaigns/{id}/metrics`). A roll-up built today therefore needs TWO levels of fan-out per child foundation: the brief ids first, then one brief-metrics call each. **The ids do not come from this service** — `GET /projects/{projectId}/briefs` is `find-brief`, which REQUIRES `event_slug` and returns a single brief, and rule 3 assigns brief lists to the Query Service deliberately. So the enumeration is a Query Service read, and only the per-brief metrics calls land here. One call per child omits every brief but one. Whether that is acceptable at TLF's brief count, or is the argument for building the project-scoped route, is a decision for whoever needs the roll-up — this note only records that the one-call-per-child shape does not exist. Keeping aggregation out of this service preserves the strict per-project permission boundary and avoids the service resolving project hierarchy.

> There are no `/{provider}/accounts` listing endpoints **under this Monitoring surface**. A project has at most one connection per provider, read directly via `GET /projects/{projectId}/connection-{provider}` (connections are not indexed into the Query Service — see the Platform Connections section). This is a statement about *stored* connections and does not forbid `GET /projects/{projectId}/connection-google-ads/accounts`, which is a different thing: a live, credential-scoped read of the accounts reachable **upstream at the provider**, used to choose which account the single connection should point at. It enumerates nothing this service stores.

### HubSpot UTM Integration

HubSpot campaigns are a **PORTAL-WIDE namespace**, not an LF-global one. A HubSpot connection is stored per project with its own token and `portal_id`, and a project with none resolves the LF system connection — so two projects share a namespace when they are configured against the SAME portal, which is the ordinary case for foundations under the LF umbrella since they share the one LF portal. This service does **not** scope UTM search within that portal: a lookup searches every campaign the connection's portal holds, and a created campaign is visible to everyone working in it. The `{projectId}` in the path gates the permission lookup AND selects which portal is visible (the caller must be a `campaign_manager` on **that** project — both OpenFGA arms in `ruleset.yaml` bind the relation to the project captured from this path, whether it arrives as a slug or a uuid); it does not filter results within the portal.

Lookup is a query by event name, passed as the `q` query parameter (loose name match over HubSpot's default searchable properties, NOT relevance-ranked — see [Platform-Specific Gotchas](#hubspot)). Because the namespace is portal-wide — every campaign in the HubSpot portal the project's connection authenticates against, which is not necessarily the LF's own — the UI **must caveat at create time** that a new UTM will be visible to everyone working in that portal, so users do not put anything project-sensitive in a UTM name. Projects configured against different portals do not see each other's campaigns.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| GET | `/projects/{projectId}/connection-hubspot/campaigns?q={name}` | `campaign_manager` | JSON | Look up HubSpot campaigns by name across the **entire HubSpot portal the project's connection authenticates against** (portal-wide namespace, not project-scoped). `projectId` gates permission AND selects which portal is visible — HubSpot connections are stored per project with their own token and `portal_id`, and a project with none resolves the LF system connection — so two projects see the same campaigns when they share a portal, which today is the ordinary case for LF foundations. Reads back each campaign's `hs_utm` token. The match is HubSpot's own `query` search over its default searchable properties — NOT an exact-name lookup, and **NOT relevance-ranked**: the CRM v3 search API has no relevance sort and no `sorts` is sent, so rows arrive in HubSpot's default order (unspecified — no `sorts` is sent and HubSpot documents no default) and **the first row is not the best match**. Every match is returned rather than narrowed to one, because choosing between similar names needs a human; any ranking a caller displays is its own. **An empty `campaigns` array is a `200`, not a `404`**: "no campaign is named that" is the answer a caller acts on by offering to create one, and it must stay distinguishable from a search that failed. A campaign with no `utm` is a real result; an absent token does **not** mean the campaign was not found, and treating it so would prompt a duplicate create. **The result set is CAPPED at 200 with no paging**: a campaign ranked below the cap is not returned, and a caller reads an absent campaign as licence to create one — so an operator who cannot find one should search a narrower term rather than assume it is absent. 200 is HubSpot's own per-request maximum (raised from 100 in September 2024). **`capped` reports when the gap is actually open**, derived from HubSpot's own total rather than the returned count. An id-less hit fails the whole response rather than being dropped, for the same fail-closed reason the missing-`results` case does. |
| POST | `/projects/{projectId}/connection-hubspot/campaigns` | `campaign_manager` | JSON | Create a portal-wide HubSpot campaign and return the `hs_utm` HubSpot assigns it, **when the create response carries one**. The token is OPTIONAL in the result: HubSpot's marketing create is not documented to return `hs_utm`, and this route deliberately does no follow-up search to fetch it — a second call after a non-idempotent write is another failure point, and its failure would make a campaign that EXISTS look like a create that did not happen. A tokenless success is a valid response, not a contract violation; the token becomes visible through the ordinary lookup on a later read. **Visible to everyone working in the HubSpot portal the project's connection authenticates against** — not necessarily the LF's own, since connections are per project with their own token and `portal_id`. The UI must warn before creating. **It always creates and performs no duplicate check**, deliberately: a search-then-create inside one call still races a concurrent caller and cannot prevent a duplicate, so the check belongs with the operator who can read the candidate names. Search first, show the matches, create only on confirmation. `hs_utm` is assigned by HubSpot and read back from the create response, never supplied here. A `2xx` carrying no id is reported as an **error**, because the campaign may or may not exist and cannot be addressed either way — check HubSpot rather than retrying into a second copy. Other failures fall into **four classes**, told apart by status. **`400` — nothing was created, and the request is correctable**: either HubSpot rejected it on the merits (a definite non-429 4xx), or the stored connection EXISTS but is not usable as configured; a `401`/`403` says so in its own words, because retrying another *name* cannot fix a permission problem. This holds for a connection the PROJECT owns. When the project has none and the request ran on the shared LF connection, the same rejection is a **`500`** instead: the system scope is unaddressable over HTTP (`rejectSystemScope`), so a 400 would tell the caller to repair a row they cannot reach and invite a retry that cannot succeed until an operator rotates the LF credential — which the 500 logs at ERROR to page. The response says nothing credential-specific, matching `classifyDiscoveryError`'s system arm. **`404` — no HubSpot connection is configured for this project**, which is a different remedy from the 400 above: connect HubSpot, rather than fix a credential. **`500` — the SHARED LF connection was refused, or the stored credential could not be decrypted**, or the service is otherwise faulted BEFORE the request went out; not the operator's to fix. All three prove nothing reached HubSpot, which is why they are reported as themselves rather than as an unconfirmed outcome — sending an operator to hunt for a campaign that was never attempted hides the real remedy. **`500` is reserved for that pre-send position**: a fault discovered AFTER the create returned without error is a `503`, not a `500`, because at that point the campaign may exist and only the service's reading of the outcome failed. **`503` — the outcome could not be confirmed, OR the request never left this service**: those share a status because both are retryable-when-things-recover rather than correctable by the caller, and the *message* distinguishes them — a pre-send failure (DNS, dial, an already-cancelled context) can promise nothing was created, which the unconfirmed case cannot. Everything else lands here too, *including any failure this service cannot positively classify*, because a non-idempotent write into a shared namespace fails closed. HubSpot marks mutating transport/429/3xx/5xx failures possibly-committed, and so is a 2xx whose body could not be decoded. |

### Optimization

Each optimization action is scoped to a single campaign under its brief and is individually permission-evaluated. Bulk cross-campaign endpoints are intentionally omitted (see rule 5).

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/status` | `campaign_manager` | JSON | Toggle campaign ACTIVE/PAUSED (Reddit, Meta, LinkedIn, X/Twitter, Google Ads, Microsoft Ads). **409** when the change is refused before the platform is contacted: the campaign is unprovisioned, or the connection row itself is unusable — no stored credential blob, or one too short for the encryptor to authenticate. Those are non-retryable, which is why none of them is a 503. **What counts as provisioned is channel-specific on Google Ads**, because the five channels it resolves do not have the same serving resources: a `default` (Search) campaign needs an ad group, an ad AND at least one keyword criterion; a `demand-gen` campaign needs the ad group and ad but NOT keywords — this service refuses keywords on that channel, so requiring one would be unsatisfiable by construction and the operator would be told to supply the very field the create path rejects; a `video` campaign takes the same gate as `demand-gen` and for the same reason, since keywords are refused there too, as does a `display` campaign; and a `performance-max` campaign has no ad groups and no ads at all, so the gate is its ASSET GROUP and the 409 says so in those words rather than describing an ad-group failure that cannot have happened. **Four of the five are created here; `video` is resolved by this gate but never created** — the Google Ads API has no call that creates a Video campaign, so a `video` row reaching this endpoint was ADOPTED, and adoption records no serving resources, which is the `targets` is empty arm. **On `performance-max` an asset group id alone is not enough**: the group is created before its asset links, so a failed or unconfirmed `assetGroupAssets:mutate` leaves the id recorded with nothing attached — an empty asset group looks finished in the Google Ads UI and cannot serve. A RECORDED link count of zero therefore refuses with a 409 naming the group. A count of `nil` does not: a row written before the count was recorded cannot distinguish "no links" from "not recorded", and refusing those would break activation on correctly provisioned campaigns. The arm is keyed on the campaign's variant, which is part of its identity rather than its config, and a row written before variants existed keeps the Search rules it was created under. **Activation also cascades to the resource that actually serves**: asset groups are created PAUSED like every other resource this service creates, so an ACTIVATE flips the asset group first and the campaign last — the campaign never reports ENABLED before the thing that delivers does — while a PAUSE flips the campaign first and the asset group after, stopping spend immediately even if the second mutate then fails. **Four further 409 reasons come from the adapter's own pre-flight**, each tagged with `ErrConnectionNotUsable`: `reason=connection_inactive` (the row's status is not `active`), `reason=credentials_undecodable` (the decrypted blob is not valid JSON), `reason=credentials_incomplete` (a required credential field is empty) and `reason=account_not_selected` (credentials stored, no ad account chosen yet). **Google Ads, Reddit, X/Twitter and Microsoft Ads emit all four.** **Meta emits the first three** (`resolveMetaCredentials`, `internal/dispatch/meta.go`) **and deliberately not the fourth**: a status update targets the campaign node by platform id and never reads `AccountConfig.AccountID`, so an account cleared after creation must not block pausing or resuming. That guard is `requireMetaAccountID`, and it is reached only from Dispatch. **LinkedIn emits all four as of LFXV2-3196** — `resolveLinkedInCredentials` mirrors `resolveMetaCredentials`, replacing the inline checks that previously fell through to 503. **A fifth adapter pre-flight reason is LinkedIn-only as of LFXV2-3281**: `reason=credentials_expired` (the stored access token has expired and could not be renewed — either no refresh token is stored, since LinkedIn issues them only to approved Marketing Developer Platform partners, or the refresh token is itself expired/revoked). It is tagged with `ErrConnectionNotUsable` like the others, so it is a 409 rather than the 500 an expired token produced before, and it is distinct from `credentials_incomplete`: nothing is missing from the row, what was saved simply aged out, and only a member re-authorization repairs it. Unlike Meta it DOES emit `account_not_selected` on this path: LinkedIn's client is constructed with a `RuntimeConfig` naming the account, so an empty account id cannot reach the platform at all, whereas Meta targets the campaign node by platform id and never reads the account. **Two further LinkedIn-only reasons come from the SAME token exchange, and neither is `credentials_expired`** — `ToggleStatus` routes every token-exchange defect through the same `linkedinConnectionDefect`/`linkedinExpiry` pair, so this row's vocabulary is identical to the metrics row's below. Only RFC 6749 §5.2 `invalid_grant` (and a body the client cannot read) means the grant itself died. `invalid_client` and `unauthorized_client` name the APPLICATION registration and carry `reason=application_credentials_invalid`: no member re-authorization repairs them, and an operator must correct the connection's stored application credentials. `invalid_request`, `unsupported_grant_type` and `invalid_scope` carry `reason=token_request_rejected`, and they are the one reason in this whole vocabulary that names NO operator remedy: LinkedIn refused the SHAPE of the request this service built, so neither stored credential was evaluated, and editing a credential cannot make a malformed refresh request well-formed. (This client sends no `scope` parameter on a refresh grant at all.) Report it as a service defect. **An account mismatch — the campaign belongs to a different ad account than the connection now resolves to — is raised by every paid-ads adapter**: Google Ads and Microsoft Ads, and as of LFXV2-3050 LinkedIn, Meta, Reddit and X/Twitter too. Campaign ids are unique only WITHIN an ad account, so a connection re-pointed between create and toggle would address an unrelated campaign, and this path changes delivery. Each adapter checks it BEFORE its own narrower provisioning guard, so a foreign-account campaign answers the mismatch rather than describing the wrong campaign's provisioning. **Absent provenance is not a mismatch**: a row created before its adapter stamped the account records none, and is waved through as "unknown" rather than being made un-pausable until a re-dispatch. **One reason stays Google-Ads-only on this path**: `reason=provider_config_invalid` (the stored `login_customer_id` is not digits-only, so no manager id can be sent) — Meta raises `ErrProviderConfigInvalid` too, but only from Dispatch, never from a toggle. **500** is reserved for defects the caller cannot act on: the project has no connection of its own, fell back to the LF system row, and THAT row is unusable; a defect in THIS SERVICE (`ErrServiceDefect` — LinkedIn refused the SHAPE of the refresh request, carrying `reason=token_request_rejected`, so no stored credential was evaluated and no operator has anything to repair either); or the stored credential blob failed GCM authentication (`ErrCredentialDecryptionFailed`), meaning the application's encryption key no longer matches it — a rotated `CREDENTIAL_ENCRYPTION_KEY` or a corrupted row, and this path cannot tell them apart — re-saving credentials repairs the corrupted row, but no reconnect touches a rotated key, so the answer is the conservative one. **404** is the third permanent answer, added alongside them: no connection row exists for this project and provider at all, and the shared system row did not cover it either — there is nothing to repair, so the caller is told to connect rather than to fix. It is deliberately not a 409 — a 409 tells the caller to repair "this project's connection", which is a scope they do not own and cannot address. The `reason` token is logged, never returned. **One request succeeds without persisting anything**: pausing a campaign in `created_degraded` pauses it upstream and returns **200** with the status and ETag UNCHANGED, no version bump and no index event. `created_degraded` records that the campaign's wiring was never verified and the row has a single status column, so writing `paused` would spend the reconciliation marker to record a run state the ad platform already holds authoritatively — and pausing reconciles nothing, it stops spend. Activating such a campaign is refused with **409**. A caller that needs to confirm the pause reads it from the ad platform, not from this row. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/metrics` | `campaign_manager` | JSON | Read live performance metrics (impressions, clicks, cost, CTR, and conversions where the channel reports them) for one campaign directly from the channel that runs it — an ad platform, or HubSpot for the email channel. **`conversions` is OPTIONAL and its absence is meaningful**: it is omitted entirely for Meta, X, Reddit and the email channel, none of which expose a campaign-level conversion count, and an absent value means "not measured here" rather than a measured zero — a consumer must not render it as `0` or fold it into a conversion total. Where it is present (Google Ads, LinkedIn, Microsoft) it is carried on the same `float64` wire type, but only two of the three can be **fractional**: Google Ads and Microsoft both type their conversion metric as a double and credit partial conversions under data-driven, position-based and offline attribution, so a campaign can genuinely hold `0.4` — for those two, do not round it, and in particular do not treat a value below 1 as zero. **LinkedIn is an integer count widened onto that shared type**: `externalWebsiteConversions` is typed `long` in the Ads Reporting schema, so it never carries a fraction — a LinkedIn value below 1 is always exactly `0`. Pure read — never persisted, unlike `GET .../campaigns/{id}`. `window` query param (`today`, `yesterday`, `last_7_days`, `last_14_days`, `last_30_days`, `this_month`, `last_month`; default `last_30_days`, except X Ads which defaults to `last_7_days` since its stats endpoint caps queryable ranges at 7 days) is a closed, platform-agnostic vocabulary — each dispatcher maps it to its own platform's date-range dialect. A platform with no `MetricsReader` wired returns 400. **409 covers two different moments, and the distinction is operational, not "was the channel ever contacted."** The first group is refused before the TENANT-SCOPED METRICS REQUEST — the read that would actually return numbers — is attempted, and waiting will not change that. Every platform returns it when the campaign is unprovisioned (empty `PlatformCampaignID`), and when the connection row is unusable in one of the two ways the SHARED resolver detects — no stored credential blob (`reason=credentials_absent`) or a blob too short to authenticate (`reason=credential_blob_malformed`). These are genuinely pre-contact: nothing is called at all. **Google Ads, Reddit and X/Twitter** additionally tag the four defects their own pre-flight detects with `ErrConnectionNotUsable`, so those are 409 too: `reason=connection_inactive` (the row's status is not `active`), `reason=credentials_undecodable` (the decrypted blob is not valid JSON), `reason=credentials_incomplete` (a required credential field is empty) and `reason=account_not_selected` (a connection created with credentials only, whose ad account has not been chosen yet). **HubSpot tags the first three but not the fourth** — there is no ad account in an email connection to choose. **Meta tags the first three and not `account_not_selected`** — `ReadMetrics` resolves through `resolveMetaCredentials`, the same helper `ToggleStatus` uses, and like the toggle it targets an existing campaign by platform id via `GET /{campaignID}/insights` without reading `AccountConfig.AccountID`, so an account cleared after creation must not block reading metrics. **LinkedIn emits all four as of LFXV2-3196** — `ReadMetrics` resolves through `resolveLinkedInCredentials`, the same helper `ToggleStatus` uses. It emits `account_not_selected` here too, for the same reason: the client cannot be constructed without an account id. **A fifth adapter pre-flight reason is LinkedIn-only as of LFXV2-3281**: `reason=credentials_expired` (the stored access token has expired and could not be renewed — either no refresh token is stored, since LinkedIn issues them only to approved Marketing Developer Platform partners, or the refresh token is itself expired/revoked). `ReadMetrics` routes it through the same `linkedinConnectionDefect`/`linkedinExpiry` pair the toggle uses, so it is tagged with `ErrConnectionNotUsable` and answers 409 rather than the 500 an expired token produced before. It is distinct from `credentials_incomplete`: nothing is missing from the row, what was saved simply aged out, and only a member re-authorization repairs it. **A token-endpoint rejection that names an RFC 6749 §5.2 code describing the CLIENT or the REQUEST — `invalid_client`, `invalid_request`, `unauthorized_client`, `unsupported_grant_type` or `invalid_scope` — is NOT this reason**: only `invalid_grant` (and a body the client cannot read) means the grant itself died. Those five split across TWO further reasons, by who can act. `invalid_client` and `unauthorized_client` name the APPLICATION registration and carry `reason=application_credentials_invalid`: no member re-authorization repairs them, and an operator must correct the connection's stored application credentials. `invalid_request`, `unsupported_grant_type` and `invalid_scope` carry `reason=token_request_rejected` instead — LinkedIn refused the SHAPE of the request this service constructed, so neither stored credential was ever evaluated and there is no field on a connection whose editing repairs it (this client does not even send a `scope` parameter on a refresh grant). It is the one reason token in this vocabulary that points at the service rather than at the caller's configuration: report it as a bug, not as a connection to repair. Reporting these three as `application_credentials_invalid`, as this service briefly did, sends an operator to audit a correct configuration — the same actionable-but-useless remedy the `credentials_expired` split exists to retire. **`reason=provider_config_invalid` remains Google-Ads-only** — the stored `login_customer_id` is not digits-only, so no manager id can be sent. **Account-identity mismatch is emitted by every adapter that verifies tenant identity — Google Ads, Microsoft Ads, LinkedIn, Meta, Reddit and X/Twitter for an ad account, and HubSpot for a portal — and the ad adapters and HubSpot verify different things.** For the ad adapters the campaign was created under a different ad account than the connection now resolves to; platform campaign ids are account-scoped, so reading one under the wrong account silently yields zeros or another account's numbers, and the fix is to reconnect the original account. Those checks resolve entirely from locally-stored account ids, so they are pre-contact in the strict sense. **Absent provenance is not a mismatch on the ad adapters**: a row created before its adapter stamped the account records none and is waved through as "unknown", since failing closed would make every pre-existing row unreadable until a re-dispatch. HubSpot is the deliberate exception — see the narrower `ErrCampaignProvenanceUnknown` below. HubSpot's is not: a HubSpot email id is a bare numeric unique only within its portal, so the dispatcher records the portal the private-app token authenticates against at create time — read by POSTing the token to `/oauth/v2/private-apps/get/access-token-info`, not from the optional operator-supplied `portal_id` config, which a credential swap leaves untouched — and `ReadMetrics` calls `AuthenticatedPortalID` against that same endpoint to learn the token's CURRENT portal before it can compare. The channel IS reached, just not for the tenant-scoped metrics themselves. **The two HubSpot remedies differ, and the response message is what carries the distinction:** a recorded-but-different portal is `ErrCampaignAccountMismatch` and can be repaired by reconnecting the original portal; a row with no recorded portal at all — which is every campaign staged before this landed — is the narrower `ErrCampaignProvenanceUnknown`, and since there is nothing to reconnect to its message says to re-dispatch instead. That second case IS strictly pre-contact, unlike the mismatch: an absent recorded portal is decided from the row alone, so it is refused before `AuthenticatedPortalID` is called at all. The order matters operationally — checked after the lookup, a legacy row read while token-info was throttled would surface as the transient 503 below rather than this 409, offering "try later" for a row that only re-dispatch can fix. The refusal is deliberate rather than a best guess, because reading across a re-point is wrong in both directions — a same-numeric collision reports another portal's opens and clicks as this campaign's, and no collision reports "not sent yet" for an email that was sent. The second moment belongs to HubSpot alone: `GetEmailMetrics` — the tenant-scoped metrics call itself — succeeds and matches no sent email in the window, which `internal/dispatch/hubspot.go` tags with `domain.ErrNoMetricsInWindow` so it lands on 409 rather than the 503 default. Nothing is broken — a staged draft nobody has sent yet is the ordinary state of this channel between `Dispatch` and the send — so read this 409 as "no data", not as "repair your connection". The response body does not separate these cases (`ConflictError` carries only `code` and `message`), so the message text is what distinguishes them: the first group names the connection, the provisioning state, or (for HubSpot's identity checks) the portal; this one names the window. **500**, as on the status toggle, covers the defects on a scope the caller cannot address, so none is a 409: the project has no connection, fell back to the LF system row, and that row is unusable; a defect in THIS SERVICE (`ErrServiceDefect`, `reason=token_request_rejected` — the refresh request this service built was malformed, so neither stored credential was evaluated and there is nothing for an operator to repair); or the stored credential blob failed GCM authentication (`ErrCredentialDecryptionFailed`) — a rotated `CREDENTIAL_ENCRYPTION_KEY` or a corrupted row, and this path cannot tell them apart — re-saving credentials repairs the corrupted row, but no reconnect touches a rotated key, so the answer is the conservative one. **404**, also as on the toggle, is the third permanent answer: no connection row exists for this project and provider and the shared system row did not cover it, so there is nothing to repair and the caller is told to connect. Both were 503 before LFXV2-3065, which invited a retry that could never succeed. Support is per-platform (see below). |
| GET | `/projects/{projectId}/briefs/{briefId}/metrics` | `campaign_manager` | JSON | **Read every campaign on a brief in one request.** Same read-through as the campaign-scoped row above, fanned out across the brief's campaigns concurrently. `window` accepts the same closed vocabulary and applies to every campaign, with the same per-platform default fallback (X Ads cannot serve `last_30_days`, so with no window named its rows are read over `last_7_days` while the others use 30 — **each row reports the window IT was read over in `metrics.window`; the top-level `window` is the requested one and does not claim to cover every row**). **A per-campaign failure does NOT fail the request.** Every campaign gets a row, including unreadable ones, and each carries its own `status`: `ok` (the only status carrying `metrics`), `unsupported` (no `MetricsReader` for the platform, or the window exceeds what it can serve — the 400s of the campaign-scoped endpoint), `not_ready` (unprovisioned, or the platform reported no data in the window — includes the ordinary state of a staged email draft nobody has sent yet), `connection_problem` (unknown provenance, account mismatch, or an unusable connection — the operator must repair the connection; retrying will not help), and `failed` (the platform read itself failed; transient, retrying may help). **A non-`ok` row omits `metrics` entirely rather than carrying zeroes** — a zero is a measurement, and substituting one for a campaign that could not be read is indistinguishable from a campaign that genuinely served nothing. `reason` carries a fixed, consumer-safe sentence, never the adapter's error text, which can embed a platform response body or an operator-supplied account id. `ok_count` reports how many rows carry a measurement, so a consumer can see that a cross-campaign total covers 2 of 6 campaigns before presenting it. **There is no cross-channel cost total**: `cost_micros` is micro-units of each platform's own native currency and this service performs no FX conversion, so summing them would produce a figure with no currency. Each `ok` row also carries `pacing`: spend against what the flight expects **by now**, not against the whole budget — a campaign three days into a thirty-day flight is expected to have spent a tenth of it. Expected spend is prorated over the OVERLAP of the row's window with the campaign's flight, so a 7-day read is compared against 7 days of plan rather than the whole elapsed flight — and a window that precedes the flight (`last_month` for a campaign that started last week) yields `unknown` rather than pacing a correct zero spend as underspending. **The flight's `end_date` is INCLUSIVE — the campaign runs through the end of that day**, matching how every ad platform is asked for the same range, so a flight of `2026-08-17`..`2026-08-18` is two days of plan and one whose start equals its end is a valid single day. Before LFXV2-3314 this endpoint treated the end date as an exclusive midnight, which cut the last day off every flight: a two-day flight was priced as one (an on-plan campaign reported 200% and `overspending`), and on the final date the window/flight overlap collapsed to zero so `pacing` read `unknown` for that whole day. **A consumer that reconciled its own pacing against this endpoint will see figures move on short flights and on every flight's last day.** **A campaign in its first day is also `unknown`**: a minute into a 30-day $1000 flight the expected spend is two cents, so a zero would raise a HIGH-priority underspending item against a campaign whose only property is being new — and platform reporting lag means the measured spend is not trustworthy that early either. **`pacing.pct` is ABSENT, not zero, when pacing cannot be derived** (no budget, no usable flight, an unreadable budget), with `pacing.label` reading `unknown` — a `0` there would be indistinguishable from a campaign that spent nothing. Pacing is **per campaign only**: like `cost_micros` it is denominated in the platform's own currency, so pacing figures must never be totalled or averaged across rows. `action_items` is derived service-side from the readable rows, so every consumer applies the same thresholds rather than each deriving its own; each item carries a stable `rule` token (`zero_delivery`, `underspending`, `budget_constrained`, `low_ctr`, `no_conversions`) to group or link on, since the `issue` prose is free to be reworded. **`zero_delivery` waits for the flight to begin** — a campaign dispatched days before its start date has delivered nothing for the same reason it has spent nothing, and it uses the same one-elapsed-day floor as pacing so both agree on when an absence is evidence. It is also **paid-ads only** — the email channel bills nothing per send and its adapter always reports `cost_micros: 0` while mapping opens onto `impressions`, so absent spend there is the normal state and carries no delivery signal; an email delivered to every recipient but opened by none would otherwise be reported as a campaign that never ran. **It also suppresses the pacing items for that campaign**: something that never started is trivially at 0% of plan, and emitting both would hand the operator two `HIGH` findings with opposite remedies — one saying no budget change will fix it, the other saying to adjust the budget. **Every rule is gated on the campaign's status**, which carries both a provisioning state and the run state the toggle sets: a `paused` campaign raises nothing, because zero spend is the intended outcome of pausing it, and neither does a `pending` one, which has not necessarily reached the platform. **`no_conversions` flags real traffic that converts nobody** — zero MEASURED conversions over enough clicks to mean anything. It is gated on the platform reporting conversions **at all**: `metrics.conversions` is ABSENT (not `0`) for Meta, X, Reddit and the email channel, none of which expose a campaign-level conversion count, and the rule never fires on an absent count — a rule that fired because data is missing would flag every campaign on those platforms forever. Like `low_ctr`'s impression floor, it carries a click floor below which zero conversions is variance rather than a broken funnel. **A row whose window does not overlap the flight raises no items either** — `last_month` for a campaign that started this month is a correct zero that means "not running yet". **Rows that could not be read raise no items**, so an empty `action_items` means nothing was flagged *among the readable rows* — check `ok_count` against the row count before presenting it as an all-clear. Request-level errors remain: `400` for an invalid `window` (refused before any platform is contacted), `404` for a missing or archived brief — a brief with no campaigns is **not** an error and returns an empty `rows` array, since that is what every brief looks like before it is dispatched. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions` | `campaign_manager` | JSON | Pause or remove Google Ads or Microsoft Advertising keywords on this campaign. **Google Ads is ALL-OR-NOTHING**: its batch is one atomic `adGroupCriteria:mutate` with partial failure disabled, so every action applied or none did — a caller stopping a budget leak is never left working out which half took effect, and on Google Ads `applied_count` therefore always equals the number requested (Microsoft Advertising is NOT atomic — see below). **`REMOVE` is IRREVERSIBLE** — Google cannot re-enable a removed criterion, only create a new one with a new id. There is deliberately no `ENABLE`: this surface only ever reduces what serves. Google Ads and Microsoft Advertising only; any other platform is **400**, as is a malformed batch (non-numeric ids, an unsupported action, a duplicated criterion, or a criterion outside this campaign's ad group). **409** when the change is refused before the ad platform is contacted — the campaign is unprovisioned (no platform campaign id, or no ad group), it belongs to a different ad account than the connection now resolves to, **it records no creating ad account at all** (`ErrCampaignProvenanceUnknown`: unlike the metrics reads, which proceed on an unrecorded tenant, this path FAILS CLOSED on both platforms — Google Ads is one customer shared across foundations and criterion ids are account-scoped bare numerics, so an unprovable tenant plus an irreversible `REMOVE` is not a risk worth taking; the message says re-dispatch, not reconnect, because there is no account to reconnect to), or the connection row is unusable; none is a 503 because waiting fixes none of them. **500** for the two operator-only defects the toggle also reports (an unusable LF system fallback, or a credential blob that failed GCM authentication), and **404** when no connection exists at all. **503** covers two DIFFERENT outcomes and the message is what separates them, so a client must read it rather than branch on the status alone: a DEFINITE upstream failure (nothing was applied — retry is the right remedy), and an **UNCONFIRMED** one where the mutate may ALREADY have been applied (a short or mismatched `adGroupCriteria:mutate` response, a 5xx, a timeout). The unconfirmed message says so and tells the caller to **verify the campaign's keywords in the platform before retrying** — retrying an irreversible `REMOVE` that already ran cannot undo it and only creates noise, so an ambiguous outcome deliberately does not get the same answer as a definite one. This mirrors the status toggle's unconfirmed arm. A malformed batch reports **400 even when the campaign is also unprovisioned**: a permanent input fault the caller must fix dominates a contingent state fault they can only wait on, matching the order both adapters validate in. Unlike the status toggle this takes **no `If-Match` and no write lock**: it persists nothing, so there is no version to bump and no index event — the keywords live upstream. **Microsoft Advertising (LFXV2-2665) is NOT atomic**, and the row says so per result rather than pretending otherwise: `PAUSE` is one `UpdateKeywords` (`PUT Keywords`, `{AdGroupId, Keywords:[{Id, Status:"Paused"}]}`) and `REMOVE` one `DeleteKeywords` (`DELETE Keywords`, `{AdGroupId, KeywordIds}`), PAUSE sent first, and Microsoft applies each item independently, naming the ones it rejected by `Index` in `PartialErrors`. The same guards run first and in the same order (batch, provisioning, ad group taken from the row, provenance FAILING CLOSED, account match), plus an ownership READ — `GetKeywordsByAdGroupId` (`POST Keywords/QueryByAdGroupId`) — that refuses (**400**, nothing changed) any keyword id that is not a live, non-deleted keyword of THIS campaign's ad group. The **200** then carries exactly one result per action in request order (`results[i]` answers `actions[i]`), each with `outcome` `APPLIED` / `FAILED` (with `error_code`) / `UNCONFIRMED`, no `resource_name` (Microsoft has none), and `applied_count` counts only `APPLIED`. An error Microsoft did not pin to an index makes every un-named item `UNCONFIRMED`; a refusal that follows a retried 429 is `UNCONFIRMED`, never `FAILED`. A Microsoft request answers **503** only when no call was answered item by item — unconfirmed for a 5xx, timeout, retried-429 refusal or unreadable 200, definite otherwise. Google Ads responses are unchanged: `resource_name` on every result, no `outcome`, `applied_count` equal to the request. **⚠️ Known limitation (Microsoft, documented, not fixed): a keyword PAUSED here is re-enabled by the next ACTIVATE of the campaign** (`PATCH …/status` → active), because the status cascade enables every keyword the campaign was created with and this endpoint persists nothing that would let it tell an operator's pause from the Paused state keywords are created in. There is no `ENABLE` action; to keep a keyword paused across a campaign pause/resume, pause it again after activating (or pause it in Microsoft Advertising), and to un-pause one, activate the campaign or enable it in Microsoft Advertising. A REMOVED keyword is NOT re-created — the cascade skips keywords no longer live. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/negative-keywords` | `campaign_manager` | JSON | **Add campaign-level negative keywords to a live campaign (LFXV2-2665). Microsoft Advertising only** — any other platform is **400** before anything is contacted. Body `negative_keywords`: 1–60 of `{text, match_type}`, `match_type` `Exact` or `Phrase` (Microsoft: "The supported values for a negative keyword are Exact and Phrase"), `text` at most 100 characters (Microsoft's NegativeKeyword.Text limit) of letters, digits, spaces and `& ' - .` with no two punctuation characters together (Microsoft's text policy refuses symbols such as `@ < > = { } [ ] \ ¤ §` and consecutive non-alphanumerics), the same text+match type at most once (refused, not de-duplicated, because results are positional). One `AddNegativeKeywordsToEntities` call (`POST EntityNegativeKeywords`, `EntityType: "Campaign"`, the campaign id taken from the row). Same guards as keyword-actions: batch first (a malformed batch is **400** even on an unprovisioned campaign), then provisioning, provenance (**fails closed**), account match — all **409** before Microsoft is contacted. **NOT ATOMIC**: the **200** carries one result per requested keyword in request order with `outcome` `APPLIED` (+ `negative_keyword_id`), `ALREADY_PRESENT` (Microsoft answered `CampaignServiceNegativeKeywordAlreadyExists`, 4335 — the requested state holds, so it counts toward `applied_count`), `FAILED` (+ `error_code`), or `UNCONFIRMED` (Microsoft answered but not about this keyword). A campaign-level (entity) error is a definite **503**. **NOT retried on 429** (an add with no idempotency key): a 429, 5xx, timeout or unreadable 200 is the **unconfirmed 503** — verify the campaign's negative keywords before retrying. Like keyword-actions it persists nothing, so **no `If-Match`, no write lock, no ETag, no index event**. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-targeting` | `campaign_manager` | JSON | **Read the keyword TARGETING of a Reddit or X campaign (LFXV2-2665, `get-keyword-targeting`).** On these two platforms a keyword is an entry in the targeting of the ONE ad group (Reddit) or line item (X) this service created, not a criterion with a status, so it cannot be paused — only removed. Read live, never persisted. Response: `platform`, `targeting_entity_id` (the ad group / line item), `keywords` (positive keywords only, in the platform's order; each `{keyword}` on Reddit, `{keyword, criterion_id, match_type}` on X), and on Reddit `revision` — a `sha256:` fingerprint of the ad group's WHOLE targeting, which a removal must send back. **Reddit** keywords come from `redditConfig.keywords` at create. **X: the create path sets NO targeting criteria**, so a campaign this service created reads as an empty list until an operator adds keywords in X Ads Manager. Before anything is returned the row must record the ad group / line item, the campaign's ad account must match the connection, and the platform must report the ad group / line item under THIS campaign. Any other platform is **400**. **409** unprovisioned, no recorded ad group / line item, a different ad account, the ad group / line item gone or reporting another campaign, or a targeting that is not a legible keyword list. **503** when the platform could not be read. No per-keyword metrics: see `docs/knowledge/architecture/keyword-targeting-reddit-x.md`. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-targeting/removals` | `campaign_manager` | JSON | **Remove keywords from a Reddit or X campaign's keyword targeting (LFXV2-2665, `remove-keyword-targeting`).** Body `keywords`: 1–20 items — `{keyword}` on Reddit (compared and echoed exactly as sent — no trimming or case folding; all-whitespace is 400), `{criterion_id}` on X — and on Reddit `revision` from the read (must be absent on X). Persists nothing: no `If-Match`, no ETag. Guards, all before anything changes: the batch; provisioning; provenance FAILS CLOSED (a row recording no creating account is 409, re-dispatch); the account match; the ad group / line item is read and must report THIS campaign; every named keyword must be in its current targeting (an X criterion id from any other line item, a negated keyword or a non-keyword criterion is 400). **Refused (409) when it would remove every keyword** — the ad group / line item would stop being keyword-targeted and serve to its other targeting alone, a widening. **Reddit**: one PATCH of the ad group carrying the WHOLE targeting object exactly as read with the named keywords taken out (Reddit replaces targeting as a whole), only if the targeting's fingerprint still equals `revision` (else **409**, nothing written); then a re-read must show exactly the written keywords and no other targeting member changed, or the outcome is UNCONFIRMED. All items share one outcome. **Default-OFF** behind `REDDIT_KEYWORD_TARGETING_WRITES_ENABLED="true"` (otherwise **400**): the whole-object round trip has not been exercised against a live ad account. **X**: one `DELETE targeting_criteria/{id}` per item, in request order, never retried on a 429, each with its own `outcome` (APPLIED / FAILED / UNCONFIRMED) and `error_code` (`NOT_SENT`, `NOT_FOUND`, `REJECTED`, `WOULD_EMPTY`); the targeting is re-listed before EACH delete, and an item whose criterion has meanwhile gone (`NOT_FOUND`) or that is now the last keyword (`WOULD_EMPTY`) is not sent, so a concurrent removal cannot combine with this one to empty the line item (the moment between that re-list and the DELETE remains); `applied_count` counts APPLIED. **503** only when no item got a definite answer; its MESSAGE separates a definite failure (retry) from an UNCONFIRMED one (read the targeting again first). Platform text is never returned. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/meta-ad-sets` | `campaign_manager` | JSON | **Read the AD SETS of a Meta campaign live (LFXV2-2665, `list-meta-ad-sets`).** Optional `window` (the metrics vocabulary; default `last_30_days`, anything else `400`). A pure read, never persisted, no ETag. Per ad set: `id`, `name`, `status` (configured), `effective_status`, `bid_strategy`, its OWN budget as `budget_type` (`daily`/`lifetime`) + `budget_amount` (whole units of `currency`, two decimals, from Meta's minor units with the account currency's own offset — the settings readback's rendering; ABSENT under Campaign Budget Optimization, or when the currency's scale is not in the supported map, never guessed), and `impressions`, `clicks`, `cost_micros`, `ctr` over the window, plus `recorded: true` on the one ad set this service created (the row's recorded ad set id; none on an adopted row). An ad set that delivered in the window but that Meta's listing no longer returns (deleted/archived since) is reported with `listed: false` and its counters only, so the campaign's spend is never under-reported. Upstream: `GET /{campaign_id}/adsets?fields=id,name,status,effective_status,daily_budget,lifetime_budget,bid_strategy,campaign_id,account_id&limit=100` (cursor paging, at most 10 pages), `GET /act_{id}?fields=currency`, and ONE `GET /act_{id}/insights?level=adset&fields=adset_id,campaign_id,impressions,clicks,spend,account_currency&filtering=[campaign.id EQUAL {id}]&date_preset=…&limit=500` (cursor paging, at most 20 pages). Every page is checked as RAW BYTES (`identityjson`: duplicated keys incl. case-folded ones, malformed UTF-8, unpaired surrogates) before it is decoded; every listed ad set must report THIS campaign and the connection's account; every Insights row must name this campaign, a canonical ad set id at most once, and the account's currency; an explicit `null` counter is refused while an absent one is `0`. All or nothing. **`400`** for a non-Meta campaign or a bad window. **`409`** when the row has no platform campaign id, does not record the ad account it was created under, records a different account than the connection, or Meta reports an ad set under another account; also for an unusable Meta connection or one with no ad account selected, and for a stored platform campaign id that is not a valid Meta id (decided locally). **`404` for a missing campaign ROW or a project with no Meta connection** — nothing Meta says is ever a `404`: its 100/33 on the campaign is a `503` (the settings readback's rule). `500` for the LF system connection, credential decryption or a service defect. **`503`** for anything unverifiable; the message is fixed text, never Meta's. |
| POST | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/meta-ad-sets/{ad_set_id}/status` | `campaign_manager` | JSON | **Pause or resume ONE ad set of a Meta campaign (LFXV2-2665, `toggle-meta-ad-set-status`).** Body `{"status": "ACTIVE"\|"PAUSED"}`; requires `If-Match` with the campaign row's ETag exactly like `toggle-campaign-status` (`428` missing, `412` stale — checked against the loaded row before anything else), and holds the campaign's write lock for the call. `ad_set_id` must be 1–32 digits without a leading zero (`400`, before any connection work). Before anything is written: the campaign must record its ad account and match the connection (`409`, zero requests); **`ACTIVE` is allowed only on the ad set this service created for the campaign** (`409` otherwise — a hand-added ad set's targeting was never verified here) and is refused on an ADOPTED campaign (`409`, the campaign toggle's rule); `PAUSED` is allowed on any of the campaign's ad sets; the campaign toggle's row-state rule applies (pending/orphan `409`, `created_degraded` may only pause); then `GET /{ad_set_id}?fields=id,campaign_id,account_id,status` must report THIS campaign under THAT account (`409`) and `ACTIVE`/`PAUSED` (`DELETED`/`ARCHIVED` `409`). Already there → `200` `ALREADY_IN_STATE`, **nothing sent**. Otherwise ONE `POST /{ad_set_id}` `{"status": …}`, **never retried**: `200` `APPLIED` (with `previous_status`) when Meta answers `{"success":true}`. **Failures:** a failed pre-write read, a write never sent, or a definite refusal (a parsed Graph error that is not `is_transient`, not code 1/2, not 408, not a throttle) → `503` "nothing was changed"; anything else after send (throttle, 5xx, 3xx, 408, transient/unknown/HTML error, timeout, a 2xx without `success:true`) → **`503` "the ad set status change is unconfirmed — it may or may not have been applied on Meta; read the ad sets before retrying"**, no ETag, write lock held for the 30 s cooldown — exactly as the campaign toggle, budget and keyword levers answer it. No Meta connection `404`; unusable connection / no ad account `409`; LF system connection, decryption or service defect `500`. If the write was APPLIED but the campaign row changed or was deleted meanwhile, `409` stating the ad set's status **was changed on Meta** (a verification error is `503` saying the same). **The ad set's status is NOT stored on the campaign row**, so the row is not written and the `200` ETag is its UNCHANGED version (the paused-`created_degraded` precedent). **Interaction:** `toggle-campaign-status` cascades ACTIVATE to the recorded ad set, so a campaign ACTIVATE re-activates the recorded ad set; a deliberate ad-set pause does not survive a campaign pause/resume. Non-Meta `400`. |
| GET | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/settings` | `campaign_manager` | JSON | **Read the campaign's CURRENT configuration from the platform and report where it diverges from what the campaign row recorded.** A pure read — never persisted — and the one read `.../metrics` cannot be: impressions, clicks, cost and CTR do not describe a campaign's *configuration*. The row records what a dispatch **asked for**; nothing pushes the recorded config upstream, and more than one path lets the recorded settings and the live campaign drift apart, so the two can legitimately disagree. **This endpoint never writes back onto the row** — doing so would change those columns' meaning from request to observation and let one transient bad read destroy the only record of the request — and it never issues a mutating upstream call. There is deliberately **no stored status and no polling**: a status that goes stale is worse than none, so divergence is answered on demand. Each entry in `fields` carries `recorded`, `upstream` and a `comparison` of `match`, `diverged` or `unknown`. `budget_amount`, `budget_type`, `campaign_name`, `advertising_channel_type`, `start_date` and `end_date` are COMPARED; `status`, `budget_delivery_method`, `budget_explicitly_shared` and `bidding_strategy_type` are reported **upstream-only** with no `recorded` counterpart, so they always carry `unknown`. Their reasons differ. `budget_delivery_method` and `budget_explicitly_shared` have no recorded side because nothing this service records expresses them, and they are reported anyway because a budget that reads as expected while being `ACCELERATED` or shared across campaigns is exactly what explains a spend anomaly the compared fields cannot. `bidding_strategy_type` is upstream-only for a third reason: `googleAdsConfig.biddingStrategy` DOES record one, but comparing it would mean mapping this service's caller vocabulary onto Google's OUTPUT_ONLY `BiddingStrategyTypeEnum`, and that mapping is unverified exactly where it is ambiguous — `target-cpa` and `maximize-conversions` are both sent as `maximize_conversions` — while on the adopt path the recorded strategy was never pushed upstream and the two sides are expected to disagree. A guessed mapping would report a false divergence on a campaign set exactly as asked, so the field stays `unknown` until a live readback settles the enum. `status` is different: the campaign row DOES record a status, and it is upstream-only because that lifecycle status and Google's delivery status are different axes — see the dedicated paragraph below. Flight dates are normalised to `YYYY-MM-DD` before comparison — Google returns `yyyy-MM-dd HH:mm:ss` in the ad account's timezone, so a raw comparison would flag every campaign that actually agrees. **`match` and `diverged` both require BOTH sides to have been read; a side that could not be read is ABSENT from the response — never zero-filled — and its verdict is `unknown`, never `match`**, because agreement asserted from an observation nobody made is a fabricated match. `diverged_count` and `unknown_count` are reported separately so "2 differ" is not read without "and 5 were not compared". **`unknown_count` is NOT a read-failure count**: it counts every field that was not compared, which on a fully healthy readback is most of them — `status` plus the three other fields that remain upstream-only, are permanently `unknown` by construction — `status` is itself one of the upstream-only fields and is not an extra one on top of them. On Google Ads that floor is four on a row whose `config_snapshot` records a channel and five on a legacy row that has none. The floor is per platform, counted on a healthy CREATED row that recorded a window: **Microsoft 3** of 6 (`status`, `budget_explicitly_shared`, `bidding_strategy_type`), **Meta 2** of 7 (`status`, `bidding_strategy_type`), **Reddit 3** of 8 (`status`, `bidding_strategy_type`, `is_campaign_budget_optimization`), **X 3** of 8 (`status`, `bidding_strategy_type`, `budget_optimization`); a row created without a window adds its two dates. Those floors assume the platform reports the budget mode the comparison needs; a HEALTHY read can sit two higher when it does not, because both budget fields are then `unknown` by construction rather than unread: **X 5** when X reports `budget_optimization` as `LINE_ITEM` or omits it (which X's current reference makes the default for campaigns this service creates), **Reddit 5** when `is_campaign_budget_optimization` is off, and **Meta 4** when the budget lives on the campaign (CBO) instead of the ad set. Read the per-field `comparison` and the upstream-only budget-mode field to tell this from a failed read. An ADOPTED row on these four records no budget and no window, so its floor is every field but `campaign_name`: **Microsoft 5**, **Meta 6**, **Reddit 7**, **X 7**. The two flight dates USED to sit in that floor, because `googleAdsConfig` carried no dates and their recorded side was always empty; they now compare for real when a campaign was created with a window, and remain `unknown` only where there is nothing to compare — a campaign created before the fields existed, or one created without them (both are optional). An ADOPTED campaign is not automatically in that set: adoption records the window the dispatch asked for, so if the adopting request supplied dates the recorded side exists and the comparison is real — which is the point, since adoption pushes nothing upstream and the readback is the only thing that can say whether the campaign actually carries it. A consumer watching this number for read failures would see a constant floor it cannot distinguish from a real one; the per-field `comparison` is what says which is which. `status` is reported with no `recorded` counterpart and is deliberately never compared — and NOT because the row lacks a column for it. The campaign row HAS a `status` column; it simply does not hold the same axis. That column carries this service's own lifecycle vocabulary, which is mostly provisioning state (`pending`, `created`, `created_degraded`, the retained-partial orphan markers, the soft-delete `deleted`) and only sometimes a run state, whereas Google's `ENABLED`/`PAUSED`/`REMOVED` is purely delivery state. A `created` campaign is not more or less `ENABLED` than a `created_degraded` one, so comparing the two columns would report a permanent, meaningless divergence on nearly every campaign while saying nothing about whether the campaign is actually serving. The upstream value is reported on its own so an operator can see the delivery state directly. **Wired for Google Ads, Microsoft Advertising, Meta, Reddit and X (Twitter) Ads**; every other platform (LinkedIn, HubSpot) returns **400**, as it does for any unwired capability. The field set above is Google's; the other four report only what their platform can answer honestly, in the same vocabulary, and each compares like with like in the units its own create path wrote. **Microsoft Advertising** (one account-scoped `GetCampaignsByIds` read, every campaign type requested): COMPARES `budget_amount` (`DailyBudget`, a plain decimal in the ad account's currency — the unit the create path sends — rendered at two places when whole cents and at full precision otherwise, and read ONLY when `BudgetType` is a daily type, since under `LifetimeBudgetStandard` it is not a daily rate), `budget_type` (`DailyBudgetStandard`/`DailyBudgetAccelerated` → daily, `LifetimeBudgetStandard` → lifetime, anything else `unknown`) and `campaign_name`; reports `status`, `budget_explicitly_shared` (a `BudgetId` attaches a shared Budget; an unreadable one is absent, never `false`) and `bidding_strategy_type` (`BiddingScheme.Type`, suffix stripped) upstream-only; reports NO flight dates, because a Microsoft campaign has none and the create path records none. **Meta** (`GET /{campaign_id}` plus the ad set the row recorded, and the ad account's currency only when there is an amount to render): COMPARES `campaign_name` (campaign) and `budget_amount`, `budget_type`, `start_date`, `end_date` (ad set — the create path puts budget and flight there); reports `status` (campaign) and `bidding_strategy_type` (ad set) upstream-only. Budgets are minor units: both sides are reduced to minor units with the account currency's own offset — the recorded side as `round(amount × offset)`, exactly what the create path sent — and rendered back by one function, so a JPY row recorded as 1000.40 compares as the 1000 actually sent; a currency outside the supported map leaves the amount `unknown`. A Campaign Budget Optimization campaign holds no ad-set budget, so both budget fields are `unknown` (the shared campaign budget is not the recorded ad-set budget's counterpart). Because `GET /{id}` is NOT account-scoped, the `account_id` Meta reports is compared with the recorded account too, and an ad set recorded on the row that belongs to another campaign upstream is refused as well (see the upstream-identity **409** below). For the same reason Graph's code 100 / subcode 33 on the campaign — documented as "does not exist, cannot be loaded due to missing permissions, or does not support this operation" — is **503** on every HTTP status, exactly as on the adoption lookup: no further read can tell a deleted campaign from one this token cannot load, so no account probe is attempted. **Meta's readback therefore never answers 404 for the platform campaign**: a campaign Meta has deleted or archived still answers with that status and is reported with it (200), and 100/33 is 503. On Meta the only 404s are the ones that never reach Meta — no such campaign row, and no connection for the channel. **Reddit** (one account-scoped campaign read — the create path sets budget, flight and bid strategy on the campaign, so no ad group is read): COMPARES `budget_amount` (`goal_value` micros, rendered like Google's), `budget_type` (`LIFETIME_SPEND` → lifetime, `DAILY_SPEND` → daily), `campaign_name`, `start_date` and `end_date`; reports `status` (`configured_status`), `bidding_strategy_type` and `is_campaign_budget_optimization` upstream-only. The budget is read only when `is_campaign_budget_optimization` is true — with it off (or unreported) `goal_value` is not the recorded budget, and both budget fields are `unknown`; the flag itself is reported so the operator can see why. An `ad_account_id` naming another account is the upstream-identity **409**. **X** (account-scoped campaign read plus the line item the row recorded): COMPARES `budget_amount` (the campaign's `daily_budget_amount_local_micro`, or the total when only a total is set), `budget_type`, `campaign_name` (campaign) and `start_date`, `end_date` (line item — X takes the flight there); reports `status` (`entity_status`), `bidding_strategy_type` (line item) and `budget_optimization` upstream-only. The budget is compared only when X reports `budget_optimization` `CAMPAIGN`: under `LINE_ITEM` (or unreported) each line item governs its own spend and a campaign-level total cap is not the recorded daily amount, so both budget fields are `unknown`. This holds for campaigns this service created as well: the create path sends no `budget_optimization` (X's current reference lists `LINE_ITEM` as its only POST value), and which value X reports for such a campaign is unverified — X's v11 announcement says `CAMPAIGN` is the default, its current reference says `LINE_ITEM` — so a created campaign may legitimately read its budget `unknown`; the readback never assumes `CAMPAIGN`. A line item recorded on the row that belongs to another campaign, or an `account_id` naming another account, is the upstream-identity **409**. Flight dates on Meta, Reddit and X are compared as **UTC calendar dates** of what the platform echoes (whatever timezone it renders in), because every create path sends them as UTC instants. Meta and Reddit nudge a start whose day has already begun forward to dispatch time + a buffer; when that nudge lands on a later UTC day that is the row's creation day or the day after, `start_date` is shown with BOTH sides and an `unknown` verdict — the readback cannot tell that nudge from a later edit — rather than a false `diverged`. A flight timestamp that does not parse against the documented layout is absent, never passed through raw. On all four, an **adopted** row (provenance only, no child ids) reads at the campaign level: fields that need a child the row never recorded (Meta's ad set, X's line item) and sides the row never recorded (an adopted row records no budget or window on these platforms) are absent and `unknown`, never an error. A `DELETED`/`ARCHIVED` Meta or Reddit campaign is reported with its status, not as absent; a deleted X campaign and a Microsoft id the account does not hold are **404**; Meta's 100/33 is **503**, never 404. Provenance is held exactly as strictly as on Google: unknown provenance is refused **409** BEFORE any connection is resolved, and an account mismatch is **409** before the campaign is read. A platform answer that contradicts the recorded identity while the connection IS the recorded account (Meta/Reddit/X reporting the campaign under another account, a recorded Meta ad set or X line item now in another campaign) is a separate **409** (`ErrCampaignUpstreamIdentityMismatch`) whose fixed message says the platform's record no longer matches what this service created and the campaign should be re-dispatched — "reconnect the original account" would be unactionable there. None of the four persists the readback or issues a mutating call. **404** when the platform holds no such campaign (it may have been deleted upstream — kept out of the 503 default, which would invite retrying a read that will keep reporting nothing), and when the project has no connection for the channel. **409** for an unprovisioned campaign (no platform campaign id, so nothing to compare), an account mismatch (the campaign belongs to a different ad account than the connection now resolves to — reading it there could return another campaign's configuration and report it as this one's divergence), unknown provenance, and an unusable connection. **Unknown provenance IS a 409 here, deliberately** — and this endpoint is STRICTER than the metrics read and the status toggle, which wave an unstamped row through. `ReadSettings` fails closed BEFORE the platform call when the row records no creating customer, returning `ErrCampaignProvenanceUnknown` joined with `ErrCampaignAccountMismatch` so existing mismatch callers keep matching while the handler's dedicated arm can tell the two apart. The reason the convention diverges here: the stored platform campaign id is unique only WITHIN a customer, so querying it under an unverified account can, on an id collision, return ANOTHER account's campaign — and this endpoint would then report a divergence between this campaign's recorded budget and a different campaign's actual one. A confidently wrong report about somebody else's account is precisely what a readback must not produce, and an absent creating account is a purely LOCAL fact no answer from Google could change. The remedies differ too: a mismatch has an original account to reconnect to, whereas unknown provenance has none, so the row must be re-dispatched. **500** for an unusable LF system connection or undecryptable credentials. **503** when the settings could not be read — which covers BOTH a platform that could not be reached AND one that answered with a response this service refused to trust (more than one row for a unique id, an unhonoured id filter, disagreeing identity fields, mutually exclusive budget amounts, a period contradicting its amount). The message names neither cause, because the two are indistinguishable to the caller and a connectivity claim would be false for the second; the specific refusal is logged. |
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/budget` | `campaign_manager` | JSON | **Change how much a campaign may spend on its ad platform, then persist the new amount.** **Google Ads, LinkedIn, Meta, Microsoft Advertising, Reddit and X** today; any other platform is **400**. The refusal list below is the UNION of the six budget models — a platform whose model has no analogue of a given refusal simply never raises it, and which platforms support the capability is decided solely by whether their dispatcher implements it, never by an allowlist in the service. Also **400** is a `budget` that is not a finite number, is not strictly greater than zero, is below the contract floor of 0.000001 (the LOOSEST floor any supported platform has: Google bills in micros, LinkedIn in whole cents, Meta in the account currency's minor unit — refused by request validation before the service runs; a direct caller below it is refused only when the amount rounds to zero micros, i.e. under half a micro; the design's `Minimum` is that micro, not zero: 0.000001 is the intentionally stricter HTTP (Goa) contract floor, while the service's own guard refuses only what rounds to zero micros), or exceeds 1,000,000,000, and a `budget_type` outside `daily`/`lifetime`. **A platform's OWN minimum is also a 400, not a 503**: the checks above are only the bounds that hold for every platform at once, and an adapter holding a stricter floor — LinkedIn's `$10` daily / `$100` lifetime, or Meta's one minor unit of the account currency — refuses the amount before anything is written, and Microsoft's own definite refusal of the amount (`CampaignServiceInvalidDailyBudget` — below the minimum, or not a settable amount, in the account currency — or a daily budget below what the campaign has already spent) leaves the campaign unchanged the same way; either is a permanent request fault and is answered as one, with the adapter's own explanation of what was wrong. **The amount is in the AD ACCOUNT's own currency, not USD** — this service never converts. **AMOUNT ONLY: the pacing model is never changed here.** A request whose `budget_type` differs from the campaign's CURRENT upstream pacing is refused **409** rather than translated — daily and lifetime write different fields on every platform (`amount_micros` vs `total_amount_micros` on Google's budget resource; `dailyBudget` vs `totalBudget` on the LinkedIn campaign; `daily_budget` vs `lifetime_budget` on the Meta ad set; Microsoft Search campaigns have only a daily `DailyBudget`, so a `lifetime` request for one is always this 409; `goal_type` `DAILY_SPEND` vs `LIFETIME_SPEND` on the Reddit campaign; X campaigns are written as a daily amount only, so a `lifetime` request for one is always this 409), and silently switching one for the other would change the campaign's whole spend model behind a request that only named a number. **Each platform models a budget differently, and that decides which guards even have a subject.** *Google:* the budget is a SEPARATE RESOURCE from the campaign (`campaign_budget`), so three facts are read from the platform before anything is written: which budget resource is attached, whether it is `explicitly_shared`, and its period. **A SHARED budget is refused (409) before any mutate**: writing it through one campaign moves the spend of every other campaign attached to it, including campaigns this service does not own and cannot see — the create path pins `explicitly_shared: false`, so only ADOPTED campaigns can reach this refusal, and the remedy is a human one in the ad platform. An unreadable shared flag is refused the same way rather than assumed unshared. *LinkedIn:* the budget is a pair of FIELDS ON THE CAMPAIGN (`{amount, currencyCode}` with a two-decimal string amount), so a campaign id fully addresses its budget and **there is nothing to share** — that refusal has no analogue and is deliberately absent rather than forgotten. In its place is a CURRENCY guard (**409**): the minimums are USD-specific and this service only ever SENDS `currencyCode "USD"`, so a campaign denominated in anything else — including one whose currency LinkedIn did not report at all — would be silently redenominated by writing over it. *Meta:* the budget is on the AD SET, in the account currency's MINOR UNITS, and **Campaign Budget Optimization is Meta's form of a shared budget** — the campaign holds one amount and distributes it across every ad set beneath it, so an ad-set write there either fails or converts the campaign off CBO, and both change spend this request never named; it is refused **409**. An ad account whose currency has no known minor-unit scale is also **409**, not 400: the amount is fine and the remedy is in Meta Ads Manager, not in the request. *Microsoft:* the budget is a pair of FIELDS ON THE CAMPAIGN (`DailyBudget`, a plain decimal in the account currency sent unrounded, and `BudgetType`) — **unless the campaign is attached to a shared Budget** (`BudgetId` set), Microsoft's form of a shared budget, which is refused **409** from the read before any write; Microsoft's own `CampaignServiceCannotUpdateSharedBudget` on the write (a budget attached between the read and the write) is the same 409, and nothing changed. An unreadable `BudgetId` is refused **409** rather than assumed unshared, and so is an **experiment campaign**, whose budget is inherited from its base campaign and cannot be set on it. The campaign's `BudgetType` must be `DailyBudgetStandard`, and the write sends it back unchanged, so only the amount moves; a Search campaign reporting `DailyBudgetAccelerated` — which Microsoft documents as available only to Audience campaigns — is a contradictory response and is refused **409** before any write rather than echoed back. *Reddit:* the budget is the CAMPAIGN's `goal_value`, in micro-units of the account currency — the create path sets `is_campaign_budget_optimization: true` with `goal_type: LIFETIME_SPEND`, so every campaign this service creates is `lifetime` and a `daily` request against one is the pacing **409** above. Only `goal_value` is written. A campaign whose budget optimization is OFF has its spend governed per ad group; that is refused **409** rather than allocated across ad groups, as is one whose budget-optimization flag or `goal_type` Reddit does not report. There is no shared-budget or currency analogue. *X:* only the CAMPAIGN's own `daily_budget_amount_local_micro` is written, in micro-units of the account currency, and only when X reports campaign budget optimization (`budget_optimization` `CAMPAIGN`). That campaigns created here have that shape is INFERRED from the create path (a daily amount on the campaign, no `budget_optimization` sent, no line-item budget) and is unverified against a live account — X's current reference lists `LINE_ITEM` as the only value. A `lifetime` request is always **409**: under campaign budget optimization X requires the daily budget, so the campaign is paced daily. Only the daily amount is PUT; `budget_optimization`, `entity_status` and `total_budget_amount_local_micro` are never sent. A campaign reporting `LINE_ITEM` or omitting `budget_optimization`, one with no daily budget (total-only or neither), one that also carries a total cap, or one with an unreadable amount is refused **409** before any write. X publishes no per-currency budget minimum, so only the service's own bounds are checked before the PUT; an amount X itself refuses comes back as X's definite 4xx, answered **503** "not modified" as for Reddit. A refusal that follows a retried 429 is instead the UNCONFIRMED **503** (verify before retrying), because the 4xx answers only the last attempt. There is no shared-budget analogue. **409** likewise when the campaign is unprovisioned, belongs to a different ad account than the connection now resolves to, records no creating ad account at all (re-dispatch, not reconnect), or the connection row is unusable; **404** when the platform holds no such campaign, or no connection exists at all; **500** for the two operator-only defects the toggle also reports. Requires `If-Match` (**428** missing, **412** mismatch) and takes the campaign write lock, since it persists. **503 covers two outcomes and the message separates them**: a DEFINITE failure (nothing changed) and an **UNCONFIRMED** one where the mutate may already have applied — unlike `keyword-actions`' irreversible `REMOVE`, re-applying a budget CONVERGES (the mutate is sent idempotent), so a retry is safe once verified, but the caller is still told to verify because this service's stored amount and the platform's may differ until they do. **UNCONFIRMED also covers a mutate Google ACCEPTED but did not acknowledge usably** — a 2xx naming no budget resource, or naming a different one: the request reached the platform, so "nothing was modified" is precisely the claim that cannot be made. **Only the budget columns are written**: `status` in particular is left exactly as found, which is what lets a `created_degraded` campaign — one that definitely exists upstream and may be spending — have its budget cut without losing its reconciliation marker. |
| PATCH | `/projects/{projectId}/briefs/{briefId}/campaigns/{id}/bid` | `campaign_manager` | JSON | **Set a campaign's MANUAL max cost-per-click bid on its ad platform, then persist it** (LFXV2-2665). Body: `bid` (Float64 > 0, in the AD ACCOUNT's own currency, contract range one micro to 1,000,000) and optional `bid_type` (only `cpc`; defaults to `cpc` when omitted — applied by the service, not a Goa default, so the generated CLI accepts a body without it). Requires `If-Match` (**428** missing, **412** stale). **Microsoft Advertising, Reddit, Meta and X** today; Google Ads, LinkedIn and the email channel are **400**. The bid goes where the create path put it: Microsoft — the default `CpcBid` of the ONE ad group this service created (its keywords carry no bids, so they inherit it); Reddit — the `bid_value` of the ONE ad group this service created; Meta — the `bid_amount` (a bid cap in the account currency's minor units) of the ONE ad set this service created; X — the `bid_amount_local_micro` of the ONE line item this service created. A row recording none (an adopted campaign) is **409**: the endpoint does not choose among ad groups, ad sets or line items. **409 when the bid would be ignored, and the strategy is NEVER switched**: Microsoft writes only under the campaign's own `EnhancedCpc` or `ManualCpc` (any automated scheme — MaxClicks, MaxConversions, TargetCpa, TargetRoas, MaxConversionValue, TargetImpressionShare, CostPerSale — any portfolio strategy, or an unreported scheme is refused); Reddit writes only to a `MANUAL_BIDDING`, `CPC` ad group that reports this campaign as its owner, and only when the campaign's own strategy allows it (with Campaign Budget Optimization on — as the create path sets it — the campaign itself must be `MANUAL_BIDDING`); Meta writes only to an ad set of this campaign under `LOWEST_COST_WITH_BID_CAP` with `billing_event` AND `optimization_goal` both `LINK_CLICKS` (a Meta cap is per optimization event, and per 1,000 impressions when billed on impressions, so only that pairing is a max CPC — `LOWEST_COST_WITHOUT_CAP`, `COST_CAP`, `LOWEST_COST_WITH_MIN_ROAS`, impression- or `CLICKS`-billed caps are refused); X writes only to a line item of this campaign with `bid_strategy` `MAX` and `pay_by` `LINK_CLICK` (`AUTO`, `TARGET`, impression-charged, deleted or unreported are refused). **Every Reddit campaign this service creates is `BIDLESS` on both the campaign and its ad group**, so the Reddit leg applies only after an operator switches BOTH the campaign's bid strategy (Campaign Budget Optimization is on for every campaign this service creates, so the ad group must match it) AND the ad group to `MANUAL_BIDDING` in Reddit Ads Manager — the adapter checks the campaign first, then the ad group. **Every Meta campaign this service creates is `LOWEST_COST_WITHOUT_CAP` billed on `IMPRESSIONS`, and every X campaign `AUTO`**, so those legs likewise apply only after an operator moves the ad set or line item to a manual per-click bid. Also **409**: unprovisioned, unknown or mismatched creating ad account, an unusable connection. **400**: a non-finite, non-positive or out-of-range bid, an unknown `bid_type`, or a bid the platform refuses on its own floor/ceiling (Microsoft: the create path's 0.01–1000 bounds, and Microsoft's floor/ceiling/invalid-bid refusals; Reddit: a 400 carrying a structured field error on `bid_value`, and only when no 429 was retried first; Meta: under one minor unit of the account currency, or a definite refusal whose `error_data.blame_field_specs` names `bid_amount`; X: a definite `INVALID_PARAMETER` 400 whose `parameter` is `bid_amount_local_micro`) — the message names the reason. **404**: the platform holds no such campaign. **503**: the platform could not be reached or did not confirm (including any Microsoft or Reddit refusal after a retried 429, and any Meta or X throttle — those writes are never retried in-call) — the row is unchanged; verify the bid in the platform before retrying. Persisted to `campaigns.max_cpc_bid` (migration `000039`), the bid lever's twin of `budget_amount`: a confirmed request, never an observation. |

**Per-campaign metrics-read support by platform**: this row documents the shared `MetricsReader` capability and endpoint; the remaining per-platform `ReadMetrics` adapters land in their own PRs.

| Platform | Supported windows |
|----------|-------------------|
| Google Ads | All seven. The adapter maps each window to the matching GAQL date literal (`last_30_days` → `LAST_30_DAYS`, and so on) behind an allow-list, so the platform-agnostic value never reaches the query as caller-supplied text. |
| Meta Ads | All seven. Each maps to a Graph Insights `date_preset` (`last_30_days` → `last_30d`, and so on) through a fixed allow-list, so an unrecognized literal fails locally rather than reaching Meta. |
| X (Twitter) Ads | `today`, `yesterday`, `last_7_days` only — the stats endpoint caps a queryable range at 7 days, so the wider windows return `400`. This is why X defaults to `last_7_days` rather than `last_30_days`. |
| LinkedIn Ads | `today`, `last_7_days`, `last_30_days`, `this_month`, `last_month`. `yesterday` and `last_14_days` return `400` — the Ad Analytics finder takes an explicit date range and these two have no mapping today. |
| Reddit Ads | `today`, `last_7_days`, `last_30_days`, `this_month`, `last_month`. `yesterday` and `last_14_days` return `400` — no date-range mapping today. |
| HubSpot (email) | All seven — but the window does NOT scope the counters. HubSpot's statistics span selects WHICH EMAILS are in scope by SEND date; the counters returned are that email's totals to date. `today` and `last_30_days` on an email sent this morning return the SAME numbers, and a window not containing the send date returns nothing at all (a **409**, not zeros). `window` in the response records what was ASKED, not a period the counters are scoped to. |

Reddit Ads is wired but **default-OFF**: the contract now follows Reddit's official public OpenAPI document (LFXV2-3282), but no request has been made against a live ad account, so the adapter returns the same 400 as an unsupported platform unless the deployment sets `REDDIT_METRICS_ENABLED=true`. That is deliberate — behaviour a schema cannot express (zero-activity rows, the account's attribution window) is still unconfirmed, and a 200 would look authoritative to every consumer while the caveats are not carried in the response.

The **email channel adds an optional `email` object** to the response, present only for HubSpot campaigns and absent for every ad platform. It carries `sent`, `delivered`, `opens`, `clicks`, `bounces`, `unsubscribes`. The parent object's `impressions`/`clicks` mirror `opens`/`clicks`, and its `cost_micros` is always `0` — HubSpot bills no per-send cost, so that `0` is "not billed here", not "free". **It must never be blended into a cross-channel cost-per-acquisition**, which would divide real ad spend across email conversions and understate CPA.

Email adds one more **409** case, and it is the ORDINARY one rather than an edge: `Dispatch` stages the cloned email as a DRAFT for a human to send, so every read between staging and the send finds no sent email in the window. Three states arrive in that one upstream shape and cannot be told apart — sent outside the window, never sent, or no such email — so the message names all three instead of guessing. A 503 here would report an outage on a healthy integration.

Microsoft Ads is supported but **default-OFF**: its only reporting surface is the Reporting API v13, which is REST/JSON but **asynchronous** (submit → poll → download). The adapter absorbs that behind one bounded call — a 15s submit+poll budget, under `metricsCallTimeout` — and answers `ErrReportNotReady` rather than hanging when a report is still building. Because the contract follows Microsoft's published docs but has not been verified against a live account, reads answer **400** until `MICROSOFT_METRICS_ENABLED="true"`; see `docs/knowledge/code/internal-dispatch.md`.

**Tentative** (later phases, same nesting + `campaign_manager` gating): budget adjust, bid-strategy change, per-keyword bid, ad/creative rotation, ad-copy edit, geo-target edit, audience edit, negative keywords, bid modifiers, scheduling, flight-date change. Adding keyword targeting on Reddit or X is supported upstream but deliberately not built: every keyword lever here only reduces what serves. Cross-platform budget reallocation, if built, is modeled as a first-class per-project resource with its own single-target mutations — not a bulk endpoint.

### Platform Connections (new — typed per provider, singleton per project)

A connection is **singleton per provider per project**: a project holds at most one connection of any given provider (one Google Ads account, one LinkedIn ad account, …). Multiplicity of accounts across the Linux Foundation lives at the **project** level, not inside a project — CNCF, OpenSearch, and TLF are each their own project, each owning its own single connection per provider. (TLF is both an umbrella over child-foundation projects *and* its own project with its own account; it owns only its own connection. Cross-foundation roll-up is a read concern handled by the UI backend — see the Monitoring note below — not by holding multiple connections on one project.)

Because the connection is a singleton, there is **no service-generated `{id}` in the path** — the provider name *is* the identity within the project. The path token is the **same provider key used everywhere else in this service** (`google-ads`, `linkedin-ads`, …, and `hubspot` for the non-ads provider), so the mapping is consistent end-to-end: path `connection-google-ads` → table `google_ads_connections`. Connections are strongly typed per provider (see [channel-connections-schema.md](channel-connections-schema.md)). The table below shows the pattern for `google-ads`; every provider (`linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`, `hubspot`) exposes the identical shape with its own typed payload.

| Method | Path | FGA relation | Type | Description |
|--------|------|--------------|------|-------------|
| POST | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Create the project's Google Ads connection (`409 Conflict` if one already exists). `projectId` MUST be a canonical slug, not a UUID (see the slug note below). **`LFX_FORCE_SYSTEM_ADS_ACCOUNT` adds a conditional 400 to this row.** While that deployment-wide env var is set to exactly `true`, a write that would newly set or CHANGE the connection's `account_id` is refused with **400** for the six PAID-ADS providers (`google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`). **HubSpot is untouched** — its `account_id` is a marketing list id that no ad-account discovery ever produced, and the guard asks `Provider.IsPaidAds()` precisely so the email channel is never caught by it. The reason is reversibility: while the flag is on, account discovery resolves the LF SYSTEM credential, so every id the picker can offer names an LF-owned account; persisting one onto a project's own row outlives the flag, leaving credentials from one account and a target from another once it is turned off. The guard reads the env var per request (the dispatch layer caches its copy at construction, so the two are not in step until a restart). On create there is no prior row by construction, so **every non-empty `account_id` in the body is newly set and is refused**. The consequence is deliberate and worth planning around: `linkedin-ads`, `reddit-ads` and `microsoft-ads` all declare `account_id` **required** on their create payloads (`design/connection.go`), so a body omitting it does not decode — **those three cannot be connected at all while the flag is on**. `google-ads`, `meta-ads` and — as of LFXV2-3319 — `twitter-ads` are credentials-first (no `Required("account_id")`), so they can still be created and then pointed at an account after the flag is cleared. |
| GET | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Get the connection (credentials redacted); returns ETag. |
| PUT | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Replace connection config (requires `If-Match`; does not set credentials). **`LFX_FORCE_SYSTEM_ADS_ACCOUNT` adds a conditional 400 to this row.** While that deployment-wide env var is set to exactly `true`, a write that would newly set or CHANGE the connection's `account_id` is refused with **400** for the six PAID-ADS providers (`google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads`). **HubSpot is untouched** — its `account_id` is a marketing list id that no ad-account discovery ever produced, and the guard asks `Provider.IsPaidAds()` precisely so the email channel is never caught by it. The reason is reversibility: while the flag is on, account discovery resolves the LF SYSTEM credential, so every id the picker can offer names an LF-owned account; persisting one onto a project's own row outlives the flag, leaving credentials from one account and a target from another once it is turned off. The guard reads the env var per request (the dispatch layer caches its copy at construction, so the two are not in step until a restart). The comparison is against the STORED value and is exact apart from a surrounding-whitespace trim, so **re-sending the id already on the row is allowed** (the write moves nothing) and **clearing a selection is allowed** (an absent/empty `account_id`, since PUT is a full replace and refusing it would trap a connection in whatever state the flag found it in). Only a change to a different non-empty id is refused. |
| DELETE | `/projects/{projectId}/connection-google-ads` | `campaign_manager` | JSON | Remove the connection (soft delete). |
| POST | `/projects/{projectId}/connection-google-ads/test` | `campaign_manager` | JSON | **This row stands for six endpoints** — `{provider}` = `google-ads`, `meta-ads`, `reddit-ads`, `twitter-ads`, `microsoft-ads` or `hubspot`; LinkedIn's `/test` is the row below. Verify the stored credential **against the provider** (LFXV2-2665). Until that ticket, all six answered `OK: true` the moment a credential blob existed in the row — never decrypting it, never authenticating, never touching the configured account — so a refresh token revoked months earlier tested clean and failed at campaign creation. Each now runs a live read with the stored credential (`Orchestrator.ProbeConnection` → the platform dispatcher's `ProbeConnection`): Google Ads `ProbeAccountReach`, Meta `GET /me/adaccounts`, Microsoft `ListAdAccounts`, Reddit `GET /ad_accounts/{id}`, X `GET` on the account root, HubSpot the private-app token-info endpoint. Where the platform enumerates accounts, the configured `account_id` must appear in the answer; Reddit and X read the configured account **directly**, which is the stronger check. **Google Ads is the exception, and answers about REACH rather than membership** (LFXV2-2665): without a login-customer id it calls `ListAccessibleCustomers` and the configured id must be in that list, but in manager mode it walks `customer_client` **unfiltered** — the same projection the account picker uses with the picker's `WHERE` removed — and then reads the account's own `manager` and `status` fields. That is deliberate: the picker filters to `status = 'ENABLED'` non-manager rows, so presence in the picker's list proves dispatch capability while ABSENCE from it proves nothing, and reusing the filtered walk would report a disabled or manager account as unreachable. The walk therefore yields four verdicts, three of them failures with distinct remedies: reachable (`OK: true`), reached but a **manager account** (a manager cannot host campaigns — point the row at a client account), reached but **not enabled** (cancelled, suspended or closed at Google — the id is right and the account is not usable), and not present in the hierarchy at all. A configured id that is not a usable customer id is rejected before the call, because the dashed form the Google Ads UI displays can never match Google's undashed answer. As of LFXV2-2665 `account_id` also carries a `Pattern` (`^([0-9]+)?$`) and a `MaxLength` at the design layer, so an HTTP caller can no longer store the dashed form at all; the runtime check stays because Goa validates only the HTTP transport, and bootstrap, migrations and rows written before that pattern existed never passed through it. HubSpot checks no account at all, because `portal_id` is not one: nothing routes on it (its only readers build `app.hubspot.com` deep links for assets that already exist) and the portal a campaign lands in is the token's own. So a HubSpot `portal_id` that is blank, stale or mismatched still answers `OK: true` — a mismatch is logged as a warning, since it builds deep links into a portal the operator is not looking at, but failing the test would report a working connection as broken. An unconfigured **ad** `account_id` is a failure, since such a connection cannot run a campaign, and it is decided **before** the upstream call on all five: letting the enumeration run first meant an unrelated `5xx` could classify inconclusive and answer with a platform that could not be reached — an outage to wait out — for a connection naming no account. A configured id the platform's own request builder refuses (Reddit's path guard) fails the same way, and deliberately does NOT report the credential as rejected — the platform never evaluated it, and the remedy is the account id on the row. Credentials are resolved through the project's **own** connection only, never the `LFX_FORCE_SYSTEM_ADS_ACCOUNT` system-row fallback — the same trust boundary as the LinkedIn row below, and for the same reason: a test answered from a borrowed row reports a connection the project does not have as healthy. `internal/dispatch/probe_owned_resolver_test.go` pins that as a source-derived guard, because the mistake compiles and only misbehaves for projects with no connection of their own. Outcomes match the LinkedIn row's shape. A confirmed rejection by the platform (an authentication or authorization refusal, a token-refresh request the platform refused on the merits, or the configured account absent from a complete enumeration — for Google Ads also a reached account that is a manager or is not enabled) is an ordinary **FAILED test** (`OK: false`) and is the ONE class whose message is echoed — and that message is authored by this service, never copied from the platform, because several of these clients render request URLs and raw response bodies and three of them carry the client secret and refresh token in the request body they would quote. A failure that proves nothing about the credential (rate limit, `5xx`, a transport or pre-send failure, an unreadable response envelope, or any error the platform adapter does not recognise) also answers `OK: false` — `ok` is declared as a conjunction, the credential authenticated AND the configured account passed that provider's own check, and an incomplete probe establishes neither half of it (deliberately not justified as "the credential did not authenticate": on the two-leg providers a refresh can SUCCEED before the account read fails) — but its message says the platform could not be reached and says nothing about the stored credential, so it is never confused with a refusal. An error that is neither — the platform refusing a request this service BUILT, which is not a verdict on the credential — is a typed **500** carrying `domain.ErrServiceDefect` (`reason=probe_request_rejected`), as is a build where the platform's dispatcher is unregistered or cannot run the probe (`reason=probe_unwired`); the alternative, treating either as inconclusive, would silently stop testing anything the day an endpoint moves. A credential that fails authenticated decryption is likewise a **500** that echoes nothing (`domain.ErrCredentialDecryptionFailed`). A connection that cannot be used as configured (inactive, or an incomplete or undecodable credential blob) fails the test with a FIXED remedy message quoting no part of the error, since one of those conditions is found by decoding the decrypted blob. A failure to READ the connection row is a **503** — nothing was learned, and it is the one outcome here retrying can fix. With no credential stored at all the platform is never contacted: the test fails naming the absent credential, because "authorize this connection" and "re-authorize this connection" are different remedies. |
| POST | `/projects/{projectId}/connection-linkedin-ads/test` | `campaign_manager` | JSON | **LinkedIn is the one provider whose `/test` departs from the row above.** It does not go through `ProbeConnection` at all: its upstream verification predates LFXV2-2665 and is strictly stronger, so it was left where it is rather than re-pointed at a weaker check. Beyond authenticating and walking the ad-account enumeration, it additionally cross-checks the connection's stored `org_id` against LinkedIn's own `reference` field on the configured ad account (`Orchestrator.VerifyAccountOrg` → `LinkedInDispatcher.VerifyAccountOrg` → `linkedin.Client.VerifyAccountOrgReference`), resolved from the project's OWN connection only — never the LF system-account fallback, same trust boundary as the `account-monitor` row below. One consequence worth stating: when `LFX_FORCE_SYSTEM_ADS_ACCOUNT` is set, campaign CREATION dispatches through the LF system row while this endpoint still verifies the project's own row, so a passing `/test` in that mode is not a prediction about which account a create will run on — it answers only "is this project's stored pairing correct". A confirmed org mismatch, the configured account being absent from a complete ad-account enumeration, or a credential/authorization failure surfaced while walking the enumeration (expired or invalid credentials, an application-authorization rejection, or a `403` from LinkedIn — a refusal LinkedIn reached on the merits against this token, which will not start succeeding on its own) is folded into an ordinary **FAILED test** (`OK: false`) — each proves the credential cannot perform the verification, so it is reported the same as any other bad connection. A non-authentication failure of the enumeration walk itself (a connection failure, a mid-flight transport error, a `429` rate limit, a `5xx`, or a pagination fault — not a credential problem and not a refusal) is different: it proves nothing about the account/org agreement, so while it too answers `OK: false` — an incomplete walk establishes neither half of the conjunction `ok` names, and the message may therefore claim neither half — not that the credential failed, and equally not that it succeeded, since the baseline gating entry to the walk is local (the row exists and carries a credential) and a walk that failed before send left LinkedIn with nothing to evaluate — its message names the unreachability and says nothing about the stored pairing, keeping it distinct from a confirmed mismatch. Several failure classes escape both of those and return a typed **500** instead: a credential that fails authenticated decryption (`domain.ErrCredentialDecryptionFailed`, service-side, never echoes cipher material into the response), and three carrying `domain.ErrServiceDefect` — a defect in this service's own token handling (a refresh request LinkedIn refused on protocol grounds, `reason=token_request_rejected`), a **non-`429`, non-`403` `4xx`** refusal of the ad-account discovery request itself (`reason=account_discovery_rejected` — that request embeds neither the stored account id nor the configured org id, so refusing it is not a verdict about the pairing), and a build in which LinkedIn's dispatcher is unregistered or cannot run the cross-check at all (`reason=org_verification_unwired`). None is evidence the stored connection is broken, so none is reported as a failed test; each names in its log the party who can actually act, which for all four is this service's owners rather than the operator. A stored `account_id` or `org_id` that is not a numeric platform id — the empty string included — also fails the test (`OK: false`), and is decided without contacting LinkedIn at all: `targeting.go` refuses the same values when binding the account and building the organization URN, so such a connection cannot create a campaign regardless of what the enumeration would have said. Two further classes never reach a verdict at all. A stored connection that cannot be used as configured (inactive, or an incomplete or undecodable credential blob) fails the test (`OK: false`) with a FIXED remedy message that quotes no part of the underlying error, because one of those conditions is detected by decoding the decrypted credential blob. And a failure to READ the connection row — a datastore outage — is a **503**, not a failed test: nothing was learned about the connection, and it is the one outcome here that retrying can fix; so is this endpoint's own orchestrator being unwired (`resolveBackendWithOrch`), which is the "this endpoint is unavailable" sense of 503 rather than anything about the connection. Every outcome above is reached by MATCHING a sentinel, including the failed verdicts (`domain.ErrOrgVerificationFailed`), which are the only class whose own message is echoed into the response — anything unrecognised also fails the test (`OK: false`) but with fixed text, its detail going to the server log instead, so a class added later cannot inherit the echo by falling through. `OK: true` on its own does not distinguish a confirmed match from an inconclusive comparison (LinkedIn has no reference on the account to compare against) — see [`internal-service.md`](knowledge/code/internal-service.md) for why that distinction is not this endpoint's to make. |
| POST | `/projects/{projectId}/connection-google-ads/set-credential` | `campaign_manager` | JSON | Replace the stored (encrypted) credential. Split out from `PUT` so credential replacement is independently permissioned/audited. Not "rotate" — the service does not generate/swap secrets upstream. |
| GET | `/projects/{projectId}/connection-{provider}/accounts` | `campaign_manager` | JSON | **This row stands for all six `AccountLister` providers** — `google-ads`, `meta-ads`, `linkedin-ads`, `microsoft-ads`, `twitter-ads` and `reddit-ads` (`list-reddit-ads-accounts`, LFXV2-2665: every business the credential can access via `GET /api/v3/me/businesses`, then each business's `GET /api/v3/businesses/{business_id}/ad_accounts`, following `pagination.next_url` with page and item caps; all or nothing — an upstream failure, a 429 that outlasts the bounded retry, a malformed body or a cap overrun is **503**, never a short or empty list; an account reachable through two businesses is listed once; label `name [currency] (business)`; the two Reddit operations are taken from Reddit's published v3 reference and not yet exercised against a live account); the Meta row below records only where Meta's own behaviour differs. Enumerate the ad accounts **reachable upstream with the stored credential**, so an operator can pick which one the connection should point at. Live read against the provider (Google Ads `customers:listAccessibleCustomers`); never persisted. Available for every provider whose dispatcher implements `AccountLister` — Google Ads, Meta Ads, LinkedIn Ads, Microsoft Ads, X/Twitter Ads and Reddit Ads today. The capability is optional per dispatcher, like `MetricsReader`, and **all of them share one handler implementation**, so the status mapping below cannot drift apart between them. Stated as the shape rather than a fixed list: the membership grows, and an enumeration is falsified by the next provider added without anything failing. **Status codes** (all four published by the Goa design): **400** when the platform has no account-discovery capability wired, **or** the stored connection exists but is not usable as it stands (inactive, incomplete credential blob, malformed stored config, or a credential blob too short to be valid ciphertext — none improve with time, so a 503 would be a false promise). **404 in two setup states, neither of them an outage.** The first is that NEITHER the project nor the LF system account has a connection — a project with no connection of its own falls back to the system row and gets a `200` listing the accounts the LF credential reaches, which is deliberate: those are the accounts its campaigns would actually run on. The second is that the project once connected and **explicitly disconnected**: `Delete` soft-deletes and `Get` filters the tombstone out, so a disconnect would otherwise be indistinguishable from never having connected and would silently earn the project the LF credential. `credsSource.systemConn` therefore probes `ConnectionRepo.Disconnected` first and refuses the fallback, so a disconnected project gets 404 **even when the LF system row exists and is perfectly usable**. That is the intended answer, not a gap: absence of a statement is what the fallback is for, and a statement to the contrary is not absence. A 503 in either case would tell the caller to retry something that cannot succeed until a connection exists. **500 for the defects no caller and no operator can edit their way out of**: a well-formed credential blob that fails authenticated decryption (a wrong/rotated application key, or a corrupted row — indistinguishable to GCM, and neither is something the caller can edit); a fallback onto a system row that is itself unusable; and a defect in THIS SERVICE (`ErrServiceDefect`, `reason=token_request_rejected`), which is matched ABOVE the 400 below precisely because that 400 names stored fields to go and check and every one of them is correct here. Stated as the property rather than a count: the previous wording fixed a number that the next sentinel routed here falsified. The second is 500 rather than the 400 above because the 400 tells the caller to fix "the stored connection" and this caller HAS none — the reserved scope is unaddressable, so only an operator can act, and the failure is deployment-wide rather than about their project. **503** when the provider call fails — and also **before Google is called at all**, when the disconnect probe or the system-row fallback read fails against the database. That probe fails CLOSED: an unanswered "did this project disconnect?" is not a no, so the request is refused rather than resolved onto the LF credential. Those are genuine retryables, which is why they are 503 rather than the 404 above. **Serves first-time setup as well as re-pointing.** `GoogleAdsConnectionConfig` no longer declares `Required("account_id")` (nor does `MetaAdsConnectionConfig`, as of LFXV2-3061, nor `TwitterAdsConnectionConfig`, as of LFXV2-3319 — Google Ads, Meta and X are the three providers whose connection can be created credentials-first. LinkedIn and Microsoft gained the discovery endpoint in LFXV2-3064, and X in LFXV2-3319, but neither of the other two is yet credentials-first, and it takes MORE than one change to make them so — naming only the bootstrap map would send the next change to do half the work. Both still declare `Required("account_id")` on their PUBLIC payloads (`LinkedInAdsConnectionConfig`, `MicrosoftAdsConnectionConfig` in `design/connection.go`), so an account-less connection cannot even be created over HTTP; `accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` separately gates whether an account-less SYSTEM row is installable; and LinkedIn additionally needs its create path to tag the missing choice, which `Dispatch` does not do today. All three gates, not just the map. Microsoft needs only the two config gates, having both halves already — X held both and was carried across BOTH config gates together in LFXV2-3319, which is what "half the work" warns against doing one at a time. Reddit has both halves since LFXV2-2665 but still declares `Required("account_id")` on `RedditAdsConnectionConfig` and is not in the bootstrap map), so the bootstrap is `POST .../connection-{provider}` with credentials only → `GET .../accounts` → `PUT .../connection-{provider}` with the chosen id. A connection between those steps stays `status=active` (discovery refuses a non-active connection, so any other status would dead-end the flow) and reports `account_id` as `""`. Operations that actually need an account report it non-retryably rather than as a 503, because waiting cannot fix a choice only a human can make — but the shape differs by endpoint, and **this paragraph describes Google Ads; Meta differs, see the row below**. For Google Ads the **status toggle** and **metrics read** need the account id (they share `validateGoogleAdsConnection` with create) and answer a synchronous **409** whose message names the missing account specifically (matched ahead of the general unusable-connection arm). **Campaign create** is asynchronous for both providers: it answers **202**, and the failure is NOT rendered into the job result — `dispatchPlatform` collapses every dispatcher error into the same `"platform campaign creation failed"` — so on the create path the reason token reaches an operator through the dispatch-failure **log line**, not through polling. No endpoint exposes a machine-readable `reason` field — `ConflictError` carries only `code` and `message` — so `account_not_selected` is a log/reason token, not part of the HTTP contract. The two mechanisms compose: a project that has connected nothing at all falls back to the LF system row, while a project that HAS connected credentials but has selected no account is served by its own row and never falls back — its connection exists, so there is nothing to fall back from. |
| GET | `/projects/{projectId}/connection-meta-ads/accounts` | `campaign_manager` | JSON | The same thing for Meta: enumerate the ad accounts reachable with the stored credential, so an operator can pick which one the connection points at. Live read against Graph `GET /me/adaccounts`; never persisted. Ids come back in `act_<digits>` form, ready to store as the connection's `account_id` verbatim. **Status mapping is literally the same code** — both handlers call one `listAccounts` helper parameterized by provider, so 400/404/500/503 cannot drift apart between them; only the caller-facing remedy text differs (Meta's names `access_token`, the field its set-credential payload carries, not `login_customer_id`). Two Meta-specific behaviours: (1) accounts Meta reports as **disabled, unsettled, pending review/settlement, in grace period or closed are RETURNED**, with the reason appended to the label (`"LF Events (disabled)"`), not filtered out — dropping them would answer "your token reaches no ad accounts" about an account sitting right there and send the operator hunting a permissions problem that does not exist; refusing to *create a campaign* on such an account stays where it already is, in the client's preflight. (2) An **incomplete enumeration is an error, never a short list**: a 2xx body with no `data` field, a `next` link with no cursor, a repeated cursor, or more than 2000 accounts all return a failure rather than what was collected, because a truncated list is indistinguishable from a complete one at the boundary and the caller acts on the absence. Meta's `paging.next` is never followed — it carries `access_token` and `appsecret_proof` as query parameters, so each page's path is rebuilt from the opaque `after` cursor instead, keeping the credential out of request URLs and error text. **Serves first-time setup as well as re-pointing, as of LFXV2-3061**: `MetaAdsConnectionConfig` no longer declares `Required("account_id")`, so the same `POST` (credentials + `page_id`) → `GET .../accounts` → `PUT` bootstrap Google Ads has is available here. What gated that was not the discovery endpoint but the other half — Meta's **campaign create**, the one Meta path that requires an account id (the status toggle and metrics read target the campaign node by id and need none), used to answer an empty account id with a generic error. `requireMetaAccountID` now tags it `ErrAccountNotSelected` + `ErrConnectionNotUsable`, so `unusableConnectionReason` names the missing choice as `account_not_selected`. **Where that reaches an operator is the log, not the job result** — create is queued work and `dispatchPlatform` collapses every dispatcher error into `"platform campaign creation failed"`. Meta gets no synchronous 409 from this sentinel the way Google Ads does, because Meta's status toggle and metrics read do not need an account id at all (they target the campaign node by id), so create is Meta's only account-needing path and it is the asynchronous one. |
| GET | `/projects/{projectId}/connection-{provider}/account-monitor?account_id=&days=` | `campaign_manager` | JSON | **This row stands for six endpoints** — `{provider}` = `google-ads`, `linkedin-ads`, `meta-ads`, `reddit-ads`, `microsoft-ads` or `twitter-ads`. **Microsoft and X are report-backed and differ as follows; everything after this paragraph describes the other four unless it names them.** Its delivery metrics come from Microsoft's asynchronous Reporting service, which takes minutes, so the campaign list is read live and the metrics come from the last report that finished: the response adds `metrics_as_of` (when that report was requested from Microsoft — the point in time the metrics describe; absent before the first finishes), `metrics_pending` (a newer report is building), and `metrics_window_start` / `metrics_window_end` (`YYYY-MM-DD`, the first and last calendar day the metrics cover, both inclusive, taken from the saved report's own window; absent before the first report finishes and omitted on the four live-read platforms, which cover exactly the requested days). `days` is the REQUESTED window echoed back; the covered days are what these two fields state. On Microsoft they are the report's `CustomDateRangeStart`/`End` UTC dates, aggregated in the report's GMT (Europe/London) time zone. On the first read for an account and window every campaign is `fetch_failed` and excluded from pacing and action items. `account_id` must match `^[1-9][0-9]{0,17}$` (re-checked by `microsoft.ValidateMonitorAccountID`), the same own-connection-only and bound-account rules apply (`ErrAccountNotManagedByConnection` → 400), and the endpoint answers 400 while `MICROSOFT_METRICS_ENABLED` is not `true`. See [Account-Monitor Endpoints](knowledge/architecture/account-monitor-endpoints.md#microsoft-a-report-backed-monitor). **X** (`twitter-ads`) works the same way for every `days` value — its synchronous stats are capped at 7 days per request — with the metrics coming from X's asynchronous stats jobs (active campaigns found via `active_entities`, one job per 20, the jobs' ids saved as one composite report id); the campaign list, budgets (`*_local_micro`, account currency) and line-item flights (account-timezone dates) are read live. `account_id` must match `^[A-Za-z0-9]+$` with at most 64 characters, the connection's own rule (re-checked by `twitter.ValidateMonitorAccountID`), the same own-connection-only and bound-account rules apply, `conversions` is never reported, the rules judge the saved report's own account-local window (not the service's UTC "today"), results files are fetched only from X's documented `ton.twimg.com` host, the rules judge only the days a line item is actually scheduled on (the union of the line items, so a gap between them is not "scheduled"), amounts in action-item text are printed as "in account currency" with no currency symbol, and the endpoint answers 400 while `TWITTER_METRICS_ENABLED` is not `true` — the stats-jobs contract is unverified against a live account, as Microsoft's is; the flag gates only the monitor, not X's per-campaign metrics read. **X limits, answered 409 `AccountMonitorConflictError` (this method's own body; `reason` is always present):** more than **200 campaigns active in the window** (one report is at most ten stats jobs of 20) → `account_too_many_active_campaigns`; an account timezone whose local midnight is not a whole UTC hour (e.g. Asia/Kolkata, +05:30) → `account_timezone_unsupported`, because X takes whole-hour window bounds only and the service never queries a shifted window under the account's own days. Both are permanent, so they fail the read rather than being retried (a still-fresh saved report is served). A 90-day window that crosses a DST fall-back covers the trailing 89 whole local days (90 such days exceed X's 90-day cap by an hour); the response still echoes `days: 90` and states the 89 covered days in `metrics_window_start` / `metrics_window_end`, which on X are the account's local days (a day whose midnight a DST spring-forward skips starts at its first existing instant). See [Account-Monitor Endpoints](knowledge/architecture/account-monitor-endpoints.md#x-a-second-report-backed-monitor). The four live reads were ported from the LFX One BFF's four independently-maintained `/api/campaigns/*/monitor` routes (`campaign-metrics.service.ts`, `linkedin-ads.service.ts`, `meta-ads.service.ts`, `reddit-ads.service.ts`), migrated so this drifting business logic lives in one service instead of four copies in the presentation tier. **Account-scoped, not project-scoped**, in the same sense as the `accounts` row above: `account_id` is the caller-resolved platform account id (already picked, e.g. via that row's picker), and `{projectId}` in the URL means "whose stored credential resolves the platform client" — **the project's OWN connection only, never the LF system-account fallback chain** (round-16/17 review; see the Trust boundary section of [Account-Monitor Endpoints](knowledge/architecture/account-monitor-endpoints.md)) — not a filter on which campaigns come back. Every campaign the resolved credential can see in that account is read and evaluated, unfiltered by this project's own campaign rows. `days` is a plain integer (7–90; out-of-range values are rejected with 400, not clamped), NOT `MetricsWindow` — that enum cannot express arbitrary values and this window must match the BFF's original explicit-range behavior (every provider's range is today-(days-1) through today, inclusive of today — Reddit's report range ends at today's 23:00 hour rather than the BFF's midnight, which dropped today). `account_id`'s shape is enforced per provider at the design layer with a Goa `Pattern` — `^[0-9]+$` for Google Ads and LinkedIn, `^act_[0-9]+$` for Meta, `^[A-Za-z0-9_]+$` for Reddit — so a malformed id is rejected with a clean 400 at the HTTP boundary before the handler ever runs, same as everywhere else this repo validates ids this way. Every provider's dispatcher also re-validates the same shape itself (`googleads.ValidateCustomerID`, `linkedin.ValidateAccountID`, `meta.ValidateAccountID`, `reddit.ValidateAccountID`), mapped to the same 400 via `domain.ErrAccountIDMalformed` — pure defense-in-depth for a non-HTTP caller that bypasses Goa entirely (see [Account-Monitor Endpoints](knowledge/architecture/account-monitor-endpoints.md)); for an ordinary HTTP request it is Goa's `Pattern` that answers first. `internal/apivalidation/monitor_account_id_drift_test.go` guards the two independent copies of each shape against drifting apart. Returns each campaign's raw metrics plus a computed pacing label (`normal`/`underspending`/`constrained`/`overspending`) and a sorted list of prioritized action items (`HIGH`/`MED`/`LOW`), plus account-level totals. **The four rule engines were unified after the port** (see [Account-Monitor Endpoints](knowledge/architecture/account-monitor-endpoints.md)). What they genuinely share now has exactly one implementation in `internal/service/rules/monitor_shared.go` — the 50/90/100 pacing ladder (`pacingLabelFor`, placing on `AccountMonitorLadder` with the `PacingLadder.Label` the brief view also uses, in `ladder.go`), the `HIGH`/`MED`/`LOW` ordering (`priorityRank`) and the budget-less row (`unknownPacingRow`) — so a change to any of the three lands on all four platforms at once. The quirks each engine carried over verbatim from its BFF source were fixed rather than preserved: LinkedIn's `MED`-vs-`MEDIUM` sort-key mismatch (`linuxfoundation/lfx-self-serve#3018`), Google's and Reddit's local pacing literals (`#3019`), Reddit's hardcoded `conversions: 0` (`#3020`), Reddit's `<40` underspend threshold labelled `<50` (`#3021`), Reddit's independently-fetched account totals (`#3022`), Google's `zz`-prefix filter dropping any campaign whose name merely began with those letters, a budget-less campaign reported as underspending, and LinkedIn's low-CTR rule excluding a 0% CTR — the worst case of the very condition it detects. What stays per-platform stays because a reason is written on the constant: Meta's low-CTR pair is 0.5%/500 impressions against 0.3%/1000 on the other three, and the clicks-without-conversions floors are 20/50/100 because the click volumes they sit on differ. A row whose per-campaign metrics fetch failed upstream, OR (Google Ads only) whose budget field was present but unparseable alongside otherwise-good metrics (round-24/25 review), carries `FetchFailed=true`; it is still returned but excluded from pacing/action-item evaluation, since running an untrusted field through the rule engine would fabricate a finding against data never actually read reliably — but `pacing_label` on that row is NOT cleared: it keeps the `"normal"` placeholder (a required enum with no "unknown" member) alongside `pacing_unknown=true`, mirroring `pacing_pct`'s own "meaningless when pacing_unknown is true" convention; a consumer must gate on `pacing_unknown`/`fetch_failed`, not trust `pacing_label` at face value. Status mapping follows the shared discovery-error classifier (`classifyDiscoveryError`): 400 when the platform has no metrics-read capability, the stored connection is unusable, `account_id` fails shape validation (`domain.ErrAccountIDMalformed` — every provider's dispatcher validates the shape itself before resolving a credential, and for an ordinary HTTP caller a malformed id is additionally refused earlier still, at the Goa layer, before this classifier or even the dispatcher ever runs), or `account_id` is well-formed but names an account the project's own resolved connection does not manage (`domain.ErrAccountNotManagedByConnection`, answerable by the Reddit, LinkedIn and Meta reads, since a connection is bound to exactly one ad account — Reddit checked it first and Google Ads is the remaining deliberate gap, being one shared customer id across every foundation — checked ahead of the "stored connection is unusable" case above, since the stored credential is fine here and only the requested account is wrong, round-18 review), 404 when the project has no connection of its own (the LF system-account fallback is refused outright on this endpoint, round-16/17 review), 500 for undecryptable credentials or a service-side defect, 503 when the per-campaign metrics read itself fails. `totals` is the sum of the `campaigns` array in the same response, on every platform, computed after the rule engine runs — so the aggregate and the list can never describe different populations, and there is no separate account-totals read left to fail (`#3022`). The `derived_from_rows` response field went with it: it existed to distinguish Reddit's platform-native figure from a row sum standing in for one that failed, and a required field that can now only ever report `false` implies a distinction the response no longer makes. |
| GET | `/projects/{projectId}/connection-hubspot/emails` | `campaign_manager` | JSON | Search the marketing emails reachable through the stored HubSpot connection, most-recently-updated first, so a caller can choose which one an email campaign will CLONE. Optional `q` matches name OR subject case-insensitively; **omitting it lists rather than fails**, which is the useful first screen for a picker nobody has typed into yet. Live read against HubSpot; never persisted. **This is a TEMPLATE picker, not an account picker** — and that is the one way it departs from the two rows above. A HubSpot connection is already scoped to the portal its private-app token authenticates against (`Client.AuthenticatedPortalID`), so there is no account to choose; what has no default is `hubspotConfig.SourceEmailID`, which campaign create REQUIRES, so without this endpoint the email channel cannot be driven from the UI at all. **Status mapping is the same code** — `classifyDiscoveryError`, lifted out of `listAccounts` unchanged, so 400/404/500/503 cannot drift between account discovery and this; only the operation noun differs ("email search could not be completed", not "account discovery"), and the 400's remedy names `private_app_token`, the PUBLISHED wire field of the set-credential payload — not `privateAppToken`, the Go/JSON shape the blob is persisted under, which no caller can send. One arm is NOT shared: a dispatcher with no `EmailSearcher` yields `ErrEmailSearchUnsupported` → 400, a separate sentinel from `ErrAccountsUnsupported` because the two capabilities are independent — HubSpot searches emails and has no ad accounts to enumerate, while the ad platforms are the reverse (they implement `AccountLister` and search no emails; Reddit implements neither, having no `ListAdAccounts` in its client). **Draft emails are RETURNED**, with `state` on each row, for the same reason Meta returns disabled accounts: hiding the row the user is looking for answers "your portal has no such email" about an email sitting right there. **Archived emails are a different case and are simply absent** — HubSpot models archival as a separate `archived` flag rather than a lifecycle state, and the search does not request archived rows, so no `state` value can describe them. `state` is REQUESTED via `includedProperties`; the list endpoint does not return it by default, so a consumer promised a lifecycle state would otherwise have received an empty string from every row. The caller sees the state and decides. An empty result marshals as `[]`, never `null`, and a searcher returning `(nil, nil)` is rejected as a contract violation rather than reported as an empty portal. **A FILTERED search is a PAGINATED WALK** — the client follows `paging.next.after` so a match beyond the first page is not missed, up to `maxListPages` (200) sequential upstream requests, and the whole walk shares one 20s deadline. It is deliberately NOT truncated: a short list would answer "no such email" about an email on a later page, and the caller cannot tell a missing template from an absent one, so a 503 (recoverable) is preferred. **An UNFILTERED listing — an omitted or blank `q` — is BOUNDED to at most 500 rows.** An empty query matches every row, which makes the picker's default screen the walk's worst case, so it stops once it has collected the cap. Those 500 are taken in SERVER order (`sort=-updatedAt` is requested as a hint) and then sorted client-side, so the response is correctly ordered within itself but is NOT a guarantee of the newest 500 in the portal — under a bound the two cannot both hold. There are no pagination fields on this endpoint: a caller that needs an older template must SEARCH for it rather than page to it. |

> **Create requires a canonical slug `projectId`.** The connection is stored keyed by `project_id`, which is the EXACT-MATCH key for the dispatch lookup, and brief/campaign create already require a canonical slug — so a UUID-keyed connection could never be joined to a dispatched campaign. `POST` (create) therefore rejects a UUID `projectId` with `400` (Pattern `^[a-z0-9]+(-[a-z0-9]+)*$`, MaxLength 35). The generated HTTP request decoder validates the pattern/length for these create routes, and the service applies the same guard for direct/non-HTTP callers (belt-and-suspenders). `GET`/`PUT`/`DELETE`/`test`/`set-credential` stay permissive (UUID-or-slug) to keep historical UUID-keyed rows reachable.
>
> **LinkedIn refresh credentials are ALL-OR-NONE.** `POST .../connection-linkedin-ads` and `POST .../connection-linkedin-ads/set-credential` accept three OPTIONAL refresh fields alongside the required `access_token` — `refresh_token`, `client_id` and `client_secret` — and reject a PARTIAL set with **400**. Supply all three, or none for a bearer-only connection. They are optional because LinkedIn issues refresh tokens only to approved Marketing Developer Platform partners, so a bearer-only connection is the common case and keeps working exactly as before. The rule is enforced in the service layer rather than by Goa's `Required`, which cannot express a conditional group — so it is a runtime rejection with no OpenAPI counterpart. Without it a connection storing, say, a `refresh_token` and `client_id` but no `client_secret` would save cleanly, silently fail `CanRefresh()`, and degrade to bearer-only — reappearing ~60 days later as the expired-token outage the refresh support exists to prevent.
>
> **The reserved `system:linuxfoundation` scope is not addressable.** It holds the LF-owned credentials a project falls back to when it has connected no account of its own, and every one of the **seven** endpoints taking a caller-supplied `projectId` refuses it: `POST` with `400` (the colon cannot satisfy the slug pattern above), and `GET`/`PUT`/`DELETE`/`test`/`set-credential`/`accounts` with `404`. `404` rather than `403` — the reserved scope is not a project this API exposes, and answering "forbidden" would confirm to an unauthorized caller that something is there. Account discovery is the seventh and the only one that does not reach storage through a shared helper, so it needs its own guard: left open, `GET /projects/system:linuxfoundation/connection-google-ads/accounts` would decrypt the LF credential and enumerate the Linux Foundation's own ad accounts. A project that has no connection of its own still sees the system account's accounts through *its own* `projectId`, deliberately — it is shown the accounts its campaigns would actually run on. Dispatch reaches the reserved scope internally; nothing reaches it over HTTP.
>
> **Installing the system account is out-of-band, by construction.** Because no request can address the reserved scope, the credentials are installed with the service binary's `bootstrap-system-account` subcommand, which speaks to the repository and encryptor directly: `DATABASE_URL=… CREDENTIAL_ENCRYPTION_KEY=… campaign-service bootstrap-system-account -provider google-ads [-account-id …] [-config login_customer_id=…] < creds.json`. Keys use the documented snake_case form, and `-provider` accepts EVERY valid provider — the six paid-ads platforms and HubSpot. HubSpot was once refused here, while the reserved-scope fallback was classification-gated to paid ads: a HubSpot system row installed cleanly, reported success, and was then reachable by nothing. The fallback now serves the email channel, so the row this installs is the credential every foundation's HubSpot operations resolve. `-config` accepts only the keys the SELECTED provider stores (`login_customer_id` for Google Ads, `org_id` for LinkedIn, `page_id`/`app_id` for Meta, `funding_instrument_id`/`as_user_id` for X, `customer_id` for Microsoft, `portal_id`/`sender_email`/`sender_name`/`brand_kit` for HubSpot, none for Reddit — `model.Provider.ConfigKeys`). Anything else is refused rather than accepted: storage has one column per key, so an unrecognised one has nowhere to go, and the earlier behaviour dropped it while still exiting 0 — telling the operator a setting was installed that nothing held. It is idempotent (a second run rotates the credential rather than violating the singleton index), reads the credential from stdin rather than a flag, requires the `-config` keys an adapter refuses to create without (LinkedIn `org_id`, Meta `page_id`, X `funding_instrument_id`), and `-account-id` may be omitted **for Google Ads, Meta and — as of LFXV2-3319 — X**, leaving the row credentials-only, and **for HubSpot**, which is a different exemption with a different reason: the three ad providers have account DISCOVERY to finish the row later, while an email connection has no ad account to select at all — `requireAccountID` exempts it by asking `!IsPaidAds()`, and nothing in the HubSpot adapter reads `AccountID`. The remaining three ad providers require it, and the reason is worth stating precisely, because **discovery capability and credentials-first bootstrap are not the same thing** and conflating them hides which half is actually missing. Two halves are needed: an endpoint that can enumerate the accounts a credential reaches, AND a failure on the path that needs the id which NAMES the missing choice, so an operator is told to go and use that endpoint. The three still require it, but for DIFFERENT reasons, and the difference is what tells you how far each is from eligibility. **This whole framework is about PAID-ADS providers, and HubSpot sits outside it entirely.** The two halves answer "can this row be finished later?", which presumes there is an account to choose. An email connection has none: nothing in the HubSpot adapter reads `AccountID`, `hubspot.AccountConfig` carries only an optional `PortalID`, and the adapter is documented above as the one that never answers `reason=account_not_selected`. So `requireAccountID` exempts it by asking `!IsPaidAds()` rather than by adding it to `accountDiscoveryProviders` — it is not a provider whose row can be finished later, it is a provider with nothing to finish. Read every clause below as applying to the six paid-ads providers; where HubSpot is named alongside them (installable, clearable), it is for this reason and not because it acquired the two halves. **Reddit now has BOTH halves** as of LFXV2-2665, which added `reddit.ListAdAccounts` and `RedditDispatcher.ListAccounts` (`GET /projects/{projectId}/connection-reddit-ads/accounts`, `list-reddit-ads-accounts`); the second half it already had, because `resolveRedditClientWithCredsCache`, which `Dispatch` calls, tags an empty account id with `ErrConnectionNotUsable` and `ErrAccountNotSelected`. It is eligible and deliberately NOT in `accountDiscoveryProviders`, so `-account-id` is still required for it: Reddit's public connection config still `Required`s `account_id` (`design/connection.go`), so admitting it to the CLI alone would make it credentials-first for bootstrap and not over HTTP — the half-flow X's admission avoided by relaxing both gates together. **X now has BOTH halves** as of LFXV2-3319, which added `twitter.ListAdAccounts` and `TwitterDispatcher.ListAccounts`; the second half it already had, because `validateTwitterConnection` tags an empty account id with `ErrAccountNotSelected` and `Dispatch` calls that validator itself rather than validating inline — the Microsoft shape, not the LinkedIn one. X's toggle and metrics paths share the same validator and answer synchronously, so the naming is not log-only there. It JOINED `accountDiscoveryProviders` in LFXV2-3319, in the same change that dropped its `Required("account_id")` — the two gates were relaxed together deliberately, because relaxing one alone leaves a provider credentials-first for the CLI and not over HTTP, or the reverse. `funding_instrument_id` is unaffected and still required at both gates: it has no discovery endpoint, so an account-less X row is finishable from inside this API but a funding-instrument-less one is not. **Microsoft now has BOTH halves** as of LFXV2-3064, which added its discovery endpoint; `MicrosoftDispatcher.Dispatch` resolves through `validateMicrosoftConnection`, which tags a missing account with `domain.ErrAccountNotSelected`. It is therefore eligible to join `accountDiscoveryProviders` and has deliberately NOT been added yet — that is a change to what the bootstrap CLI accepts, and it belongs in its own change rather than riding along with the endpoints. **LinkedIn gained only the FIRST half** in the same ticket, and this document previously claimed otherwise by naming `resolveLinkedInCredentials` — a function that does tag `ErrAccountNotSelected`, but which the CREATE path never reaches. `LinkedInDispatcher.Dispatch` resolves the connection inline and answers a missing account id with a bare `notCreated`, so adding LinkedIn to `accountDiscoveryProviders` today would still produce an unclassified create failure that never names the missing choice. Routing `Dispatch` through the shared resolver — preserving its `notCreated` semantics — is the remaining work, and it is what earns LinkedIn the second half. Stating which half is missing matters because the halves are earned separately. Meta is the one provider where the halves ever came apart: it gained enumeration in LFXV2-3062 (the row above) and was still refused here, because its `Dispatch` answered an empty account id with a generic error. LFXV2-3061 supplied the second half — `requireMetaAccountID` tags it `ErrAccountNotSelected` + `ErrConnectionNotUsable`, which `unusableConnectionReason` reports as `account_not_selected`, so the dispatch-failure log line names the missing choice from a fixed vocabulary instead of carrying an unclassified error. State that precisely rather than as "the job result says so": create is queued work and `dispatchPlatform` collapses every dispatcher error into `"platform campaign creation failed"`, so the reason token is log-only on this path — for Google Ads too. LFXV2-3061 also dropped `Required("account_id")` from `MetaAdsConnectionConfig` to match. Meta is in `accountDiscoveryProviders` as of that ticket. Both halves, not either alone, are the bar for adding the next provider. The account-discovery endpoint above then LISTS the accounts those credentials can reach, but it is a GET and assigns nothing: selecting one means re-running `bootstrap-system-account` with `-account-id`, which rotates onto the same row. On that rotation an omitted flag means KEEP, not clear — restating the whole row should not be required — so removals are said explicitly: `-clear-account-id` returns a Google Ads, Meta, X or HubSpot row to credentials-only — the first three because discovery can re-select an account, HubSpot because it never needed one — and `-config login_customer_id=` (empty value) drops a config column. Both are refused where the state they would produce is one the installer already refuses to create: clearing LinkedIn's `org_id` or its account id fails exactly as omitting them at install time does, and a clear issued before the row exists is refused rather than dropped. Finally, an UNRECOGNISED first argument now exits 2 instead of falling through to server startup — `campaign-service bootstrap-system-acount …` used to parse cleanly and bring up an idle HTTP server, leaving the Job green with nothing installed.
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
| `demand-gen` | Demand Gen (YouTube, Discover, Gmail) |
| `performance-max` | Performance Max (every Google inventory from one asset group) |
| `video` | Video / YouTube — **adoption and reporting only; CREATE IS REFUSED**, because the Google Ads API cannot create or mutate Video campaigns ([Google's Video overview](https://developers.google.com/google-ads/api/docs/video/overview)) |
| `display` | Display network (responsive display ad, no channel sub-type) |

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
redditConfig?: object           — Reddit-specific params (see RedditConfig below)
metaConfig?: object             — Meta-specific params (see MetaConfig below)
twitterConfig?: object          — X/Twitter-specific params (see TwitterConfig below)
microsoftConfig?: object        — Microsoft Ads-specific params (see MicrosoftConfig below)
hubspotConfig?: object          — HubSpot (email channel) params (see HubSpotConfig below)
```

#### RedditConfig (the `redditConfig` object)

Reddit Ads per-platform config. The dispatcher creates a PAUSED campaign -> ad group ->
ad (promoted post). Targeting is communities (subreddits) + keywords + geo; a conversion
pixel is required on EVERY objective. The ad's creative comes from EITHER a supplied
`postUrl` (promote an existing post) OR, when `postUrl` is absent, an `imageUrl` the client
uses to AUTHOR a promoted image post itself (the brief->servable-ad path, parity with
`googleAdsConfig`/`metaConfig`). With neither, the campaign + ad group are created but the ad
is left for manual creation in Reddit Ads Manager. **Budget is in USD** (a LIFETIME spend cap,
not daily).

```
budgetUsd: number               — LIFETIME spend cap in USD. Must be finite and POSITIVE and
                                  round to at least one micro-dollar; NaN/Inf/non-positive is
                                  rejected during dispatch (a pre-create job failure, since
                                  CreateCampaigns is async). Omitting it fails the platform job.
startDate: string               — YYYY-MM-DD (required, calendar-valid).
endDate: string                 — YYYY-MM-DD (required, must be AFTER startDate).
objective?: string              — awareness | traffic | conversions | video_views.
                                  Defaults to conversions.
geoTargets?: string[]           — ISO 3166-1 alpha-2 country codes (e.g. ['US','JP']), spelled as
                                  in `metaConfig`/`googleAdsConfig`. Uppercased/de-duplicated; an
                                  invalid code is rejected before any create. Defaults to ['US'].
subreddits?: string[]           — Community targeting as subreddit NAMES ('r/golang' or 'golang';
                                  the 'r/' prefix is stripped). Sent as `communities` names, not
                                  t5_ IDs. A name Reddit rejects is dropped with a warning step
                                  (never orphans the campaign).
interests?: string[]            — Interest targeting. Reddit wants opaque interest IDs while briefs
                                  produce human labels, so these are commonly dropped with a warning
                                  (label->ID resolution tracked in LFXV2-3261).
keywords?: string[]             — Keyword targeting attached to the ad group.
variants?: [{headline, body}]   — Ad copy variants. When no `postUrl`/`imageUrl` drives an ad, one
                                  "ready" instruction per variant is emitted (headline + display UTM
                                  URL) for manual ad creation. On the author-a-post path the FIRST
                                  variant's headline is the authored post's headline.
postUrl?: string                — OPTIONAL existing Reddit post to promote. Accepts a t3_ id, a
                                  reddit.com/comments/<id> URL, or a redd.it short link; validated
                                  against reddit.com/redd.it hosts. When set it TAKES PRECEDENCE and
                                  `imageUrl`/`callToAction` are ignored. The stored config snapshot keeps
                                  only its scheme and host — path, query and fragment are all stripped,
                                  because any of the three may carry a secret; an http(s) value that
                                  cannot be reduced to a scheme and host is dropped entirely.
imageUrl?: string               — OPTIONAL public absolute http(s) image URL. When set and `postUrl`
                                  is absent, the client AUTHORS a promoted ("dark") IMAGE post from it
                                  (Reddit ingests and re-hosts the image at create time; there is no
                                  LINK post type and no separate upload step) and attaches it as the
                                  ad's creative. A malformed URL / embedded userinfo / non-http(s)
                                  scheme is rejected before any create. The stored config snapshot keeps
                                  only its scheme and host (a signed URL may carry a secret in its path
                                  as readily as in its query).
callToAction?: string           — OPTIONAL button label for an AUTHORED post (see `imageUrl`).
                                  Case-insensitive, resolved to Reddit's exact title-case label (e.g.
                                  'Learn More', 'Sign Up', 'Buy Tickets'); an unknown value is rejected
                                  before any create. Defaults to 'Learn More'. Ignored when `postUrl`
                                  is set.
conversionPixelId?: string      — OPTIONAL per-campaign override of the connection's conversion pixel.
                                  A pixel is REQUIRED for every objective; normally it comes from the
                                  Reddit connection, and a create with none configured is refused
                                  before any upstream call.
videoGoal?: string              — REQUIRED when objective is video_views: VIDEO_VIEW_6S | VIDEO_VIEW_15S
                                  (Reddit has no bare VIDEO_VIEWS goal). Ignored for other objectives.
```

The connection supplies the ad account id and OAuth2 credentials — not this campaign config.

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
geoTargets?: string[]           — OPTIONAL ISO 3166-1 alpha-2 country codes the campaign should serve
                                  in, attached as CAMPAIGN-level location criteria. At most 30. OMITTED
                                  means NO location criteria, i.e. Microsoft serves the campaign
                                  everywhere. An unsupported or unresolvable code REFUSES the create
                                  before anything is created rather than silently serving everywhere.
```

`geoTargets` is accepted (LFXV2-3279), using the SAME ISO 3166-1 alpha-2 vocabulary as
`redditConfig`/`metaConfig`/`linkedInConfig`. Microsoft's location criteria take numeric
`LocationId` values rather than ISO codes, so the service resolves each code against Microsoft's
own geographical-locations file at create time — no ISO→LocationId table is hardcoded.

**Omitting it means the campaign serves EVERYWHERE**, which is the pre-existing behaviour and is
why an unusable value is refused rather than dropped. A code that is not a Microsoft-supported
country, or that cannot be resolved against the locations file, fails the create **before any
campaign is created** — nothing is left behind to clean up. Resolution is all-or-nothing: one
unresolvable code fails the whole set rather than targeting a subset nobody approved. Because
`CreateCampaigns` is asynchronous, that surfaces as a failed job rather than a synchronous 4xx.

A campaign REUSED by a retry has its existing location criteria read and reconciled, so only
missing locations are attached; if that read fails, the create refuses rather than risk
duplicating criteria or reporting an untargeted campaign as targeted.

The connection supplies the ad account id (`account_id`, the digits-only `CustomerAccountId`) and
an OPTIONAL manager/MCC id (`customer_id`, the `CustomerId` header) via the Microsoft connection
config — not this campaign config.

#### GoogleAdsConfig (the `googleAdsConfig` object)

Google Ads per-platform config. Which campaign is created depends on `channel`. The default
(`search`) is a PAUSED search campaign with an ad group + a Responsive Search Ad (GA-3), then
keyword/audience targeting attached to that ad group (GA-4) — without it, the ad group has zero
criteria and the campaign can never serve, even once a human enables it. `demand-gen` creates a
Demand Gen campaign with an ad group and (given `demandGenCreative`) one ad; `performance-max`
creates a Performance Max campaign with NO ad group and NO ad at all — its creative is an ASSET
GROUP built from `performanceMaxCreative`; `video` CREATES NOTHING — the Google Ads API
supports fetching and reporting on Video campaigns but cannot create or mutate them, so a
`video` create is REFUSED before the first budget mutate rather than stranding a paid
budget at the campaign step (`adoptExisting` still works, and so does monitoring, because
only creation is impossible); `display` creates a DISPLAY campaign with NO channel
sub-type, an ad group of type `DISPLAY_STANDARD`, and (given `displayCreative`) one
responsive display ad built from images this service fetches and uploads. **Budget is in whole units of the ad ACCOUNT's
currency**, not USD — the service does no FX conversion (mirroring `metaConfig`).

Every field below is OPTIONAL except `budget`, and every one of them is additive: a config
that names none of them produces exactly the single-ad-group, single-ad campaign this
service created before they existed. Additive does NOT mean channel-independent — the
entries below marked SEARCH ONLY are REFUSED, not ignored, on any other `channel`. Each
entry states which channels accept it, because the channels do not refuse the same set:
`performance-max` takes languages, ad schedules and proximity that `demand-gen` refuses, and
refuses device bid modifiers and demographic exclusions alongside it. Each is validated
BEFORE the first budget mutate, so a
refused value cannot strand a paid campaign — and the same validation runs on the
`adoptExisting` path, so a config is refused identically whether it creates or adopts.

```
budget: number                  — Whole units of the account currency (e.g. 2500 = 2500 USD/JPY/…),
                                  applied as the campaign's DAILY budget. Must be a finite, POSITIVE
                                  number; NaN/Inf or a non-positive value is rejected by the client
                                  during dispatch (a pre-create job failure, since CreateCampaigns is
                                  async). Omitting it leaves the shell with no budget, which fails the
                                  platform job asynchronously — supply it explicitly.
channel?: string                — OPTIONAL which Google Ads campaign type to create: `search` (the
                                  default), `demand-gen`, `performance-max`, `video` or `display`. ABSENT
                                  MEANS `search`,
                                  deliberately: every caller predating this field omits it and means
                                  Search, so absence must not repoint them. An unrecognised value is
                                  REFUSED, never defaulted — defaulting a typo'd `demandgen` to Search
                                  would spend the Demand Gen budget on Search ads and report success.
                                  `video` is accepted and VALIDATED like any other channel but its
                                  CREATE is refused: Google cannot create Video campaigns. It remains a
                                  legal value because `adoptExisting` and monitoring do work on Video.
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

                                  SEARCH ONLY, and REFUSED rather than ignored on every other
                                  `channel`: keyword criteria are attached only on the Search cascade,
                                  Demand Gen's ad group takes no criteria at all, and Performance Max
                                  has no ad group to hang them on. Accepting them elsewhere would
                                  validate every term and then discard the lot.
audienceSegments?: string[]     — OPTIONAL Google Ads resource names of EXISTING audiences to attach
                                  to the ad group (GA-4) as observation-only criteria — bid/report on
                                  the segment without narrowing delivery to it. This client does not
                                  create audiences; each entry must be a Customer Match user list
                                  (`.../userLists/{id}`) the caller already built elsewhere. Custom
                                  audiences are not supported: this client attaches audience criteria
                                  only on the Search ad-group cascade, and `audienceSegments` is refused
                                  outright on every other `channel` (below), so the Demand Gen and
                                  Performance Max campaigns it now creates never reach an audience
                                  attach at all.
                                  Any other resource-name shape (customAudiences, userInterest,
                                  combinedAudience, etc.) is rejected. At most 20 entries; duplicates are
                                  deduped. When non-empty, the client sets the ad group's
                                  `targetingSetting.targetRestrictions` (AUDIENCE, bidOnly) on the ad group
                                  create so these segments stay observation-only rather than Google's
                                  default of restricting delivery to the audience alone.

                                  SEARCH ONLY, and REFUSED rather than ignored on every other
                                  `channel`, for the same reason `keywords` is: audience criteria are
                                  attached only on the Search cascade, so an audience named on any
                                  other channel would be validated and then dropped, leaving a campaign
                                  that reads as targeted and is not.
geoTargets?: string[]           — OPTIONAL locations the campaign should serve in (LFXV2-3283). Each
                                  entry is EITHER an ISO 3166-1 alpha-2 country code, spelled as in
                                  `metaConfig`/`redditConfig`, OR a raw numeric geo target constant id
                                  from Google's published geo-targets table (LFXV2-2665) — which is how
                                  a caller addresses a CITY, region, metro or postal code, none of which
                                  a country code can express. The two spellings mix freely in one list
                                  and are told apart by SHAPE (two letters vs all digits), so nothing is
                                  ambiguous and no flag says which kind an entry is.

                                  Each is resolved to Google's numeric geo target constant and attached
                                  as a location criterion at the level the CHANNEL requires: campaign
                                  level for Search and Performance Max, AD GROUP level for Demand Gen
                                  (which rejects campaign-level location criteria). Case/whitespace-insensitive and
                                  de-duplicated by the RESOLVED id — "US" and "2840" are one criterion;
                                  at most 60 entries.

                                  A numeric id is checked for SHAPE only, but that shape check is
                                  stricter than "all digits": the entry must be the CANONICAL base-10
                                  spelling of a POSITIVE int64, which is the type Google exposes these
                                  ids as. `0` names nothing, `02840` is a non-canonical spelling of
                                  2840, and a 21-digit run overflows the type — all three fail the job
                                  BEFORE any Google Ads request, alongside the country codes.

                                  What the shape check cannot do is tell you the id EXISTS. This client
                                  cannot know whether 1014044 names a real place without asking Google,
                                  and a lookup would make the pre-create validation send a request. A
                                  well-formed id naming nothing is therefore refused by Google at the
                                  criteria mutate, AFTER the campaign exists — the cost of reaching past
                                  the curated country map, and the one way a geo target can fail late.
                                  Country codes are verified locally in full and so always fail early.

                                  Both channel creates set `geoTargetTypeSetting.positiveGeoTargetType`
                                  to PRESENCE. Google's default is PRESENCE_OR_INTEREST, under which a
                                  user anywhere in the world who merely shows INTEREST in the targeted
                                  country stays eligible — so criteria alone would attach targeting
                                  that does not restrict spend. It is set unconditionally, so a
                                  campaign that gains criteria later (by adoption, or by hand in the
                                  Google Ads UI) restricts by presence rather than reverting to the
                                  permissive default.

                                  An unsupported code (including a plausible typo like "USA") fails the
                                  job BEFORE any Google Ads request is made, rather than being dropped —
                                  a dropped code would create a campaign that spends worldwide while
                                  reporting success.

                                  Omitted/empty, NO location criteria are created and the campaign
                                  serves wherever the ad ACCOUNT's defaults allow — usually worldwide.
                                  That is the pre-LFXV2-3283 behaviour, preserved so callers predating
                                  this field keep working; the dispatcher logs a WARN when it happens.
                                  Supply it for any campaign with a target region.
excludedGeoTargets?: string[]   — OPTIONAL locations the campaign must NOT serve in (LFXV2-2665).
                                  Exactly the vocabulary, caps, case rules and dedupe of `geoTargets`
                                  above (country codes or raw constant ids, at most 60), attached as
                                  NEGATIVE location criteria at the same channel-dependent level. The
                                  two lists are bounded and de-duplicated independently — a parent
                                  region may legitimately be targeted while a city inside it is
                                  excluded.

                                  Listing the SAME resolved location in both fails the job BEFORE any
                                  Google Ads request. Google resolves that contradiction by letting the
                                  exclusion win, so the campaign would silently not serve where the
                                  caller plainly asked it to.

                                  Applies to EVERY `channel` — unlike proximity below, an excluded
                                  location is the same criterion at either level, and Performance Max
                                  attaches it at the campaign level like Search. Omitted/empty, no
                                  exclusions are attached.
proximityTargets?:              — OPTIONAL radius targeting: "everyone within N of this point"
  {latitude, longitude,           (LFXV2-2665). `latitude`/`longitude` are decimal degrees (e.g.
   radius, radiusUnit}[]          37.7749, -122.4194), converted to the microdegrees Google's GeoPoint
                                  carries. `radius` must be > 0 and within Google's ceiling for the
                                  unit: 500 MILES or 800 KILOMETERS. `radiusUnit` is REQUIRED and must
                                  be "MILES" or "KILOMETERS" — there is deliberately no default, since
                                  a radius of 50 means two very different campaigns depending on the
                                  unit and guessing would silently buy ~2.5x (or 0.4x) the intended
                                  area. At most 20 entries, counted as SUBMITTED; a non-finite
                                  coordinate, an out-of-range radius or an unknown unit fails the job
                                  before any request.

                                  De-duplicated by the RENDERED criterion — microdegree coordinates,
                                  radius and normalised unit — so a trailing zero on a coordinate or a
                                  differently-cased unit does not produce the duplicate criterion Google
                                  rejects. As with `geoTargets`, a repeat COLLAPSES rather than failing
                                  the job: a proximity target carries no bid modifier, so two identical
                                  entries cannot disagree about anything. Units are NOT converted — 10
                                  MILES and 16.09 KILOMETERS stay two criteria.

                                  REFUSED rather than ignored on `demand-gen` ALONE: that channel takes
                                  location criteria on the ad group, where this client has not verified
                                  proximity against a real account. Dropping it silently would create a
                                  campaign serving nationwide when the caller asked for a 25-mile
                                  radius. `performance-max`, `video` and `display` take campaign-level
                                  geo exactly as Search does, so radius targets are accepted on all
                                  three.
negativeKeywords?:              — OPTIONAL Search keyword EXCLUSIONS, attached at CAMPAIGN level (not
  {text, matchType}[]             ad group), so they keep applying to any ad group a human adds later.
                                  Same `text`/`matchType` rules as `keywords` above (≤80 runes; EXACT,
                                  PHRASE or BROAD, case-insensitive), at most 60 entries, deduped
                                  independently of the positive list — a term may legitimately appear
                                  in both. An empty text or unsupported matchType fails the job BEFORE
                                  any Google Ads request is made.

                                  SEARCH ONLY, and REFUSED rather than ignored on every other
                                  `channel`, as `keywords` and `audienceSegments` now are. An
                                  exclusion exists to STOP spend, so dropping it quietly would leave
                                  the campaign paying for exactly the queries you named. Omitted/empty, no
                                  exclusions are attached and the campaign is eligible for every query
                                  its positive keywords match.
cpcBid?: number                 — OPTIONAL manual CPC bid for the ad group, in whole units of the ad
                                  ACCOUNT's currency (the same no-FX-conversion caveat `budget`
                                  carries). Accepted range 0.01..100000.0 inclusive; NaN/Inf or a
                                  value outside it fails the job before any Google Ads request. The
                                  range is deliberately loose — it exists to catch a micros-vs-units
                                  mistake, not to mirror a Google limit, and the ceiling is sized for
                                  the weakest currency an account can be opened in (1000 JPY is under
                                  $7) rather than for USD, because the value is never converted.

                                  0 (or omitted) means UNSET: no bid field is sent and the ad group
                                  inherits whatever Google derives, which is what every campaign
                                  created before this field existed did. An explicit 0 is NOT sent as
                                  a zero bid. SEARCH only — no other channel here has a manual bidding
                                  strategy, and a non-zero bid on one of them is REFUSED before
                                  anything is created, not dropped: their ad-group payloads carry no
                                  bid field at all (Performance Max has no ad group whatsoever), so
                                  accepting it would discard it silently.

                                  Also REFUSED under any `biddingStrategy` other than `manual-cpc`:
                                  an automated strategy sets the bids itself, so a CPC bid supplied
                                  with one is never bid. The refusal covers a per-group `cpcBid` in
                                  `adGroups` as well as this campaign-level one.
biddingStrategy?: string        — OPTIONAL how the campaign bids. Named with the Google Ads UI's own
                                  labels, lower-cased and hyphenated, because the operator choosing one
                                  is reading that UI and not the proto:

                                    manual-cpc                 you set the bid (see `cpcBid`)
                                    maximize-clicks            most clicks the budget allows
                                    maximize-conversions       most conversions the budget allows
                                    target-cpa                 ..., at a target cost per conversion
                                    maximize-conversion-value  most conversion VALUE the budget allows
                                    target-roas                ..., at a target return on ad spend

                                  `target-cpa`/`target-roas` are the UI's names for
                                  `maximize-conversions`/`maximize-conversion-value` WITH a target set;
                                  both spellings are accepted and resolve to the same Google strategy.
                                  The only difference is that the target is REQUIRED under the
                                  target-bearing name and optional under the maximize- one.

                                  Omitted, the channel default is used and the payload is byte-identical
                                  to what this service sent before the strategy was selectable:
                                  `manual-cpc` on Search, `maximize-clicks` on Demand Gen,
                                  `maximize-conversions` on Performance Max. An unknown name is refused
                                  and the error lists the supported set.

                                  On `demand-gen` ONLY `maximize-clicks` is accepted. That is not a
                                  Google limit but the limit of what this client has verified: a
                                  recorded live-API check (2026-08-14, v23) had Demand Gen accept
                                  `targetSpend` and reject `maximizeConversions` with
                                  BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET — AFTER the
                                  budget was created. Anything else on that channel is refused before
                                  the budget mutate rather than risking that orphan.

                                  On `performance-max` the accepted set is the four conversion-oriented
                                  strategies — `maximize-conversions`, `target-cpa`,
                                  `maximize-conversion-value`, `target-roas`. Performance Max has no
                                  manual form at all, so `manual-cpc` and `maximize-clicks` are refused
                                  there before the budget mutate.

                                  On `video` the accepted set is the NARROWEST of the four:
                                  `maximize-conversions` and `target-cpa`, and nothing else. That is
                                  not a Google limit either — it is the pair Google's published
                                  VIDEO_ACTION documentation names, and unlike every other channel's
                                  set it has NOT been checked against the live API, because the only
                                  reachable account is a production one and a `validateOnly` mutate is
                                  still a POST to it. The refusal message says so in those words
                                  rather than claiming a verification that never happened. Widening
                                  the set is a live-API question: run a `validateOnly`
                                  `campaigns:mutate` for VIDEO/VIDEO_ACTION on a non-production
                                  account and record the result in `videoBiddingStrategies`.

                                  On `display` the accepted set is the five AUTOMATED strategies —
                                  `maximize-clicks`, `maximize-conversions`, `target-cpa`,
                                  `maximize-conversion-value` and `target-roas`. It is NOT live-verified
                                  either, on exactly Video's terms, and the refusal message says so.
                                  `manual-cpc` is the DELIBERATE omission and is this client's
                                  limitation rather than Google's: Google accepts manual CPC on a
                                  Display campaign, but this client's Display AD GROUP payload carries
                                  no bid field and `cpcBid` is refused off Search, so a manual-CPC
                                  Display campaign created from here would serve bidding a number
                                  nobody supplied. Closing that is an implementation question — give
                                  the ad group a bid field — not an API one.
targetCpa?: number              — OPTIONAL target cost per conversion, in whole units of the ad ACCOUNT's
                                  currency (same no-FX caveat as `budget`). Accepted range
                                  0.01..1000000.0; 0 or omitted means UNSET and no target is sent.
                                  REQUIRED with `target-cpa`; optional with `maximize-conversions`;
                                  REFUSED with any other strategy rather than dropped, since a target
                                  the strategy cannot carry is an instruction that would vanish.
targetRoas?: number             — OPTIONAL target return on ad spend. A RATIO, NOT a percentage: 4.0
                                  means "four units of conversion value per unit spent", i.e. 400%.
                                  Accepted range 0.01..1000.0 — Google's own documented bounds; 0 or
                                  omitted means UNSET. REQUIRED with `target-roas`; optional with
                                  `maximize-conversion-value`; REFUSED with any other strategy.

                                  Note 400 IS inside the accepted range even though it is also how
                                  400% is commonly mis-typed. Refusing it would refuse a target Google
                                  accepts, so the ratio spelling is documented here and named in the
                                  out-of-range error rather than guessed at.
conversionActions?: string[]    — OPTIONAL the conversion actions THIS campaign optimizes toward,
                                  overriding the account-level conversion goals. Each entry is either a
                                  bare numeric id (`987654321`) or the full resource name
                                  (`customers/<customer-id>/conversionActions/<id>`); bare ids are
                                  qualified with the campaign's own account. At most 100 entries;
                                  duplicates across the two spellings are deduplicated, not refused.

                                  A full resource name naming a DIFFERENT customer is refused — a
                                  conversion action cannot be shared across accounts, and the create
                                  would otherwise fail after the budget mutate. Omitted/empty, no
                                  selective-optimization field is sent and the campaign bids toward the
                                  account's own conversion goals, which is what every campaign created
                                  before this field existed did.

                                  ACCEPTED on `search`, `video` and `display`; REFUSED rather than
                                  ignored on `demand-gen` and `performance-max`. The split follows the
                                  mechanism, not a Search-versus-the-rest rule: this client attaches
                                  them through `campaign.selective_optimization`, which Google defines
                                  for SEARCH, DISPLAY, VIDEO and APP campaigns — so refusing them on
                                  Video or Display would refuse a payload Google accepts. Demand Gen
                                  conversion goals (`conversion_goal_campaign_config`) and Performance
                                  Max campaign conversion goals are NOT implemented — both need a
                                  second mutate after the campaign exists. A Performance Max campaign
                                  therefore inherits the ACCOUNT's conversion goals, which is what
                                  Google applies when none are named.

                                  Sent at CREATE time via `campaign.selective_optimization` rather than
                                  through `campaignConversionGoal`, which is update-only: attaching
                                  goals after the campaign exists needs a second mutate that can fail
                                  and leave a campaign bidding toward the wrong goals.

                                  To populate a picker, the account's available actions are read by
                                  `googleads.ListConversionActions` — id, name, status, type, category
                                  and whether each is primary for the account goal. It exists only as a
                                  Go client method today: there is NO endpoint, no Goa method and no
                                  mount, so a caller has no HTTP route to source these ids from yet.
                                  Creating a
                                  conversion action is deliberately NOT offered: it is half a
                                  measurement setup (the other half is a site tag), and one created
                                  without its tag reports as configured while recording nothing.
startDate?: string              — OPTIONAL campaign flight window as `YYYY-MM-DD` (spelled as in
endDate?: string                  `metaConfig`/`redditConfig`). Each is INDEPENDENTLY optional: an
                                  omitted `startDate` leaves Google's default (the campaign starts
                                  today) and an omitted `endDate` leaves it running until someone
                                  stops it. When both are present, the end must not be BEFORE the
                                  start — the same day on both sides is accepted and is a full 24-hour
                                  flight.

                                  The format is strict: `2026-8-1` is refused, because Go's date parse
                                  would otherwise accept it and silently render back a date the caller
                                  never wrote. There is deliberately no "start date is in the past"
                                  check — Google interprets these in the ad ACCOUNT's timezone, which
                                  this service does not know, so a UTC "today" would refuse a start
                                  date Google accepts for an account several hours behind.

                                  Applies to EVERY `channel`: a flight window is a property of the
                                  campaign, not of the channel — all three create payloads carry these
                                  fields, resolved by the one shared preflight. Sent as v23's
                                  `startDateTime`/`endDateTime` with the account-timezone day
                                  boundaries (`00:00:00` / `23:59:59`) — the pre-v23 `startDate`/
                                  `endDate` request fields were REMOVED and are rejected. The
                                  `23:59:59` end boundary is what makes `endDate` INCLUSIVE: the
                                  campaign serves through the end of the day named. Both are
                                  validated before the budget mutate, so a malformed date fails
                                  without orphaning a paid campaign. Unlike `redditConfig`, both
                                  are OPTIONAL here.
languages?: string[]            — OPTIONAL languages the campaign targets (LFXV2-2665), as campaign-level
                                  language criteria. Each entry is EITHER an ISO 639-1 code (EN, DE, JA)
                                  OR a raw numeric language constant id from Google's language-constants
                                  table, told apart by shape exactly as `geoTargets` does, deduped by the
                                  resolved id; at most 40 entries. Targets the user's Google INTERFACE
                                  language, not the ad copy's language — Google does not translate.

                                  Omitted/empty, NO language criteria are created and the campaign is
                                  eligible in every language, which is what every campaign created
                                  before this field existed did.

                                  REFUSED rather than ignored on `demand-gen` — as are `adSchedules`,
                                  `deviceBidModifiers`, `excludedAgeRanges` and `excludedGenders`,
                                  which one guard refuses together there. Demand Gen attaches this
                                  targeting at the AD GROUP level, where this client has not verified
                                  it against a real account; accepting the field and dropping it would
                                  create a campaign with none of the targeting the caller asked for.

                                  ACCEPTED on `performance-max`, which Google documents as taking
                                  LANGUAGE, LOCATION and AD_SCHEDULE campaign criteria. Only
                                  `deviceBidModifiers`, `excludedAgeRanges` and `excludedGenders` are
                                  refused there; refusing languages too would be an over-refusal of
                                  something Google accepts.

                                  ACCEPTED on `video` AND on `display`, along with `adSchedules`,
                                  `deviceBidModifiers`, `excludedAgeRanges` and `excludedGenders` — the
                                  whole set, exactly as on Search. A VIDEO or DISPLAY campaign carries
                                  all five as CAMPAIGN criteria and each cascade attaches all five, so
                                  none of them is dropped. One NAMED GAP inside `deviceBidModifiers`:
                                  Google supports a TV-screen device on these two channels and this
                                  client sends it on none, because whether that criterion carries a bid
                                  modifier is not settled by the documentation and guessing lands at a
                                  mutate that runs after the budget. That is this client's limitation,
                                  not Google's.
adSchedules?:                   — OPTIONAL dayparting (LFXV2-2665): the intervals in the ad ACCOUNT's
  {dayOfWeek, startHour,          timezone during which the campaign may serve. `dayOfWeek` is
   startMinute, endHour,          MONDAY..SUNDAY (case-insensitive). `startHour` is 0..23 and `endHour`
   endMinute, bidModifier?}[]     1..24 (24 being midnight at the END of the day, and then only with
                                  minute 0). Minutes are 0, 15, 30 or 45 — Google models them as an
                                  enum, not a number. The end must be strictly after the start. At most
                                  42 entries, AND at most 6 per day of the week — 42 is 6x7, so the
                                  list cap alone would admit seven Monday intervals, which Google
                                  refuses. Both limits are Google's own, so neither refuses anything
                                  upstream would have accepted; they only move the refusal to before
                                  the budget mutate.

                                  Intervals on one day must NOT overlap, and an overlap fails the job
                                  before any Google Ads request. The window is HALF-OPEN, so
                                  09:00-12:00 and 12:00-17:00 do not overlap and an ordinary split-day
                                  schedule is accepted; 09:00-12:00 and 11:00-13:00 do, and are
                                  refused. An exact repeat is not an overlap — it is one criterion
                                  written twice, and collapses (or is refused for a conflicting
                                  `bidModifier`, below). Intervals on DIFFERENT days never interact.

                                  `bidModifier` is OPTIONAL PER INTERVAL and is a true tri-state: absent
                                  means no adjustment, and an explicit 0 is Google's -100% opt-out
                                  (do not serve). Otherwise it must be between 0.1 and 10.0.

                                  Omitted/empty, no schedule criteria are created and the campaign is
                                  eligible around the clock. Supplying ANY interval restricts the
                                  campaign to the intervals listed — Google treats the set as
                                  exhaustive, so a single Monday interval means a Monday-only campaign.

                                  REFUSED rather than ignored on `demand-gen`, accepted on
                                  `performance-max`, `video` and `display`; see `languages` for why.
deviceBidModifiers?:            — OPTIONAL per-device bid adjustments (LFXV2-2665). `device` is one of
  {device, bidModifier}[]         MOBILE, DESKTOP, TABLET (case-insensitive); at most 3 entries and a
                                  device may appear only ONCE — two criteria for the same device are a
                                  conflict Google rejects after the campaign exists, and the caller
                                  plainly meant one of the two values.

                                  There is NO TV-screen value, on ANY channel. Google supports that
                                  device only on Display and Video campaigns — which this service now
                                  creates — so on those two the omission is this client's limitation
                                  rather than Google's, and is named as one. It stays closed because the
                                  documentation does not settle whether a TV-screen criterion carries a
                                  bid modifier, and this client cannot send one without: a wrong guess
                                  is rejected at the criteria mutate, after the campaign is paid for.

                                  `bidModifier` is REQUIRED here (unlike on `adSchedules`, where the
                                  absent case means "listed but unadjusted"; a device entry with no
                                  modifier would say nothing at all). Same range: exactly 0 is the
                                  -100% opt-out that stops the campaign serving on that device,
                                  otherwise 0.1..10.0.

                                  Omitted/empty, no device criteria are created and the campaign bids
                                  equally on every device.

                                  REFUSED rather than ignored on `demand-gen` AND on
                                  `performance-max`; ACCEPTED on `video` and `display`; see `languages`
                                  for why.
excludedAgeRanges?: string[]    — OPTIONAL demographic EXCLUSIONS (LFXV2-2665), attached as negative
excludedGenders?: string[]        campaign criteria. Age ranges are 18-24, 25-34, 35-44, 45-54, 55-64,
                                  65+ or UNDETERMINED (Google's own AGE_RANGE_* enum names are accepted
                                  too); genders are MALE, FEMALE or UNDETERMINED. At most 20 entries
                                  each, bounded independently.

                                  EXCLUSIONS only — there is no positive demographic field. Google
                                  targets demographics by excluding the buckets you do not want, and
                                  UNDETERMINED covers every user whose demographic Google has not
                                  inferred, which on Search is a large share of traffic: excluding it
                                  narrows reach far more than the other buckets do.

                                  Omitted/empty, no demographic criteria are created and the campaign
                                  is eligible for every bucket.

                                  REFUSED rather than ignored on `demand-gen` AND on
                                  `performance-max`; ACCEPTED on `video` and `display`; see `languages`
                                  for why.
sitelinks?:                     — OPTIONAL sitelink extensions (LFXV2-2665): extra links shown under the
  {text, description1?,           ad. `text` ≤25 runes, required and unique within the list.
   description2?, finalUrl}       `description1`/`description2` are ≤35 runes each and ALL-OR-NOTHING —
                                  one without the other is refused, because Google renders a sitelink
                                  with a single description line as if it had none, silently discarding
                                  copy the caller wrote. `finalUrl` is required and must be a servable
                                  http(s) URL; it is UTM-tagged by the same builder as the ad's own
                                  destination and must be ≤2084 bytes AFTER tagging. At most 20.

                                  Lengths are RUNE counts, not the double-width WEIGHT the RSA copy
                                  uses, and over-long text is REFUSED rather than truncated: generated
                                  ad copy may be cut because this service wrote it, but extension text
                                  is written by a human for a reason.
callouts?: string[]             — OPTIONAL callout extensions (LFXV2-2665): short non-clickable phrases
                                  ("Free workshops"). ≤25 runes each, de-duplicated, at most 20.
structuredSnippets?:            — OPTIONAL structured-snippet extensions (LFXV2-2665): a header and the
  {header, values[]}[]            list it labels ("Courses: Kubernetes, Observability, Security").
                                  `header` ≤25 runes and unique within the list; 3..10 distinct `values`
                                  of ≤25 runes each — fewer than 3 is refused because Google will not
                                  serve the snippet. At most 10 snippets.

                                  The header is checked for SHAPE only, not against Google's published
                                  header vocabulary: the valid set is LANGUAGE-DEPENDENT and Google
                                  revises it, so a local list would refuse headers Google accepts. An
                                  unrecognised header is rejected upstream, after the campaign exists.

callExtensions?:                — OPTIONAL call extensions (LFXV2-2665): a phone number shown with the
  {countryCode, phoneNumber}[]    ad. `countryCode` is an ISO-3166-1 alpha-2 code (upper-cased before
                                  sending). `phoneNumber` may carry digits, spaces and `+-().` only —
                                  LETTERS are refused, because Google rejects vanity numbers such as
                                  1-800-FLOWERS — and must hold at least 4 digits and be ≤35 runes.
                                  De-duplicated on country plus digits-only number. At most 20.
promotions?:                    — OPTIONAL promotion extensions (LFXV2-2665): a discount shown with the
  {promotionTarget,               ad. `promotionTarget` (the thing discounted) is required, ≤25 runes
   discountPercent?,              and unique case-insensitively within the list.
   discountAmount?,
   ordersOverAmount?,             EXACTLY ONE discount arm is required — `discountPercent` (0 < p ≤ 100)
   currencyCode?,                 or `discountAmount`; supplying both or neither is refused, because
   promotionCode?, occasion?,     Google models them as a oneof and would pick for you. A percentage is
   languageCode?,                 sent as micros of a FRACTION (Google's 1,000,000 = 100%), so 25 becomes
   startDate?, endDate?,          250,000. `discountAmount` and `ordersOverAmount` each require
   redemptionStartDate?,          `currencyCode` (ISO-4217, three upper-case letters).
   redemptionEndDate?,
   finalUrl}[]                    At most ONE eligibility arm: `promotionCode` (≤20 runes) or
                                  `ordersOverAmount` — a promotion cannot be both code-gated and
                                  spend-gated.

                                  `occasion` is checked for SHAPE only (`^[A-Z][A-Z0-9_]*$`), not
                                  against Google's published occasion list, for the same reason
                                  structured-snippet headers are: the list is revised upstream and a
                                  local copy would refuse values Google accepts.

                                  The two date windows are ordered INDEPENDENTLY: an offer may be
                                  redeemable after the ad stops running, so `redemptionEndDate` is not
                                  required to fall inside `startDate`..`endDate`. Each window is
                                  `YYYY-MM-DD` and each requires its own start before its own end.

                                  `finalUrl` is required, UTM-tagged by the same builder as the ad's
                                  destination, and ≤2084 bytes after tagging. At most 20.
prices?:                        — OPTIONAL price extensions (LFXV2-2665): a table of offerings shown
  {type, priceQualifier?,         with the ad. `type` (e.g. `EVENTS`), `priceQualifier` (e.g. `FROM`)
   languageCode,                  and each offering's `unit` (e.g. `PER_DAY`) are SHAPE-checked only,
   offerings: {header,            like `occasion` above. `languageCode` is required.
     description, amount,
     currencyCode, unit?,         3..8 `offerings` — fewer than 3 is refused because Google will not
     finalUrl}[]}[]               serve the table. Per offering: `header` and `description` ≤25 runes
                                  each, `amount` with an ISO-4217 `currencyCode`, and a required
                                  `finalUrl` tagged and bounded exactly as above — EVERY row is its own
                                  clickable destination. Headers are de-duplicated case-insensitively;
                                  Google serves one row per header. At most 10 price extensions.

leadForms?:                     — OPTIONAL lead form (LFXV2-2665). The one extension that changes WHERE
  {businessName, headline,        THE LEAD GOES: the user's details are collected inside Google rather
   description,                   than at the registration URL, so a campaign that sets one is changing
   callToActionType,              what a conversion means for it. AT MOST ONE per campaign — Google
   callToActionDescription,       links a single lead form, so a second is refused here rather than
   privacyPolicyUrl,              created as an account-level asset and then rejected at the link.
   fields: string[],
   postSubmitHeadline?,           Required: `businessName` (≤25 runes), `headline` (≤30),
   postSubmitDescription?,        `description` (≤200), `callToActionDescription` (≤30),
   postSubmitCallToActionType?,   `callToActionType` (the button label, e.g. `SIGN_UP`),
   desiredIntent?,                `privacyPolicyUrl`, and at least one entry in `fields` — a form that
   customDisclosure?}[]           collects nothing cannot generate a lead. 1..12 `fields`, each an
                                  input-type name (`FULL_NAME`, `EMAIL`, …), de-duplicated on the type
                                  because Google renders one input per type.

                                  `privacyPolicyUrl` is required BY GOOGLE and is the one caller URL
                                  this client validates but does NOT UTM-tag: it is a link Google
                                  renders inside the form, not an ad destination, so tagging it would
                                  attribute a policy read as an ad click. It is otherwise held to the
                                  same checks as every destination — http(s) only, host required, no
                                  embedded credentials, ≤2084 bytes — and is reduced to scheme+host in
                                  `config_snapshot`.

                                  `postSubmitHeadline` (≤25 runes) and `postSubmitDescription` (≤200)
                                  are ALL-OR-NOTHING: one without the other renders a half-written
                                  thank-you screen. `postSubmitCallToActionType` stands alone — Google
                                  renders it on its own default screen too.

                                  `callToActionType`, `postSubmitCallToActionType`, `desiredIntent`
                                  (e.g. `HIGH_INTENT`) and every `fields` entry are SHAPE-checked only
                                  (`^[A-Z][A-Z0-9_]*$`), like `occasion` above. `customDisclosure`
                                  (≤200 runes) is only permitted on accounts Google has allow-listed
                                  for it, which this client cannot check — an account without the
                                  allow-list is refused at the mutate.

                                  All seven extension fields are SEARCH ONLY and are REFUSED, not
                                  ignored, on every other channel. Omitted/empty, no assets are created
                                  and the ad serves with no extensions — the pre-LFXV2-2665 behaviour.

                                  IMAGE and LOCATION extensions, and the lead form's optional
                                  BACKGROUND IMAGE, are NOT supported. Each of the first two carries
                                  bytes — as does the background image — which would give the Search
                                  create path a network fetch phase it does not have today; a location
                                  extension cannot be created through this API at all — it is derived
                                  from a Business Profile linked to the account. A lead form without a
                                  background image renders on Google's default and is servable.
adGroups?:                      — OPTIONAL multiple themed ad groups (LFXV2-2665), each with its own
  {name, cpcBid?, keywords?,      keywords and up to 3 Responsive Search Ads. SEARCH ONLY and REFUSED on
   audienceSegments?,             every other `channel`: Demand Gen creates its own single ad group,
   ads?: {headlines?,             and Performance Max has no ad groups at all. At most 20 groups, at
          descriptions?}[]}[]     most 3 ads per group.
                                  `name` is REQUIRED per group and is a THEME LABEL, not the full name:
                                  the created ad group is named `<composed campaign name> | <label>`.
                                  Names must be distinct case-insensitively — Google rejects duplicates
                                  at the mutate, by which point the earlier groups already exist.

                                  Every other key is a PER-FIELD override of the campaign-level value,
                                  and inheritance is per field rather than all-or-nothing: a group that
                                  sets only `keywords` keeps the campaign's `cpcBid`, audiences and ad
                                  copy. Omitting a key inherits; `cpcBid: 0` also inherits, since 0
                                  already means "unset" on the campaign-level field. Each override is
                                  validated by the same rules as its campaign-level counterpart
                                  (`keywords`, `audienceSegments`, `cpcBid`, `headlines`,
                                  `descriptions` above), with the group's name in the error.

                                  Duplicate ad copy across two ads in a group is NOT refused — Google
                                  accepts it; it is wasteful, not invalid.

                                  Omitted/empty, exactly ONE ad group with ONE ad is created from the
                                  campaign-level fields, byte-for-byte what this config produced before
                                  the field existed. The groups are created in the order listed, and a
                                  failure partway through leaves the groups before it in place — the
                                  error names which group of how many failed and how many were created.
demandGenCreative?:             — OPTIONAL Demand Gen ad (LFXV2-2665). DEMAND GEN ONLY, and REFUSED on
  {marketingImages?: string[],    Search — the mirror of every SEARCH ONLY entry above. Supplying it is
   squareMarketingImages?:        what turns a Demand Gen campaign from a shell into one that can serve:
     string[],                    omitted, the campaign is still created with NO AD, exactly as before
   portraitImages?: string[],     this field existed, and a human must add creative in the Google Ads UI.
   tallPortraitImages?: string[],
   logoImages?: string[],         Images are given as https URLs that THIS SERVICE fetches and uploads as
   headlines?: string[],          Google Ads image assets; Google is never handed the URL. The fetch is
   descriptions?: string[],       anonymous (no credentials are sent), follows no redirects, refuses any
   businessName?: string,         address that is not a public IP, caps each body at 5 MiB AND the sum
   callToActionText?: string}     of every image in one creative at 64 MiB, and accepts only PNG, JPEG
                                  and GIF. Every image is fetched and checked BEFORE the first budget
                                  mutate, so a bad URL cannot strand a paid campaign. The 64 MiB total
                                  is a separate refusal from the per-image cap: images that each pass
                                  5 MiB and every count and shape bound can still be refused on the sum.
                                  Fetching is sequential and bounded per image AND as a phase: it may
                                  spend at most HALF the request's remaining deadline, so a set of slow
                                  image hosts is refused here rather than leaving the campaign mutates
                                  to run out of time after something has been created.

                                  Each list has its own shape, checked against the decoded image:
                                    marketingImages          1.91:1, min 600x314
                                    squareMarketingImages    1:1,    min 300x300
                                    portraitImages           4:5,    min 480x600
                                    tallPortraitImages       9:16,   min 600x1067
                                    logoImages               1:1,    min 128x128
                                  Aspect ratios are allowed Google's documented +-1%. A URL must be
                                  https with a host, and may not repeat within its list.

                                  At most 20 marketing images COMBINED across the four marketing lists
                                  (not 20 each), and at least one `marketingImages` OR one
                                  `squareMarketingImages` — Google requires each when the other is
                                  absent, so neither alone is mandatory and the pair is. Logos are
                                  counted separately: 1-5, at least one REQUIRED.

                                  `headlines` 1-5 (<=30 weighted chars) and `descriptions` 1-5 (<=90).
                                  These are NOT the RSA counts above (3-15 / 2-4) even though the width
                                  limits coincide. `businessName` is REQUIRED, <=25 weighted chars;
                                  `callToActionText` is optional, <=30 runes. Over-long copy is
                                  REFUSED, not truncated — unlike the RSA `headlines`/`descriptions`
                                  above, because a Demand Gen ad is created from exactly what you named.

                                  The ad is created PAUSED, like every other resource this service
                                  creates. The result carries `creativeAssetIds` and `adId`; a failure
                                  after the assets upload still reports the asset ids, so a retry does
                                  not lose them. The same validation runs on the `adoptExisting` path.
performanceMaxCreative?:        — OPTIONAL Performance Max ASSET GROUP (LFXV2-2665). PERFORMANCE MAX
  {marketingImages?: string[],    ONLY, and REFUSED on every other `channel`. Performance Max has no ad
   squareMarketingImages?:        groups and no ads: this field IS its creative. Omitted, the campaign
     string[],                    is still created, with no asset group — reconcilable in the Google Ads
   portraitImages?: string[],     UI, and the same shape `adoptExisting` needs for a campaign whose asset
   logoImages?: string[],         group was built by hand.
   landscapeLogoImages?:
     string[],                    Images are given as https URLs that THIS SERVICE fetches and uploads as
   headlines?: string[],          Google Ads image assets, under exactly the rules `demandGenCreative`
   longHeadlines?: string[],      states (anonymous, no redirects, public IPs only, 5 MiB per image and
   descriptions?: string[],       64 MiB across the whole creative, PNG/JPEG/GIF only, every image
   businessName?: string,         fetched and checked BEFORE the first budget mutate). Google is never
                                  handed the URL.
   youtubeVideoIds?: string[],
   assetGroupName?: string,       Each list has its own shape, checked against the decoded image:
   path1?: string,                  marketingImages          1.91:1, min 600x314
   path2?: string}                  squareMarketingImages    1:1,    min 300x300
                                    portraitImages           4:5,    min 480x600
                                    logoImages               1:1,    min 128x128
                                    landscapeLogoImages      4:1,    min 512x128
                                  Ratios are allowed Google's documented ±1%.

                                  BOTH a `marketingImages` and a `squareMarketingImages` entry are
                                  REQUIRED — unlike Demand Gen, where either satisfies the other. At
                                  most 20 marketing images COMBINED across the three marketing shapes.
                                  `logoImages` 1-5, at least one REQUIRED; `landscapeLogoImages`
                                  optional, at most 5.

                                  The text counts are Performance Max's OWN and are NOT Demand Gen's:
                                  `headlines` 3-15 (≤30 weighted chars) against Demand Gen's 1-5,
                                  `longHeadlines` 1-5 (≤90) as a SEPARATE field type, `descriptions`
                                  2-5 (≤90) of which at least ONE must fit ≤60 weighted chars for the
                                  short slot Google renders on constrained surfaces — checked as ANY
                                  entry, not the first. `businessName` is REQUIRED, ≤25 weighted chars.
                                  Over-long copy is REFUSED, not truncated.

                                  `youtubeVideoIds` are optional, at most 5, and are IDs — a YouTube
                                  URL is refused rather than parsed, because guessing which part of a
                                  URL is the id is how the wrong video gets attached. `assetGroupName`
                                  defaults to `<eventName> - Asset Group`. `path1`/`path2` are the
                                  display-path segments rendered after the domain, ≤15 runes each;
                                  `path2` without `path1` is REFUSED rather than promoted or dropped.

                                  The asset group is created PAUSED, matching the campaign. It takes
                                  THREE mutates — assets, then the group, then the links that carry
                                  each asset's field type — because Google has no call that does more;
                                  the links use the resource names Google RETURNED, never rebuilt ones.
                                  The result carries `creativeAssetIds` and `assetGroupId`, so a
                                  failure after the assets upload does not lose them. The same
                                  validation runs on the `adoptExisting` path.
videoCreative?:                 — OPTIONAL Video RESPONSIVE VIDEO AD (LFXV2-2665). VIDEO ONLY, and
  {youtubeVideoIds?: string[],    REFUSED on every other `channel`.
   headlines?: string[],
   longHeadlines?: string[],      **THE GOOGLE ADS API CANNOT CREATE A VIDEO CAMPAIGN, so this field is
   descriptions?: string[],       reached only on the `adoptExisting` path.** Google's Video overview
   callToActions?: string[]}      says so without qualification; a `channel: "video"` CREATE is refused
                                  by `CreateVideoCampaign`'s first statement, before any request is
                                  sent, so no budget is left behind. FETCHING, reporting, adoption, the
                                  activation gate and monitoring are all unaffected — only creation is
                                  impossible. Everything described below is the validation this field
                                  gets on adoption, and the cascade it describes is retained in the
                                  code, unexported and deliberately unreachable, so that the channel
                                  can be created the day Google supports it.

                                  Omitted, an adopted campaign and its ad group carry NO AD —
                                  reconcilable in the Google Ads UI, and the shape `adoptExisting`
                                  needs for a campaign whose ad was built by hand. The closing step
                                  says NO AD in those words so an operator is never told a campaign is
                                  ready when nothing can serve.

                                  Unlike every other creative field here, this one carries NO URLs and
                                  this service fetches NOTHING. A YouTube video stays on YouTube and is
                                  referenced by its BARE id: `youtubeVideoIds` are ids, not watch URLs,
                                  and a URL is REFUSED rather than parsed — guessing which part of a URL
                                  is the id is how the wrong video gets attached. 1-5 ids, each checked
                                  against YouTube's id alphabet. Because there is no fetch phase at all,
                                  the Video cascade has no network I/O before its first budget mutate.

                                  The text counts and widths are Video's OWN and are NOT Demand Gen's or
                                  Performance Max's: `headlines` 1-5 at ≤15 WEIGHTED chars (not 30 —
                                  VIDEO_ACTION renders a short headline), `longHeadlines` 1-5 at ≤90 as
                                  a SEPARATE field type, `descriptions` 1-5 at ≤70 (not 90), and
                                  `callToActions` 0-5 at ≤10. Headlines, long headlines and descriptions
                                  are each REQUIRED when a creative is supplied; `callToActions` is
                                  optional and ABSENT means Google supplies its own default — an empty
                                  list is never sent, because that would mean "no call to action".
                                  Over-long copy is REFUSED, not truncated.

                                  The ad group is type `VIDEO_RESPONSIVE` and is created ENABLED,
                                  exactly as Demand Gen's is; the AD is created PAUSED, as is the
                                  campaign. Only Search creates its ad group paused. Nothing serves
                                  either way — a paused campaign delivers nothing whatever its children
                                  say — but a reconciler reading an ENABLED ad group under a PAUSED
                                  campaign is looking at a correctly created Video campaign, not a
                                  half-enabled one. It
                                  takes TWO mutates — the video assets, then the ad that references the
                                  resource names Google RETURNED, never rebuilt ones. The result carries
                                  `creativeAssetIds` and `adId`, so a failure after the assets are
                                  created does not lose them, and an AMBIGUOUS failure (5xx, timeout, a
                                  malformed or short 2xx, a resource name naming another account or
                                  another ad group) is reported UNCONFIRMED rather than failed, so a
                                  retry does not create a second ad. The same validation runs on the
                                  `adoptExisting` path.
displayCreative?:               — OPTIONAL Display RESPONSIVE DISPLAY AD (LFXV2-2665). DISPLAY ONLY, and
  {marketingImages?: string[],    REFUSED on every other `channel`. Omitted, the campaign and its ad
   squareMarketingImages?:        group are still created with NO AD — reconcilable in the Google Ads
     string[],                    UI, and the shape `adoptExisting` needs for a campaign whose ad was
   logoImages?: string[],         built by hand. The closing step says NO AD in those words so an
   squareLogoImages?: string[],   operator is never told a campaign is ready when nothing can serve.
   headlines?: string[],
   longHeadline?: string,         Images are given as https URLs that THIS SERVICE fetches and uploads
   descriptions?: string[],       as Google Ads image assets, under exactly the rules
   businessName?: string,         `demandGenCreative` states (anonymous, no redirects, public IPs only,
   callToActionText?: string}     5 MiB per image and 64 MiB across the whole creative, PNG/JPEG/GIF
                                  only, every image fetched and checked BEFORE the first budget
                                  mutate). Google is never handed the URL.

                                  FOUR image slots, and they are neither Demand Gen's five nor
                                  Performance Max's five — there is no portrait shape here, and
                                  `squareLogoImages` exists on neither sibling:
                                    marketingImages          1.91:1, min 600x314
                                    squareMarketingImages    1:1,    min 300x300
                                    logoImages               4:1,    min 512x128
                                    squareLogoImages         1:1,    min 128x128
                                  Ratios are allowed Google's documented ±1%.

                                  The two MARKETING arrays are RECIPROCALLY required — Google requires
                                  each when the other is absent — so either one alone satisfies the
                                  requirement and supplying neither is refused. At most 15 marketing
                                  images COMBINED across the two shapes. BOTH logo arrays are OPTIONAL,
                                  each capped at 5 independently: a ceiling with no floor, unlike
                                  Demand Gen, which refuses an ad with no logo.

                                  `longHeadline` is a SINGLE STRING, not a list — the one place this
                                  channel's shape departs from Performance Max and Video, which both
                                  take a list of long headlines. It is REQUIRED when a creative is
                                  supplied, ≤90 weighted chars. `headlines` 1-5 (≤30), `descriptions`
                                  1-5 (≤90), `businessName` REQUIRED ≤25 — all weighted chars.
                                  `callToActionText` is OPTIONAL, ≤30 runes, and ABSENT means Google
                                  supplies its own button text. Over-long copy is REFUSED, not
                                  truncated.

                                  The ad group is type `DISPLAY_STANDARD` and is created ENABLED,
                                  exactly as Demand Gen's and Video's are; the AD is created PAUSED, as
                                  is the campaign. Only Search creates its ad group paused. The campaign
                                  carries NO `advertisingChannelSubType` — the deliberate contrast with
                                  Video, which pins `VIDEO_ACTION`. It takes TWO mutates — the image
                                  assets, then the ad that references the resource names Google
                                  RETURNED, never rebuilt ones. The result carries `creativeAssetIds`
                                  and `adId`, so a failure after the assets are created does not lose
                                  them, and an AMBIGUOUS failure is reported UNCONFIRMED rather than
                                  failed, so a retry does not create a second ad. The same validation
                                  runs on the `adoptExisting` path.
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
                                  On Google the adopted campaign also takes the brief SLOT its
                                  `campaign.advertising_channel_type` maps to, and the slot is keyed on
                                  that TYPE alone — the lookup does not read
                                  `advertising_channel_sub_type`. So ANY `VIDEO` campaign fills the
                                  `video` slot, including a YouTube reach, bumper or sequence campaign
                                  this service cannot itself create, after which a later `video`
                                  dispatch on that brief finds the slot taken, and ANY `DISPLAY`
                                  campaign fills the `display` slot on the same terms. One slot per
                                  channel type is the model — `SEARCH` has sub-types too and behaves
                                  the same way — not a Video- or Display-specific gap. A channel type this service does not
                                  create (`SHOPPING`, `HOTEL`, an unrecognised future value, or an
                                  absent field) is REFUSED rather than defaulted into a slot.
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
                                  safe. Generated copy is applied by `subject`/`bodyHtml` below.
subject: string                 — OPTIONAL. Replaces the cloned draft's subject line. Unset leaves
                                  the template's own subject, which is what every campaign did
                                  before LFXV2-2775. Applied BEST-EFFORT: a failure here logs and
                                  leaves the template's subject rather than failing the dispatch,
                                  because the email is already cloned and correctly targeted.
bodyHtml: string                — OPTIONAL. Replaces the content of the cloned draft's FIRST
                                  rich-text block in the template's own layout order, leaving
                                  every other block as the template wrote it. A draft with no
                                  rich-text block at all is left untouched, and so is one
                                  with no LAYOUT: a classic (non-drag-and-drop) template
                                  records no reading order, so its blocks come back in
                                  opaque key order and the "first" is as likely the
                                  unsubscribe footer as the lede. The write requires a
                                  layout-PLACED first block for that reason; without one
                                  the draft keeps its template body. HubSpot templates
                                  may carry several (header blurb, body, footer note) and the
                                  API exposes no marker for which is "the" body; first in the
                                  template's own layout order is the closest thing to one, and
                                  it is where a template's body copy sits. Writing only ONE
                                  block keeps every other one recoverable by hand. Before this,
                                  nothing was written at all unless the draft had exactly ONE
                                  rich-text widget, so every multi-block template staged with
                                  the template's own placeholder copy instead of the campaign's.
                                  Applied BEST-EFFORT like `subject`, and BEFORE utm tagging, so
                                  the tags land on this body and survive. NOTE there is no
                                  preheader field: Marketing Emails v3 exposes no preheader
                                  property, so accepting one would report success while HubSpot
                                  ignored it.
                                  An operator edit made to the draft between staging and dispatch
                                  may be REVERTED: the write re-sends the whole content object from
                                  a snapshot read moments earlier, and HubSpot exposes no ETag or
                                  revision on the draft endpoints to condition the write on. The
                                  window is one PATCH after one GET, but it covers the ENTIRE draft
                                  rather than only the block being written.
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
heroImageUrl: string             — OPTIONAL. Hosted hero image rendered as its own module.
heroLinkUrl: string               — OPTIONAL. Link target for the hero image; ignored without
                                  `heroImageUrl`.
heroImageAlt: string             — OPTIONAL. Alt text for `heroImageUrl`. Trimmed; when blank or
                                  absent the dispatcher falls back to the generic "Event banner",
                                  so a caller should send the event's own name here to give
                                  screen-reader listeners something more specific than that.
                                  Ignored without `heroImageUrl`.
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
instagramUserId?: string        — Instagram account (IGSID) bound to the ad creative (sent as the
                                  top-level `instagram_user_id` adcreative field). Meta requires it to
                                  PUBLISH whenever an Instagram placement is used — the default
                                  `placements` enable Instagram Feed — otherwise Meta refuses ("Please
                                  add Instagram account"), even though the ad is created. PRESENCE is
                                  NOT validated locally: this service does not reject a create that
                                  omits it, so a missing pairing surfaces only as an async publish block
                                  on Meta, not a synchronous error. The FORMAT is: a supplied value must
                                  be a numeric IGSID, and a malformed one fails the dispatch job
                                  pre-create (it is otherwise only consumed at the creative call, after
                                  the campaign and ad set exist, where the failure is non-fatal and
                                  would leave a billable campaign with no publishable ad). Omit for a
                                  Facebook-only campaign. Sent only when non-empty; a blank/whitespace
                                  value is treated as absent.
dsaBeneficiary?: string         — EU Digital Services Act "advertiser" disclosure, set on the ad set
                                  (`dsa_beneficiary`). Meta requires BOTH `dsaBeneficiary` and
                                  `dsaPayor` to PUBLISH an ad set that targets a regulated location, and
                                  blocks publish ("Please add Advertiser" / "Please add Payer") until
                                  they are present. Supply BOTH or NEITHER: a ONE-SIDED pair (exactly
                                  one of the two) IS validated locally and fails the dispatch job
                                  pre-create, because it is deterministically unpublishable and knowable
                                  before any billable call. Omitting BOTH is NOT validated locally —
                                  that is the ordinary non-regulated flow; like `instagramUserId`, a
                                  disclosure missing where Meta requires one then surfaces only as an
                                  async publish block on Meta. Sent only when non-empty;
                                  blank/whitespace is treated as absent (so a whitespace-only
                                  counterpart still counts as one-sided). Omit both for non-regulated
                                  targeting.
dsaPayor?: string               — EU DSA "payer" disclosure counterpart, set on the ad set
                                  (`dsa_payor`); same rules as `dsaBeneficiary` — both are required by
                                  Meta together for regulated locations, a one-sided pair is rejected
                                  locally pre-create while both-absent is not, sent only when non-empty.
variants: AdVariant[]           — One ad per variant; at least one is required.
```

`AdVariant` (an entry in `variants`):

```
primaryText: string             — Required; non-empty; at most 125 runes
headline: string                — Required; non-empty; at most 40 runes
description?: string             — At most 30 runes
imageUrl?: string               — OPTIONAL https URL to a single image for this variant.
                                  When set, the ad renders as a SINGLE-IMAGE ad: the URL is
                                  sent as `object_story_spec.link_data.picture`, the
                                  documented by-URL field, and META fetches the image
                                  server-side — this service never fetches it. Omitted/empty
                                  yields the previous bare-link creative, so the field is
                                  purely additive. No separate upload call is made.
                                  Validated pre-create: must be absolute, https, and carry NO
                                  embedded userinfo (Meta fetches it, so credentials in the
                                  URL would be handed to Meta). A malformed value fails the
                                  platform job before any paid resource is created.
                                  The image must be reachable by Meta's fetchers; a creative
                                  Meta rejects over the image fails only THAT variant's ad
                                  (non-fatal), and is reported in the result Steps with the
                                  URL's query/fragment stripped (it may be pre-signed).
imageAssetId?: string            — OPTIONAL id of a creative asset previously uploaded against
                                  THIS brief (see POST .../briefs/{brief_id}/creative-assets).
                                  The alternative to `imageUrl`: instead of Meta fetching a
                                  URL, the service resolves the asset to its stored bytes at
                                  dispatch, POSTs them to `/act_<id>/adimages`, and attaches
                                  the returned account-scoped hash as
                                  `object_story_spec.link_data.image_hash`.
                                  Must be a valid UUID naming an asset that exists FOR THIS
                                  BRIEF — an asset belonging to another brief or project is
                                  rejected, as is an asset with no stored bytes. Every such
                                  failure fails the platform job BEFORE any paid resource is
                                  created (no campaign, no ad set, no spend) and RELEASES the
                                  dispatch claim; a bad reference never degrades to a
                                  link-only ad, because the caller asked for an image and
                                  silently creating an imageless ad would spend budget on a
                                  creative nobody approved.
                                  Resolution is bounded: variants naming the SAME asset id
                                  resolve once and share one buffer, and a config naming more
                                  than 240 MiB of DISTINCT assets is refused pre-create.
                                  MUTUALLY EXCLUSIVE with `imageUrl`. Meta documents
                                  `link_data.picture` as "Specify this field or `image_hash`
                                  but not both", so a variant supplying BOTH has no correct
                                  interpretation and is REFUSED — locally, before any
                                  upstream call is made, rather
                                  than discovered at the per-variant creative step where the
                                  campaign and ad set already exist. Supply one or neither;
                                  neither still yields the bare-link creative.
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
tweetId?: string                — An existing promotable tweet id to promote. Takes precedence
                                  over tweetText — if both are set, tweetId is promoted as-is and
                                  tweetText is ignored (recorded as a step). Omitted (and tweetText
                                  also omitted) → the manual-tweet workflow: the campaign + line
                                  item are created and the operator attaches the promoted tweet
                                  manually (the result carries a warning + the sanitized destination
                                  URL). A create that can't confirm the promoted-tweet association
                                  is reported as an UNCONFIRMED degraded outcome, not a clean
                                  success.
tweetText?: string               — Used ONLY when tweetId is empty: authors a NEW tweet carrying
                                  this text (the brief's UTM'd registration URL is appended if not
                                  already embedded — that URL keeps its own existing query string
                                  verbatim beside the UTM parameters, since it is the ad's real
                                  click destination, and keeps its #fragment for the same reason —
                                  `#register` and a hash-router route both decide where the click
                                  actually lands), then promotes it.
                                  Because that query AND fragment are PUBLISHED verbatim, this is
                                  the one path that first screens EVERY URL in the composed tweet
                                  text — the registration URL's own parameters and any link the
                                  caller put in their own copy, whether or not that link is
                                  written with an `http(s)://` scheme, since X linkifies and
                                  publishes `www.host/…?…` and bare `host.tld/…?…` alike — and
                                  refuses, pre-create, a key that matches its credential
                                  denylist. That denylist is exactly four tiers, and it is a
                                  DENYLIST, not a judgement about what looks like a secret: an
                                  exact name (`access_token`, `sessionId`, `jwt`, `password`,
                                  `signature`, `JSESSIONID`, `PHPSESSID`, …); an unambiguous
                                  fragment anywhere in the name (`token`, `secret`, `oauth`,
                                  `hmac`, `assertion`, `csrf`, `xsrf`, `saml`, …, so
                                  `secret_token`, `_csrf` and `SAMLResponse` are all refused); a
                                  credential word standing as a whole `-`/`_`/`.`-delimited
                                  component (`auth`, `sid`, `pwd`, `passwd` — so `auth_cookie`
                                  and `connect.sid` are refused while `author` and `aside` are
                                  not); and `key`, either as the name's final component
                                  (`api_key`, `auth_key`) or as a component qualified by the word
                                  in front of it (`access_key_AKIA…`, `api-key-…`). All tiers are
                                  case-insensitive and separator-insensitive. What this does NOT
                                  claim to catch: a credential parameter whose name shares no word
                                  with the lists above, and a secret carried in the URL's PATH
                                  rather than its query or fragment — an events service cannot
                                  denylist path words without refusing `/sessions/`. The same screen
                                  runs over a URL's FRAGMENT when it is written in `key=value`
                                  form, because the OAuth implicit flow delivers its bearer token
                                  after the `#` and such a URL may have no query at all; a plain
                                  section anchor (`#register`) has no key and passes. A query
                                  string or fragment
                                  that cannot be parsed is refused rather than read as having no
                                  parameters. A URL carrying embedded userinfo
                                  (`https://user:pass@host/…`) is refused whether or not it has a
                                  query, which closes the gap that the registration URL's own
                                  userinfo check never covered links pasted into caller copy. The
                                  error never renders the caller's key: it points at the parameter
                                  by the fixed VOCABULARY WORD that classified it, because a
                                  parameter NAME is free text too and
                                  `?oauth_token_<secret>=x` would otherwise reproduce the secret in
                                  a persisted, logged error. A BARE query component with no `=` may
                                  itself be the credential, names no word at all, and redacts the
                                  URL instead. Values are never rendered. Routing and
                                  attribution parameters are unaffected, `code` and `pin` included
                                  — a discount code is not a credential.
                                  The authored tweet is ALWAYS promoted-only
                                  (`nullcast=true`, sent explicitly, never relied on
                                  as X's default) — it never appears on the public timeline or to
                                  followers. Rejected pre-create if the composed text (counting any
                                  embedded `http(s)` URL at X's fixed t.co weight of 23 characters
                                  each, not
                                  raw length — schemes match case-insensitively, and punctuation
                                  around the link is counted as prose, not as part of it: trailing
                                  ASCII sentence marks and unmatched closing brackets are trimmed
                                  off the end, while `<`, `>` and CJK sentence punctuation
                                  (`。`, `、`, `！`, `？`, `，`, `：`, `；`) end the link outright,
                                  since a CJK sentence puts no space before its stop) exceeds the
                                  280-character cap, or if its raw size
                                  exceeds an 8 KiB request bound that the weighted cap — under
                                  which a scheme-ful URL costs 23 whatever its length — does not
                                  impose. A SCHEME-LESS link (`events.example/r?…`) is counted at
                                  its RAW length instead, even though X linkifies and shortens it:
                                  deciding which dotted token X actually linkifies needs
                                  twitter-text's TLD registry, and every token guessed wrong near
                                  the 280 boundary is a create refused for copy X would have
                                  accepted. The credential screen below does read scheme-less
                                  links — screening one X does not linkify costs a refusal the
                                  operator fixes by deleting a parameter, which is the cheap
                                  direction; weighting one does not. That cap is X's WEIGHTED
                                  one, not a rune count: runes outside twitter-text's weight-1
                                  ranges (CJK and beyond) cost 2 each, while an emoji presentation
                                  sequence — skin tone, ZWJ family, keycap, country flag — costs 2
                                  in total. An authoring failure is
                                  non-fatal — the campaign + line item still return, degraded — with
                                  three distinct outcomes: a definite rejection (safe to retry/author
                                  manually), an UNCONFIRMED outcome (may have published — verify in
                                  X Ads Manager and delete any stray tweet before retrying), or a
                                  clean id that is then promoted like an explicit tweetId.
asUserId?: string                — Pins which of the ad account's promotable users authors the
                                  tweet (only meaningful with tweetText). Omitted → auto-resolved
                                  via the account's promotable-users list: exactly one candidate is
                                  used, zero or several are refused (never guessed). The refusal
                                  carries the COUNT of candidates, never their user ids: the message
                                  reaches an operator through the campaign's persisted warning and
                                  steps, and the ids are visible in X Ads Manager, where whoever
                                  sets `asUserId` is already looking. The candidate list is read
                                  across every page, so a pinned user is not reported absent for
                                  sitting on page two; if the page cap is reached with results
                                  still outstanding, the lookup is refused as inconclusive rather
                                  than concluding from a truncated list; so is a FULL page that
                                  returns no next_cursor X gives a meaning to. A short page is
                                  conclusively last under X's documented rule and resolves
                                  normally.
                                  The CONNECTION wins when it declares `as_user_id`: a request
                                  naming a different handle is refused pre-create, and one naming
                                  none inherits the connection's rather than auto-resolving. The
                                  promotable-users list answers "is this handle promotable by this
                                  ad account", which on the SHARED LF system connection is true of
                                  every LF handle — so it cannot answer whether THIS project may
                                  publish as that handle, and without `as_user_id` nothing was
                                  asking. The refusal names neither the requested nor the
                                  configured id.
```

Connection prerequisites (from the X connection, not this config): the OAuth1 4-tuple (consumer
key/secret + access token/secret), plus an `account_id` AND a `funding_instrument_id` — both
REQUIRED, both ALPHANUMERIC (`^[A-Za-z0-9]+$`, e.g. `account_id` `8r7gb`), and both
pattern/length-validated (`MaxLength 64`) at connection creation. The X client requires both and
interpolates them into the account-scoped request path, so a missing/malformed value is rejected as
a 4xx at connection creation rather than surfacing as an asynchronous dispatch failure.
An OPTIONAL `as_user_id` (numeric, `^[0-9]+$`, `MaxLength 32`) declares which promotable handle
this connection's tweets are authored under. It is an authorization control rather than a routing
value: where it is set, a campaign config naming a different `asUserId` is refused pre-create and
one naming none inherits it; where it is absent, dispatch behaves exactly as before.
That refusal is scoped to requests that actually AUTHOR a tweet — `tweetId` empty and `tweetText`
non-empty, both judged AFTER trimming, so a whitespace-only `tweetId` counts as absent here
exactly as it does at the client and cannot skip the check by looking present. An explicit `tweetId` wins, so no tweet is authored, no promotable user is resolved, and
a mismatched `asUserId` is ignored along with the `tweetText` it would have signed, rather than
failing a request over a field nothing reads (the same treatment an unused, malformed `tweetText`
gets). Bootstrap holds a seeded `as_user_id` to the same 32-character bound as the HTTP contract —
it writes past Goa straight to the repository, and the shared fallback row has no second opinion
downstream.

Destination URL: the ad points at the brief's registration URL. The X client validates it before any
upstream create — it must be an absolute **http/https** URL with a real hostname and carry NO
embedded userinfo/credentials; a violation fails the dispatch job pre-create. The same userinfo
rejection now also applies to every URL found in caller-supplied `tweetText`, which the registration
URL's validator never saw. A registration URL whose query cannot be parsed is refused rather than
published: the query is parsed only to VALIDATE it, and an unreadable one cannot be screened for
credentials at all, so the dispatch fails rather than publishing a query nothing has read. A query
that parses is then copied into the destination **byte for byte** — parameter order and percent
escaping included — with the generated `utm_*` pairs appended after it, and a pre-existing `utm_*`
key dropped where it collides. Where nothing collides the query is not reassembled at all — the
original bytes are used as written, so a query's empty components (`a=1&&b=2&`) survive too. The
#fragment is screened the same way, and a BARE fragment that looks like a credential (`#access_token`
with no `=`) is refused rather than published, while section anchors such as `#register` pass. An
earlier build round-tripped the query through Go's encoder, which
sorts keys and rewrites `%20` as `+`; that broke the verbatim promise on the one URL in the flow
where byte fidelity is the point. Validation errors
redact the URL (**scheme+host only** — the path is dropped too, because a magic-link or reset
credential lives in a path segment as often as in a query) so a persisted error can't leak a
userinfo/path/query secret.
**On the `tweetText` path that protection is not sufficient by itself:** the registration URL is
embedded in the tweet that gets published, INCLUDING its own pre-existing query parameters, which
are kept verbatim because they are what routes the visitor. It is then publicly visible — in the
tweet and in X Ads Manager — so the registration URL must not carry a `?token=…`-style credential.
Redaction protects what is *persisted*; nothing can un-publish what was sent to X.

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
hubspotUrl?: string     — Deep link to this campaign's email in the HubSpot editor.
                          Email (HubSpot) channel only, populated once the portal that
                          created the draft is known — absent for every other platform and
                          for campaigns created before this field existed.
```

Per-platform errors are carried inside each `result` entry rather than in a
separate top-level array; the job's own start/finish times are available from
the job record's timestamps and are not echoed in the poll payload.

### CampaignCreateResult (future, richer per-platform result)

> Not yet emitted. Today the job result carries the minimal `PlatformResult`
> shape above (`platform`/`ok`/`campaignId`/`error`/`hubspotUrl`). Once the per-provider
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

### CampaignMonitorResponse (proposed body for the unbuilt `GET .../{provider}/metrics`)

**No such type exists in the code** — this block describes a route that was never implemented (see the warning under [Monitoring](#monitoring-insights-phase)). It is recorded as a sketch; the shipped equivalent is `BriefMetrics` (`design/brief.go`), whose shape differs.

Two differences matter to anyone who builds this. `accountTotals` is only coherent because it is scoped to ONE provider, and therefore one account in one currency: the service performs no FX conversion, and `BriefMetrics` deliberately carries no cross-channel cost total for exactly that reason. Any widening of this shape across providers would break that rule. And `BriefMetrics` never zero-fills a row it could not read — each row carries its own `status`, so a campaign that served nothing stays distinguishable from one that could not be measured. A total computed over rows without checking their status would silently understate.

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
pacingLabel: string             — underspending | normal | constrained | overspending | unknown
```

> **Note on `pacingLabel`.** This block previously listed `severe` and omitted `unknown`. Neither
> matched what the service emits: LFXV2-3314 moved pacing derivation into `internal/service/rules`
> and pinned the vocabulary in the Goa design, where the enum is
> `underspending | normal | constrained | overspending | unknown`. `severe` was never produced.
> `unknown` is load-bearing rather than a filler value — it is what a row carries when pacing
> could not be derived at all (no budget, no usable flight, a window that does not overlap the
> flight, or a campaign in its first day), and it is what stops that state being rendered as a
> confident `0%`.

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
- Demand Gen campaigns use ad group level geo targeting (not campaign level); Search and
  Performance Max use campaign level
- Performance Max has no ad groups and no ads: its creative is an asset group, created as
  assets → asset group → asset-group links, three mutates because Google has no call that
  does more
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
- **Metrics reads are NOT YET EXERCISED LIVE (LFXV2-3282) and therefore DEFAULT-OFF**: the `ReadMetrics` adapter is wired but gated on `REDDIT_METRICS_ENABLED=true` (any other value, including unset, fails closed). With the gate closed, `GET .../campaigns/{id}/metrics` answers 400 for a Reddit campaign. The `POST /ad_accounts/{account_id}/reports` request and response shapes come from Reddit's **official public OpenAPI document** (`https://ads-api.reddit.com/api/v3/openapi.json`), superseding the earlier LFXV2-2995 finding that no public documentation existed. What remains unconfirmed is behaviour the schema cannot express — whether a campaign with no activity is omitted or returned as an explicit zero row, and whether the account's attribution window shifts the numbers — plus the fact that `spend` is microcurrency in the **ad account's own billing currency**, which this client does not read. See the [internal/platform/reddit knowledge doc](knowledge/code/internal-platform-reddit.md).
- **Keywords are ad-group TARGETING, replaced as a whole on write (LFXV2-2665)**: there is no per-keyword id or status — `keywords` is a string array inside the ad group's `targeting` object, and a PATCH replaces the whole object, so `remove-keyword-targeting` writes back every dimension it read. Gated by `REDDIT_KEYWORD_TARGETING_WRITES_ENABLED` until that round trip is exercised live.

### X/Twitter Ads
- OAuth 1.0a with HMAC-SHA1 signing (not OAuth 2.0)
- 1 request/second write rate limit
- Exponential backoff retry on 429 responses **for reads only**. Retry eligibility is an explicit per-endpoint `idempotent` flag, never inferred from the HTTP method, and every one of the client's four creates (campaigns, line_items, promoted_tweets, tweet) passes `false` — they take the retry-exhausted exit on their FIRST 429. X answers a 429 at OR AFTER committing the write it throttled, so a create's repeat is not free: the find-or-create lookups run above the retry loop, `DUPLICATE_PROMOTABLE_ENTITY` does not name the line item holding the tweet, and tweet authoring has no idempotency key at all, so a retry publishes a SECOND tweet. A create's 429 therefore surfaces as an ambiguous outcome for an operator to verify, not as an automatic re-issue.
- Only "lf-events" account currently supported
- **Keywords are line-item targeting criteria, and this service creates none (LFXV2-2665)**: each keyword is its own `targeting_criteria` entity (`BROAD_`/`PHRASE_`/`EXACT_`/`UNORDERED_KEYWORD`, `operator_type` `EQ`, or `NE` for a negative). `CreateCampaign` never calls `targeting_criteria`, so keyword targeting exists only where an operator added it in X Ads Manager; `remove-keyword-targeting` deletes criteria one at a time.

### HubSpot
- UTM lookup uses a loose name match over HubSpot's default searchable properties, returned in HubSpot's own order (unspecified) — there is no relevance ranking, so the first row is not the best match
- 15-second HTTP timeout per call
- If unavailable, falls back to campaign slug for UTM
