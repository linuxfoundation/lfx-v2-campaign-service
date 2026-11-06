# 2026-11-06 — #514 the Search image request and observed demographics pass validateOnly probes

**Docs** — Records two validateOnly probes and drops the "not verified" wording they settle. No
request shape or behaviour changes.

- **Search images (#477, #487).** The exact request `createSearchCampaignAssets` sends was
  accepted on paused LFX Search campaign 24426212391 (v23, account 8666746580, nothing
  persisted). It is one `googleAds:mutate` that creates two named 1:1 `AD_IMAGE` image assets,
  one named 1:1 `BUSINESS_LOGO` image asset and one `BUSINESS_NAME` text asset, each under a
  temporary id, then creates the four `campaignAsset` links that name those temporary ids:
  eight operations in all. `search_assets.go`, the `searchAssets` row in
  `docs/api-catalog.md` and the googleads code page now say so. A persisted create is still
  unverified.
- **Observed demographics (#497).** The 2026-11-06 probe on #497 accepted the ad group's
  `AGE_RANGE`, `GENDER` and `INCOME_RANGE` `bidOnly` restrictions and the positive criteria.
  The `observedDemographics` row, the dispatch code page and the
  `googleAdsDemographicTargetingEnabled` comments no longer call the shape unprobed. The gate,
  `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED`, still defaults off; lfx-v2-argocd#2031 turns it
  on.
