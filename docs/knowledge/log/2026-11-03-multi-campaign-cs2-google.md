# 2026-11-03 — multi-campaign cs-2: a second, labelled Google Ads campaign per slot

**Update** — the second campaign-service step of letting one event run several campaigns on one
platform: Google Ads joins Microsoft.

**Allowlists.** `google-ads` is added to both `model.ProviderSupportsSlotVersions` (`new_version`)
and `model.ProviderNamesByStage` (`funnel_stage` / `tactic`). The two move together, pinned by
`TestMultiCampaignAllowlists`. On Google the slot is the dispatched channel, so Search, Demand Gen,
Performance Max, Video and Display each number their own versions.

**Names from the claimed row.** `GoogleAdsDispatcher` sets `NameSuffix` to
`model.SlotNameSuffix(brief.ID, slot)` and `StageToken` / `Tactic` from `model.DispatchLabels`.
`googleads.ComposeName` builds the budget, campaign and Search ad group names from them (the
Demand Gen, Display and Video ad groups keep their event-named names, unique within their campaign):
- an unlabelled slot-1 row keeps its legacy 5-segment names byte for byte;
- a labelled row has the 7-segment shape `LFX | <kind> | <project> | <event> | <stage> | <tactic> | <brief>[-<n>]`;
- a row with a stage but no tactic has no tactic segment;
- the tactic is its slug, except `content-promotion`, written `content-promo` so the data pipeline's
  substring ad-format parser does not read "proMOTION" as the Motion format.

**Name-keyed paths.** Every one of them takes the composed name, so slot 2 never binds to slot 1:
- `DUPLICATE_CAMPAIGN_NAME` on slot 2 reports slot 2's name with no id;
- the budget reuse after `DUPLICATE_NAME` looks up slot 2's budget name only;
- `adoptExisting` searches slot 2's campaign name.

**UTM (N1).** The `utm_campaign` last-resort fallback reads the new `CampaignInput.UTMCampaignFallback`
(the bare brief id), so it never moves with the slot. It does not change when the slug or event name
is set either.

**Contract wording.** `expected_slot_versions` is per dispatched slot: on Google, the campaigns of the
request's channel, not the platform's highest version.

**Rollback.** No migration. `deployment.md` has the runbook:
1. remove `google-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`;
2. settle non-terminal Google rows that are slot ≥2 or labelled;
3. roll the image back.

**Tests.**
- Dispatch: `TestGoogleAds_NamesComeFromTheClaimedRow` (goldens), `TestGoogleAds_Slot2NeverBindsSlot1ByName`, `TestGoogleAds_BudgetNameIsUniquePerSlot`, `TestGoogleAds_UTMCampaignDoesNotMoveWithTheSlot`.
- Client: `TestUTMCampaignFallbackIsTheBareBriefID`, `TestComposeName_LabelSegments`.
- Live: `TestLiveGoogleSecondCampaignThroughTheOrchestrator`, which runs the real orchestrator and repositories with the real Google dispatcher against a fake Google API.
