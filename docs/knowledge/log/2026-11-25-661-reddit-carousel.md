# 2026-11-25 — #661 Reddit carousel posts from stored images

**Update** — The carousel half of #549 gap 5, split out as #661. `redditConfig.carouselCards`
(2 to 6 cards, each a stored PNG/JPEG `imageAssetId`, a `headline` and a `destinationUrl`)
authors one promoted CAROUSEL post on the legacy profile-posts endpoint, one content entry per
card, with each card image served at a late-minted signed image URL. Built the way #650 built
VIDEO posts.

- Behind `REDDIT_CAROUSEL_ENABLED` (default off: constant, chart `"false"`, README); a create
  naming `carouselCards` while off, or with a card count or field outside the schema's limits, is
  a synchronous 400; dispatch re-checks and proves every card asset before any Reddit call.
- The snapshot keeps the card asset ids, sanitizes headlines and destinations; the asset guard
  already reads the cards' `imageAssetId`. No migration.
- design (description only) + `make apigen`, api-catalog, new concept
  [Reddit Carousel Posts](../architecture/reddit-carousel-posts.md), code concept updated.

The post shape has not been sent live; a paused probe in a test account is owed before the flag
goes on.
