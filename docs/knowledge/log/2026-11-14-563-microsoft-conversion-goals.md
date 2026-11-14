# 2026-11-14 — #563 Microsoft conversion goals and conversion bidding at create

**Update** — Gap 7 of the Microsoft Advertising gap review (#563), as the owner decided on
2026-11-14: `microsoftConfig.conversionGoalIds` names EXISTING conversion goals on the shared
account (no UET tag or goal is created in LFX; marketing ops owns the tag), and
`microsoftConfig.bidding` gains the `maxConversions` and `targetCpa` strategies.

- `microsoft.ValidateConversionGoalIDs`: at most 20 ids, each a canonical positive int64, no repeat.
- Before anything is created, one `GetConversionGoalsByIds` read must return every id, none Paused
  or Deleted; otherwise the job fails with nothing created.
- The goals are set as the new campaign's `GoalIds`; a campaign found by name keeps its goals with
  a warning step. Goals this run set are recorded as `conversionGoalIds` in the result.
- `{"strategy":"maxConversions"}` and `{"strategy":"targetCpa","targetCpa":25}` are sent as
  `MaxConversionsBiddingScheme` (with `TargetCpa`), only with at least one goal, never with
  `bidAmount` or `cpcBid`; recorded as automatic bidding. `targetCpa` is refused on every other
  platform's bidding object.
- Picker: `GET /projects/{project_id}/microsoft-ads/conversion-goals`
  (`list-microsoft-ads-conversion-goals`), on the gateway's `marketing-ops-api` rule, with
  HTTPRoute and parity-test coverage.
- `conversionGoalIds` is gated on `global_marketing_ops` (`service.MicrosoftMarketingOpsInputs` and
  the dispatch field-classification test). Everything is behind
  `MICROSOFT_CONVERSION_GOALS_ENABLED` (chart `"false"`): off, a 400 before any job and a 400 from
  the picker.

Not verified live: no Microsoft call was made. `ConversionGoals/QueryByIds`, the space-delimited
`ConversionGoalTypes`, `Campaign.GoalIds` and the MaxConversions scheme are from the Campaign
Management v13 reference.
