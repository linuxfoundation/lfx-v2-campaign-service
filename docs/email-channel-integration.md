<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Email as an LFX Marketing Channel — Integration Design

**Status:** Proposal for sign-off. Extends the "Future: Organic & Communication
Channels" row for **Email / HubSpot** in [architecture.md](architecture.md) from
UTM-only into a full **email marketing channel**: AI **content generation** +
**audience/segment building** + **email staging** in HubSpot.

**Reference implementation:** a standalone FastAPI app (`LF-Marketing-Ops`, local
`/Users/misharautela/Email-Automation/`) already does this end-to-end. It is a
**reference for the HubSpot / Snowflake / LLM integration surface only** — this
design does **not** lift-and-shift the Python BFF. The broker architecture (D1–D7)
governs, exactly as it does for the paid ad platforms.

---

## 1. How email fits the existing architecture (no new paradigm)

Email is the same shape as an ad platform, reusing decisions already made:

| Existing decision | Email application |
|---|---|
| **D3** one typed table per provider | `hubspot_connections (email)` **already defined** in [channel-connections-schema.md](channel-connections-schema.md) — `portal_id`, `sender_email`, `sender_name`, `brand_kit`, `account_id`=list/audience id, encrypted `{ private_app_token }`. Nothing to add. |
| **D4** brief is the funnel unit, shared across channels | An email campaign is a `campaigns` row with `platform = 'hubspot'` (email) under a shared `brief_id` — one brief, many channels. |
| **Phase 1 Planning** (scrape → extract → generate copy) | The brief's AI copy generation extends to produce **email content** (subject, preheader, body sections, CTA) instead of only ad copy. |
| **Phase 2 Implementation** (orchestrator → `PlatformDispatcher` fan-out) | Email is an **email dispatcher** whose `Dispatch` does content-gen → audience build → HubSpot email stage, instead of an ad-platform create. |
| **D2** every path gated on `campaign_manager` under `/projects/{projectId}/…` | Unchanged — email endpoints nest the same way and gate the same relation. |
| **D6/D7** ETag/If-Match + app-level AES-256-GCM creds | Unchanged — the connection row already carries `version` and app-encrypted `private_app_token`. |

So the **connection layer is already done**. The new work is three capabilities:
**(A) email content generation**, **(B) audience/segment building**, **(C) email
staging**. (A) is an extension of existing copy-gen; (C) is new dispatcher actions;
**(B) is the one genuinely new concept and the only real design decision below.**

---

## 2. The three external ports (broker boundary)

The dispatcher owns these upstream calls (mirrors how the ad clients own their
platform APIs). Ports are Go interfaces so tests fake them and the machine-specific
coupling in the reference app does **not** cross into the service.

### 2a. HubSpot port (`internal/platform/hubspot`)
Single private-app bearer token per connection. Operations (from the reference app,
HubSpot REST base `https://api.hubapi.com`):

| Capability | HubSpot endpoint(s) |
|---|---|
| Read prior emails as style/structure reference | `GET /marketing/v3/emails` (filtered), `GET /marketing/v3/emails/{id}` (walk flexAreas/widgets) |
| Clone a source email | `POST /marketing/v3/emails/clone` |
| Set subject / from / preheader | `PATCH /marketing/v3/emails/{id}` |
| Set body content (DnD widgets) | `PATCH /marketing/v3/emails/{id}` |
| Set send list (+ suppressions) | `PATCH /marketing/v3/emails/{id}` (full `to` object; ILS vs legacy list routing via `GET /crm/v3/lists/{id}` processingType) |
| Validate the staged email | `GET /marketing/v3/emails/{id}` |
| CRM lists CRUD/search | `POST /crm/v3/lists/search`, `GET /crm/v3/lists/{id}`, `POST /crm/v3/lists/`, `PUT /crm/v3/lists/{id}/filter-branch` |
| Event-definition lookup (for behavioral filters) | `GET /events/v3/event-definitions` |
| Re-host images to CDN | `POST /files/v3/files` (multipart) |
| UTM campaign resolve | `GET /marketing/v3/campaigns/{guid}` |

**Do NOT port from the reference app:** the hardcoded portal-8112310 module IDs, the
baked-in LF footer/physical-address/social block, the Windows CLI path. Body/footer
templating must be driven by the connection's `brand_kit`, not hardcoded — this is a
real design item (see §5).

### 2b. Snowflake port (`internal/platform/…` read-only)
Key-pair auth, **read-only**. Source table: **`ANALYTICS.PLATINUM_LFX_ONE.event_registrations`**
(the curated/authoritative PLATINUM layer, 43 cols) — RESOLVED to PLATINUM over the
reference app's `Silver_Segment.EVENT_REGISTRATIONS` (81 raw cols). Verified 2026-07-20
that PLATINUM has the columns the audience build needs — `EVENT_NAME`, `EVENT_ID`
(plus `EVENT_START_DATE`, `EVENT_LOCATION`, `IS_PAST_EVENT`, `EVENT_URL`,
`EVENT_COUNTRY`). Used to resolve exact past-edition `EVENT_NAME` strings that become
HubSpot `BEHAVIORAL_EVENT` filter values. This is the same PLATINUM_LFX_ONE the
Marketing Insights dashboard uses (`foundation_slug`-scoped in general; this model is
`event_id`-keyed), so the broker reads one governed source. Credential custody
follows D7 (key from k8s secret).

### 2c. LLM port
Already abstracted in the reference app (`llm_gateway`, LiteLLM → Anthropic → CLI
chain). In the LFX broker this becomes the platform's sanctioned AI path (the LF
LiteLLM cluster proxy), NOT a `claude --dangerously-skip-permissions` subprocess
(that flag is a local-dev artifact and must not ship). Deterministic: temp 0, pinned
model, one canonical tool format.

---

## 3. Capability A — Email content generation

Extends the brief's existing AI copy generation. Input: the brief (event details,
program type → funnel stage, brand kit). Output stored on the brief/campaign:
`{ subject, preheader, sections[], cta }`. Reuses the 13-stage funnel mapping
(`stage_detector`) the paid side already conceptually has (program_type → strategy).
Reference-email "learn the house voice + richest structure" is an optimization, not
required for v1. **Lands now** (no dispatcher dependency) as an extension of brief
copy-gen.

---

## 4. Capability B — Audience/segment building  ← THE DESIGN DECISION

The reference app builds, per email: N inclusion lists (BEHAVIORAL_EVENT past
registrants, PAGE_VIEW web visitors, geo+master LIST_MEMBERSHIP, topic/newsletter),
a Combined Suppression list (OR of standard opt-out/GDPR/exclusion lists), and a
**Master Audience List** (OR of inclusion branches, each carrying a `NOT_IN_LIST`
suppression — HubSpot rejects AND-roots/nested-ORs, so suppression is distributed
per branch). A brand→master-list-ID table gates the geo lists.

This has **no home in the current architecture**. Three options — needs your call:

**Option B1 — Audience is a dispatcher sub-step (no new resource).**
The email dispatcher builds the audience inline during `Dispatch`, stores the
resulting `master_list_id` in the campaign's `config_snapshot`/`result`. Simplest;
matches "a channel dispatch is one atomic op." Downside: audience isn't independently
inspectable/reusable, and rebuilding is coupled to a full dispatch.

**Option B2 — Audience is a first-class resource under the brief** (recommended).
A new typed `campaign_audiences` table (brief-scoped, `version`/ETag, `platform`),
with its own `POST/GET` endpoints (`/projects/{p}/briefs/{b}/audiences`), gated on
`campaign_manager`. The email dispatcher references a built audience by id. Pros:
inspectable, reusable across sends, testable in isolation, matches D4's "typed
resource subordinate to the brief." Cons: more surface. **This best fits D3/D4.**

**Option B3 — Audience lives entirely in HubSpot; the service only orchestrates.**
The service builds the lists via the HubSpot port but persists nothing beyond the
`master_list_id` reference on the campaign. Thinnest persistence; but loses the LFX
audit/version story and makes "what audience did we send to" a HubSpot-only question.

Recommendation: **B2** — it's the only option consistent with the "typed resource,
Query-Service-indexed, ETag-concurrency, campaign_manager-gated" spine the rest of
the service uses. It also cleanly separates "build the audience" from "stage+send the
email," which the reference app already does as two phases.

---

## 5. Capability C — Email staging (dispatcher actions)

New `PlatformDispatcher` for `platform='hubspot'`: clone a source email → set
subject/from/preheader → set body content (brand-kit-driven, NOT hardcoded modules)
→ set send list (the built audience's `master_list_id` + suppressions) → validate.
Same UNCONFIRMED/reconcile discipline the ad dispatchers use (HubSpot ops are
mutating; a partial failure must be reconcilable — e.g. an email cloned but not
content-set is a recoverable partial, surfaced on the campaign `result`, not a blind
retry that clones twice).

**Blocked on PR #11** (the `PlatformDispatcher` interface) — same blocker as every ad
dispatcher. Buildable-now pieces: the HubSpot port + Snowflake port + content-gen +
(if B2) the `campaign_audiences` resource. The dispatcher wiring lands when #11 does.

---

## 6. Sequencing

1. **Now (unblocked):** HubSpot port + Snowflake port (Go clients, faked in tests);
   email content-gen extension on the brief; **the audience-building decision
   (§4)** and, if B2, the `campaign_audiences` table + endpoints.
2. **On #11 merge:** the email `PlatformDispatcher` (Capability C), wired into the
   orchestrator fan-out so a brief with `platform=hubspot` produces an email campaign.
3. **Brand-kit templating** (§2a/§5): replace the reference app's hardcoded portal
   modules/footer with a per-project `brand_kit`-driven body/footer — a prerequisite
   for multi-project email, since the reference app is single-portal-hardcoded.

## 7. Decisions (signed off 2026-07-20)
1. **Audience home: B2 — a first-class `campaign_audiences` resource. ✅ DECIDED (Misha).**
   The table stores a POINTER + provenance, never the audience contents (which stay in
   HubSpot): `id`, `project_id`, `brief_id`, `platform='hubspot'`,
   `hubspot_master_list_id` (the pointer to the real HubSpot list),
   `suppression_list_ids`, `inclusion_summary` (how it was built — past events, geo,
   etc.), `status`, `version`/etag, `created_at`/`created_by`. Endpoints
   `POST/GET /projects/{p}/briefs/{b}/audiences`, FGA-gated on `campaign_manager` like
   briefs/campaigns. Makes the audience inspectable ("what did we send to?"), reusable
   across sends, and versioned.
2. **Snowflake source: `PLATINUM_LFX_ONE.event_registrations`** (per Misha's earlier
   directive — verified it carries EVENT_NAME/EVENT_ID, the fields the audience build
   needs). Read-only, one table.
3. **`platform` value: `hubspot`** — end-to-end key consistency (path
   `connection-hubspot` → table `hubspot_connections` → `platform='hubspot'`).
4. **JIRA: new epic** "end-to-end email integrations for event marketing" (per Misha).
