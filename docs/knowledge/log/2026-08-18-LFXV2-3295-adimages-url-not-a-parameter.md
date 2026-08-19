# 2026-08-18 — LFXV2-3295 adimages url is not a parameter

**Fix** — the single-image creative path uploaded to `POST /act_<id>/adimages` with a
`url` field and attached the returned hash as `link_data.image_hash`. `url` is not a
create parameter on that edge. The upload would have been rejected against a live
account, and the rejection would have arrived AFTER the campaign and ad set were
already created — the variant then failing non-fatally, so single-image ads would
have silently never attached while the create still reported success.

Meta's reference for the ad-account `adimages` edge lists exactly two Creating
parameters: `bytes` (a base64 image file) and `copy_from` (`source_account_id` +
`hash`, to copy an image between accounts). `url` DOES appear on that page — as a
field of the RETURNED image object, alongside `url_128`, `hash`, `width`, `height`.
Reading an output field as an accepted input is what produced the bug, and it is the
specific way this class of mistake is easy to make: the name is present in the docs,
just on the other side of the call.

The replacement is `link_data.picture` on the creative itself, which the
`AdCreativeLinkData` reference documents as "URL of a picture to use in the post.
Specify this field or `image_hash` but not both. ... The image specified at the URL
will be saved into the ad accounts image library." That is the same by-URL feature —
Meta fetches server-side, this service still never dereferences a caller URL, so the
SSRF surface the original design avoided stays avoided — with one fewer round-trip
and no undocumented parameter. Because the two are mutually exclusive per that same
reference, only `picture` is sent, never `image_hash`. `/adimages` is now called
nowhere in the repo.

Note the reviewer's suggested alternative was `link_data.image_url`, quoted as "A URL
for the image for this creative. We save the image at this URL to the ad account's
image library." That string does not appear on the `AdCreativeLinkData` reference and
`image_url` is not a documented field there; `picture` is, carrying the equivalent
sentence. The right conclusion — stop using `/adimages` `url` — was reached through a
field name that would not have worked either, so the replacement was taken from the
parameter table rather than from the suggestion.

**A design comment is not evidence about a vendor contract.** The removed code
explained at length why by-URL upload was chosen over multipart bytes, and the
reasoning was sound; none of it established that the endpoint accepted the field. The
`httptest` fixtures then encoded the same assumption on the server side, so the tests
agreed with the client about a contract neither had checked. A fake cannot falsify the
belief that built it — only the primary reference can, which is where this was settled.

**Privacy: the caller image URL could reach persisted `Steps`.** Every hand-built
error string in the image path carefully omitted the URL, but on a Meta rejection the
`*APIError` was returned verbatim, and `APIError.Message` is the Graph message copied
unmodified — which for a rejected parameter commonly echoes that parameter's value. It
flowed into the per-variant handler and was rendered by `truncateErr`, which only
clamps length. A caller URL may be pre-signed, and a signature is a bearer credential
granting time-boxed read access, so `Steps` — which are persisted and logged — became
an undisclosed sink for it. Moving the URL from the upload body to a creative
parameter does not retire this: it is now a parameter Meta can reject and echo.

The fix scrubs at the SINK, not at each error site: `scrubURLFromErr` replaces the
caller's URL — verbatim or percent-encoded — with its `redactURL` form (scheme+host+
path) before the message is rendered into a step, keeping which image failed while
dropping the query, fragment, and userinfo. Sink-side is the load-bearing choice: a
message reflected from upstream can carry the URL in a form no error site anticipated,
so guarding the point of persistence covers paths the individual `fmt.Errorf` calls do
not. This mirrors `displayMetaUTMURL` — the full value still goes to Meta, only the
persisted copy is sanitized.

**The leak had four sinks, not one.** The review cited the plain `Ad %d failed` step;
the ambiguous branch renders two more (`UNCONFIRMED`, with and without an orphaned
creative id), and the failed branch a fourth with the creative id. All four take the
same error and all four were unscrubbed. Fixing only the quoted line would have left
the leak reachable through the ambiguous path — the likelier one for a 5xx. A
mutation reverting just the two `UNCONFIRMED` sinks to `truncateErr` still fails the
privacy test, which is what proves the sibling paths are covered rather than assumed.

Mutation-tested, each reverted after: disabling the scrubber, skipping the
percent-encoded form, dropping `redactURL` from the replacement, reverting the
ambiguous sinks — all caught by the URL-echo tests; attaching `picture`
unconditionally — caught by the additive test; sourcing every variant's picture from
variant 1 — caught by the isolation test; and removing the pre-create URL validation —
caught by the pre-spend money guard, which still proves no mutating call is made for a
malformed URL. Campaigns remain created PAUSED and nothing in this change can publish
or spend.
