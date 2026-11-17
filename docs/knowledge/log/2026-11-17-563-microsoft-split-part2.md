# 2026-11-17 — #563 Microsoft gaps split: part 2

**Note** — The second PR of the #613 split (branch `feat/563-microsoft-gaps-part2`) adds, on
top of part 1 (#617):

- The remarketing-list picker, `GET /projects/{projectId}/microsoft-ads/remarketing-lists`
  (`list-microsoft-ads-remarketing-lists`), behind `MICROSOFT_REMARKETING_LISTS_ENABLED`.
- Conversion goals and conversion bidding at create: `microsoftConfig.conversionGoalIds`,
  `bidding.strategy` `maxConversions` / `targetCpa` with `bidding.targetCpa`, and the
  conversion-goal picker, `GET /projects/{projectId}/microsoft-ads/conversion-goals`
  (`list-microsoft-ads-conversion-goals`), all behind `MICROSOFT_CONVERSION_GOALS_ENABLED`.
  `conversionGoalIds` needs `global_marketing_ops` (`service.MicrosoftMarketingOpsInputs`).
- Both picker routes in the HTTPRoute regex and the gateway's `marketing-ops-api` rule, pinned by
  `parity_test.go`, and the shared streaming decoder `decodeCappedList` with its identity check.

Both flags default to `"false"` in the chart. The new fields are documented in
`docs/api-catalog.md`; the one-line create-campaigns method and `config` attribute descriptions in
`design/brief.go` are left unchanged, because each edit regenerates several OpenAPI and CLI files.
Multiple ad groups (`adGroups`, `MICROSOFT_AD_GROUPS_ENABLED`, the brief-dependent create
preflight and the bid write's ad group reconciliation) and the full `config` description follow in
part 3.
