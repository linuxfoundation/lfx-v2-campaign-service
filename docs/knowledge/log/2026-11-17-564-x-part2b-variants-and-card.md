# 2026-11-17 — #564 X gaps, part 2b: tweet variants and the website card return

**Update** — the second follow-up to PR #614, stacked on part 2a, brings back the authored-tweet
features that PR moved out, with their tests:

- `twitterConfig.variants` (gap 10, `TWITTER_TWEET_VARIANTS_ENABLED`): 1-5 tweets, each authored as
  its own nullcast tweet with `utm_content=variant-N` and promoted on the one line item. A failed
  variant does not stop the others and nothing is rolled back; the row is `created_degraded`.
  Every promoted tweet is recorded in `CampaignResult.PromotedTweets`.
- `twitterConfig.card` (gap 6, `TWITTER_WEBSITE_CARD_ENABLED`): one website card from
  `POST accounts/:account_id/cards`, carrying the image's or the video's `media_key`, attached by
  `card_uri` to every authored tweet and recorded as `CardURI`.
- The synchronous composed-text check: `PreflightCreate` composes each tweet (`tweetText` or a
  variant) with the brief's destination URL exactly as the job will, so copy that only goes over
  X's limit once the destination is appended is a 400, not a failed job. The brief reaches the
  dispatcher on the context (`model.WithCreatePreflightBrief`, set by
  `BriefService.CreateCampaigns`).

With variants back, a video or an image needs `tweetText` or `variants` (never `tweetId`), and
the video can be the card's media. Both new flags default to `"false"` in the chart.

The create-campaigns method description now lists the synchronous refusals of parts 2a and 2b
(`conversationTopics`, `platforms`, `devices`, `ageBucket`, `gender`, `audienceExpansion`,
`followerLookalikeUserIds`, `variants`, `card`) and is written one sentence per source line, as the
`config` attribute is since part 2a. `upload-creative-asset` again names the card as a consumer of
an MP4 asset.

Still to come: the positive keyword add lever (gap 12), in part 2c.

Not verified live: no X call was made.
