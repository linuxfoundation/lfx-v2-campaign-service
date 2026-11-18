# 2026-11-14 — Meta carousel ads (#550)

**Update** — `metaConfig.format: "carousel"` creates ONE Meta ad whose 2-10 cards are the
variants (`link_data.child_attachments`: image, headline, description, per-card UTM link and
button; variant 1's `primaryText` as the message), behind `META_CAROUSEL_ENABLED` (default off).
Unknown formats, the gate and the card rules are synchronous 400s, re-checked in `Dispatch` and the
client. One ad is the full count, so a created carousel is `created`. See
[Meta Carousel Ads](../architecture/meta-carousel-ads.md).
