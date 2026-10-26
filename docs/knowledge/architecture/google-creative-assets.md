---
type: "Architecture Doc"
title: "Google Ads Creative Image Assets"
description: "How a Google Ads dispatch uses stored creative assets: any of the fourteen Demand Gen, Performance Max and Display image slots accepts an imageAssetId object beside a plain URL string, the dispatcher canonicalizes and loads those bytes under the container's shared asset budget before anything upstream exists, and hands them to the platform client as ResolvedImages keyed by the lfx-asset slot entry it rendered, where a pre-resolved image runs the same per-image cap, decode and geometry checks as a downloaded one."
resource: "internal/dispatch/googleads_creative_assets.go"
---

# Google Ads Creative Image Assets

`upload-creative-asset` stores a PNG/JPEG against a brief. Meta, Microsoft Advertising, Reddit,
X and LinkedIn already consume stored assets; this page is the Google Ads part (LFXV2-2665).
Code: `internal/dispatch/googleads_image_ref.go` (the config shape),
`internal/dispatch/googleads_creative_assets.go` (resolve + budget) and
`internal/platform/googleads/demandgen_creative.go` (validation, shared by both sources).

## Why Google is different from its five siblings

The other five platforms hand the vendor a URL or upload to a vendor-side media endpoint.
Google will not fetch a URL at all — `ImageAsset` takes raw bytes — so the campaign service was
already downloading every creative URL and POSTing the bytes. A stored asset therefore changes
only **where the bytes come from**, not what happens to them: the resolve replaces the download,
and everything after it is the path that already existed.

The second difference is the number of places an image can appear. Google has **fourteen** image
slots across three creative channels — five Demand Gen, five Performance Max, four Display —
where the siblings have one slot, or one per variant. `googleAdsCreativeSlots` enumerates them
by pointer, and the resolve, the config-key contract test and the fixture that exercises every
slot all drive off that one enumeration rather than off a hand-written list.

## Config

Every image slot takes a list whose entries are **either** a plain URL string, exactly as before,
**or** an object `{"imageAssetId": "<uuid>"}` (`{"url": "…"}` is also accepted). The two shapes
mix freely in one slot. `CampaignCreateInput.config` is declared `Any`, so this needed no Goa
change.

An asset entry has to be an **object**, and that is a hard constraint rather than a style choice.
`postgres.checkNewSnapshotAssetRefs` refuses a `config_snapshot` that newly names an asset which
no longer exists, and `collectSnapshotAssetIDs` finds those references **by key** — a string
directly under a key matching `imageassetid`, case-insensitively. Array *elements* carry no key.
A bare uuid inside the existing `[]string` slot, or a flat `[]string` under a new key, would be
silently skipped and the dangling-reference guard would read as passing. See
`internal/dispatch/config_asset_key_test.go`, which both pins the key and counts one key per
enumerated slot, so a slot added later cannot contribute nothing unnoticed.

`googleAdsImageRef`'s `UnmarshalJSON` and `MarshalJSON` are deliberately **asymmetric**: a URL
that arrived as a bare string is re-encoded as a bare string. `googleAdsSnapshotConfig`
re-marshals the config to persist it, so encoding every URL as `{"url": "…"}` would silently
rewrite the shape of every pre-existing snapshot on its next write.

`MarshalJSON` decides which shape to write with `namesAsset()`, the same predicate the resolver,
`slotEntries` and `sanitizeSnapshotImageRefs` use. Testing the raw string instead — as it did
until the review fix — disagreed with all three for a whitespace-only id: the resolver trims, so
such an entry is URL-backed and is never canonicalized, yet the encoder still wrote it as an
object carrying an `imageAssetId` key that `collectSnapshotAssetIDs` skips. Nothing dangled,
because that collector parses the id, but the snapshot recorded a shape no other path produces —
the divergence `TestPlatformConfigsNameTheirAssetUnderOneKey` exists to keep out.

`sanitizeSnapshotImageRefs` reduces a URL — `config_snapshot` is persisted **unencrypted** and a
signed query string IS the credential — and leaves an asset id alone. The id is this service's
own primary key and is the exact value the dangling-reference guard reads back out of the
snapshot; reducing it would make the stored reference unresolvable and the guard blind.

The exemption is a property of the **id field**, not of the entry, and the URL reduction is
therefore **unconditional**. Keying it on `namesAsset()` left a signed URL unreduced whenever an
entry carried both — such an entry is refused at resolve time, but refusal and persistence are
different paths, so "it would have been refused" is no argument that the value never reaches the
snapshot. Reducing every entry is safe because `redact.SnapshotURL("")` returns `""`, so the
asset-backed entries that exist in practice — id only, no URL — round-trip unchanged.

## Before anything is created

`resolveGoogleCreativeAssets` runs in `Dispatch` before `googleads.CampaignInput` is built, which
is before the budget mutate. Every failure is a `domain.PreflightRefusal` wrapped `notCreated`:
the claim is released and no budget, campaign or ad group was mutated. The same placement, and
the same reason, as the client's fetch of a creative URL.

1. An entry naming **both** a URL and an asset id, or **neither**, is refused with the slot name
   and the caller's own 1-based position.
2. Each id must parse as a uuid and is **canonicalized back into the config**, so two valid
   spellings of one uuid are one asset: read once, charged once, and rendered as one slot entry
   rather than two that the client's duplicate rule would then refuse.
3. `GetAssetSize` is scoped to the brief's project and id, so an asset of another brief or
   project is "does not exist for this brief". The size is read as one BIGINT — the bytes are not
   loaded to find out how big they are.
4. The running total of the **distinct** assets is checked against `maxVariantAssetBytes`, then
   charged against the `AssetReserver`. The reservation is taken **before** the read: charging
   afterwards bounds nothing, because every concurrent dispatch would already hold its full
   allowance by the time it blocked on the semaphore.
5. `GetAsset` loads the bytes. Zero stored bytes is refused rather than passed through as an
   image the client must fetch and cannot.

The reservation is released by a `defer` on a `succeeded` flag, so a panic releases it too, and
is held for the **whole dispatch** rather than just the resolve — the bytes are, since they ride
in the `CampaignInput` and are POSTed to Google later in the same call.

On both live paths `bindCreativeAssetSharing` re-points the Google dispatcher at the container's
ONE reserver, the one the Meta, Microsoft, X and LinkedIn dispatches and the signed-asset route
also charge. `registerDispatchers`' own reserver would be a second budget, which is exactly the
defect #311 found on X. A container test pins the **identity**, not just that a reserver is set.
Google is the heaviest holder of the five: a Performance Max config can name an asset in every
slot.

## The dispatch/platform boundary

The platform client keeps flat `[]string` slot lists. `slotEntries()` renders each ref to one
string — a URL unchanged, an asset as `lfx-asset:<canonical-uuid>` — and the bytes travel beside
it in `CampaignInput.ResolvedImages`, keyed by that exact entry string.

`googleads.ResolvedImageRefPrefix` is **exported** and `internal/dispatch` uses it rather than
repeating the literal: the entry string and the map key are two halves of one contract, and a
contract spelled twice can drift. The drift would be silent — the client would try to fetch
`lfx-asset:<uuid>` over HTTP. A URI scheme rather than an invented sentinel: that is what the
string IS at that point, the identity of an image that is not at a URL.

The scheme is **reserved at the trust boundary**, and not — as this page and three code comments
claimed until the review fix — by the client's https rule. That rule never sees such a string:
`validateImageEntries` tests the prefix *before* it parses the URL, so a caller who writes
`"lfx-asset:…"` straight into a slot lands in the prefix branch. `resolveGoogleCreativeAssets`
therefore refuses a URL-backed entry whose URL carries the prefix, naming the slot and position
and **not** echoing the value: all that is known about it there is its prefix, and a creative URL's
signed query string can itself be the credential for the image. That resolver is the only place
that can still tell a caller's string from one this service rendered — after `slotEntries()` the
two are the same `[]string`.

That map is assigned at exactly **one** site, `ResolvedImages: resolvedImages` in
`internal/dispatch/googleads.go`, and nothing below the dispatch layer can notice if the line
goes away: the resolver's own tests call `resolveGoogleCreativeAssets` directly and the platform
side's tests build `CampaignInput` themselves, so both suites stay green while every
stored-asset campaign fails on the money path. `googleads_resolved_images_wiring_test.go` closes
that hole from the outside — a Demand Gen config naming stored assets, dispatched over the fake
API server, asserting the stored **bytes** appear base64-encoded in the `assets:mutate` body and
that the asset ids do not. One channel is enough because all three share that single assignment
and `fetchSlotImages` below it; what differs is the per-channel mapper, and each of those is
already pinned platform-side. (The dial-guard constraint recorded in
`googleads_display_wiring_test.go` is about *fetching*; a pre-resolved entry is never fetched, so
this path reaches `assets:mutate` with no test-only hole.)

## What a pre-resolved image does not skip

`validateImageEntries` checks a token against the supplied map instead of against the URL rules —
absent bytes are a **caller invariant failure**, refused in the shape phase with every other
preflight refusal — and applies the duplicate rule to tokens exactly as to URLs.

Its first act in that branch is the **suffix check**, before the entry is echoed anywhere:
`isResolvedAssetID` requires the canonical 8-4-4-4-12 lowercase hex that `uuid.UUID.String()`
renders on the dispatch side, and anything else is refused naming the slot and position only.
This is the client-side backstop to the reservation above, and it is spelled out rather than
taken from `github.com/google/uuid` — no package under `internal/platform` depends on that
module, and the contract here is not "some uuid" but "the one spelling our own resolver writes".

`fetchSlotImages` then runs a pre-resolved image through `validateImageBytes`, the same function
the downloaded path runs: the per-image size cap, that there are any bytes, `image.DecodeConfig`
(decoded, never a Content-Type sniff) and the slot's minimum size and aspect ratio. It also
counts into the same running `maxCreativeTotalImageBytes` total. None of those checks is a
property of HTTP, and an image that skipped them would be the one way to get a wrong-ratio asset
past the preflight and into the **ad mutate**, which runs after the budget and campaign have
committed.

`imageRefForError` renders a token verbatim and a URL reduced. `redactURLForError` is wrong for a
token and wrong silently: `lfx-asset:<uuid>` parses and is absolute but has no `Host`, the one
condition it treats as unrenderable, so a refusal naming an asset would collapse to the literal
`(redacted)` and name nothing. There is also nothing to redact — the id is a primary key, not a
credential.

## The per-image cap is applied twice, and the first time is on the SIZE

`resolveGoogleCreativeAssets` refuses an asset larger than `googleads.MaxImageBytes` immediately
after `GetAssetSize` and **before** `reserve` or `GetAsset`, so an oversized blob is never
materialised and no allowance is taken for it. This is the same placement the aggregate
`maxVariantAssetBytes` ceiling already used, and the same shape the LinkedIn, X and Microsoft
dispatch paths in this package already had against their own `<platform>.MaxImageBytes`
(`linkedin_creative.go`, `twitter_creative.go`, `microsoft_creative.go`); Google's new path was
the only sibling missing it.

The bound comes from the `googleads` **package** — the same constant every slot in
`ImageSlotCatalog` carries and the one the fetch path enforces with a `LimitReader` — rather than a
literal spelled a second time in `internal/dispatch`, so the two halves cannot drift. It is **not**
Google's own limit, which is larger: it is the bound on what this service will hold in memory per
image, and the refusal says so rather than borrowing the siblings' "the vendor rejects this"
wording, which on those platforms names a genuine vendor cap. It was previously named
`maxDemandGenImageBytes`, which was a misnomer: `fetchSlotImages`, `fetchOneImage` and
`validateImageBytes` are shared by Demand Gen, Display and Performance Max alike, so the cap was
never Demand Gen's. Exporting it as `MaxImageBytes` matches the three siblings deliberately.
