# Feature Specification: Meta single-image ad creative (uploadable)

**Feature Branch**: `feat/meta-image-video-creatives`

**Created**: 2026-08-16

**Status**: Draft

**Input**: User description: "Build the campaign service to create full-fledged campaigns. Start with Meta: let the user upload an image so the ad is a real image ad, not text+link only. Single image first, but the design must scale to video and carousel. Campaigns still activate explicitly (a user-confirmed toggle), never automatically."

## Context

Today the Meta adapter creates a complete Campaign → Ad Set → Ad(s) hierarchy in
`PAUSED` state, but every ad creative is **link/text only**: `object_story_spec.link_data`
carries a message, headline, description, and a `LEARN_MORE` button pointing at the
registration URL (`internal/platform/meta/client.go:3041-3052`). No image, video, or
carousel is ever attached; `/act_{id}/adimages` and `/act_{id}/advideos` are never called.
This feature adds the ability for a caller to **upload an image** and have the resulting
Meta ad render that image — the first step toward creative-complete, launch-ready campaigns.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Upload an image and get an image ad on Meta (Priority: P1)

A marketer preparing a Meta campaign uploads a single image asset for the campaign's
brief, then creates the campaign. The resulting Meta ad displays the uploaded image
(with the existing headline / primary text / CTA), instead of a bare link card.

**Why this priority**: This is the MVP and the whole point of the slice — it turns
Meta ads from text-only into real image ads, which is the capability gap a marketer
feels first. Delivered alone it is already useful.

**Independent Test**: Upload one valid PNG/JPEG for a brief, create a Meta campaign
referencing it, and confirm the created (paused) ad's creative carries an `image_hash`
resolved from that upload — verifiable end-to-end against a Meta test ad account.

**Acceptance Scenarios**:

1. **Given** an approved brief with a connected Meta account (page_id + ad account),
   **When** the caller uploads a valid image and creates a Meta campaign referencing
   that asset, **Then** the service creates a `PAUSED` campaign/ad set/ad whose creative
   is a single-image `link_data` with a valid `image_hash`, and the campaign reaches
   status `created`.
2. **Given** a campaign create request that references **no** image asset,
   **When** the campaign is created, **Then** behaviour is unchanged from today — a
   link/text-only creative is produced (backward compatible).

### User Story 2 - Reject unusable images with a clear error (Priority: P2)

A marketer uploads a file that Meta would reject (wrong format, too large, wrong
dimensions/aspect ratio). The service rejects it at upload time with a specific,
actionable error, before any campaign is created.

**Why this priority**: Fail-fast at upload prevents the far worse failure mode —
a paid campaign object created upstream that then can't attach its creative, leaving
an orphaned/`created_degraded` campaign.

**Independent Test**: Upload an oversized file and a non-image file; assert each is
rejected with a 4xx and a message naming the specific violation, and that no asset
record is persisted.

**Acceptance Scenarios**:

1. **Given** a file exceeding the max size or of an unsupported MIME type, **When**
   the caller uploads it, **Then** the request is rejected with a validation error and
   no asset is stored.

### User Story 3 - Retry never duplicates the uploaded image (Priority: P3)

If campaign creation is retried after an ambiguous failure, the previously-uploaded
image is reused rather than re-uploaded, so the Meta ad account does not accumulate
duplicate `adimages` for the same source bytes.

**Why this priority**: Consistent with this repo's existing single-flight / idempotency
posture (`created_degraded`, retained claims). Duplicate media is money-adjacent clutter,
not a launch blocker, so it ranks below correctness of the happy path.

**Independent Test**: Dispatch the same brief+platform twice; assert only one
`/adimages` upload occurs per source image (the second dispatch reuses the cached hash).

### Edge Cases

- Image uploaded but the Meta account is later found inactive at dispatch preflight →
  the campaign create fails before any mutating call (existing preflight behaviour), and
  the stored asset remains reusable for a later attempt.
- `/adimages` upload succeeds but the subsequent ad/creative create fails → the campaign
  is retained as `created_degraded` per existing dispatch semantics; the uploaded hash is
  persisted so a reconcile does not re-upload.
- Asset referenced by a campaign is missing/deleted at dispatch time → the dispatch fails
  with a clear "referenced asset not found" error rather than silently falling back to a
  link-only ad.
- Multiple variants referencing the same image → the image is uploaded once and the same
  `image_hash` is reused across variants.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST accept an uploaded image asset scoped to a brief and
  return an opaque `asset_id` the caller can later reference from a campaign-create request.
- **FR-002**: The service MUST validate an uploaded image at upload time (supported MIME
  types — at minimum PNG and JPEG; maximum file size; and Meta's minimum dimension / aspect
  constraints for single-image feed ads) and reject violations with a specific 4xx error
  before persisting anything.
- **FR-003**: The service MUST persist the uploaded image bytes and metadata (MIME type,
  byte size, content checksum, owning project/brief) in **PostgreSQL as a `bytea` column**
  (single-datastore model — no external object storage), durably enough to survive the async
  gap between upload and campaign dispatch.
- **FR-004**: The per-variant Meta config (`metaConfig.variants[]`) MUST gain an optional
  image reference (the `asset_id` from FR-001). When present, the Meta creative MUST be built
  as a single-image `link_data` carrying the resolved `image_hash`; when absent, the creative
  MUST remain the current link/text-only form (backward compatible).
- **FR-005**: At dispatch time the Meta client MUST upload the referenced image bytes to the
  **resolved ad account** via `POST /act_{id}/adimages`, obtain the account-scoped `image_hash`,
  and use it in `object_story_spec.link_data.image_hash`. (Meta `image_hash` is per-ad-account,
  so the upload cannot happen at asset-upload time when the account is not yet known.)
- **FR-006**: The Meta client MUST make the `/adimages` upload idempotent per (ad account +
  content checksum): a repeat dispatch of the same source image MUST reuse the existing
  `image_hash` rather than creating a duplicate ad image. The resolved hash MUST be persisted
  (in the campaign `result` JSONB) so a reconcile/retry reuses it.
- **FR-007**: The creative-construction path MUST be refactored behind a format abstraction
  (e.g. a creative-spec builder keyed by creative type) so that VIDEO and CAROUSEL formats can
  be added later without rewriting the single-image path. Single image is the only format
  implemented in this feature.
- **FR-008**: Campaign activation MUST remain a separate, explicit operation. A single-image
  campaign MUST be created `PAUSED` and only go live through the existing
  `toggle-campaign-status` endpoint (ACTIVE↔PAUSED), which MUST continue to enforce the
  zero-ads / not-servable guard. This feature MUST NOT auto-activate.
- **FR-009**: If uploading the image or building the image creative fails after the campaign
  or ad set already exists upstream, the campaign MUST be retained as `created_degraded`
  (existing semantics), never left silently as a link-only ad presented as success.
- **FR-010**: The knowledge base MUST be updated with the change: the Meta concept file
  (`docs/knowledge/code/internal-platform-meta.md`) and its `index.md` bullet (verbatim), the
  Meta section of `docs/api-catalog.md`, and a new `docs/knowledge/log/2026-08-16-*.md` entry;
  `go run ./cmd/okfvalidate ./docs/knowledge` MUST pass.

### Key Entities *(include if feature involves data)*

- **CreativeAsset**: An uploaded image belonging to a project/brief. Attributes: `asset_id`,
  `project_id`, `brief_id`, `mime_type`, `byte_size`, `checksum`, the stored bytes, and audit
  fields. It is source media; it is **not** yet a Meta `image_hash` (that is account-scoped and
  resolved at dispatch).
- **AdVariant (Meta, extended)**: The existing text creative (`primaryText`, `headline`,
  `description`) plus an optional `imageAssetId` reference to a CreativeAsset.
- **Campaign.result (extended usage)**: Gains a record of the resolved per-account `image_hash`
  for idempotent reuse on retry.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A campaign created with an image reference produces a Meta ad whose creative
  carries a valid single-image `image_hash` (verified on a Meta test ad account) in 100% of
  valid-input cases.
- **SC-002**: A campaign created with **no** image reference is byte-for-byte equivalent in
  behaviour to today's link/text-only creative (zero regression on the existing path).
- **SC-003**: Invalid uploads (unsupported type, oversize, under-dimension) are rejected at
  upload time with a specific error in 100% of cases, and never result in an upstream campaign.
- **SC-004**: Re-dispatching the same brief uploads each source image to a given ad account at
  most once (no duplicate `adimages`).
- **SC-005**: The feature is structured so a follow-up video or carousel format adds a new
  creative-spec builder without modifying the single-image path (demonstrated by the abstraction
  boundary in review).

## Assumptions

- The target project already has a connected Meta account with a valid `page_id` and ad
  account id; connection/credential handling (AES-256-GCM) is reused unchanged.
- The double-confirmation activation UX (a toggle switch followed by a confirmation) is a
  **purely front-end concern in `lfx-self-serve`**, not this backend. This backend's obligation
  is a safe, explicit activation endpoint, which already exists — no server-side confirmation
  gate (no `confirm=true` flag) is added here.
- The shared TypeScript contract (`@lfx-one/shared` `campaign.constants.ts`) that the Go client
  mirrors may need a coordinated field addition for the image reference; that coordination is a
  dependency to track, assumed additive and backward compatible.
- A tracking ticket (LFXV2-XXXX) will be filed for this feature; the branch is named by feature
  slug in the interim.

## Out of Scope

- Video and carousel creative formats (enabled by the FR-007 abstraction; implemented later).
- Existing-post promotion (`object_story_id`) and collection ads.
- Real audience targeting beyond the current geo-country + placement targeting (interests,
  custom audiences, lookalikes).
- Meta instant lead forms / `LEAD_GENERATION` parity (deferred; not yet ticketed).
- Auto-activation and the front-end toggle + confirmation UI (`lfx-self-serve`).
