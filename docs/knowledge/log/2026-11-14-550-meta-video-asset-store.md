# 2026-11-14 — MP4 creative assets brought forward for Meta video (#550)

**Update** — The creative-asset store accepts `video/mp4` uploads: a structural MP4 check
(`internal/service/creative_asset_video.go`: MP4 `ftyp` brand, top-level boxes tiling the file,
`moov` + `mdat`), migration `000057` widening the `mime_type` CHECK, the `videoAssetId` snapshot
key in the dangling-asset guard, and a pre-create refusal of a video in every image slot (Meta,
Microsoft, X, Google, Reddit). These are the asset-store pieces of #586
(`feat/586-linkedin-video-ads`), cherry-picked with the same file names, migration number and
behaviour so that branch merges trivially; Meta video (#550 gap 9) builds on them. No consumer
reads a video yet in this entry.
