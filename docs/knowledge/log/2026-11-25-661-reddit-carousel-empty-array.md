# 2026-11-25 — #661 Reddit carousel: an explicit empty carouselCards is refused

**Fix** — PR #679 review: `"carouselCards": []` decoded to a non-nil empty slice, and
`checkRedditCarouselChoice` treated it as absent. It skipped the `REDDIT_CAROUSEL_ENABLED` gate and
the 2-to-6 count, so a create with no other creative made a campaign and ad group with no post and
recorded a clean `created`. Only an omitted or `null` field is now absent; `[]` reaches the gate
(400 while off) and the count check (400, 0 cards). See
[Reddit Carousel Posts](../architecture/reddit-carousel-posts.md).
