---
type: "Architecture"
title: "Meta Video Ads"
description: "Meta video ads from stored MP4 creative assets (#550 gap 9), behind META_VIDEO_ENABLED: each distinct video uploaded once to /advideos after the ad set exists, one bounded readiness wait shared by every video inside the provider call, Meta's preferred auto-generated thumbnail, a video_data creative per variant, and every minted video id kept on the result so a retry never re-uploads."
tags:
  - meta
  - creative-assets
  - video
---

# Meta Video Ads

`metaConfig.variants[].videoAssetId` names an MP4 uploaded to the brief's creative-asset store
(`upload-creative-asset`, `content_type` `video/mp4`; the store and migration `000057` are #586's,
brought forward). The variant becomes a video ad. Image and video variants may share an ad set.

## Gate and pre-flight

`META_VIDEO_ENABLED` (exactly `"true"`; default off). `checkMetaVideoVariants` runs in
`PreflightCreate` (synchronous 400) and again in `Dispatch` (not-created refusal): the gate, a uuid
id, and no `imageUrl`/`imageAssetId` on a video variant. `resolveVariantAssets` resolves the video
slot like the image slot (brief scope, dedupe, `AssetReserver`) and `checkMetaSlotMime` refuses a
non-MP4 in the video slot and an MP4 in the image slot. The client re-checks with
`meta.ValidateVideoVariant` (no image alongside, resolved bytes, `video/mp4`). Every refusal wraps
`domain.ErrCreateConfigInvalid` and is a `domain.PreflightRefusal`.

## Flow (`internal/platform/meta/video.go`)

After the ad set exists and before the first creative, `prepareVideos`:

1. uploads each DISTINCT video (by content) once: `POST /act_<id>/advideos`, multipart `source`
   part named `creative.mp4`. Never retried, not even on a throttle, because every upload creates
   a new video object (unlike content-addressed `/adimages`);
2. polls `GET /{video_id}?fields=status` every 3 s until `video_status` is `ready`, within ONE 60 s
   wait shared by all videos (it runs inside the orchestrator's two-minute provider call);
3. reads `GET /{video_id}/thumbnails?fields=uri,is_preferred` and takes the preferred https
   thumbnail (else the first).

Each variant's creative carries `object_story_spec.video_data` {`video_id`, `image_url`,
`message`, `title`, `link_description`, `call_to_action` {`type`, `value.link` = the UTM URL}}.

## Failure and retry safety

A video that fails to upload, ends `error`, is still processing when the wait ends, or has no
usable thumbnail fails only its variants: a Steps line, no creative or ad request, and the campaign
persists `created_degraded` (reused as-is, never re-dispatched). A cancelled caller during the
uploads or the wait returns the partial result, so the claim is retained. Every minted id is on
`CampaignResult.VideoIDs` (asset id -> video id) and each ad's `AdResult.VideoID`.

## Not live-verified (blockers for the flag)

- Meta's encoding time for a 20-30 MiB MP4 against the 60 s wait; if routinely slower, decouple the
  upload from create rather than lengthening the wait.
- The non-resumable `source` upload of a 30 MiB file inside the 30 s per-request timeout.
- The thumbnails edge shape (`uri`, `is_preferred`) and that `video_data.image_url` accepts it.
- `video_data.link_description` and the call to action on every objective.
