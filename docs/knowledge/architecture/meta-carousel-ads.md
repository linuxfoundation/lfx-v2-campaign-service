---
type: "Architecture"
title: "Meta Carousel Ads"
description: "Meta carousel ads (#550 gap 10), behind META_CAROUSEL_ENABLED: metaConfig.format carousel turns the 2-10 variants into the cards of ONE ad (link_data.child_attachments, each card with its image, headline, description, own UTM link and button; variant 1's primaryText as the one message), checked before any request in PreflightCreate, Dispatch and the client, with one ad counted as the full result."
tags:
  - meta
  - creative
---

# Meta Carousel Ads

`metaConfig.format` is `single` (default, one ad per variant) or `carousel`. A carousel replaces
the per-variant ad loop with ONE ad whose cards are the variants, in order.

## Gate and pre-flight

`META_CAROUSEL_ENABLED` (exactly `"true"`; default off). `checkMetaFormat` runs in
`PreflightCreate` (synchronous 400) and `Dispatch` (not-created refusal): an unknown format, the
gate, and `meta.ValidateCarousel` — 2-10 variants, every card with an image (`imageUrl` or
`imageAssetId`) and no `videoAssetId`, variant 1's `primaryText` required and every later card's
empty or identical. `format` is type-checked with the other metaConfig strings. The client
re-runs `NormalizeFormat` and `ValidateCarousel` before any request; refusals wrap
`domain.ErrCreateConfigInvalid` and are `domain.PreflightRefusal`s.

## Request (`internal/platform/meta/carousel.go`)

After the ad set, `createCarouselAd` uploads each stored card image through the per-campaign
`/adimages` cache, then posts one creative:

```
object_story_spec.link_data = {
  link: <card 1 UTM link>, message: <variant 1 primaryText>,
  child_attachments: [{link: <card N UTM link, utm_content=variant-N>, name: headline,
                       description?, picture | image_hash,
                       call_to_action: {type, value: {link}}}, ...]}
```

and one ad (`<eventName> - Carousel Ad`, PAUSED). `multi_share_optimized` is not sent (Meta's
default).

## Result and failures

`AdCount` 1, `Format` `"carousel"`, `Ads[0].CardImageHashes`. `Dispatch` counts one ad as the full
request (`metaRequestedAds`), so a created carousel is `created`, not `created_degraded`. A card
upload failure (no creative sent) or a creative/ad failure is a Steps line and a degraded success;
a cancelled caller returns the partial.

## Not live-verified (blocker for the flag)

The `child_attachments` payload as sent (per-card `call_to_action`, `picture` alongside
`image_hash` cards in one carousel, card `description`) against a live ad account.
