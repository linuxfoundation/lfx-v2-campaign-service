# 2026-11-06 — #538 the Search image slot minimums and the 20-image cap are verified live

**Fix** — Probes the Search campaign asset geometry and counts at their boundaries, fixes the two
places where this service refused what Google accepts, and drops the "not live-verified" wording
those probes settle.

- **Probes (#538).** validateOnly `googleAds:mutate` requests on account 8666746580 (v23), each
  carrying a temporary-id budget (1.00/day, non-shared), a temporary-id PAUSED Search campaign and
  `buildSearchAssets`' own asset and `campaignAsset` operations, so no existing resource was named
  and nothing persisted. Google enforces the image spec on the link's `field_type`
  (`mediaUploadError`) and the counts as `resourceCountLimitExceededError: RESOURCE_LIMIT` on the
  first link over the cap. Square `AD_IMAGE`: 300x300 accepted, 299x299 / 300x299 / 299x300
  refused `DIMENSIONS_NOT_ALLOWED`. Landscape `AD_IMAGE`: 600x314 and 1200x628 accepted, 599x314
  and 600x313 refused `IMAGE_CONSTRAINTS_VIOLATED`. ±1% with the boundary included: 303x300 and
  1188x628 accepted, 304x300 and 1218x628 refused `ASPECT_RATIO_NOT_ALLOWED`. Twenty `AD_IMAGE`
  links accepted and the 21st refused whether square, landscape or mixed (19 + 2). One
  `BUSINESS_LOGO` accepted, a second refused; logos from 16x16 up accepted, 15x15 refused.
- **Landscape ad images.** `searchAssets.landscapeAdImages` (new, optional, ImageRef[]): 1.91:1,
  at least 600x314, linked as `AD_IMAGE` beside `adImages`, the two at most 20 together. Until
  now the service refused the landscape shape, which Google accepts.
- **Exact aspect boundary.** `creative.AspectWithin` decides ±1% by cross-multiplication, and both
  the catalog's `ShapeRule.fits` and the create path's `checkImageGeometry` call it. The float
  division it replaces drifted 303x300 to 0.010000000000000009 and refused it. This applies to
  every channel's slots.
- **Logo floor kept.** Google's API takes a `BUSINESS_LOGO` from 16x16, and the service keeps
  128x128, the size Google's business-logo specification documents. `searchImageSlots` now says
  that this floor is above the API's on purpose.
- **Docs.** `search_assets.go`, the `searchAssets` row in `docs/api-catalog.md`, the googleads,
  dispatch and creative code pages and the creative-assets architecture page. The shared negative
  keyword list items stay NOT LIVE-VERIFIED. No resource was created.
