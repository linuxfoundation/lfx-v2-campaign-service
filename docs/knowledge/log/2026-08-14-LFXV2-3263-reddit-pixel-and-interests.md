# 2026-08-14 — LFXV2-3263 Reddit conversion pixel and interest fallback

**Fix** — Reddit campaign creation could not succeed from the UI at all. Two
defects, both found by testing against the LIVE ad account rather than by
reading the API documentation — and in both cases the documentation is what the
code had been written from.

The conversion pixel was gated on `objective == "conversions"`, which is what
Reddit's docs describe. The live API disagrees: a CLICKS/Traffic create was
rejected with
`{"field":"conversion_pixel_id","message":"conversion_pixel_id is required"}`
and accepted unchanged once the pixel was supplied. It is now sent for EVERY
objective, and a create with none configured is refused before any upstream
call rather than after one.

The pixel is stored on the CONNECTION (migration 000025, a
`conversion_pixel_id` column plus the `providerConfigKeys` entry that gives the
repository its column list). It identifies the advertiser's pixel — one per ad
account, the same for every campaign created through it — so asking for it per
campaign would make an account-level constant something an operator can get
wrong once per campaign. A per-campaign value still overrides when present.
Nothing is backfilled: an existing connection genuinely has none, and any guess
would attribute conversions to a pixel that is not that advertiser's.

The second defect only surfaced once the first was fixed. Reddit's interests are
opaque ids (`technology_v3`), while the brief generator produces human labels
("Artificial Intelligence", "Machine Learning") — so the ad-group create was
refused with "You cannot set invalid interests", and the PAUSED campaign created
moments earlier was ORPHANED. Interests now join communities in the existing
400-triggered fallback: `baseTargeting` carries neither, so the retry drops them
by construction rather than by remembering to.

Both dimensions are dropped on ONE retry even when the 400 names only one.
Retrying per-dimension would send a second doomed request, and every extra
ad-group POST is another chance to create one nothing points at. The Steps trail
names each dropped dimension, because interests and communities are re-added in
different places in Reddit Ads Manager.

**Verification** — Proven end to end against the live LF ad account: campaign
`2568197354830044390` and ad group `2568197363222933184` created (both PAUSED),
with the Steps trail recording "Community and interest targeting both rejected …
retrying without either". That is the both-rejected path, exercised for real.

**Note** — The same label-vs-id mismatch affects SUBREDDITS: all 15 the brief
supplied were rejected. The fallback now recovers from it, but every Reddit
campaign consequently runs on keywords and geo alone until the brief generator
emits Reddit's own ids (or the client resolves labels through
`GET /targeting/interests` and `/targeting/communities`). Tracked as LFXV2-3261 —
this change makes the failure survivable, it does not make the targeting work.

A test that asserted the documented behaviour
(`TestCreateCampaign_PixelIgnoredForNonConversion`) was REPLACED, not extended:
it pinned the broken contract, so restoring the docs-shaped gate would have made
the suite green again.
