# 2026-11-26 — #675 LinkedIn manual CPC on a website visits campaign

**Update** — `linkedInConfig.objective: "traffic"` with a manual bid is now created as
`objectiveType` `WEBSITE_VISIT`, `costType` `CPC`, `optimizationTargetType` `NONE` and the bid as
`unitCost` (name segment `Traffic`), behind the new `LINKEDIN_ADS_TRAFFIC_MANUAL_BID_ENABLED`
(chart default `"false"`). Text Ads, which are manual only, can so be website visits too.

- Flag off (the default): the 400 is unchanged —
  `linkedInConfig.objective "traffic" is offered with automatic bidding only; …`.
- Unchanged either way: objective omitted (manual → `WEBSITE_CONVERSION`), automatic traffic
  (`MAX_CLICK`), manual conversions, and automatic conversions requiring `conversions`.
- `linkedin.campaignBidding` no longer refuses manual + `WEBSITE_VISIT`; the dispatcher's
  `linkedinObjective` owns the gate.
- By-name reuse under `WEBSITE_VISIT` now compares the bidding mode, as #582 did for
  `WEBSITE_CONVERSION`: an unreported `optimizationTargetType` aborts a manual create and is still
  reused by an automatic one.
- The settings readback's recorded objective for a manual row is `WEBSITE_VISIT` when the
  create's result records it with target `NONE`, `WEBSITE_CONVERSION` otherwise.

LinkedIn's "Ad Budget Type Allowable Combinations" allows `NONE` for `WEBSITE_VISIT`; LF's KubeCon
NA 2026 Text Ads campaign (894070046) ran this setup. Not yet probed live from this service: run a
paused probe create before enabling the flag.
