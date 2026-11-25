# 2026-11-25 — #661 Reddit carousel cards tagged utm_content=card-k

**Update** — Owner decision on PR #679: each Reddit carousel card's destination now carries
`utm_content=card-<k>` (1-based card position) instead of the post-level `variant-1`, so a
registration attributes to the card that drove it (`carouselCardUTM`). The plain `card-<k>` form
was chosen over `variant-1-card-<k>` because nothing in this service, the LFX One BFF
(lfx-self-serve's four `utm_content` writers) or lf-dbt (presence checks and extraction only)
parses the `variant-` prefix. See [Reddit Carousel Posts](../architecture/reddit-carousel-posts.md).
