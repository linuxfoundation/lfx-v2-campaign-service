# 2026-11-01 — #437 Google creative requests that v23 refuses

**Fix** — three Google creative requests were built in a shape the Google Ads API v23
refuses. In each case the create first committed the budget and a PAUSED campaign, then
failed at the creative step, which left a campaign LFX can never activate. None of the
three was visible in tests, because the fake servers accepted every shape. The only live
creates of these types sent no creative, so they never reached these code paths.

- **Demand Gen ad key.** The ad was sent as `demandGenMultiAssetAdInfo`, which is the
  message name. The Ad oneof field is `demand_gen_multi_asset_ad` (`ad.proto` = 62), so
  the JSON key is `demandGenMultiAssetAd`. Google refuses an unknown key with a 400 at
  `adGroupAds:mutate`.
  - `TestAdTypeWireKeysMatchTheAdOneof` now pins the key for every channel's ad-type arm.
  - Every other request-payload JSON tag in `internal/platform/googleads` was checked
    against the v23 protos, and none is misspelled.
- **Performance Max asset group.** The group was created by a standalone
  `assetGroups:mutate`, with its links sent in a second request. Google's Performance Max
  guide says a non-retail asset group "cannot be created in isolation": it must be
  created together with all of its AssetGroupAsset links in one bulk mutate request.
  - The group and its links now go in one `googleAds:mutate`. The group carries the
    temporary resource name `assetGroups/-1`, which the links reference.
  - `partialFailure` stays off, so the request is atomic. A definite refusal therefore
    reports no group id.
  - A 2xx that does not describe every link as one of this group's links, in this
    account, keeps the group id and records zero links, so the activation gate still
    refuses.
  - `pmaxCascade` now refuses a standalone asset-group or asset-group-asset create, as
    Google does.
- **Display image rules.** `ResponsiveDisplayAdInfo` requires at least one landscape
  **and** at least one square marketing image, and caps its two logo arrays at five
  combined. The validator had copied Demand Gen's reciprocal reading and a per-array
  logo cap.
  - Both shapes are now required, each refused by its own name before the budget mutate.
  - The logo ceiling is checked across both arrays.
  - `ImageSlotCatalog` follows: Display publishes per-slot minimums on the two marketing
    shapes and no either-or rule. Its logos share a `logo image` group at five, with no
    per-slot maximum.

Demand Gen's reciprocal marketing pair and its 1–5 logo are correct against the proto and
are unchanged. The Performance Max logo limits are not stated in the v23 protos, so they
were left as they are.
