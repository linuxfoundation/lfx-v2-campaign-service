# 2026-11-04 — #477 reconciled with main after #470, #480, #481 and #482

**Update** — #477 was rebased onto main after #480 merged. Both sides of every conflict were
kept: the Display ad groups (#475), the Performance Max signals (#461), the Demand Gen video ad
and ad lists (#460, #474) and the Search campaign assets all sit side by side in
`googleAdsCreativeSlots`, the snapshot sanitiser, `googleAdsMutateOperation` and its response.

What changed beyond keeping both hunks:

- `attachSharedNegativeKeywordLists` (the `campaignSharedSets:mutate` link loop) runs
  `resultIDSet.repeat`, as every other link and criteria loop does since #480: a repeated link
  is UNCONFIRMED and named as a repeat. Removing the check fails
  `TestCreateCampaign_SharedListRepeatedLinkIsUnconfirmed`.
- `createSearchCampaignAssets` reports its asset ids once each through `uniqueIDs`, while
  still matching each campaign link to the asset at its own index.
- Main no longer counts the Google image slots, so #477's "sixteen" is gone from the code
  comments, the KB and the #477 log entry.
- The snapshot's early return names `searchAssets` beside `displayAdGroups`; the KB and
  `TestGoogleAdsSnapshotConfig_EachEarlyReturnFieldAlone` now count twelve fields, and dropping
  `searchAssets` from the early return fails that test's new case.
- The `google-creative-assets` and `internal-platform-googleads` descriptions combine both
  sides, and their index bullets repeat them verbatim.
