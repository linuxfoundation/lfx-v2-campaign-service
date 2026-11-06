# 2026-11-06 — #520 the Search image, business logo and business name assets are verified live

**Docs** — Records a live create of the Search campaign asset request and drops the "not
live-verified" wording it settles. No request shape or behaviour changes.

- **Live create (#520, #487).** campaign-service's own `CreateCampaign` created paused Search
  probe campaign **24444541804** (account 8666746580, v23, budget 1.00/day, non-shared). First,
  a validateOnly pass of the same eight-operation `googleAds:mutate` was accepted on paused
  campaign 24426212391. The live request carried two named 1:1 `AD_IMAGE` image assets, one named
  1:1 `BUSINESS_LOGO` image asset and one `BUSINESS_NAME` text asset, each under a temporary id,
  then the four `campaignAsset` links that name them. It answered 2xx with all eight results, and
  `createSearchCampaignAssets`' positional result check passed.
- **Read-back.** GAQL on `campaign_asset` returned two `AD_IMAGE` rows (600x600 and 300x300 PNG),
  one `BUSINESS_LOGO` row (512x512 PNG) and one `BUSINESS_NAME` row ("KubeCon + CloudNativeCon"),
  all `ENABLED`, source `ADVERTISER`, and naming the asset ids the response returned. Google
  answered the `BUSINESS_NAME` text create with an EXISTING text asset of the same text
  (74427373151). That is the reuse case the result check already accepts.
- **Docs.** `search_assets.go`, the `searchAssets` row in `docs/api-catalog.md` and the googleads
  code page now cite the live create. The shared negative keyword list items on that page stay
  NOT LIVE-VERIFIED, and so do the slot minimums and the 20-image cap, which are Google's
  documented specifications.
