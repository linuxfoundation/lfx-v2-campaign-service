# 2026-11-14 — #564 X video: MP4 creative assets (from #586)

**Update** — The video/mp4 creative-asset store pieces of `feat/586-linkedin-video-ads` are taken
onto the X gaps branch unchanged, as their own commit, so the X video creative (#564 gap 7) can
build on them and the two branches merge trivially:

- `upload-creative-asset` accepts `video/mp4`, checked structurally
  (`internal/service/creative_asset_video.go`) and stored with no dimensions.
- Migration `000057_creative_assets_video_mp4` widens `creative_assets.mime_type` (same number,
  name and SQL as #586).
- `model.MimeTypeMP4`; every image slot (Meta, Microsoft, X, Google, Reddit) refuses a video asset
  before any request; the snapshot dangling-asset guard also reads a `videoAssetId` key.

The LinkedIn video consumer from #586 is not part of this commit.
