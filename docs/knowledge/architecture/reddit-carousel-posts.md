---
type: "Architecture Doc"
title: "Reddit Carousel Posts"
description: "Reddit CAROUSEL posts from stored images (#661, split from #549 gap 5): redditConfig.carouselCards names 2 to 6 cards, each a stored PNG/JPEG, a headline (the card's caption, at most 180 characters) and a destination URL, authored as one CAROUSEL post whose images Reddit fetches from late-minted signed image URLs; built from Reddit's public v3 post schema, off unless REDDIT_CAROUSEL_ENABLED, and the post shape needs a paused live probe in a test account before the flag goes on."
resource: "internal/dispatch/reddit_carousel.go"
---

# Reddit Carousel Posts

#661, split from #549 gap 5 after the VIDEO half shipped in #650 ([Reddit Video
Posts](reddit-video-posts.md)). Before it, a Reddit create could author an IMAGE or (behind its
flag) a VIDEO post; a speaker or sponsor carousel needed a hand-made `postUrl`.

## Request

`redditConfig.carouselCards` is an array of 2 to 6 cards:

```json
{"carouselCards": [
  {"imageAssetId": "<stored PNG/JPEG of this brief>", "headline": "Keynote: ...",
   "destinationUrl": "https://events.example.org/speakers/a"},
  ...
]}
```

The carousel is the post's only creative: never with `postUrl`, `imageUrl`, `imageAssetId`,
`videoAssetId` or `thumbnailAssetId`. Each card's `imageAssetId` is tagged as such, so the
snapshot's dangling-asset guard (`postgres.snapshotAssetIDs`) already protects it; no new key
or migration.

## Post shape

The same legacy `POST /profiles/{account}/posts` the image and video paths use
(`createPromotedPost`), one content entry per card in card order:

```json
{"data": {"type": "CAROUSEL", "headline": "...", "allow_comments": false,
          "content": [{"media_url": "<signed image URL>", "caption": "<card headline>",
                       "call_to_action": "Sign Up",
                       "destination_url": "<card destinationUrl + UTM, utm_content=card-<k>>"}, ...]}}
```

From `components_schema_post`: `media_url` is "required for image, video and carousel posts",
`destination_url` "required for image and carousel posts", and `caption` is "a carousel image's
caption" with `maxLength` 180, which is the card headline's limit here. The schema allows up to
40 content entries; 2 to 6 is Reddit's carousel ad specification and the range #661 asks for. No
`thumbnail_url` is sent (the schema requires it only for video). The post headline and the CTA
follow the image post's rules. Each card's destination gets the post's UTM parameters with
`utm_content=card-<k>`, the card's 1-based position (`carouselCardUTM`), so a registration
attributes to the card that drove it. A carousel is one post, so the post-level `variant-1` would
say nothing; the plain `card-<k>` form was chosen on 2026-11-25 (owner decision) after checking
that nothing in this service, the LFX One BFF or lf-dbt parses the `variant-` prefix. A carousel create is one
post and one ad: while `REDDIT_AD_PER_VARIANT_ENABLED` is on, a carousel with two or more
variants is a 400. The structured-post endpoint's `CarouselCreative` is the other documented
shape; it is not used, as for images and video.

## Serving the media

Each card's image is served by the signed creative-asset route at an ordinary image URL
([Signed Creative-Asset URLs for Reddit](signed-creative-asset-urls.md)), minted by the client
in card order immediately before the one post request. Every minted URL is scrubbed from the job
error, the steps and the ad warning (`scrubRedditOutcome`).

## Validation

- **Pre-flight (synchronous 400)**: `checkRedditCarouselShape` on the raw config (a non-array
  or non-string card field, flag on or off), then `checkRedditCarouselChoice` on the best-effort
  decode: the flag off (named, checked first), a mix with another creative, two or more variants
  with one ad per variant on, a card count outside 2 to 6, signed asset URLs not configured, and
  per card a missing or non-UUID `imageAssetId`, a blank or over-180-character headline, or a
  `destinationUrl` that is missing, not absolute http(s), carries userinfo or has a malformed
  query. Refusals name the 1-based card and never echo a headline or URL.
- **Dispatch** runs the same rules again, then `checkRedditCarouselAssets` before any Reddit
  call: each asset exists for the brief with bytes, is PNG/JPEG, and fits a signed URL.
- **Client** (`reddit.CampaignInput.CarouselCards`, `validateCarouselInput`) re-checks the
  count, every card and the exclusivity before any request; a source error or bad minted URL is
  `(nil, err)`.

## Snapshot

The config snapshot keeps each card's asset id, passes the headline through
`sanitizeSnapshotText` and reduces the destination with `sanitizeSnapshotURL`; the full URL
still reaches Reddit.

## Partial results

As for video: a refused post degrades to a campaign and ad group with no ad
(`created_degraded`), an in-flight cancellation is an UNCONFIRMED partial, and a definite
campaign refusal after a successful or unconfirmed carousel post returns a partial result naming
it, so the claim is kept and a retry does not author a second post.

## Deployment gate

`REDDIT_CAROUSEL_ENABLED` (default off; the chart sets `"false"`). It names no account
resource. It keeps an unverified post shape off the shared ad account.

## Needs a paused live probe before the flag goes on

- Reddit accepts `type: CAROUSEL` with per-card `media_url`, `caption`, `destination_url`
  and `call_to_action` on the legacy profile-posts endpoint, or requires the structured
  `CarouselCreative`.
- Whether the caption limit Reddit enforces is the schema's 180 or the help centre's shorter
  carousel headline limit, and whether a per-card CTA is honoured.
- Image size and aspect requirements for carousel cards.
