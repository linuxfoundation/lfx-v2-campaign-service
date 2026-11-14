# 2026-11-14 — #564 X website card

**Update** — `twitterConfig.card` (gap 6 of the X Ads gap review, #564), `{headline}`, creates a
website card with `POST accounts/:account_id/cards` (the creative image as `MEDIA`, the headline
and the UTM'd registration URL as `DETAILS`, `utm_content=website-card`) and attaches it by
`card_uri` to every tweet the create authors.

- Created once, only when a tweet is about to be authored; a card failure publishes nothing and
  leaves the row `created_degraded` (UNCONFIRMED when the card may exist).
- Requires `imageAssetId` and an authored tweet (`tweetText` or `variants`).
- New default-off flag `TWITTER_WEBSITE_CARD_ENABLED`; synchronous 400s re-checked in `Dispatch`.
- The result records `CardURI`.

Not verified live: no X call was made; the body follows the published v12 reference.
