# 2026-11-17 — #563 part 4: the Microsoft gap inputs in the create-campaigns contract

**Docs** — The last part of the split #563 work (see `2026-11-17-563-microsoft-split.md`) updates
the two long descriptions in `design/brief.go` that parts 1-3 (#617, #622, #623) left alone, so
the published OpenAPI documents and the CLI describe every Microsoft create input. No behaviour
changes.

- The `config` attribute gains a short section each for `searchPartners`, `adSchedules`, the
  device, age and gender bid adjustments, `sharedNegativeKeywordListIds`, `structuredSnippets`,
  `logoAssetId` / `logoBusinessName`, `conversionGoalIds` with the `maxConversions` and
  `targetCpa` bidding strategies, and `adGroups`. Each names its deployment flag and its
  synchronous 400 (`invalid campaign config: microsoftConfig.<field>: ...`, or
  `microsoftConfig.bidding: ...` for a bidding strategy). `docs/api-catalog.md` stays the full
  reference.
- The create-campaigns method description adds the two missing 400 rules (conversion goals and
  conversion bidding behind `MICROSOFT_CONVERSION_GOALS_ENABLED`, ad groups behind
  `MICROSOFT_AD_GROUPS_ENABLED`). Its Microsoft shared-account sentence now lists every field
  `service.MicrosoftMarketingOpsInputs` gates on `global_marketing_ops`: `conversionGoalIds` and
  each `adGroups` entry's `audienceIds` and `excludedAudienceIds` join `audienceIds`,
  `excludedAudienceIds` and `sharedNegativeKeywordListIds`.
