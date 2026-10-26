# 2026-10-26 — Google creative images accept a stored asset id (LFXV2-2665)

**Update** — Google was the last of the six ad platforms whose creative image could
not come from the asset library the UI uploads into. Every Google image slot now
accepts an `imageAssetId` alongside the URL it already took, and the dispatcher
resolves those bytes locally before anything upstream exists. New concept file:
[Google Ads Creative Image Assets](../architecture/google-creative-assets.md).

## Why not reuse Reddit's signed URL

Reddit solves the same problem by minting a short-lived signed public URL and
handing it to the vendor. Google never fetches a URL — `ImageAsset` takes raw
bytes, and the service was already downloading every creative URL to POST them —
so a signed URL would have meant this service serving bytes to itself over HTTP,
and would have put the whole feature behind
`creative-asset-url-signing-key` being configured. Resolving through `GetAsset`
behind the existing `AssetReserver` reuses the admission control that is already
the bound on asset bytes held in memory, and needs no new configuration.

## The config shape was forced, not chosen

The natural shape — a second `[]string` of ids beside the existing URL list —
is silently broken. `postgres.collectSnapshotAssetIDs` finds an asset reference
**by key**: a string directly under a key matching `imageassetid`, case
insensitively. Array *elements* carry no key, so every id in a flat list would
be skipped and `checkNewSnapshotAssetRefs` would read as passing while the
snapshot named assets that no longer exist.

So an asset-backed entry is an **object**, matching what `meta.AdVariant` and
`linkedin.CreativeVariant` already do, and a slot entry became a ref that is
either a URL or an id. `googleAdsImageRef.MarshalJSON` is deliberately
asymmetric — a URL that arrived as a bare string re-encodes as a bare string —
because `googleAdsSnapshotConfig` re-marshals the config to persist it, and the
object form would otherwise rewrite the shape of every existing snapshot on its
next write.

## Fourteen slots, one enumeration

Google has fourteen image slots across three creative channels (five Demand Gen,
five Performance Max, four Display) where the siblings have one. They are
enumerated once, by pointer, in `googleAdsCreativeSlots`, and the resolve, the
config-key contract test and the fixture that exercises every slot all drive off
that list. A slot added later and left out of it resolves nothing and would fail
at the fetch, which is a far later and worse place to find out — so
`TestGoogleAdsConfigNamesItsAssetInEverySlot` counts one key per enumerated slot
rather than merely checking that some slot contributes one.

Four comments on this branch said *eleven* slots, a count from an earlier plan
that never matched the code. Corrected to fourteen in
`googleads_creative_assets.go`, `config_asset_key_test.go`,
`signed_asset_wiring_test.go` and `container.go`.

## Ordering and the budget

`resolveGoogleCreativeAssets` runs in `Dispatch` before `googleads.CampaignInput`
is built, which is before the budget mutate, and every failure is wrapped
`notCreated` — the same position, for the same reason, as the client's fetch of
a creative URL. The reservation is taken **before** the read, as
`MetaDispatcher.resolveVariantAssets` does, and released structurally through a
`succeeded` flag so a panic releases it too; the caller holds it for the whole
dispatch because the bytes ride in the `CampaignInput`.

`bindCreativeAssetSharing` re-points the Google dispatcher at the container's one
`AssetReserver` on both live paths. Left out, it would charge
`registerDispatchers`' own reserver and reintroduce the two-budget defect #311
found on X — and Google is the heaviest of the five holders, since a Performance
Max config can name an asset in every slot.

## The boundary token

The platform client keeps flat `[]string` slot lists — 84 non-test references
made a struct conversion a PR-size problem on its own — so `slotEntries()`
renders each ref to one string, an asset as `lfx-asset:<canonical-uuid>`, and the
bytes travel beside it in `CampaignInput.ResolvedImages` keyed by that exact
string. `googleads.ResolvedImageRefPrefix` is exported and `internal/dispatch`
uses it rather than repeating the literal, because the entry and the map key are
two halves of one contract and the drift would be silent: the client would try to
fetch `lfx-asset:<uuid>` over HTTP.

Ids are canonicalized with `uuid.Parse().String()` **back into the config**, so a
caller naming one asset in two valid spellings gets one key, one read, one
charge, and one slot entry rather than two the duplicate rule would then refuse.

## A pre-resolved image skips nothing

`validateImageEntries` refuses a token with no bytes behind it as a caller
invariant failure, in the shape phase, and applies the duplicate rule to tokens
as to URLs. `fetchSlotImages` then runs the bytes through the same
`validateImageBytes` the downloaded path runs — per-image cap, `image.DecodeConfig`,
minimum size and aspect ratio — and counts them into the same
`maxCreativeTotalImageBytes` total. None of those checks is a property of HTTP,
and skipping them would make an asset-backed slot the one way to get an
out-of-spec image past preflight and into the ad mutate, which runs after the
budget and campaign have committed.

`imageRefForError` renders a token verbatim. `redactURLForError` is wrong for one
and wrong silently: `lfx-asset:<uuid>` parses and is absolute but has no `Host`,
the one condition it treats as unrenderable, so a refusal naming an asset would
collapse to `(redacted)` and name nothing. There is also nothing to redact — the
id is this service's own primary key, not a credential, which is also why
`sanitizeSnapshotImageRefs` reduces a URL and leaves an id alone.

## Test note

`pngOf`'s near-uniform image encodes to a few hundred bytes at any dimensions and
cannot exercise a byte cap. The creative-total test uses a fixed-seed
incompressible noise PNG instead, shared under many distinct map keys so the
running total sees every reference while the test allocates one image.
