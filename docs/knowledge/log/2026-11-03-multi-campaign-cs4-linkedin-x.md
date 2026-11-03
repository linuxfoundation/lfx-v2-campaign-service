# 2026-11-03 — multi-campaign cs-4: a second, labelled LinkedIn or X campaign

**Update** — the fourth campaign-service step of letting one event run several campaigns on one
platform. LinkedIn and X join Microsoft, Google, Reddit and Meta, so every ad platform now takes
`new_version`, `funnel_stage` and `tactic`; only HubSpot is still refused.

**Allowlists.** `linkedin-ads` and `twitter-ads` are added to both
`model.ProviderSupportsSlotVersions` (`new_version`) and `model.ProviderNamesByStage`
(`funnel_stage` / `tactic`), pinned together by `TestMultiCampaignAllowlists`. Each platform has
one slot (variant `default`), so `expected_slot_versions` is that slot's latest version. The
service tests that pin the refusal now use HubSpot, the one provider left outside.

**Names from the claimed row, still 8 segments.** The dispatchers pass the claimed row's
`StageToken`, `Tactic` (`model.CampaignLabels.NameTokens`) and `SlotToken` (`model.SlotNameToken`),
exactly as Reddit and Meta do:
- an unlabelled slot-1 row keeps its legacy name byte for byte (both platforms `MoFU`);
- segment 8 is the row's stage token, and a tactic-only row keeps `MoFU`;
- segment 5 is `Prospecting <tactic>`, plus ` #<n>` from slot 2 on (`Prospecting retargeting #2`,
  or `Prospecting #2` unlabelled). Segments 2 and 7 are untouched, every token is pipe-stripped,
  and the LinkedIn campaign-group and X line-item names carry no token.

**No binding by name.** Unlike Reddit and Meta, both clients reuse a campaign by its composed
name on create: LinkedIn's `findCampaignByNameInGroup` (in the event's shared campaign group) and
X's `findCampaignByName`, which runs on every create. The lookup key is the row-derived name, so
slot 2 never finds slot 1's live campaign: it creates its own, LinkedIn creates its creatives under
it, and X creates its line item (and promoted tweet) under it. A retry of the same row composes
the same name and reuses its own campaign.

**Length.** Both clients' existing 255-character campaign-name checks run on the composed name
before the first request, so an over-long labelled or later-slot name is refused as a
`PreflightRefusal`, with zero requests, and the dispatcher releases the claim. No unlabelled name
that fit before is refused.

**lf-dbt.** As for Reddit and Meta, lf-dbt reads segment 5 by position, so its `targeting` column
holds the raw `Prospecting retargeting #2`; the same lf-dbt normalisation follow-up covers it.

**Rollback.** No migration. `deployment.md` has the runbook: remove `linkedin-ads` and
`twitter-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`, settle non-terminal
slot ≥2 or labelled rows (the older image would bind an unlabelled slot-2 re-dispatch to slot 1 by
name), then roll the image back.

**Tests.**
- Dispatch: `TestLinkedInX_NamesComeFromTheClaimedRow`, `TestLinkedInX_Slot2NeverBindsSlot1`, `TestLinkedInX_OverlongLabelledNameIsRefusedBeforeAnyRequest`, against stateful fakes that answer the by-name lookups.
- Clients: `TestBuildCampaignName_LabelTokens` (LinkedIn), `TestBuildTwitterCampaignName_LabelTokens`, `TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest` (both).
- Service: `TestBriefService_CreateCampaigns_LinkedInAndXAcceptALabelledNewVersion`, the unlisted-label refusals on LinkedIn and X, and the HubSpot refusals.
- Live: `TestLiveLinkedInXSecondCampaignThroughTheOrchestrator`, which runs the real orchestrator, repositories and dispatchers against stateful fake LinkedIn and X APIs.
