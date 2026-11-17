# 2026-11-17 — #564 X gaps, part 2a: create targeting, audience expansion and look-alikes return

**Update** — the first follow-up to PR #614 (which kept only the X video and the MP4 asset store)
brings back the create-time inputs that PR moved out, with their tests:

- `conversationTopics` (gap 8), `platforms`, `devices`, `ageBucket` and `gender` (gap 9) on
  `twitterConfig`, behind `TWITTER_CREATE_TARGETING_ENABLED`. The listed values are checked against
  `GET targeting_criteria/conversations`, `/platforms` and `/devices` before anything is created;
  `gender` is sent as X's `1` or `2`, and a criterion echo that spells an all-digit value as a bare
  JSON number now reads as that number's text (`targetingCriterionWire.criterion`).
- `audienceExpansion` (gap 11, `TWITTER_AUDIENCE_EXPANSION_ENABLED`), sent as `audience_expansion`
  on a line item the create makes; a reused line item keeps its own and a step says so.
- `followerLookalikeUserIds` (gap 4c, `TWITTER_FOLLOWER_LOOKALIKES_ENABLED`), shape-checked only
  and sent as `SIMILAR_TO_FOLLOWERS_OF_USER` criteria.

Every new criterion counts toward the 60-criterion per-create cap and the 30-criterion cap of a
create with a video. Both new flags default to `"false"` in the chart. The `config` attribute of
`create-campaigns` documents the new keys; to keep the PR small enough for the reviewer to read
every file, the create-campaigns method description is updated once, in part 2b.

Still in later parts: tweet `variants` (gap 10), the website `card` (gap 6) and the positive
keyword add lever (gap 12). Until then a video still needs `tweetText`.

Not verified live: no X call was made.
