# 2026-10-16 — Meta's ad-set budget read refuses a response naming no ad set

**Fix** — `meta.Client.GetAdSetBudget` now refuses a 2xx whose `id` is absent, `null` or empty,
as it already refused one naming a different ad set.

- The echo check was `resp.ID != "" && resp.ID != adSetID`, so a body with no id was accepted as
  the requested ad set's budget. Its fields then drove `MetaDispatcher`'s budget-write guards,
  where an absent `daily_budget`/`lifetime_budget` reads as "this ad set does not use this
  field" and an absent campaign budget reads as "not CBO".
- Same bug and same fix as LinkedIn's `GetCampaignBudget`
  ([2026-10-15-linkedin-budget-read-requires-id](2026-10-15-linkedin-budget-read-requires-id.md)).
  Sibling precedent: `GetAdSetBid` reads the same ad-set object and already requires the echo
  with the same `returned no ad set id instead` wording.
- The refusal is a pure-read failure, so the dispatcher still reports it as definite (nothing
  was written).
- Tests: `TestGetAdSetBudget_RefusesAResponseNamingNoAdSet` (absent, null, empty id).
  Mutation-checked: restoring the `!= ""` short-circuit fails all three cases.
- Concept updated: `code/internal-platform-meta.md`.
