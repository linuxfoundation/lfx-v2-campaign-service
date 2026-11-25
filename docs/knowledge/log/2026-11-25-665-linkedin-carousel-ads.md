# 2026-11-25 — #665 LinkedIn carousel ads

**Update** — `linkedInConfig.format` takes `"carousel"` (#552 gap 9, split to #665) with
`carouselCards` (2 to 10 stored images of the brief, each with a headline of at most 45
characters and an optional `destinationUrl`) and `carouselIntroText` (at most 255), behind
`LINKEDIN_ADS_CAROUSEL_ENABLED` (off by default). The campaign is `SPONSORED_UPDATES` with
`format: CAROUSEL` and Ad Format name segment `Carousel`; bidding, objective and delivery options
are the default create's, unchanged. Each distinct card image is uploaded once through the Images
API, then one dark post carries `content.carousel` with each card's landing page tagged
`utm_content=card-<k>`, and one DRAFT creative references it. With the flag off, or with a shape,
count, length or destination the LinkedIn spec refuses, the create is a 400 before a job exists;
Dispatch repeats every check and checks the assets (brief scope, PNG/JPEG, 10 MB, 4320x4320)
before any LinkedIn request. A lead generation carousel is refused. Document ads (the other half
of #665) follow in a separate PR. Not yet created on a live account; the paused probe is recorded
in the PR.
