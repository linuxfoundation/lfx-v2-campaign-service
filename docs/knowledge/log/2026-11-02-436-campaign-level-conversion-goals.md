# 2026-11-02 — #436 campaign-level conversion goals on every Google channel

**Fix** — Conversion actions now reach Google on every channel this client creates
(Search, Display, Demand Gen and Performance Max; Video is wired the same way but its
creation stays refused). After the campaign is created, `applyCampaignConversionGoal`
(`internal/platform/googleads/conversion_goal.go`) finds or creates one
`CustomConversionGoal` per action set, named `LFX | conversion goal | <hash>` from the
sorted set so campaigns naming the same actions share it. Then one atomic
`googleAds:mutate` sets the campaign's `ConversionGoalCampaignConfig` to
`goal_config_level` CAMPAIGN with that goal and every inherited account goal still biddable
to `biddable=false`, so the campaign bids only toward the chosen actions.

Why: the old path sent `campaign.selective_optimization` at create time on Search, Video
and Display and refused the list on Demand Gen and Performance Max. Google documents that
field for App campaigns only, and it never ran against the live account; a read-only GAQL
over all 41 LFX campaigns showed none carrying it and every one at `goal_config_level =
CUSTOMER`, so every campaign bid toward the account's ~20 primary goals.

Behaviour:
- The goal step runs first among the post-create steps and every channel returns on its
  failure: the campaign id comes back with an error saying the goal was NOT applied and
  that nothing past the campaign was built.
- No existing goal is ever written: the account is shared by every foundation, so a goal may
  count conversions for live campaigns in other projects. A goal named for the set but
  REMOVED, or edited to hold something else, is left as it is; an enabled goal holding exactly
  the set is reused, or a new one is created under the set's second name (`<name> | 2`).
- A create that loses a `CUSTOM_GOAL_DUPLICATE_NAME` race uses the winner if it holds the set.
- Google refuses a second goal holding the same action list under any name
  (`DUPLICATE_CONVERSION_ACTION_LIST`). The enabled goal holding exactly that set, often
  one made by hand, is then reused.
- An atomic write whose outcome is unknown (a 5xx, a transport failure, an exhausted 429, or
  a refusal after a retried 429) is UNCONFIRMED, as is a 2xx whose results do not name the
  config and each goal taken out of bidding. A failure before that write stays NOT applied:
  only it changes what the campaign bids toward.
- Under a click strategy (manual CPC, maximize clicks, and Demand Gen's only strategy) the
  step line says the goal sets what counts as a conversion, not what the campaign bids for.

Tests: `conversion_goal_test.go` covers create, reuse, a removed or edited goal left as it is
(the second name, and the fallback to a goal holding the set), the duplicate-name race, the
duplicate-action-list reuse, a create without a usable name, the biddable write and its result
checks, both failure arms, an UNCONFIRMED write and the click-strategy wording, plus one subtest per creating channel proving the step runs after its campaign
(each mutation-checked). `googleads_bidding_test.go` asserts the dispatch config reaches the
goal create and the config update, and that no create carries `selectiveOptimization`.

Verified live under `validateOnly` (2026-11-03, LF account, nothing created): the custom goal
create; the atomic config update with every biddable campaign goal set not biddable (either
order); and the `DUPLICATE_CONVERSION_ACTION_LIST` refusal for a set an existing goal holds.
The biddable update alone is refused with `EMPTY_CONVERSION_GOALS`, which is why the two go
together. A real create is still unexercised.
