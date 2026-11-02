# 2026-11-02 — #439 Performance Max link results matched per operation

**Fix** — after the one atomic `googleAds:mutate` that creates a Performance Max asset
group with its links (#437), a link result counted as confirmed when it was an
`assetGroupAssets` name in this account whose group half was the new group. The rest of
the name was not compared, so a truncated name (`555~`, `555~900`) or one link name
repeated for every result could still record a full `AssetGroupAssetLinks` count, which
is what the activation gate trusts.

- `isAssetGroupAssetLink` replaces `isAssetGroupAssetOf`. The response is index-aligned
  with the operations, so result `i` must be exactly
  `customers/<cid>/assetGroupAssets/<groupId>~<assetId>~<fieldType>`, with the asset id
  and field type of operation `i`. Any other name keeps the group and records zero links
  (UNCONFIRMED), as before.
- The test fake now echoes each operation's asset and field type, and the link-result
  tests cover a truncated name, a repeated name, another asset and another field type.

Real-world risk was low: with `partialFailure` off a 2xx already means every operation
applied, and Google generates these names itself. Raised by the review bots on #438.
