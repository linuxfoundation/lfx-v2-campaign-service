# 2026-11-04 — #477 Search campaign image assets and shared negative keyword lists

**Update** — The live KubeCon NA 2026 Search campaign carries seven image assets, a business
logo and a business name as campaign assets, and three shared negative keyword lists (16, 177
and 5 keywords). #473 left both to this change. Both inputs are optional, validated before the
budget mutate, and refused on every channel but Search.

- `searchAssets` (`CampaignInput.SearchAssets`, `search_assets.go`): `adImages` are `AD_IMAGE`
  (square only, at least 300x300, at most 20), `businessLogoImages` is one `BUSINESS_LOGO`
  (square, at least 128x128), `businessName` is one `BUSINESS_NAME` text asset of at most 25
  display-width units. The images use the shared slot, fetch, decode and geometry machinery in
  one bounded pass before the budget, and stored-asset entries resolve through
  `googleAdsCreativeSlots` with snapshot sanitising. The assets and their
  campaign links are ONE atomic `googleAds:mutate` under temporary asset ids, after the
  extensions and before the ad group. Ids: `searchAssetIds`.
- `sharedNegativeKeywordLists` (`CampaignInput.SharedNegativeKeywordLists`,
  `shared_negative_lists.go`): EXISTING `customers/<cid>/sharedSets/<id>` lists ATTACHED
  through one `campaignSharedSets:mutate`, right after the campaign-level negatives. This
  client never creates or edits a shared set: a list is kept once and attached to many
  campaigns, which is the smallest shape #477 asks for. Another account's set is refused
  before the budget, because Google refuses it (`INVALID_CUSTOMER_ID`) only at the attach.
  Ids: `sharedNegativeKeywordListIds`.

Both steps follow the partial-result contract: a definite refusal records no id, an ambiguous
one is UNCONFIRMED, and a 2xx whose results do not match their operations is UNCONFIRMED and
keeps only the same-account ids it did describe.

Evidence: the 2026-11-03 validateOnly probes on #477 (v23) accepted `AD_IMAGE` and
`BUSINESS_LOGO` campaign assets naming existing square image assets, a `BUSINESS_NAME` link to
a text asset created by temporary id in the same `googleAds:mutate`, and a `campaignSharedSet`
naming an existing `NEGATIVE_KEYWORDS` set; another customer's set was refused. NOT
LIVE-VERIFIED: an image asset created by temporary id and linked in the same request (what
this sends), a multi-operation `campaignSharedSets:mutate`, a shared set of another type on
Search, and the slot minimums and 20-image cap. `ImageSlotCatalog` does not yet publish the two
Search slots.
