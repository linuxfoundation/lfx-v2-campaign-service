# 2026-09-25 — LFXV2-2665 authored tweet survives an abort before promotion

**Fix** — `authoredTweetID` in `twitter.Client.CreateCampaign` was declared
*below* the `partialResult` closure, so the closure captured a variable that was
still the zero value at every point it could be called. A `pace(ctx)`
cancellation between the authoring POST and the `promoted_tweets` POST therefore
returned `AuthoredTweetID: ""` for a tweet that had provably been published —
the prose Steps entry was the only surviving trace of it. The declaration moved
above the closure, and the abort error now names the tweet via a new
`authoredTweetStatus` helper: `authored tweet <id> PUBLISHED, not yet promoted`.
This is the one irreversible artifact the flow creates; the campaign and line
item are `PAUSED` and are found-or-created by name on a retry.

**Fix** — `weightedTweetLen` now scans the composed text for URLs and weights
each at t.co's fixed 23 characters, instead of being handed only the URL the
composer appended. The old form under-counted when a caller's text already
embedded the registration URL (the append is skipped in that case) or carried
URLs of its own, rejecting copy X would have accepted. The variadic `urls`
parameter is gone.

**Fix** — `buildTwitterUTMURL` drops the destination's `#fragment`, which never
reaches a server and so can carry no attribution. Its pre-existing query
parameters are deliberately kept verbatim — unlike `displayTwitterUtmURL`, which
strips them — because this URL is the ad's real click destination and those
parameters are routinely what routes the visitor. Documented at the call site
and in `docs/api-catalog.md`, since it publishes whatever the brief supplied.

**Note** — two review suggestions were adjudicated as *no change* with the
reasoning recorded in-code rather than left for a future review pass to
re-raise: authoring is not find-or-create (a retry after campaign/line-item
reuse publishes a second tweet), deferred to the LFXV2-2665 idempotency work and
made tolerable by the partial-result fix above; and promotable-user resolution
stays after the creates, because a resolve failure is a non-fatal degrade
matching how authoring failures already degrade.

**Note** — test concurrency: the tweet-request capture in
`TestTwitter_TweetTextIsMappedAndAuthorsCleanCreated` and the shared recorder in
`newAuthorTweetTestServer` are both mutex-guarded, since the `httptest` handler
runs on the server's goroutine while assertions read from the test's.

Refs: LFXV2-2665
