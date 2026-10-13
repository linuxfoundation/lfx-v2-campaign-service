# 2026-10-13 — LinkedIn dark posts can carry a stored creative image

**Update** — a LinkedIn variant can now name a stored creative asset
(`linkedInConfig.variants[].imageAssetId`). The image is uploaded through LinkedIn's Images API
and becomes the dark post's article thumbnail, the field `imageUrn` already filled. It joins Meta,
Microsoft, Reddit and X as a consumer of stored assets.

- Before anything is created, each asset is resolved within the brief and refused on its
  `byte_size` above 5,000,000 bytes (LinkedIn's "5 MB"). The distinct assets are charged once to
  the shared `AssetReserver`, then loaded and checked:
  PNG/JPEG and the single image ad bounds for horizontal, square and vertical images. Every
  failure releases the claim.
- After the campaign is ensured and before the first dark post, each distinct asset is uploaded
  once: initializeUpload with the organization as owner, then the PUT (both never retried), then
  a poll until AVAILABLE within one shared 45 s wait. The PUT goes only to https
  `linkedin.com` hosts, because it carries the bearer token. The URNs are recorded in the
  result as `imageUrns`. No migration.
- If an image fails, no dark post is created. The campaign is `created_degraded` with zero
  creatives, the claim is retained so nothing re-uploads, and a step reads "NOT uploaded" or
  "UNCONFIRMED". Upstream text is never recorded.
- Scope: the Images API needs nothing beyond what the dark post already needs
  (`w_organization_social` plus a page role). The live token's missing `w_organization_social`
  (2026-08-14) still blocks dark posts, with or without an image.
- Docs: `upload-creative-asset` description, `docs/api-catalog.md`, the new
  [LinkedIn Creative Image Assets](../architecture/linkedin-creative-assets.md) concept, and
  `internal/platform/linkedin`.
- PR #315 review fixes:
  - **Shared wait.** One context now bounds the whole availability phase, covering every status
    GET and every sleep. Before, the deadline was checked only between polls, so a slow GET
    (each has its own 30 s timeout) or an AVAILABLE reply could run past it.
  - **Status values.** Only documented image statuses are reported; anything else reads
    `unknown`, so no upstream string reaches a step.
  - **Pre-send failures.** These are now a typed, redacted error. A status GET retries one at
    the next tick; on initializeUpload or the PUT it stays definite.
  - **Shared reserver.** `bindCreativeAssetSharing` now re-points the LinkedIn dispatcher at
    the container's shared `AssetReserver`, as it already did for Meta and Microsoft. A test
    checks that it is the same reserver instance.
