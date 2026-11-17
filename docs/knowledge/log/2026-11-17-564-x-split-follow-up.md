# 2026-11-17 — #564 X gaps split: most features move to a follow-up PR

**Note** — PR #614 was too large for the automated reviewer to read every changed file, so it now
carries only the X video creative (`twitterConfig.videoAssetId`, `TWITTER_VIDEO_ENABLED`, gap 7) and
the MP4 creative-asset store it needs (#586, migration `000057`). Everything below moves, unchanged,
to a follow-up PR (branch `feat/564-x-gaps-part2`) that opens after #614 merges:

- create targeting: `conversationTopics` (gap 8), `platforms`, `devices`, `ageBucket` and `gender`
  (gap 9), with the numeric GENDER echo read;
- `audienceExpansion` (gap 11, `TWITTER_AUDIENCE_EXPANSION_ENABLED`);
- `followerLookalikeUserIds` (gap 4c, `TWITTER_FOLLOWER_LOOKALIKES_ENABLED`);
- tweet `variants` (gap 10, `TWITTER_TWEET_VARIANTS_ENABLED`) and the synchronous composed-text
  check that carries the brief into `PreflightCreate`;
- the website `card` (gap 6, `TWITTER_WEBSITE_CARD_ENABLED`);
- the positive keyword add lever, `add-keyword-targeting` (gap 12, `TWITTER_KEYWORD_ADD_ENABLED`),
  with the `PositiveKeyword` missing-operator read.

The 2026-11-14 to 2026-11-17 #564 log entries for those features describe the follow-up PR, not
this one. In this PR a video needs `tweetText` (variants and cards are not here yet).
