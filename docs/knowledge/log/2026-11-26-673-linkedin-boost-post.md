# 2026-11-26 — #673 LinkedIn boost of an existing organization post

**Update** — `linkedInConfig.format: "boost"` with `linkedInConfig.boostPost`
(`urn:li:share:<id>` or `urn:li:ugcPost:<id>`, exactly) creates the ordinary campaign group and
SPONSORED_UPDATES campaign and ONE DRAFT creative whose `content.reference` is that post, instead
of a dark post per variant. Behind the new `LINKEDIN_ADS_BOOST_POST_ENABLED` (chart default
`"false"`; off, a synchronous 400 naming the flag).

- Before the campaign group POST the job reads the post (`GET /rest/posts/{encoded URN}`) and
  refuses it, nothing created, unless the account's organization authored it and it is
  `PUBLISHED`, `PUBLIC`, organic (`MAIN_FEED`), not a reshare, and text-only, an article, a single
  image or a video. A video post's campaign is created `SINGLE_VIDEO`. 404 and 403 are refusals of
  the post; a 401 is the connection's expiry. A member author is named only as "a LinkedIn
  member", never by person URN.
- Ad Format name segment `Boost`; no legacy-name lookup. On a campaign found by name, a creative
  already referencing the post (creatives finder, `contentReferences`) is reused. The finder's
  body must pass `identityjson`; a failed lookup creates nothing and returns no result, so the
  claim is released and the next create repeats the lookup. Same-name boosts in one process are
  serialised through the creative step by the event's group lock, so they post one creative.
- Result `boostedPostUrn`; snapshot `format` and `boostPost`. Variants, text ads, and `boostPost`
  under another format are refused synchronously.

Not yet probed live: the post read needs `r_organization_social` on the connection's token, and
the reference creative has not been created from this service. Run a paused probe before enabling.

Account grants (#581) are not scoped to a project: a project falling back to the LF system row may
name any grant, and a boost through it passes the post check for that grant's organization. That
is unchanged by this entry; tracked separately.
