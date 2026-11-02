# 2026-11-02 — #436 campaign-level conversion goals on every Google channel

**Fix** — Conversion actions now reach Google on every channel this client creates
(Search, Display, Demand Gen and Performance Max; Video is wired the same way but its
creation stays refused). After the campaign is created, `applyCampaignConversionGoal`
(`internal/platform/googleads/conversion_goal.go`) finds or creates one
`CustomConversionGoal` per action set, named `LFX | conversion goal | <hash>` from the
sorted set so campaigns naming the same actions share it, then sets the campaign's
`ConversionGoalCampaignConfig` to `goal_config_level` CAMPAIGN with that goal.

Why: the old path sent `campaign.selective_optimization` at create time on Search, Video
and Display and refused the list on Demand Gen and Performance Max. Google documents that
field for App campaigns only, and it never ran against the live account; a read-only GAQL
over all 41 LFX campaigns showed none carrying it and every one at `goal_config_level =
CUSTOMER`, so every campaign bid toward the account's ~20 primary goals.

Behaviour:
- The goal step runs first among the post-create steps, with the geo step's partial-result
  contract: a failure returns the campaign id and an error saying the goal was NOT applied.
- A goal found REMOVED, or holding a different set, is restored to the set its name stands
  for; a create that loses a `DUPLICATE_NAME` race looks the winner up and uses it.
- A config update whose 2xx names another campaign's config is UNCONFIRMED.
- Under a click strategy (manual CPC, maximize clicks, and Demand Gen's only strategy) the
  step line says the goal sets what counts as a conversion, not what the campaign bids for.

Tests: `conversion_goal_test.go` covers create, reuse, restore (removed and edited), the
duplicate-name race, both failure arms, an UNCONFIRMED config result and the click-strategy
wording, plus one subtest per creating channel proving the step runs after its campaign
(each mutation-checked). `googleads_bidding_test.go` asserts the dispatch config reaches the
goal create and the config update, and that no create carries `selectiveOptimization`.

Open: the goal path has not run against the live account. A `validateOnly` probe of the
custom goal create and the config update is the remaining check before relying on it.
