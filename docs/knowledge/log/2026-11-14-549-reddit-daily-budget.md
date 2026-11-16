# 2026-11-14 — Reddit daily budget at create (#549)

**Update** — `redditConfig.budgetType` chooses the pacing of `budgetUsd` on a Reddit create:
`"lifetime"` (the default, `goal_type` `LIFETIME_SPEND`, unchanged) or `"daily"` (`DAILY_SPEND`,
so `budgetUsd` is a per-day cap). This is gap 9 of the Reddit Ads gap review. The campaign row
records the matching `budget_type`, so the budget lever, the settings readback and the account
monitor, which already read both goal types, compare against the right quantity.

`reddit.ResolveBudgetGoalType` holds the mapping. `RedditDispatcher.PreflightCreate` refuses an
unknown value, `null`, an empty or non-string value, a case variant of the key, or the field
while the flag is off with a synchronous 400; `Dispatch` and the client re-check before any Reddit
request. The field sits behind `REDDIT_DAILY_BUDGET_ENABLED` (chart `"false"`) until a live
create confirms Reddit accepts `DAILY_SPEND` on a budget-optimized campaign with an `end_time`.
Gap review: #549.
