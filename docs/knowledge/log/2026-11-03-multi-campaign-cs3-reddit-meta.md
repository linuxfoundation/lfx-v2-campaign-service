# 2026-11-03 — multi-campaign cs-3: a second, labelled Reddit or Meta campaign

**Update** — the third campaign-service step of letting one event run several campaigns on one
platform. Reddit and Meta join Microsoft and Google.

**Allowlists.** `reddit-ads` and `meta-ads` are added to both `model.ProviderSupportsSlotVersions`
(`new_version`) and `model.ProviderNamesByStage` (`funnel_stage` / `tactic`). The two lists move
together, pinned by `TestMultiCampaignAllowlists`. Each platform has one slot (variant `default`),
so `expected_slot_versions` is that slot's latest version.

**Names from the claimed row, still 8 segments.** Both names follow the data pipeline's
`Program | Event | Region | Objective | Targeting | Ad Format | Project | Funnel` contract, which
lf-dbt parses positionally only at exactly 8 segments. The dispatchers pass the claimed row's
`StageToken`, `Tactic` (`model.CampaignLabels.NameTokens`) and `SlotToken`
(`model.SlotNameToken`: `""` on slot 1, `#<n>` after):
- an unlabelled slot-1 row keeps its legacy name byte for byte (Reddit `ToFU`, Meta `MoFU`);
- segment 8 is the row's stage token, and a tactic-only row keeps the legacy funnel;
- segment 5 is `Intent <tactic>`, plus ` #<n>` from slot 2 on (`Intent retargeting #2`, or
  `Intent #2` unlabelled). The uniqueness token never becomes a segment and never touches
  segments 2 or 7.

**Tactic tokens.** `model.Tactic.NameToken` now holds the mapping Google used privately
(`content-promotion` → `content-promo`), and all three platforms share it. Each slug was checked
against lf-dbt's substring parsers for funnel, objective, targeting, ad format, region and program
(`macros/parse_campaign_name.sql`). There are two hits:
- `proMOTION`, already handled by that mapping;
- the program parser's `%T_C%` wildcard, which matches the `t c` in `Intent competitor` or
  `Intent content-promo`. It is harmless, because every one of these names starts with `Events`,
  which that parser matches first, and segment 1 is parsed positionally anyway.

**No binding by name.** Reddit never looks a campaign up by name. Meta's `findCampaignByName` runs
only under `ReconcileByName`, which no dispatcher sets. Slot 2 therefore always creates its own
campaign.

**Length.** Reddit's 200-rune check already covers every composed name before the first request.
Meta documents no limit, so it now refuses a LABELLED or later-slot name over 255 runes before
its account GET. Unlabelled Meta names stay unbounded, as before.

**Rollback.** No migration. `deployment.md` has the runbook:
1. remove `reddit-ads` and `meta-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`;
2. settle non-terminal slot ≥2 or labelled rows;
3. roll the image back.

**Tests.**
- Dispatch: `TestRedditMeta_NamesComeFromTheClaimedRow`, `TestRedditMeta_Slot2NeverBindsSlot1`, `TestMeta_OverlongLabelledNameIsRefusedBeforeAnyRequest`.
- Clients: `TestBuildRedditCampaignName_LabelTokens`, `TestBuildCampaignName_LabelTokens`, `TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest` (Reddit and Meta).
- Service: `TestBriefService_CreateCampaigns_RedditAndMetaAcceptALabelledNewVersion`, plus the refusal of unlisted labels for Reddit and Meta.
- Live: `TestLiveRedditMetaSecondCampaignThroughTheOrchestrator`, which runs the real orchestrator, repositories and dispatchers against fake Reddit and Meta APIs.
