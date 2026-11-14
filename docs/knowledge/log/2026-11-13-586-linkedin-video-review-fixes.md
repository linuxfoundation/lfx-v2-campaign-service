# 2026-11-13 — #586 LinkedIn video review fixes

**Fix** — Pre-PR review of the LinkedIn video branch. Reddit's image slot now refuses a
`video/mp4` asset before any Reddit request: `checkRedditAsset` reads the bytes-free
`GetAssetMetadata` row (size and mime type in one query) instead of `GetAssetSize`, so a stored MP4
named by `redditConfig.imageAssetId` no longer passes preflight and 404s at the signed URL after the
campaign and ad group exist. Migration `000057`'s up drops `creative_assets_mime_type_check`
without `IF EXISTS`, so a name mismatch fails loudly; the down stays `NOT VALID`. README documents
`LINKEDIN_ADS_VIDEO_ENABLED`. Recorded as a live-verification blocker (architecture doc, chart and
constant comments): the 60 s video availability wait runs inside the two-minute provider call and
is not lengthened — measure real processing time for a 20-30 MiB MP4 before enabling the flag, and
if it is routinely slow, decouple the upload from create (pre-upload, then reference the
`AVAILABLE` URN). See [LinkedIn Video Sponsored Content](../architecture/linkedin-video-ads.md).
