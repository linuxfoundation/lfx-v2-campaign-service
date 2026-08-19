# 2026-08-18 — Meta single-image ad creatives (LFXV2-3295)

**Update** — Meta creatives already existed: `createVariantAd` has always built a
website-click `object_story_spec.link_data` creative (page id, UTM link, primary
text, headline, optional description) and POSTed it to `/act_<id>/adcreatives`.
What it could NOT do was carry an image, so every Meta ad rendered as a bare link
ad. This ticket closes exactly that gap rather than adding creatives from nothing.

`AdVariant` gains an OPTIONAL `ImageURL`. When set, `createVariantAd` attaches it
as `link_data.picture` on the creative it already builds — the field the
`AdCreativeLinkData` reference documents as "URL of a picture to use in the post.
Specify this field or `image_hash` but not both. ... The image specified at the URL
will be saved into the ad accounts image library." Because the two are mutually
exclusive per that same reference, `image_hash` is never sent. When `ImageURL` is
empty the creative body is byte-for-byte what it was before — the field is additive
and no existing caller changes behavior.

There is NO separate upload call: `/act_<id>/adimages` is called nowhere in the
repo. An earlier revision of this change did upload there with a `url` field and
attach the returned hash; `url` is not a create parameter on that edge, and the
whole detour was removed. That correction, and why the reviewer's suggested
`link_data.image_url` was also not the right key, is recorded in
`2026-08-18-LFXV2-3295-adimages-url-not-a-parameter.md`.

Attaching BY URL is deliberate: Meta fetches the image server-side, so this client
never dereferences the caller's URL and gains no outbound-fetch/SSRF surface, and
the whole creative stays a single JSON create inside the existing hardened `do()`
request path instead of needing a second, separately-bounded transport.

Money-safety follows the `buildPromotedObject` precedent — validate before the
first mutating call. `validateVariantImageURL` (absolute, https, no embedded
userinfo) runs in the up-front per-variant loop next to the copy-limit checks, so a
malformed URL is a clean pre-spend rejection instead of a failure landing after the
campaign and ad set already exist. Userinfo is rejected specifically because Meta
fetches the URL and would otherwise be handed the basic-auth secret.

Retry policy is unchanged and load-bearing: the creative goes through `doCreate`,
so a 429 is NOT retried. The uniform rule "a mutating POST is never auto-repeated"
is what keeps the paid creates safe, so no per-endpoint exception was carved out.

A creative rejected over its picture stays non-fatal for that variant, is surfaced
in `Steps` with the existing partial-result shape, and never emits the caller's URL
(it may carry a pre-signed query) or the access token into an error string. The
persisted-sink handling for that URL — both the `Steps` renderings and the
`config_snapshot` — is recorded in `2026-08-18-LFXV2-3295-presigned-image-url-sinks.md`.

No dispatcher change was needed: `meta.AdVariant` carries no json tags, so
`encoding/json` matches the UI's `imageUrl` key case-insensitively onto `ImageURL`.
Because that mapping is IMPLICIT, `TestMeta_DispatchMapsVariantImageURL` sends the
literal `imageUrl` key over the wire and asserts it reaches the creative's
`link_data.picture` — a field rename would otherwise silently drop every image
while still reporting success.

**Verification** — mutation tests, each caught by the specifically relevant
assertion: dropping the `picture` attachment; attaching `picture` unconditionally
(caught by `TestCreateCampaignNoImageOmitsPicture`); sourcing every variant's
picture from variant 1 (caught by `TestCreateCampaignPerVariantImageIsolation`);
sending the image URL as the creative's click `link`; skipping the up-front URL
validation; dropping the https check; dropping the userinfo check; leaking the
image URL into an error string; creating the ad anyway after a failed creative;
and renaming the `ImageURL` field (caught by the dispatcher wire-contract test).
