# 2026-11-14 — Reddit pixel picker read (#549)

**Update** — `GET /projects/{project_id}/reddit-ads/pixels` (`list-reddit-ads-pixels`) lists the
conversion pixels on the Reddit ad account a create for the project uses, for a
`redditConfig.conversionPixelId` picker. This is gap 10 of the Reddit Ads gap review. It reads
Reddit `GET /ad_accounts/{ad_account_id}/pixels` (`list_pixels_by_ad_account` in the public v3
OpenAPI document), follows `pagination.next_url` for at most 10 pages and answers `account_id`,
`pixels` (`id`, `name`, optional `business_id`, sorted by name ignoring case, then id),
`pixel_count` and `truncated`. An answer it cannot believe is a 503 with nothing partial.

The route sits in the gateway's `marketing-ops-api` rule (`global_marketing_ops`) beside the
custom-audience read, with the HTTPRoute regex and the chart parity test updated to match. The
read answers 400 "not supported" until `REDDIT_PIXEL_LIST_ENABLED` (chart `"false"`) is turned on
after a live read confirms the shape. Gap review: #549.
