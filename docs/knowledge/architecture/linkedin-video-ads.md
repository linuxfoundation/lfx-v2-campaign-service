---
type: "Architecture Doc"
title: "LinkedIn Video Sponsored Content"
description: "How a LinkedIn dispatch creates video Sponsored Content behind LINKEDIN_ADS_VIDEO_ENABLED: linkedInConfig.variants[].videoAssetId names a stored MP4, every variant must carry one, the campaign is created SINGLE_VIDEO and named Video, each distinct video is uploaded once through the Videos API (initializeUpload, part PUTs, finalizeUpload, AVAILABLE poll) after the campaign exists, and each dark post carries it as content.media with the registration URL as landing page."
resource: "internal/platform/linkedin/videos.go"
---

# LinkedIn Video Sponsored Content

LF runs video Sponsored Content ("Motion") on LinkedIn. Since #586 a LinkedIn create can make
one, alongside the single-image/article ads described in
[LinkedIn Creative Image Assets](linkedin-creative-assets.md). Code:
`internal/service/creative_asset_video.go` (MP4 upload check),
`internal/dispatch/linkedin_creative.go` (gate, request shape, resolve + validate) and
`internal/platform/linkedin/videos.go` (validation, the Videos API calls and the video post, run
from `CreateCampaign`).

## Request shape (opt-in)

```jsonc
"linkedInConfig": {
  "variants": [
    { "introText": "...", "headline": "...", "videoAssetId": "<creative asset uuid>" },
    { "introText": "...", "headline": "...", "videoAssetId": "<same or another uuid>" }
  ]
  // budget, dates, geo, targeting, bidding: unchanged
}
```

- `videoAssetId` names a creative asset uploaded to THIS brief with
  `content_type: "video/mp4"` (`upload-creative-asset`).
- **All or none.** A video campaign has format `SINGLE_VIDEO`, which takes only video creatives
  (Campaign Formats: "SINGLE_VIDEO can only have creatives with SPONSORED_VIDEO type"), so every
  variant carries a `videoAssetId` or none does. A video variant carries no `imageUrn` or
  `imageAssetId`.
- A config with no `videoAssetId` is byte-for-byte the old create: no `format` field on the
  campaign (LinkedIn sets it from the first creative), `Static` in the name, article posts.
- `headline` is the video's title; `introText` the commentary.

## Gate and pre-flight

`LINKEDIN_ADS_VIDEO_ENABLED` (chart default `"false"`; only exactly `"true"` enables). The
Videos API upload and the video post have not been exercised against a live account.
`checkLinkedInVideoVariants` runs in `PreflightCreate` (synchronous 400,
`ErrCreateConfigInvalid`) and again in `Dispatch`: flag off, a non-uuid id, a video with an image,
or a video/non-video mix. Dispatch then resolves each distinct asset scoped to the brief under the
same single `AssetReserver` reservation as images (one reservation for both kinds, so a dispatch
never holds part of the budget while waiting for the rest), refusing before any request: an asset
that does not exist for this brief, outside 75 KiB..500 MB on its recorded size, not
`video/mp4` (an image named as a video, or a video named as an image), or failing
`linkedin.ValidateVideo`. Every image slot on every platform (Meta, Microsoft, X, Google,
LinkedIn images) refuses a `video/mp4` asset at dispatch; Reddit's signed URL already served
PNG/JPEG only.

## Wire sequence

All after the campaign group and campaign exist (`createSponsoredCampaign` sends
`"format": "SINGLE_VIDEO"`; the name's Ad Format segment is `Video`, so a video and an image
campaign of one event never share a name and `findCampaignByNameInGroup` never binds one to the
other). Per distinct video
([Videos API](https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/videos-api)):

1. `POST /rest/videos?action=initializeUpload`
   `{"initializeUploadRequest":{"owner":"urn:li:organization:<org>","fileSizeBytes":N,"uploadCaptions":false,"uploadThumbnail":false}}`.
2. The returned `uploadInstructions` are checked BEFORE any byte is sent: non-empty, contiguous
   from byte 0, ending on byte N-1, every `uploadUrl` a LinkedIn host (`trustedUploadURL`, the
   image path's rule: the bearer token travels with the PUT). Then each part
   `PUT <uploadUrl>` with exactly `[firstByte, lastByte]` as `application/octet-stream`, keeping
   its `ETag`.
3. `POST /rest/videos?action=finalizeUpload`
   `{"finalizeUploadRequest":{"video":urn,"uploadToken":t,"uploadedPartIds":[ETag...]}}`, in
   instruction order.
4. `GET /rest/videos/{urn}` until `AVAILABLE`, sharing `awaitMediaAvailable` with images: one
   wait for all videos (default 60 s, `WithVideoPolling`), transient poll failures retried,
   `PROCESSING_FAILED` or a definite 4xx stops at once.

Then per variant `POST /rest/posts` — the article post's dark envelope (`feedDistribution: NONE`,
`adContext.dscAdAccount`) with `"content":{"media":{"title":headline,"id":urn}}`,
`"contentLandingPage": <UTM-tagged registration URL>` and `"contentCallToActionLabel":
"REGISTER"` — and the DRAFT creative referencing the post, exactly as for images.

## Idempotency and failures

The image path's construction, unchanged: no call in the upload is retried (each
initializeUpload mints a new URN), and the upload runs after the campaign is ensured and before
the first dark post, so every exit from the first upload call on returns a non-nil partial; the
dispatcher retains the claim and the orchestrator never re-dispatches a retained row, so a retry
can never upload a video twice or post it twice. Every minted URN is recorded in the result's
`videoUrns` (asset id -> URN). A failure leaves the PAUSED campaign `created_degraded` with zero
creatives and a Step saying `NOT uploaded` (definite: a refused initializeUpload or part, a bad
upload plan, a part without an ETag) or `UNCONFIRMED` (an ambiguous initializeUpload or finalize),
or `Videos NOT used` (processing failed or not AVAILABLE in time). Signed upload URLs never reach
a Step, a log line or an error (each part is labelled `video upload URL (part i of n)`).

## Limits and timing

LinkedIn video ads accept MP4, 75 KB-500 MB, 3 s-30 min (help a424737); `MinVideoBytes` takes the
binary 75 KiB and `MaxVideoBytes` the decimal 500 MB so neither bound admits a file LinkedIn could
count as out of range. The creative-asset store caps a file at 30 MiB (`000029`), so that is the
practical ceiling. Codec, duration, frame rate and dimensions are LinkedIn's to judge: a video it
cannot use ends `PROCESSING_FAILED` before any post references it. The whole dispatch still runs
inside the orchestrator's two-minute provider call, which is why the availability wait is bounded
at 60 s; a long video that is still `PROCESSING` then is reported, not waited for.

## Not done here (#586 follow-ups)

- The `VIDEO_VIEW` objective (`costType` `CPV`, `optimizationTargetType` `MAX_VIDEO_VIEW`) and the
  other video objectives: video campaigns use the existing bidding plans (`WEBSITE_VISIT`
  automatic, `WEBSITE_CONVERSION` manual), both of which allow `SINGLE_VIDEO`.
- A caller-chosen call to action (fixed `REGISTER`), captions (SRT) and a custom thumbnail.
- Resuming a video that was still `PROCESSING` when the wait ended (its URN is in `videoUrns`; the
  campaign is retained, so today an operator finishes it in Campaign Manager).
- Videos larger than the 30 MiB asset-store ceiling, and classifying video assets in the
  creative plan (they have no dimensions, so the slot classifier treats them as unclassifiable).
