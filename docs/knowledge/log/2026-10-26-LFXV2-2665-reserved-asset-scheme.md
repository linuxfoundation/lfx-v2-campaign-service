# 2026-10-26 — The resolved-asset scheme is reserved against caller image URLs (LFXV2-2665)

**Fix** — The local pre-PR review of the Google asset-id branch found that
`lfx-asset:` was reachable by a caller, and that three comments and one
architecture page asserted a mechanism that does not run. Both are fixed in one
commit, together with the API catalog, which still told callers an image slot
takes only URL strings.

## The defect

`validateImageEntries` tests `ResolvedImageRefPrefix` **before** it parses the
URL and applies the https/host rules. Nothing upstream stopped a caller from
putting a string carrying that prefix into a slot: `googleAdsImageRef`'s
bare-string arm reads it as a URL ref with an empty `ImageAssetID`, so
`resolveGoogleCreativeAssets` ignored it and `slotEntries()` rendered it
unchanged. It then landed in the prefix branch, missed the `resolved` map, and
the refusal echoed it verbatim through `capForError` into an error that
persists **unencrypted** as a Steps entry — a creative URL's signed query string
being the credential for the image. The two branches immediately below it each
carry a comment refusing to echo the caller's value, so this was an internal
inconsistency in one function rather than a difference of opinion about risk.

## Fixed at two layers, deliberately

1. **The trust boundary.** `resolveGoogleCreativeAssets` refuses a URL-backed
   entry whose URL begins with `googleAdsImageRefScheme`, naming the slot and
   the caller's 1-based position and **not** the value. That resolver is the
   only place that can still tell a caller's string from one this service
   rendered; after `slotEntries()` both are the same `[]string`.
2. **The client backstop.** The prefix branch's first act is now a suffix check:
   `isResolvedAssetID` requires the canonical 8-4-4-4-12 lowercase hex that
   `uuid.UUID.String()` produces, checked before anything is echoed. Spelled out
   rather than imported — no package under `internal/platform` depends on
   `github.com/google/uuid`, and the contract is "the one spelling our own
   resolver writes", not "some uuid".

Reordering the branches instead would have been the smaller edit and the wrong
one: a resolved entry is not a URL, so sending it through `url.Parse` and the
https rule first means the https refusal becomes the message a caller sees for a
missing-bytes invariant failure.

## Comments that described a mechanism that does not run

The `googleAdsImageRefScheme` and `ResolvedImageRefPrefix` doc comments and the
scheme test all said the https rule prevents collision with a caller's entry. It
cannot: the prefix is tested first, so such a string never reaches that rule.
All three now say where the reservation actually happens. The same sentence was
in `docs/knowledge/architecture/google-creative-assets.md` and is corrected
there.

## One more predicate that did not agree

`googleAdsImageRef.MarshalJSON` branched on `r.ImageAssetID == ""` while `namesAsset()` — used by
the resolver, `slotEntries` and `sanitizeSnapshotImageRefs` — trims first. A whitespace-only id is
URL-backed to all three and is never canonicalized, but the encoder still wrote it as an object
carrying an `imageAssetId` key. Nothing dangles, because `collectSnapshotAssetIDs` parses the id
and skips it, so this is a persisted-shape divergence rather than a guard bypass — exactly the
class `TestPlatformConfigsNameTheirAssetUnderOneKey` exists to keep out. `MarshalJSON` now branches
on `namesAsset()`, so one predicate answers the question everywhere.

## A concept page that still promised the fetch

`docs/knowledge/code/internal-platform-googleads.md` describes the package whose
creative files this branch changed, and it stated the download as an
unconditional property of every image in three places — the Demand Gen
hardened-transport list, the Performance Max `fetchSlotImages` sentence and the
Display `fetchDisplayImages` one. An entry carrying `ResolvedImageRefPrefix` runs
none of that: `fetchSlotImages` finds it in `CampaignInput.ResolvedImages` and
never dials. All three now name the second source and separate the rules that are
properties of the **download** — https, no redirects, the public-IP dial guard —
from those that are properties of the **bytes**, which run on both paths through
`validateImageBytes` and the one running 64 MiB creative total.

## Also in this commit

`describeGoogleAdsImageRef` had no production caller — only its own test — and
is deleted with it; `"fmt"` left `googleads_image_ref.go` with it.

`docs/api-catalog.md` declared all fourteen Google image slots as `string[]` and
told callers images "are given as https URLs that THIS SERVICE fetches". Both
have been untrue since the asset-id commit. The three creative blocks now
declare `ImageRef[]` and state the two entry shapes, that a stored asset is read
rather than fetched, and that it runs the identical checks and counts into the
same 64 MiB creative total.
