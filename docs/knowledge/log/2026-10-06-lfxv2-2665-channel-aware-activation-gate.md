# 2026-10-06 — LFXV2-2665 Channel-aware activation gate and Performance Max asset group toggling

**Fix** — `GoogleAdsDispatcher.ToggleStatus` derived "fully provisioned" from ONE channel's
shape: an ad group, an ad, and at least one keyword criterion. That is Search's shape and
only Search's, so the branch shipped two channels it can create and neither it can launch.
Demand Gen clears the ad-group gate and dies on the keyword gate, which it can never satisfy
because this client REFUSES keywords on the channel — the gate told the operator to supply
exactly the field the create path rejects. Performance Max has no ad groups and no ads at
all, so it failed the first gate and was refused with a sentence about ad groups it does not
have. `googleAdsActivationGate` now keys on the campaign's `Variant` — part of its identity
rather than its config, so a row cannot drift into the wrong arm through a config edit, and a
row written before variants existed normalises to `VariantDefault` and keeps the Search rules
it was created under. Search keeps both gates; Demand Gen keeps the ad-group gate and drops
the keyword one; Performance Max gates on its asset group and says so in those words.

**Fix** — Performance Max asset groups are created PAUSED, like every other resource this
client creates, and nothing could un-pause one. The campaign resource could therefore be
flipped to ENABLED while its asset group stayed PAUSED, reporting a launch that cannot
deliver — precisely the false success `ErrCampaignNotProvisioned` exists to prevent, on the
one channel whose creative this client creates paused. `Client.UpdateAssetGroupStatus` sends a
single `assetGroups:mutate` UPDATE masked to `status`: an update carries only the masked
field, because the create shape's required name/campaign/finalUrls would either be rejected
or, worse, rewrite them. It is held to the same standard as `UpdateCampaignStatus` — sent
idempotent, so bounded 429 retries do not turn ordinary throttling into an avoidable
UNCONFIRMED, and a 2xx that does not acknowledge the one operation is wrapped in
`unconfirmedAssetGroupStatusError`, the Performance Max counterpart of
`unconfirmedCampaignStatusError` satisfying the same `Unconfirmed() bool` interface
`IsOutcomeUnconfirmed` honours. The id is pinned to digits before it is interpolated into a resourceName, the rule
`UpdateCampaignStatus` has always applied to campaign ids and for the same reason: anything
else could alter the resource path. The dispatcher reads the id from the persisted `Result`
blob's `assetGroupId` and checks it before the ad-group targets: a Performance Max campaign has an
asset group and no ad groups, every other channel the reverse, so the two are mutually
exclusive by construction. Keying the cascade on what the blob RECORDED rather than on the
variant keeps it consistent with `googleAdsToggleTargets`, which has always derived the
cascade from what was created rather than from what was asked for. ACTIVATE flips the asset
group first and the campaign last; PAUSE keeps the existing order, campaign first.

**Docs** — A doc comment inserted by the previous round sat flush against the one above it,
so godoc attributed the whole block to `parsedAssetIDs` and left `assetID` undocumented. Split
into two blocks, each above the function it describes. `docs/api-catalog.md`'s status-toggle
row now states the per-channel activation gate and the cascade order on the row itself; it
had been inferable only from the adoption note further up, and nothing in the Performance Max
catalog entries said what activating one of those campaigns would do.
