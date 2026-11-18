# 2026-11-13 — #586 LinkedIn video Sponsored Content

**Update** — A LinkedIn create can make video Sponsored Content ("Motion"), behind
`LINKEDIN_ADS_VIDEO_ENABLED` (chart default `"false"`). The MP4 creative-asset store it reads
(`upload-creative-asset` accepting `video/mp4`, migration `000057`, the image slots refusing a video)
already landed with the X video work (#614); this change adds only the LinkedIn consumer.
`linkedInConfig.variants[].videoAssetId` (all variants or none; synchronous 400 for a bad id, an
image alongside, a mix, or the flag off) makes the campaign `SINGLE_VIDEO`, named with `Video` in
the Ad Format segment, uploads each distinct video once through the Videos API (initializeUpload,
plan checked before any byte, part PUTs with ETags, finalizeUpload, AVAILABLE within 60 s), and
posts it as `content.media` with the registration URL as `contentLandingPage` and `REGISTER`. Same
idempotency as images: upload after the campaign, before the first post, so every failure is a
retained partial with the URN in `videoUrns`. Rebuilt on the Text Ads branch (2026-11-17): the Ad
Format segment comes from one function (`Text`, `Video` or `Static`), and a video create, like a
text one, skips the pre-#587 legacy-name lookup, whose name is always `Static`. Live-verification
blocker: the 60 s availability wait runs inside the two-minute provider call; measure real
processing time for a 20-30 MiB MP4 before enabling the flag. `VIDEO_VIEW`, captions, thumbnails
and resuming a still-processing video are follow-ups. See
[LinkedIn Video Sponsored Content](../architecture/linkedin-video-ads.md).
