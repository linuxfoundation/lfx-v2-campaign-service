---
type: "Architecture Doc"
title: "X Ads Creative Image Assets"
description: "Stored creative images on X Ads: an imageAssetId on the X campaign config is uploaded through the X API v2 chunked media upload, added to the ad account Media Library under a SHA-256-derived name that a retry reuses, and attached as media_keys on the authored promoted-only tweet, all before the campaign POST."
resource: "internal/platform/twitter/media.go"
---

# X Ads Creative Image Assets

LFXV2-2665. Before this, only the Meta dispatcher consumed assets stored by
`upload-creative-asset`. X campaigns can now use one as the image of the tweet they
promote.

## What we create, and where an image fits

The X create flow is campaign -> line item -> promoted tweet. The promoted tweet is either an
existing `tweetId` or a NEW promoted-only (nullcast) tweet authored from `tweetText` through
`POST accounts/:account_id/tweet`. The image can only go on the **authored** tweet: an
existing tweet's media cannot be changed. So `imageAssetId` requires `tweetText` and no
`tweetId`, and any other combination is refused before X is contacted. A silent drop would
run the campaign without the image the caller asked for.

## Upstream contract (docs.x.com, checked 2026-10-13)

| Step | Endpoint | Notes |
|---|---|---|
| Upload | `POST https://api.x.com/2/media/upload/initialize` (JSON: `media_type`, `total_bytes`, `media_category=tweet_image`), `/{id}/append` (multipart: `segment_index`, `media`), `/{id}/finalize`; `GET /2/media/upload?command=STATUS&media_id=` while processing | OAuth 1.0a user context is accepted, which is the credential X connections already store. Images are limited to 5 MB and segments to 5 MB. |
| Library | `POST accounts/:account_id/media_library?media_key=&name=&file_name=`; `GET …/media_library?q=&media_type=IMAGE&count=50` | The tweet docs say that for `media_keys`, "The media asset must be in the account's Media Library". `q` matches name, title, file_name and description. |
| Tweet | `media_keys` on `POST accounts/:account_id/tweet` | Up to 4 images. We send one. |

## Flow and safety properties

- **Validate before mutating.** The dispatcher (`resolveTwitterImage`) parses the id and
  checks the tweetText/tweetId combination. It then reads the asset's **size** before loading
  the blob and refuses anything over `twitter.MaxImageBytes` (5 MiB). It reserves those bytes
  on the shared `AssetReserver` (the same budget Meta uses, bound once in
  `registerDispatchers`) and only then loads the asset. Every refusal is `notCreated`, so
  the claim is released and X is not contacted. The client checks size, type and sniffed bytes
  again before making any request.
- **Asset ownership.** `GetAssetSize`/`GetAsset` are scoped by the brief's project and
  brief, so an asset belonging to another brief or project comes back not-found.
- **Before anything billable.** The image step runs after account verification and before the
  campaign POST. Any upload or library failure returns `(nil, err)`, and no campaign, line
  item or tweet exists at that point.
- **Upload once per (account, image).** There is no content addressing. The library entry is
  named `lfxasset<sha256 hex>`, computed in the client from the uploaded bytes. Before
  uploading, the client looks the name up and reuses a live IMAGE entry with exactly that name.
  It applies `findByName`'s pagination rules: a short page settles the lookup, and a full page
  with no usable cursor is an error rather than a not-found. The key is recorded in
  `CampaignResult.MediaKey`.
- **Outcome classes.** These match the client's existing writes. A definite 4xx or pre-send
  failure reads "failed before any campaign was created". A transport error, 5xx, mutating 3xx
  or mutating 429 reads "UNCONFIRMED". A stalled upload session expires on its own. An
  ambiguous library add may have created the entry, and the retry's by-name lookup finds it. A
  library 2xx that does not echo the uploaded key is not treated as confirmation. Upstream
  bodies never reach the error text, which carries method, path and status only.
- **Pacing.** Every media POST (initialize, each append, finalize, the library add) takes a
  slot from the account's write pacer. The lookup and status reads do not.
- **Memory.** Each append frames the multipart body around the chunk (prefix + chunk +
  suffix) and does not copy it, as the Meta `/adimages` upload does.

## Known limits

- **Not verified against a live account.** X's v2 media upload requires the connection's app
  to have X API v2 access as well as Ads API access. Confirm this on the LF system connection
  before relying on it.
- If X's `q` term matching ever fails to match the 72-character name, a retry uploads a
  second library entry. That is a duplicate media item, not a duplicate tweet or spend.
- Library entries are never deleted, matching the `creative_assets` table's own retention.
- One image only, and no card. A website card would need `POST accounts/:id/cards`, which
  this service does not create.

See [internal/platform/twitter](../code/internal-platform-twitter.md) and
[internal/dispatch](../code/internal-dispatch.md).
