# Feature Specification: Google Demand Gen single-image ad creative (uploadable)

**Feature Branch**: `feat/google-demandgen-creatives`

**Created**: 2026-08-17

**Status**: Draft

**Input**: User description: "Build the campaign service to create full-fledged, launch-ready campaigns across all platforms and all media types. Google's Search/RSA path is already launch-ready; its Demand Gen (image/video/carousel) path only creates a paused shell that a human must finish in the Google Ads UI. Close that gap the same way the Meta slice did: let the user upload an image so the Demand Gen ad is a real image ad. Single image first, but the design must scale to carousel and video. Campaigns still activate explicitly."

## Context

Google Ads is the most complete platform in this service, but only for **text media**.
The Search / Responsive Search Ad path is genuinely launch-ready: budget → campaign →
ad group → real RSA creative → keyword/audience targeting → status-toggle cascade →
metrics → discovery → adoption. Nothing is pending there.

**Demand Gen — Google's image/video/carousel channel — is a PAUSED SHELL.**
`CreateDemandGenCampaign` (`internal/platform/googleads/demandgen.go`) creates
budget → campaign → ad group and then deliberately **stops** — no ad, no assets, no geo.
Its own doc comment says so:

> *"It deliberately creates NO AD and NO KEYWORDS. That is not an omission: Demand Gen
> ads are image/video asset based, the assets are uploaded by a human in the Google Ads
> UI, and the legacy path ends the same way with 'upload images and publish in Google Ads
> UI.'"*

The campaign's closing step literally reads: *"add targeting, upload images and publish in
the Google Ads UI."* There is **no asset/image upload path anywhere** in the googleads
client. This is exactly the "paused shell MVP" gap the mission exists to close — the same
gap the Meta slice (`specs/004-meta-single-image-creative`) just closed for Meta.

This feature adds the ability for a caller to **upload an image** and have the resulting
Google **Demand Gen** ad render that image — the first launch-ready creative slice for
Google's asset-based channel. It reuses the platform-agnostic creative-asset storage and
upload endpoint the Meta slice introduced (`creative_assets` table, `CreativeAssetRepo`,
`POST /projects/{project_id}/briefs/{brief_id}/creative-assets`) unchanged; nothing new is
persisted.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Upload an image and get a Demand Gen image ad on Google (Priority: P1)

A marketer preparing a Google Demand Gen campaign uploads a single image asset for the
campaign's brief, then creates the campaign. The resulting Google Demand Gen ad displays
the uploaded image (with the existing headline / description / business name / logo /
final URL), instead of the campaign ending as a shell with no ad.

**Why this priority**: This is the MVP and the whole point of the slice — it turns Demand
Gen from a paused shell a human must finish in the UI into a real, serve-ready image ad.
Delivered alone it makes Google's image channel launch-ready end to end.

**Independent Test**: Upload one valid PNG/JPEG for a brief, create a Google Demand Gen
campaign referencing it, and confirm the created (paused) campaign carries a real Demand
Gen ad whose marketing image resolves from that upload — verifiable end-to-end against a
Google Ads test account.

**Acceptance Scenarios**:

1. **Given** an approved brief with a connected Google Ads account (customer id, and — for
   an MCC — login-customer-id), **When** the caller uploads a valid image and creates a
   Google Demand Gen campaign referencing that asset (plus the required business name and
   logo), **Then** the service creates a `PAUSED` budget → campaign → ad group → **Demand
   Gen ad** whose marketing image is the uploaded asset, and the campaign reaches status
   `created`.
2. **Given** a Demand Gen create request that references **no** image asset, **When** the
   campaign is created, **Then** behaviour is unchanged from today — a paused shell
   (budget → campaign → ad group, no ad), backward compatible.

### User Story 2 - Reject unusable images with a clear error (Priority: P2)

A marketer uploads a file that Google Ads would reject (wrong format, too large, wrong
dimensions/aspect ratio for a Demand Gen marketing image). The service rejects it at
upload time with a specific, actionable error, before any campaign is created.

**Why this priority**: Fail-fast at upload prevents the far worse failure mode — a paid
budget + campaign + ad group created upstream that then can't attach its ad, orphaning a
real campaign (the Demand Gen create commits four sequential paid mutates before the ad).

**Independent Test**: Upload an oversized file and a non-image file; assert each is
rejected with a 4xx naming the specific violation, and that no asset record is persisted.
(Reused unchanged from the Meta slice's upload validation — this feature adds only the
Google-specific dimension/aspect constraints where they differ.)

**Acceptance Scenarios**:

1. **Given** a file exceeding the max size or of an unsupported MIME type, **When** the
   caller uploads it, **Then** the request is rejected with a validation error and no asset
   is stored.

### User Story 3 - Retry never duplicates the uploaded image asset (Priority: P3)

If campaign creation is retried after an ambiguous failure, the previously-uploaded image
is reused rather than re-uploaded, so the Google Ads account does not accumulate duplicate
`Asset` resources for the same source bytes.

**Why this priority**: Consistent with this repo's existing idempotency posture (the
Demand Gen create already returns non-nil partials with reconcilable ids on every
ambiguous arm). Duplicate media is clutter, not a launch blocker, so it ranks below
happy-path correctness.

**Independent Test**: Dispatch the same brief+platform twice; assert only one image
`Asset` is created per source image (the second dispatch reuses the cached/idempotent
asset resource name).

### Edge Cases

- Image uploaded but the Google account is later found unusable at dispatch preflight →
  the create fails before any mutating call, and the stored asset remains reusable.
- The image `Asset` is created but the subsequent ad create fails after budget/campaign/
  ad group exist upstream → the campaign is retained as a reconcilable partial /
  `created_degraded` per existing Demand Gen semantics; the created asset resource name is
  persisted so a reconcile does not re-create it.
- Asset referenced by a campaign is missing/deleted at dispatch time → the dispatch fails
  with a clear "referenced asset not found" error rather than silently creating a shell
  with no ad.
- Demand Gen ad required fields (business name, logo, ≥1 marketing image) not fully
  supplied → the dispatch fails validation **before** the first paid mutate, never
  orphaning a budget for a locally-incomplete ad request.
- `ACTIVATE` requested on a Demand Gen campaign → the existing status-toggle provisioning
  gate (which today requires ≥1 keyword criterion, a Search-only concept) MUST recognise a
  fully-provisioned Demand Gen campaign (ad + assets present) as activatable instead of
  refusing it.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST let a caller reference an uploaded image asset
  (`asset_id` from the existing `POST .../briefs/{brief_id}/creative-assets` endpoint) from
  a Google Demand Gen campaign-create request. No new upload endpoint, table, or migration
  is introduced — the Meta slice's `creative_assets` storage is platform-agnostic and
  reused unchanged.
- **FR-002**: Upload-time validation (supported MIME — PNG/JPEG; maximum file size; content
  sniffing) is reused from the Meta slice unchanged. This feature MUST additionally validate,
  before any paid mutate at dispatch, Google Demand Gen's marketing-image constraints
  (minimum dimensions and the accepted aspect ratios — exact numbers pinned in `research.md`)
  and reject a non-conforming image with a specific error.
- **FR-003**: The per-platform Google config (`googleAdsConfig`) and the client's
  `CampaignInput` MUST gain an optional creative reference: a **media format** (single image
  today) plus the image `asset_id`(s), the **business name**, and the **logo** asset required
  by a Demand Gen ad. When a creative reference is present, `CreateDemandGenCampaign` MUST
  create a real Demand Gen ad on the ad group; when absent, it MUST remain today's paused
  shell (backward compatible).
- **FR-004**: At dispatch time the Google client MUST upload the referenced image bytes to
  the **resolved ad account** as a Google Ads `Asset` (`assets:mutate` with an `ImageAsset`),
  obtain the account-scoped asset resource name, and reference it from the Demand Gen ad's
  marketing image. (A Google image `Asset` is per-customer, so the upload cannot happen at
  asset-upload time when the account is not yet known — mirrors Meta's per-ad-account
  `image_hash`.)
- **FR-005**: The `assets:mutate` upload MUST be idempotent per (customer + content
  checksum): a repeat dispatch of the same source image MUST reuse the existing asset
  resource name rather than creating a duplicate `Asset`. The resolved resource name MUST be
  persisted (in the campaign `result` JSONB, alongside the existing Demand Gen ids) so a
  reconcile/retry reuses it. (Exact Google-side dedupe behaviour for identical image bytes
  is pinned in `research.md`; the per-dispatch cache is the guaranteed layer.)
- **FR-006**: The Demand Gen ad-construction path MUST be built behind a **format
  abstraction** (a creative/ad builder keyed by media format) so CAROUSEL and VIDEO Demand
  Gen ad formats can be added later without rewriting the single-image path. Single image is
  the only format implemented in this feature.
- **FR-007**: The Demand Gen create MUST preserve its existing **partial-result / reconcile
  contract**: every step that may have committed upstream (budget, campaign, ad group, and
  now the image asset and the ad) returns a NON-NIL result carrying what is known so far, so
  the orchestrator can distinguish "nothing created" from "something exists and needs
  reconciling". A create that fails after the ad group exists MUST NOT release the claim.
- **FR-008**: All Demand Gen ad + creative input (business name present, logo asset
  resolvable, ≥1 marketing image resolvable and dimension-valid, final URL well-formed and
  UTM-tagged) MUST be validated **before the first (budget) paid mutate**, mirroring the
  Search path's `precomputeAdGroupAdInputs` ordering, so a purely-local input error never
  orphans a real budget/campaign/ad group.
- **FR-009**: Campaign activation MUST remain a separate, explicit operation. A Demand Gen
  campaign MUST be created `PAUSED` and only go live through the existing
  `toggle-campaign-status` endpoint. The status-toggle **ACTIVATE provisioning gate** MUST be
  made channel-aware: for a Demand Gen campaign, "provisioned" means a Demand Gen ad with its
  required assets exists (NOT the Search-only ≥1-keyword-criterion condition), so a
  legitimately-provisioned Demand Gen campaign is activatable and an unprovisioned one is
  still refused with the existing `ErrCampaignNotProvisioned` (409). This feature MUST NOT
  auto-activate.
- **FR-010**: The final URL for the Demand Gen ad MUST be built and UTM-tagged by the
  existing `buildAdFinalURL` helper (`utm_source=google`, `utm_medium=cpc`, campaign/content
  from the brief), reusing the Search path's URL discipline (scheme/host/userinfo/query
  validation + redaction on error) rather than a second URL builder.
- **FR-011**: The knowledge base MUST be updated: the googleads concept file
  (`docs/knowledge/code/internal-platform-googleads.md`) and its `index.md` bullet
  (verbatim), the Google section of `docs/api-catalog.md`, and a new
  `docs/knowledge/log/2026-08-17-*.md` entry; `go run ./cmd/okfvalidate ./docs/knowledge`
  MUST pass.

### Key Entities *(include if feature involves data)*

- **CreativeAsset (reused, unchanged)**: The uploaded image belonging to a project/brief —
  `asset_id`, `project_id`, `brief_id`, `mime_type`, `byte_size`, `checksum`, bytes, audit
  fields. Platform-agnostic source media introduced by the Meta slice; this feature adds no
  columns. It is **not** a Google `Asset` resource name (that is customer-scoped and resolved
  at dispatch).
- **Google `Asset` (image) — upstream**: The Google Ads `ImageAsset` created at dispatch via
  `assets:mutate`; referenced by resource name from the Demand Gen ad's marketing image.
- **CampaignInput (Google, extended)**: Gains an optional creative reference — media format
  (single image), image `asset_id`(s), business name, and logo asset — consumed by
  `CreateDemandGenCampaign`.
- **CampaignResult (extended usage)**: Gains a record of the resolved per-customer image
  `Asset` resource name(s) and the created ad id, for idempotent reuse on retry and for
  reconcile.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A Demand Gen campaign created with an image reference produces a Google Demand
  Gen ad whose marketing image resolves from the uploaded asset (verified on a Google Ads
  test account) in 100% of valid-input cases.
- **SC-002**: A Demand Gen campaign created with **no** image reference is behaviourally
  equivalent to today's paused shell (zero regression on the existing path).
- **SC-003**: Invalid images (unsupported type, oversize, under-dimension / bad aspect) are
  rejected with a specific error before any paid mutate in 100% of cases, and never orphan an
  upstream budget/campaign/ad group.
- **SC-004**: Re-dispatching the same brief creates each source image `Asset` in a given
  customer at most once (no duplicate `Asset` resources).
- **SC-005**: The feature is structured so a follow-up carousel or video Demand Gen format
  adds a new builder without modifying the single-image path (demonstrated by the abstraction
  boundary in review).
- **SC-006**: A fully-provisioned Demand Gen campaign is activatable through
  `toggle-campaign-status`, and an unprovisioned one is still refused (409) — the gate no
  longer wrongly refuses a keyword-less-but-complete Demand Gen campaign.

## Assumptions

- The target project already has a connected Google Ads account with a valid customer id
  (and login-customer-id for MCC access); OAuth2/developer-token handling is reused unchanged.
- The Meta slice (`feat/meta-image-video-creatives`, `creative_assets` + upload endpoint) is
  the base this branch stacks on; that storage is platform-agnostic and reused as-is. When
  the Meta slice merges to main, this branch rebases onto main.
- Demand Gen requires a **business name** and a **logo** asset in addition to marketing
  images; the caller/brief supplies these. Exact required-asset counts and image constraints
  are pinned in `research.md` from the Google Ads v23 docs before coding.
- The double-confirmation activation UX is a purely front-end concern in `lfx-self-serve`;
  this backend's obligation is a safe, explicit, channel-aware activation gate — no
  server-side confirmation flag is added.
- The shared TypeScript contract (`@lfx-one/shared`) may need a coordinated additive field
  for the Google creative reference; tracked as a dependency, assumed backward compatible.
- A tracking ticket (LFXV2-XXXX) will be filed; umbrella is LFXV2-2665 (launch-ready
  creatives), and the Demand Gen port this builds on is LFXV2-3257. The branch is named by
  feature slug in the interim.

## Out of Scope

- Carousel and video Demand Gen creative formats (enabled by the FR-006 abstraction;
  implemented later).
- Google Search / RSA changes — that path is already launch-ready and unchanged here.
- Geo targeting for Demand Gen (the code flags this as its own change applying to BOTH
  channels; tracked separately, not a media-type item).
- Audience targeting beyond what the Demand Gen ad group already carries.
- Auto-activation and the front-end toggle + confirmation UI (`lfx-self-serve`).
