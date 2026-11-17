# 2026-11-14 — #564 X one promoted tweet per variant

**Update** — `twitterConfig.variants` (gap 10 of the X Ads gap review, #564), `[{tweetText}]`
with 1-5 entries, authors and promotes one nullcast tweet per variant on the one line item an X
create makes, each destination tagged `utm_content=variant-N`.

- Every promoted tweet is stored on the campaign as `PromotedTweets` in the result blob (no
  migration); `PromotedTweetID` / `AuthoredTweetID` keep the first.
- Decided outcome (2026-11-14): a variant failing after a tweet was published leaves the row
  `created_degraded` with a step naming the failed variants; nothing is rolled back.
- Pause/resume and the budget, bid and keyword levers act on the campaign and line item, so they
  govern every variant.
- New default-off flag `TWITTER_TWEET_VARIANTS_ENABLED`; synchronous 400s re-checked in
  `Dispatch`; the connection's `as_user_id` binds variants as it binds `tweetText`.

Not verified live: no X call was made.
