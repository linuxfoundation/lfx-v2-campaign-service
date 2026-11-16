# 2026-11-14 — Reddit one ad per variant (#549)

**Update** — While `REDDIT_AD_PER_VARIANT_ENABLED` is `"true"`, a Reddit create that authors its
post (`redditConfig.imageUrl` or `imageAssetId`, no `postUrl`) with 2-5 `variants` authors one
promoted post and one ad per variant in the single ad group, each with its own
`utm_content=variant-N`. This is gap 6 of the Reddit Ads gap review. Every confirmed ad id is kept
in the campaign row's result blob as `adIds` (`adId` stays the first) with per-variant
`variantAds`; no migration. A failed variant does not roll back the others: the campaign is
`created_degraded` and a step names the failed variants. The status toggle cascades over every ad
(`UpdateCampaignTreeStatus`, `redditChildIDs` reading `adIds`); the monitor, metrics, settings,
adoption and the levers are campaign- or ad-group-level and read no ad id.

`RedditDispatcher.PreflightCreate` refuses more than five variants or a blank or over-long variant
headline with a synchronous 400; `Dispatch` and the client re-check before any Reddit request. The
flag (chart `"false"`) stays off until a live create confirms Reddit serves several ads from
authored posts in one ad group. Gap review: #549.
