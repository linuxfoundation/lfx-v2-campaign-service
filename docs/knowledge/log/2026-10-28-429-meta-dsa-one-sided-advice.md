# 2026-10-28 — 429: Meta DSA one-sided advice follows the geo targets; dispatcher check pinned

**Fix** — two findings left on `main` by #414 (Meta EU DSA disclosures).

**The one-sided-pair message no longer offers advice that is then refused.** When only one of
`dsaBeneficiary` and `dsaPayor` is supplied, `meta.CheckPublishIdentity` used to answer "supply
both or omit both" whatever was targeted. For a create whose `geoTargets` include an EU country,
omitting both is refused by the very next guard, and removing the EU countries alone still
leaves a one-sided pair. With an EU country targeted, the message now names the countries and
says "supply both, or remove the EU countries and omit both disclosures"; otherwise it still says
"supply both or omit both". `TestCheckPublishIdentityOneSidedAdviceFollowsTheGeoTargets` covers
both directions of the pair with an EU target, and the EU cases fail against the old message.

**The dispatcher's own DSA check is pinned.** `TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest`
passed with the dispatcher's `checkMetaPublishIdentity` call deleted, because the Meta client runs
the same check before its first request. What only the dispatcher's check prevents is the work
`Dispatch` does before the client exists: resolving a variant's uploaded image reads the asset
store and reserves memory for its bytes. The test now gives a variant an `imageAssetId` and
asserts the asset store is never read; with the dispatcher check deleted it fails on that read.
