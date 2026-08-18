# 2026-08-18 — Meta single-image ad creatives (LFXV2-3295)

**Update** — Meta creatives already existed: `createVariantAd` has always built a
website-click `object_story_spec.link_data` creative (page id, UTM link, primary
text, headline, optional description) and POSTed it to `/act_<id>/adcreatives`.
What it could NOT do was carry an image — there was no `image_hash` in
`link_data`, and `/act_<id>/adimages` was never called anywhere in the repo, so
every Meta ad rendered as a bare link ad. This ticket closes exactly that gap
rather than adding creatives from nothing.

`AdVariant` gains an OPTIONAL `ImageURL`. When set, `createVariantAd` first calls
the new `uploadAdImage` (`POST /act_<id>/adimages` with a `url` field, so META
fetches the image server-side) and attaches the returned content-addressed hash as
`link_data.image_hash`. When empty, no upload call is made and the creative body is
byte-for-byte what it was before — the field is additive and no existing caller
changes behavior.

Uploading BY URL (rather than multipart bytes) is deliberate: the client never
dereferences the caller's URL, so it gains no outbound-fetch/SSRF surface, and the
call stays inside the existing hardened JSON request path instead of needing a
second, separately-bounded transport.

Money-safety follows the `buildPromotedObject` precedent — validate before the
first mutating call. `validateVariantImageURL` (absolute, https, no embedded
userinfo) runs in the up-front per-variant loop next to the copy-limit checks, so a
malformed URL is a clean pre-spend rejection instead of a non-fatal failure landing
after the campaign and ad set already exist. Userinfo is rejected specifically
because Meta fetches the URL and would be handed the basic-auth secret.

Response handling refuses to guess. `/adimages` returns an `images` OBJECT keyed by
an arbitrary per-request key (Meta echoes the source filename), so the hash is read
from the sole entry; zero entries, more than one entry, or an empty/whitespace hash
are all returned as `transportError` — i.e. AMBIGUOUS ("may exist, verify"), never a
clean failure, since Meta may have stored the image. Picking arbitrarily from a Go
map would be worse than failing: map iteration order is randomized, so a
multi-entry reply could attach a DIFFERENT image to a paid ad on each run.

Retry policy is unchanged and load-bearing: the upload goes through `doCreate`, so a
429 is NOT retried. `/adimages` has no idempotency key. (The endpoint is
content-addressed and so naturally idempotent upstream, but the uniform rule "a
mutating POST is never auto-repeated" is what keeps the paid creates safe, so no
per-endpoint exception was carved out.)

Ordering: the image is uploaded BEFORE the creative. An ad image is a free,
unpublished library asset whereas the creative and ad are the resources that matter
for clean-up, so a failure at any step leaves at most a stray library image — never
a creative referencing an image that failed to upload. An upload failure stays
non-fatal per-variant, is surfaced in `Steps` with the existing partial-result
shape, and never emits the caller's URL (it may carry a pre-signed query) or the
access token into an error string.

No dispatcher change was needed: `meta.AdVariant` carries no json tags, so
`encoding/json` matches the UI's `imageUrl` key case-insensitively onto `ImageURL`.
Because that mapping is IMPLICIT, `TestMeta_DispatchMapsVariantImageURL` sends the
literal `imageUrl` key over the wire and asserts it reaches `/adimages` — a field
rename would otherwise silently drop every image while still reporting success.

**Verification** — 13 mutation tests, every one caught by the specifically relevant
assertion: dropping the `image_hash` attachment; writing the hash into `description`
instead; sending the image URL as the creative's click `link`; skipping the up-front
URL validation; dropping the https check; dropping the userinfo check; accepting a
multi-image reply; accepting an empty hash; leaking the image URL into an error
string; retrying the upload on 429; uploading when `ImageURL` is empty; creating the
creative anyway after a failed upload; and renaming the `ImageURL` field (caught by
the dispatcher wire-contract test).
