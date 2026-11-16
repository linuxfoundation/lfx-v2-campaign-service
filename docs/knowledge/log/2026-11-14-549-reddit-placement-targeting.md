# 2026-11-14 — Reddit placements, platforms and expansion at create (#549)

**Update** — `redditConfig.placements` (FEED, COMMENTS_PAGE), `redditConfig.platforms` (DESKTOP,
MOBILE_NATIVE, MOBILE_WEB) and `redditConfig.expandTargeting` set the ad group's
`targeting.locations`, `targeting.platforms` and `targeting.expand_targeting`, which the create
had fixed at `FEED` + `COMMENTS_PAGE`, `ALL` and `true`. This is gap 7 of the Reddit Ads gap
review: turning expansion off for a community-only test, or limiting a campaign to one placement
or device class. Omitted, the create is byte-for-byte unchanged.

`reddit.NormalizePlacementTargeting` holds the rules (case-insensitive, de-duplicated, fixed
order; blank or unknown entries refused; `ALL` is not an entry). `RedditDispatcher.PreflightCreate`
refuses a breach, a wrong JSON type or any of the three while the flag is off with a synchronous
400; `Dispatch` and the client re-check before any Reddit request. All three are sent on every
ad-group attempt. They sit behind `REDDIT_PLACEMENT_TARGETING_ENABLED` (chart `"false"`) until a
live create confirms Reddit accepts a non-default value. Gap review: #549.
