# 2026-08-16 — LFXV2-2665 Meta single-image ad creatives

**Creation** — Meta campaigns can now carry a real single-image ad creative
instead of a link-only one, end to end: upload an image against a brief, then
reference it from an ad variant and have it attached to the created ad. This is
the first step of evolving the paid-ads builder from a paused-shell MVP toward
launch-ready campaigns; the seam is deliberately format-agnostic so video and
carousel can follow without reopening the creative/ad POST flow.

The pieces, and where each is documented:

- **Storage.** Migration `000026` adds a `creative_assets` table — an image
  subordinate to a brief (`brief_id` → `campaign_briefs(id)`), tenant-scoped by
  `project_id`, `mime_type` CHECK-constrained to PNG/JPEG, a `checksum`
  (lowercase-hex SHA-256) that with `brief_id` is the content-addressed dedupe
  key, and the `bytes` as `BYTEA`. Insert-only: no `version`, no `updated_at`.
  The bytes are stored PLAINTEXT — an ad image is not a secret, unlike the
  credential blobs. `CreativeAssetRepo.CreateAsset` gates on the parent brief in
  SQL (`WHERE EXISTS` an active same-project brief) so an absent/archived/foreign
  brief returns `ErrNotFound` without racing an archival, and its `ON CONFLICT
  (brief_id, checksum) DO UPDATE` no-op makes a re-upload return the existing row
  (preserving the first uploader). See
  `docs/knowledge/code/internal-infrastructure-postgres.md`.

- **Upload endpoint.** `POST .../briefs/{brief_id}/creative-assets`
  (`upload-creative-asset`) is synchronous and touches no ad platform: it
  DECODES the bytes to prove they are a real PNG/JPEG, refuses a declared/sniffed
  mismatch, and stores the SNIFFED type. `bytes` is Goa's `Bytes` (base64 in the
  JSON body), bounded `[1, 30 MiB]` at decode; it answers `201` with no ETag
  because the row carries no version. See `docs/knowledge/code/design.md` and
  `docs/knowledge/code/internal-service.md`.

- **Resolution at dispatch.** The Meta dispatcher's `resolveVariantAssets` turns
  each variant's `imageAssetId` into bytes BEFORE any upstream create, scoped to
  this brief via `GetAsset(projectID, briefID, assetID)`. A malformed id, an
  unbound asset store, or a cross-brief/cross-project reference fails the dispatch
  through `notCreated`, RELEASING the claim rather than stranding it after a paid
  campaign exists. See the Dispatch-adapter section of
  `docs/knowledge/code/internal-platform-meta.md`.

- **Client.** `createVariantAd` uploads the image FIRST (so a rejected image
  fails the variant with nothing created), then builds the creative via
  `objectStorySpec` — the format seam — and the ad. `uploadImage` POSTs a
  multipart FILE part to `/{accountID}/adimages`; Meta content-addresses ad
  images, so it is idempotent and needs none of the create-ambiguity/retry
  machinery, and it reuses the client's error vocabulary (pre-send → plain,
  non-2xx → `*APIError`, unparseable/hashless 2xx → `*transportError`). The
  returned `image_hash` is recorded per ad in `CampaignResult.Ads` and round-trips
  through the persisted result blob, so a future reconcile pass can tell which
  asset backs a live ad. A per-variant image/creative/ad failure stays non-fatal
  (`created_degraded`). See `docs/knowledge/code/internal-platform-meta.md`.

**Note** — Full activation of a created campaign is gated by a front-end toggle
plus double-confirmation in `lfx-self-serve`; there is no server-side confirm
flag. The client still creates everything PAUSED, unchanged. Video and carousel
formats, and full LEAD_GENERATION / instant-form parity, remain deferred under
the same LFXV2-2665 umbrella.
