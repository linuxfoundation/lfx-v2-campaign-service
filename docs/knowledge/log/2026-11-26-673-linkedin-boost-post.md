# 2026-11-26 — #673 LinkedIn boost of an existing organization post

**Update** — `linkedInConfig.format: "boost"` with `linkedInConfig.boostPost`
(`urn:li:share:<id>` or `urn:li:ugcPost:<id>`, exactly) creates the ordinary campaign group and
SPONSORED_UPDATES campaign and ONE DRAFT creative whose `content.reference` is that post, instead
of a dark post per variant. Behind the new `LINKEDIN_ADS_BOOST_POST_ENABLED` (chart default
`"false"`; off, a synchronous 400 naming the flag).

- Before the campaign group POST the job reads the post (`GET /rest/posts/{encoded URN}`) and
  refuses it, nothing created, unless the account's organization authored it and it is
  `PUBLISHED`, `PUBLIC`, organic (`MAIN_FEED`), not a reshare, and text-only, an article, a single
  image or a video. A video post's campaign is created `SINGLE_VIDEO`. A 404 is a refusal of the
  post; a 401 is the connection's expiry. A 403 (the connection cannot read posts) skips these
  checks, sends no campaign format and lets LinkedIn's creative POST decide (see below). A member author is named only as "a LinkedIn
  member", never by person URN.
- Ad Format name segment `Boost`; no legacy-name lookup. On a campaign found by name, a creative
  already referencing the post (creatives finder, `contentReferences`) is reused. The finder's
  body must pass `identityjson`; a failed lookup creates nothing and returns no result, so the
  claim is released and the next create repeats the lookup. Same-name boosts in one process are
  serialised through the creative step by the event's group lock, so they post one creative.
- Result `boostedPostUrn`; snapshot `format` and `boostPost`. Variants, text ads, and `boostPost`
  under another format are refused synchronously.

- A creative LinkedIn refuses after the group and campaign exist returns the partial
  (`created_degraded`, campaign PAUSED), as a failed image creative does. `UGC_FORBIDDEN` and the
  reshare, visibility and feed-distribution `UGC_*` codes get service sentences; for
  `UGC_FORBIDDEN`: the connection's member needs an ADMINISTRATOR, DIRECT_SPONSORED_CONTENT_POSTER
  or CONTENT_ADMIN role on the authoring page.

Probed live 2026-11-26 (LinkedIn-Version 202611, paused probes, all cleaned up):

- With the LF token, `GET /rest/posts/{urn}` answers 403 `ACCESS_DENIED`
  (`partnerApiPostsExternal.GET`): the token lacks `r_organization_social`.
- On LF Events (ad account 509430019, org 208777), a paused WEBSITE_VISIT / MAX_CLICK campaign with
  no format, then `POST adAccounts/{a}/creatives {campaign, content: {reference:
  urn:li:share:…}, intendedStatus: DRAFT}`, gave 201. It reads back as
  `content.standardUpdateAd {contentAuthor: urn:li:organization:208777, reference}`; the
  creatives finder with `contentReferences=List(<post>)` finds it; LinkedIn set the campaign
  `format` to `STANDARD_UPDATE` itself.
- On CNCF (500928401) with a CNCF post, the same creative POST gave 403 `UGC_FORBIDDEN`
  ("Accessing the UGC resource is forbidden"): the token's member has no sponsoring role on that
  page.
- A DRAFT creative cannot be archived (not allowed from DRAFT to ARCHIVED unless reviewStatus is
  APPROVED), but it can be DELETEd.

So `r_organization_social` is optional: with it, ineligible posts are refused before anything is
created; without it, the create goes ahead and a post LinkedIn will not sponsor ends as a degraded
create.

**Update (review round 2)** — skipping the post read must not skip the author check, or a caller
could boost any post the connection's member may sponsor, another organization's page included.
Without the read, `finishBoost` now reads the creative back (`checkBoostCreativeAuthor`) and
accepts it only when `content.standardUpdateAd.reference` is the requested post and its
`contentAuthor` is the account's organization. Otherwise, or when the read fails, it DELETEs the
DRAFT creative it just created and fails with `ErrBoostPostIneligible` and no result, so nothing is
recorded and the campaign (left PAUSED with no ad) cannot be activated through the service. A
creative found on a reused campaign is checked too but never deleted, since it may be serving; the
error names it. A creative refusal on a reused campaign no longer claims the campaign is PAUSED.

Account grants (#581) are not scoped to a project: a project falling back to the LF system row may
name any grant, and a boost through it passes the post check for that grant's organization. That
is unchanged by this entry; tracked separately.
