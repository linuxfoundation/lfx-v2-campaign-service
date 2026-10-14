---
type: "Architecture Doc"
title: "Microsoft Creative Image Assets"
description: "How a Microsoft Advertising dispatch uses a stored creative asset: microsoftConfig.imageAssetId names one image of the brief, validated against Microsoft's limits before anything is created, then uploaded once with AddMedia and attached to the Search campaign as a campaign-level Image ad extension, with the ids recorded and a failed attach degrading the campaign as NOT attached or UNCONFIRMED."
resource: "internal/dispatch/microsoft_creative.go"
---

# Microsoft Creative Image Assets

`upload-creative-asset` stores a PNG/JPEG against a brief. Meta and Microsoft Advertising
consume stored assets; this page is the Microsoft half (LFXV2-2665). Code:
`internal/dispatch/microsoft_creative.go` (resolve + attach) and
`internal/platform/microsoft/image_extension.go` (validation + the four API calls).

## Why an Image ad extension

The service creates one Microsoft campaign type: a PAUSED Search campaign whose ad is a
ResponsiveSearchAd, which carries text assets only. Microsoft takes an uploaded image in two
places: the ResponsiveAd, which belongs to Audience campaigns this service does not create, and
the [ImageAdExtension](https://learn.microsoft.com/en-us/advertising/campaign-management-service/imageadextension?view=bingads-13),
which can be associated "with the account or with campaigns and ad groups in the account". The
extension is the narrowest integration that needs no new campaign type. It is associated at
CAMPAIGN level, so it applies to the campaign this dispatch created and nothing else in the
account.

## Config

`microsoftConfig.imageAssetId` (optional) and `microsoftConfig.imageAltText` (optional, at most
35 characters, defaults to the event name cut to 35, refused without an asset id). See
`docs/api-catalog.md` § MicrosoftConfig.

## Before anything is created

`resolveMicrosoftImage` runs before `CreateCampaign`. Every failure is a pre-create error, so the
claim is released and nothing exists upstream:

1. The id must parse as a uuid. It is canonicalized, so the snapshot never stores a caller
   spelling.
2. `GetAssetSize` is scoped to the dispatching brief's project and id. An asset of another
   brief or project is "does not exist for this brief".
3. Anything over `microsoft.MaxImageBytes` (76,800) is refused on its `byte_size`, before the
   blob is loaded. That number comes from the
   [Image](https://learn.microsoft.com/en-us/advertising/campaign-management-service/image?view=bingads-13)
   reference: `Data` "can contain a maximum of 102,400 characters" of base64, and 76,800 bytes
   encode to exactly 102,400 characters. Stored assets may be 30 MiB, so most real uploads are
   too large for Microsoft. They are refused, never resized.
4. The size is charged against the SHARED `AssetReserver`. `registerDispatchers` builds one
   reserver and hands it to both Meta and Microsoft, because two reservers would be two budgets.
   The reservation is held until the dispatch returns and is released by the resolver itself on
   any failure or panic.
5. `GetAsset` loads the bytes. Empty bytes are refused rather than silently creating an
   image-less campaign.
6. `microsoft.ValidateImage` checks: PNG or JPEG, header format matching the stored MIME type,
   GenericImage bounds (40x20 minimum, 2592x2048 maximum), and the alt text. Only the image header
   is decoded.

## After the campaign is confirmed

`Client.AttachCampaignImage` runs only when `CreateCampaign` returned no error. An UNCONFIRMED
create has no confirmed campaign to attach to. The calls:

1. [GetAdExtensionsAssociations](https://learn.microsoft.com/en-us/advertising/campaign-management-service/getadextensionsassociations?view=bingads-13)
   (`AdExtensionsAssociations/Query`, ImageAdExtension, Campaign). If an image extension is
   already associated, nothing is uploaded. This is the idempotency step:
   [AddMedia](https://learn.microsoft.com/en-us/advertising/campaign-management-service/addmedia?view=bingads-13)
   is not content-addressed (every call mints a new MediaId), so it is not safe to resend the way
   Meta's `/adimages` upload is. A lookup that cannot rule out an existing extension refuses to
   upload.
2. AddMedia (`Media`) with one `Image`, `MediaType` `GenericImage`, base64 `Data`. Sent once and
   never retried on 429.
3. [AddAdExtensions](https://learn.microsoft.com/en-us/advertising/campaign-management-service/addadextensions?view=bingads-13)
   with `ImageMediaIds` [MediaId], `Layouts` ["SearchSingle"] and `AlternativeText`. No
   `Images` field: the reference documents converting media ids to `Images`. No URL either: the
   extension inherits the ad's UTM-tagged destination.
4. [SetAdExtensionsAssociations](https://learn.microsoft.com/en-us/advertising/campaign-management-service/setadextensionsassociations?view=bingads-13)
   with `AssociationType` `Campaign`.

## Strict response decoding

Every 2xx body must actually answer before it counts. Each rule below fails closed:

- **Lookup.** `AdExtensionAssociationCollection` must be present as an array. `[]`, `[null]`
  and empty slots mean none. An omitted field, `null` or a non-array is a failed lookup, and
  nothing is uploaded. Associations with no `AdExtension`, or with an explicit `Type` other than
  `ImageAdExtension`, are skipped. An image association that reaches the entity check counts only
  when its `EntityId` is this campaign and its `AssociationType` is `Campaign`; otherwise it
  fails the lookup and is never treated as AlreadyAttached. Every image association is checked
  before any is trusted, and the first valid id is returned only when no other entry fails.
- **AddAdExtensions errors vs. truncation.** Only actual error evidence counts toward a definite
  rejection. That evidence is the whole-wire `AnyErrors` tallies, so an error beyond the
  16-entry retention window still counts. Truncation or null-only placeholders with a null
  identity stay UNCONFIRMED, because they explain nothing.
- **Association success.** A `PartialErrors` array that is present but carries no codes (for
  example `[null]`) is UNCONFIRMED, not success.
- **Raw-body integrity.** Every image-extension response body passes `identityjson.Check`
  before it is decoded. That rejects malformed UTF-8, unpaired surrogates, and keys declared
  twice, including under the case fold. Without it, a duplicate key could mark a field present
  from one spelling and decode it empty from the other. An untrustworthy lookup fails, and
  nothing is uploaded; an untrustworthy mutation body is UNCONFIRMED.
- **AddMedia.** `PartialErrors` beside `MediaIds` is decoded. A null id is a definite rejection
  only on whole-wire `AnyErrors` evidence, so a rejection past the 16-entry window still counts.
  Null placeholders, or a null id with no explanation, are UNCONFIRMED.
  A returned id next to any error entry is also UNCONFIRMED. The id is recorded, and the attach
  stops before AddAdExtensions.
- **AddAdExtensions.** `NestedPartialErrors` is `BatchErrorCollection[]`. It is decoded with
  `boundedNestedErrorItems`, the same decoder as geo, and both the collection-level codes and
  each nested `BatchErrors` are classified. A null identity with any error is definite. An
  identity returned alongside errors is UNCONFIRMED: its id is recorded, and the attach stops
  before association.
- **SetAdExtensionsAssociations.** The presence of `PartialErrors` is tracked as in
  `updateStatusResponse`. `null` or `[]` confirms success; an omitted field or a top-level
  `null` is UNCONFIRMED.
- **Error text.** An association failure's message makes no claim about whether the
  extension is attached. The NOT attached / UNCONFIRMED step wording decides that.

## Recording and outcomes

The ids are written to the persisted result as `imageMediaId` and `imageAdExtensionId`, the
Microsoft counterpart of the image_hash Meta records per ad. A retry that reuses the campaign by
name finds the extension in step 1 and records it without new media. Nothing else persists
across dispatches (no table, no migration): the orchestrator never re-dispatches a campaign
that already has a row, so a re-run reaches the image step only when the campaign was reused by
name, and step 1 covers exactly that case.

A failed attach does not fail the job. The campaign exists, so an error would retain the claim
for a reconcile that never re-runs this step. Instead the campaign is `created_degraded` with a
Steps entry:

- `Image extension NOT attached: …` for a definite failure: a 4xx, a PartialError explaining a
  null id, or a failed lookup (only a read was sent).
- `Image extension UNCONFIRMED (it may be attached — verify …)` for an ambiguous one: transport
  failure, 5xx, mutating 429, or a 2xx with no usable id.

Step text is the client's status-only error vocabulary and PartialError codes. Upstream message
text never appears. A failure after AddMedia leaves an unused library image, and one after
AddAdExtensions leaves an unassociated extension. Neither serves or spends, and their ids are in
the result.
