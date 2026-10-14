# 2026-10-13 — X Ads campaigns can use a stored creative image

**Update** — `twitterConfig.imageAssetId` attaches a creative asset uploaded with
`upload-creative-asset` to the authored promoted-only tweet. The client uploads it through the X
API v2 chunked media upload (`tweet_image`, OAuth 1.0a user context), adds it to the ad account's
Media Library under a SHA-256-derived name, and sends it as `media_keys` on
`POST accounts/:id/tweet`. All of this happens before the campaign POST, and every media write
goes through the account pacer. A retry reuses the library entry by name, so it does not upload
again. The dispatcher validates the id, the tweetText/tweetId combination, brief ownership and
X's 5 MiB limit (from the stored size) before loading the asset. It holds the bytes on the shared
`AssetReserver`, which `registerDispatchers` now binds onto both Meta and X. No migration.

See [X Ads Creative Image Assets](../architecture/x-creative-assets.md).
