---
type: "Architecture Doc"
title: "LinkedIn Creative Image Assets"
description: "How a LinkedIn dispatch uses stored creative assets: linkedInConfig.variants[].imageAssetId names an image of the brief, validated against LinkedIn's single image ad limits before anything is created, then uploaded once per asset through the Images API after the campaign exists, polled until AVAILABLE and used as the dark post's article thumbnail, with the URNs recorded and a failure degrading the campaign as NOT uploaded or UNCONFIRMED."
resource: "internal/dispatch/linkedin_creative.go"
---

# LinkedIn Creative Image Assets

`upload-creative-asset` stores a PNG/JPEG against a brief. Meta, Microsoft Advertising, Reddit,
X and LinkedIn consume stored assets; this page is the LinkedIn part (LFXV2-2665). Code:
`internal/dispatch/linkedin_creative.go` (resolve + validate) and
`internal/platform/linkedin/images.go` (validation + the Images API calls, run from
`CreateCampaign`).

## Where the image goes

Each LinkedIn variant is a dark post (`feedDistribution: NONE`) with an ARTICLE content block,
wrapped in a DRAFT creative. The article's `thumbnail` has always been fillable from
`variants[].imageUrn`. A stored asset fills the same field, so the ad format does not change.
The [Posts API](https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/posts-api)
documents this path: "To create an article post with thumbnail image, please use Images API to
upload thumbnail image and use the ImageUrn on thumbnail field."

## Config

`linkedInConfig.variants[].imageAssetId` (optional, per variant), mutually exclusive with
`imageUrn`. Variants naming the same asset share one upload. See `docs/api-catalog.md` §
LinkedInConfig variant images.

## Before anything is created

`resolveLinkedInImages` runs before `CreateCampaign`. Every failure is a pre-create error, so
the claim is released and nothing exists upstream:

1. Each id must parse as a uuid and is canonicalized into the config (the snapshot never stores
   a caller spelling). A variant with both `imageUrn` and `imageAssetId` is refused.
2. `GetAssetSize` is scoped to the brief's project and id. An asset of another brief or project
   is "does not exist for this brief".
3. Anything over `linkedin.MaxImageBytes` (5,000,000) is refused on its `byte_size`, before the
   blob is loaded. LinkedIn's [single image ad spec](https://www.linkedin.com/help/lms/answer/a426534)
   says "5 MB"; the decimal reading is used so nothing LinkedIn could count as larger is sent.
4. The total of the distinct assets is charged ONCE against the shared `AssetReserver`. On
   both live paths `bindCreativeAssetSharing` re-points the LinkedIn dispatcher at the
   container's ONE reserver, the one the Meta, Microsoft and X dispatches and the signed-asset
   route also charge. `registerDispatchers`' own reserver would be a second budget. A container
   test pins the identity, not just "a reserver is set". The reservation is held until the
   dispatch returns and released by the resolver itself on any failure or panic.
5. `GetAsset` loads the bytes; empty bytes are refused rather than creating an image-less ad.
6. `linkedin.ValidateImage` checks PNG or JPEG, header format matching the stored MIME type,
   fewer than 36,152,320 pixels (Images API), and the spec's per-orientation bounds: horizontal
   640x360..7680x4320, square 360x360..4320x4320, vertical 360x640..2430x4320. Only the header
   is decoded. `CreateCampaign` re-runs the same check, so the client refuses a bad image before
   any POST however it is called.

## The upload

[Images API](https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/images-api),
run after the campaign is ensured and before the first dark post:

1. `POST /rest/images?action=initializeUpload` with `{"initializeUploadRequest":{"owner":
   "urn:li:organization:<org>"}}` (the dark post's author). Each call mints a new URN, so it is
   sent once and never retried, 429 included.
2. `PUT <uploadUrl>` with the bytes, the image MIME type and the bearer token (the image upload
   "requires a valid OAuth token in the 'Authorization' header"). Never retried. Because the
   token travels with it, the URL must be https on `linkedin.com` or a subdomain; any other host
   is refused and nothing is sent. The signed URL never appears in an error or step.
3. `GET /rest/images/{urn}` until `AVAILABLE`, at a 2 s interval within ONE 45 s wait shared by
   all images (the whole dispatch has a 2-minute budget). The Images API has no synchronous
   upload, and a post created before its image is processed is not shown to members.
   ONE context carries the wait, and every status GET and every sleep runs under it, so a slow
   reply cannot carry the phase past it. An AVAILABLE reply for an earlier image cannot either.
   `PROCESSING_FAILED` or a non-429 4xx stops at once. A 429, a 5xx, a transport failure or a
   pre-send dial failure (DNS, connection refused) is retried at the next tick. A pre-send
   failure on initializeUpload or the PUT stays definite, because nothing was sent. Only the
   documented statuses (`AVAILABLE`, `PROCESSING`, `PROCESSING_FAILED`, `WAITING_UPLOAD`) are
   ever reported. Any other value reads `unknown`, so an unexpected upstream string never reaches
   a step.

**Scopes.** `/rest/images` accepts `rw_ads`, `w_member_social`, `w_organization_social` or
`w_power_creators`. An organization-owned image needs ADMIN or DSC permission on that page. The
dark post already needs `w_organization_social` and that role, so the upload adds no new
credential requirement. The 2026-08-14 live run found that token lacking
`w_organization_social` (the dark post answered 403). That blocks dark posts with or without an
image, and this change does not fix it.

## Upload at most once

- Within a dispatch, each distinct asset is uploaded once and its URN is reused by every
  variant that names it.
- Across dispatches, the upload runs only after the campaign is ensured, so every exit from that
  point returns a non-nil partial result. The dispatcher retains the claim on a partial result
  and the orchestrator never re-dispatches a retained row, so no later dispatch can mint a
  second URN for the same campaign. No table or migration is needed. LinkedIn has no
  content-addressed lookup that would let a re-run find an earlier image anyway.
- The URNs are recorded on the result as `imageUrns` (asset id -> URN), including one that was
  minted and then failed, so the result names every image the run created.

## Outcomes

If an image fails, no dark post or creative is created. The campaign exists (PAUSED, zero
creatives) and is `created_degraded`, the claim is retained, and a Steps entry says which:

- `Image for asset … NOT uploaded: …` is definite: a 4xx on initializeUpload or the PUT, an
  untrusted upload host, or an invalid image.
- `Image for asset … UNCONFIRMED …` covers a 5xx, a mutating 429 or 401, a transport failure,
  or an unreadable 2xx on initializeUpload or the PUT. An image may exist; it is unreferenced and
  serves nothing.
- `Images NOT used …` covers a poll timeout, `PROCESSING_FAILED` or a definite poll error. Only
  GETs were sent, so this is always definite.

Step and error text is the client's status-only vocabulary (`LinkedIn API POST
images?action=initializeUpload -> 403`). No upstream response body appears in it.
