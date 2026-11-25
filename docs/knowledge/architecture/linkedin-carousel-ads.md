---
type: "Architecture Doc"
title: "LinkedIn Carousel Sponsored Content"
description: "How a LinkedIn dispatch creates carousel Sponsored Content behind LINKEDIN_ADS_CAROUSEL_ENABLED: linkedInConfig.format carousel with 2 to 10 carouselCards (a stored image of the brief, a headline of at most 45 characters, an optional destination) and carouselIntroText, a CAROUSEL campaign named Carousel, each distinct image uploaded once through the Images API, then one dark post carrying content.carousel with each card tagged utm_content=card-<k> and one creative."
resource: "internal/platform/linkedin/carousel.go"
---

# LinkedIn Carousel Sponsored Content

Speaker and sponsor carousels on LinkedIn (#665, split from #552 gap 9). Since #665 a LinkedIn
create can make a carousel ad, alongside the single-image ads of
[LinkedIn Creative Image Assets](linkedin-creative-assets.md), the text ads and the
[video ads](linkedin-video-ads.md). Code: `internal/dispatch/linkedin_carousel.go` (gate, request
shape), `internal/dispatch/linkedin_creative.go` (card images resolved and checked with every other
stored image) and `internal/platform/linkedin/carousel.go` (limits, landing pages, the post and the
creative, run from `CreateCampaign`).

## Request shape (opt-in)

```jsonc
"linkedInConfig": {
  "format": "carousel",
  "carouselIntroText": "Meet the keynote speakers",   // required, at most 255 characters
  "carouselCards": [                                  // 2 to 10; no variants beside them
    { "imageAssetId": "<uuid>", "headline": "Keynote: Jane Doe" },
    { "imageAssetId": "<uuid>", "headline": "Panel", "destinationUrl": "https://events.example.org/schedule" }
  ]
}
```

`LINKEDIN_ADS_CAROUSEL_ENABLED` must be exactly `"true"` (chart default `"false"`); off, the create
is a synchronous 400 naming the flag. The request shape is checked by `checkFormatShape` /
`checkCarouselFieldShape` and `linkedinFormat` / `linkedinCarouselFormat` in `PreflightCreate`
and again in `Dispatch`, before any LinkedIn request.

## Limits (refused, never truncated)

From LinkedIn's carousel ad specifications (help a88137) and the Carousel Ads API
(li-lms-2026-11): 2 to 10 cards; card headline at most 45 characters (the API's media title takes
under 400); intro text at most 255; images PNG or JPEG, at most 10 MB and 4320x4320 pixels (no
minimum is documented); destination `http(s)`, at most 2,000 characters, checked on the tagged URL
before the campaign group exists. A lead generation carousel (one form for every card) is refused:
that creative shape is not built.

## What is created

1. A `SPONSORED_UPDATES` campaign with `format: CAROUSEL` ("Dynamic, Carousel, and Video Ad
   Campaigns must have their format set during creation"), with the default create's bidding,
   objective and delivery options unchanged. The name's Ad Format segment is `Carousel`;
   `findCampaignByNameInGroup` refuses a same-name campaign of another format
   (`sponsoredFormatKind`).
2. Each distinct card image uploaded once through the Images API after the campaign exists and
   polled to AVAILABLE (`uploadVariantImages`), exactly as a single-image thumbnail.
3. ONE dark post: `content.carousel.cards[]` of `{media: {id, title}, landingPage}` in card order,
   `commentary` the intro text, and `contentLandingPage` card 1's landing page (the documented
   sample sends one).
4. ONE DRAFT creative referencing the post, named `<event> | carousel-1`.

Each card's landing page is its `destinationUrl` (the brief's registration URL when omitted) with
the create's UTM parameters and `utm_content=card-<k>`, `k` the 1-based card position — the
per-card attribution chosen for carousels (the Reddit carousel's, #661). A failure after the
campaign exists returns the partial (claim retained, no re-dispatch); an ambiguous post or
creative failure is reported UNCONFIRMED.

## Snapshot

The config snapshot keeps each card's canonical `imageAssetId` (so the asset-deletion guard sees
it), its headline and the intro text through `sanitizeSnapshotText`, and only the scheme and host
of a `destinationUrl`.

## Not yet verified

No live LinkedIn call has been made. Owed before enabling: one paused create in the shared ad
account with two cards, confirming the campaign accepts `format: CAROUSEL`, the post accepts
`content.carousel` with `contentLandingPage`, and the creative references it; then archive it.
