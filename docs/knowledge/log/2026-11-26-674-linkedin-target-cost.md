# 2026-11-26 — #674 LinkedIn: optional target cost on automatic bidding

**Update** — `linkedInConfig.bidding` takes an optional `targetAmount`, automatic bidding's target
in USD at whole cents. It is sent as the campaign's `unitCost` beside the automated
`optimizationTargetType` (`MAX_CLICK`, or `MAX_CONVERSION` under objective `conversions`), with
`costType` `CPM`. That mirrors the KubeCon NA 2026 Campaign Manager campaigns on the CNCF account
(873501596 `MAX_CONVERSION` + `CPM` + 10; 912330026 `MAX_CLICK` + `CPM` + 17.88). Omitted, the
create is unchanged.

- Behind the new `LINKEDIN_ADS_TARGET_COST_ENABLED`, off by default (constants, chart values,
  README, deployment concept). LinkedIn's "Create and Manage Campaigns" does not describe
  `unitCost` on a `MAX_*` campaign, so a paused live probe is owed before it is enabled.
- Refused before a job exists: the flag off, `null` or a non-number, with a manual bid, and an
  amount `linkedin.ValidateTargetCost` refuses (`ValidateBidAmount`'s rules, its own sentences).
  The key is an unknown bidding field on every other platform.
- The row records `automatic` and no max CPC bid; the result blob records `targetCost`. The bid
  lever refuses `MAX_*`, so it never rewrites the target; updating a target is out of scope. Name
  reuse and the settings readback are unchanged.
- Docs: `api-catalog.md` Bidding section, the Goa `config` description (regenerated `gen/`), and
  the `internal/platform/linkedin` and `internal/dispatch` concepts.

Tests: `internal/platform/linkedin/target_cost_test.go`,
`internal/dispatch/linkedin_target_cost_test.go`, and a target-cost row in the LinkedIn bid lever's
refusal table.
