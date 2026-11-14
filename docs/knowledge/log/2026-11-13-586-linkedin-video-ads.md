# 2026-11-13 — #586 LinkedIn video Sponsored Content

**Update** — A LinkedIn create can make video Sponsored Content ("Motion"), behind
`LINKEDIN_ADS_VIDEO_ENABLED` (chart default `"false"`). `upload-creative-asset` accepts
`video/mp4`, checked structurally (MP4 `ftyp` brand, boxes tiling the file, `moov` and `mdat`),
stored with no dimensions; migration `000057` widens the `mime_type` CHECK, and every image slot
refuses a video asset at dispatch. `linkedInConfig.variants[].videoAssetId` (all variants or
none; synchronous 400 for a bad id, an image alongside, a mix, or the flag off) makes the campaign
`SINGLE_VIDEO`, named with `Video` in the Ad Format segment, uploads each distinct video once
through the Videos API (initializeUpload, plan checked before any byte, part PUTs with ETags,
finalizeUpload, AVAILABLE within 60 s), and posts it as `content.media` with the registration URL
as `contentLandingPage` and `REGISTER`. Same idempotency as images: upload after the campaign,
before the first post, so every failure is a retained partial with the URN in `videoUrns`. The
dangling-asset snapshot guard now reads `videoAssetId` too. `VIDEO_VIEW`, captions, thumbnails
and resuming a still-processing video are follow-ups. See
[LinkedIn Video Sponsored Content](../architecture/linkedin-video-ads.md).
