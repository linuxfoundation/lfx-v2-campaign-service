# 2026-10-07 — Qualify the readback unknown_count floor by budget mode

**Docs** — review of #286. The api-catalog `/settings` row gave created X campaigns an
`unknown_count` floor of 3 of 8, but when X reports `budget_optimization` as `LINE_ITEM` or omits
it, both budget fields are `unknown` by construction and a healthy read counts 5. The row now
qualifies each floor by the budget mode the platform reports: X 5 (LINE_ITEM or omitted), Reddit 5
(campaign budget optimization off), Meta 4 (campaign-level CBO budget), and points to the
per-field `comparison` and the upstream-only budget-mode field to tell this from a failed read.
