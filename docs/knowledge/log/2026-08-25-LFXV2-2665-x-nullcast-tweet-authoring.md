# 2026-08-25 — X/Twitter nullcast tweet authoring

**Update** — X was the last of the six paid platforms this service dispatches
to that could not author its own ad creative: `twitter.CampaignInput` accepted
only a pre-existing `TweetID`, and omitting it produced a manual-workflow
runbook message instead of anything servable. `CreateCampaign` now also
accepts `TweetText` (+ optional `AsUserID`), and when `TweetID` is empty it
authors a NEW promoted-only tweet via `POST accounts/:account_id/tweet` on the
Ads API host — the same host, auth, and query-param create contract every
other endpoint in this client already uses — then falls through into the
existing `promoted_tweets` promote step with the new id.

Key design points, detailed in
[internal/platform/twitter](../code/internal-platform-twitter.md#authoring-a-promoted-tweet-brief---servable-ad):

- `nullcast=true` is sent EXPLICITLY on every authored tweet, never relied on
  as X's documented default — the one failure mode that matters (a real tweet
  landing on the LF public timeline) is exactly the one an implicit default
  can't be tested for.
- An explicit `TweetID` always wins over `TweetText`.
- The posting handle is auto-resolved via `GET promotable_users` and fails
  closed on zero or multiple candidates rather than guessing.
- The 280-character cap is enforced with X's t.co URL weighting (any embedded
  URL counts as a fixed 23 characters), not raw length, so a long UTM'd
  registration URL cannot cause a false rejection.
- Authoring happens at Step 4 (immediately before the promote call), not
  alongside the campaign/line-item creation like the reddit client's post
  authoring — X's campaign/line item cost nothing while PAUSED, so the
  published tweet, not a paid resource, is the artifact worth minimizing the
  exposure window for.
- Outcome classification reuses the same ambiguous-before-definite house rule
  as the existing `promoted_tweets` switch (commit `88224984`), including
  treating a 2xx-with-no-id as UNCONFIRMED rather than a clean success.

Scope is text-only for this pass — image/video tweets need a chunked upload to
a different host this client does not implement, so that remains a manual
workflow.

`internal/dispatch/twitter.go`'s `twitterConfig` gained `tweetText`/`asUserId`
json fields mapped straight into `CampaignInput`; the existing degrade check
(keyed on an empty `PromotedTweetID`) needed no new trigger, since a
successfully authored + promoted tweet already populates that field the same
way an explicit `tweetId` does.

New tests: `internal/platform/twitter/authortweet_test.go` (happy path,
`nullcast=true` on the wire, explicit-TweetID-wins, pre-mutation weighted-length
rejection, 2xx-no-id degrade, each of the three warning arms via a dead-port
transport for the proven pre-send case, and `resolvePromotableUser`'s
pinned/single/ambiguous/none cases) and additions to
`internal/dispatch/twitter_test.go` for the new config plumbing.
