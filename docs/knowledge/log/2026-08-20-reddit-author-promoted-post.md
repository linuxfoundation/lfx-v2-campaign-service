# 2026-08-20 — Reddit: author a promoted image post so a brief reaches a servable ad

**Update** — The Reddit client could only ATTACH a caller-supplied `PostURL` as
an ad's creative; with no post it created a campaign + ad group and left the
operator to make the post by hand in Reddit Ads Manager. Google and Meta author
their own creatives, so Reddit was the odd one out on the brief -> servable-ad
path. This closes that gap: `CreateCampaign` can now AUTHOR a promoted ("dark")
image post itself.

Live API probing (current creds, scope `adsread adsedit`) established the shape,
which has no public documentation:

- Post creation is `POST /profiles/{accountID}/posts` — Reddit's profile id IS
  the ad-account id (`t2_...`), NOT under `/ad_accounts/`. An empty-body POST
  returns 400 (validation), not 403, confirming `adsedit` authorizes it.
- Body: `{"data":{"type":"IMAGE","headline":..,"allow_comments":false,
  "content":[{"media_url":..,"call_to_action":..,"destination_url":..}]}}`.
  `allow_comments:false` = promoted-only (not distributed to a feed on its own).
- There is NO LINK post type and NO separate media-upload endpoint: Reddit
  ingests the external `media_url` at post-create time and re-hosts it to
  `i.redd.it`, so authoring requires a public absolute http(s) image URL.
- `call_to_action` is a TITLE-CASE enum ("Learn More", "Sign Up", "Buy Tickets",
  …), not `LEARN_MORE`. The 24 accepted labels were captured live on 2026-08-20
  and stored in `redditCTAs`.

New optional `CampaignInput` fields (plumbed through `redditConfig`, like the
existing `postUrl`): **`ImageURL`** (config `imageUrl`) and **`CallToAction`**
(config `callToAction`, case-insensitive, defaults to "Learn More"). An explicit
`PostURL` always wins — both are ignored when it is set. When `ImageURL` is set
and no `PostURL`, the client authors the post and feeds its `t3_` id into the
existing ad-creation path. `destination_url` reuses `buildRedditUTMURL` so the
post and ad share the same landing URL; the headline is the first variant's
headline, else the event name, capped at `maxRedditHeadlineRunes` (300).

Ordering and failure handling: the image URL / CTA / headline are validated UP
FRONT (before any mutating call). The post is authored BEFORE the paid campaign
(`Step 1.5`) — a dark post is zero-cost, so an authoring FAILURE degrades to a
campaign + ad group with no ad (the same state a missing `PostURL` produces)
rather than orphaning a paid resource; a caller cancellation mid-authoring is
fatal. The degrade step reports ONLY the HTTP status, never the error body,
because a reflective Reddit validation error can echo the `destination_url`
(which carries the caller's permitted secret-bearing query). The dispatch adapter
now also SANITIZES `ImageURL` (query/fragment stripped) in the config snapshot,
matching the existing `PostURL` handling — a signed image URL can carry a secret
and `config_snapshot` is stored unencrypted.

Tests: `internal/platform/reddit/authorpost_test.go` covers the happy path (post
body shape, dark flag, CTA case-insensitive resolution, authored id flowing into
the ad, posts-before-campaign call sequence, destination_url carrying the real
query), the degrade path (400 → no ad, status-only step, no secret leak), an
explicit `PostURL` winning over `ImageURL`, and pre-mutation validation (bad
scheme, userinfo, unknown CTA, over-long headline make no network call).
`internal/dispatch/reddit_test.go` gains `TestReddit_ConfigSnapshotRedactsImageURL`.
The concept file [`code/internal-platform-reddit.md`](../code/internal-platform-reddit.md)
gained an "Authoring a promoted post (brief -> servable ad)" section.
