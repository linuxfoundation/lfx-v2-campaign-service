# 2026-08-17 — LFXV2-2665 Google Demand Gen single-image creative

**Creation** — Google Ads Demand Gen campaigns can now carry a real
single-image ad creative instead of stopping at a paused shell, end to end:
reference three role images from a brief, have them uploaded and attached to a
created ad, and let the campaign be activated on the strength of that ad rather
than a keyword. This is the first platform after Meta in the same push to evolve
the paid-ads builder from a paused-shell MVP toward launch-ready campaigns, and
it deliberately REUSES the platform-agnostic `creative_assets` table + repo +
upload endpoint Meta introduced — no new table, migration, endpoint or handler.
The ad builder is format-keyed so carousel and video can follow without
reopening the flow.

The pieces, and where each is documented:

- **Image asset upload.** `internal/platform/googleads/assets.go`
  `uploadImageAsset` POSTs one `ImageAsset` (raw bytes base64-encoded, no name,
  no type) to `customers/{cid}/assets:mutate` and returns the single
  `customers/{cid}/assets/{id}` resource name. It is deliberately **cache-free /
  content-addressed**: `idempotent = false` (a blind retry could double-create),
  and there is NO app-side `(customer, checksum)` dedupe — Google is
  content-addressed for identical bytes (research O3, not live-verified), and the
  worst case of a missing dedupe is a harmless leaked library asset on a retry,
  never a spend. See `docs/knowledge/code/internal-platform-googleads.md`
  (GA-6).

- **Ad build.** Google has no dedicated single-image Demand Gen ad type: the
  non-carousel/non-video ad is `DemandGenMultiAssetResponsiveDisplayAd`, which
  requires image assets in three aspect-ratio roles — landscape marketing
  (1.91:1), square marketing (1:1) and logo (1:1) — plus a business name
  (≤ 25 runes). `internal/platform/googleads/demandgen_ad.go`
  `precomputeDemandGenAd` validates with NO request first (unknown media format,
  absent/over-length business name, any missing image-role bytes, bad ad copy, or
  an over-length final URL all fail before the first `:mutate`); it reuses the
  Search `composeAdCopy` but caps headlines at 5 (Demand Gen's max; RSA allows
  15). `buildDemandGenAd` is format-keyed (`single_image` today).
  `uploadDemandGenAssets` uploads the three roles in order, and `createDemandGenAd`
  mirrors the Search ad block exactly (ambiguous → UNCONFIRMED, definite 4xx →
  clean fail, full kind/account/composite/ad-group-id validation before the AdID
  is trusted). `adCreate` in `adgroup_ad.go` gains an `omitempty`
  `demandGenMultiAssetResponsiveDisplayAd` field so the Search create stays
  byte-identical. **The three assets upload BEFORE the budget `:mutate`**, so a
  bad creative or a failed upload returns `(nil, err)` with nothing spent; a nil
  creative keeps the legacy paused-shell path untouched.

- **Resolution at dispatch.** `internal/dispatch/googleads.go`
  `resolveDemandGenCreative` turns the three role asset ids into bytes BEFORE any
  upstream create, scoped to this brief via `GetAsset(projectID, briefID,
  assetID)`. All three roles are REQUIRED; a missing id or a `GetAsset` failure
  fails the dispatch through `notCreated`, RELEASING the claim rather than
  stranding it after a paid campaign exists. See the Dispatch-adapter section of
  `docs/knowledge/code/internal-platform-googleads.md`.

- **Channel-aware activation.** ACTIVATE no longer requires a keyword criterion
  for every campaign — that requirement was Search-only, and a Demand Gen campaign
  legitimately has no keywords. The dispatcher now stamps
  `CampaignResult.Channel` (`"search"` / `"demand-gen"`) onto the persisted
  result at create time (both success and reconcilable-partial, and on adoption),
  and the ACTIVATE gate keys on it EXPLICITLY rather than inferring the channel
  from which criteria happen to be present (inference would be circular): the
  ad-group/ad must be provisioned for both channels, Search additionally needs a
  keyword, Demand Gen needs only its ad (which `createDemandGenAd` stamps only
  after the assets uploaded and the ad mutate succeeded). An empty `Channel` on a
  row created before the field existed takes the Search path, so no existing
  campaign's gate behaviour changes. See "Status toggling (GA-3c)" in
  `docs/knowledge/code/internal-platform-googleads.md`.

**Note** — Full activation of a created campaign is still gated by a front-end
toggle plus double-confirmation in `lfx-self-serve`; the client creates
everything PAUSED, unchanged. Carousel and video Demand Gen formats remain
deferred under the same LFXV2-2665 umbrella. The live `validateOnly` probes for
the Demand Gen ad/asset shapes (research gates O1/O2/O4) were NOT run — this
workstation has no Google Ads account access — so those wire shapes are pinned
from the v23 reference docs, not confirmed against a live account.
