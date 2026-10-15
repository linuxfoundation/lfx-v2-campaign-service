# 2026-10-15 — X campaign create reuses what an UNCONFIRMED attempt left upstream

**Update** — a retried X (`twitter-ads`) create now reuses the promoted tweet an earlier
attempt attached to the reused line item, and the name lookups refuse ambiguous or foreign
matches instead of reusing the first one listed.

- Before: campaigns and line items were found by name, but Step 4 always authored a NEW tweet
  and re-POSTed `promoted_tweets`, so a retry after an UNCONFIRMED promotion published a second
  tweet under the LF handle and attached a second spending creative.
- `listPromotedTweets` (`promoted_tweet_lookup.go`) reads the reused line item's live
  associations; one is reused (no authoring, no re-POST, warning step), two or more refuse, a
  failed read blocks Step 4's writes. Runs only on a reused line item, so a fresh create is
  unchanged.
- `findByNameChecked`: two or more live exact-name matches refuse; deleted matches skipped; a
  campaign on another funding instrument, or a line item under another campaign, refuses.
- Still not found: a tweet published but never promoted (its id is returned in
  `AuthoredTweetID` with the pass-back instruction, as before).
- Tests: `create_idempotency_test.go` with a stateful fake X server. Mutation-checked: 12/12
  killed.
- Concept updated: `code/internal-platform-twitter.md`.
