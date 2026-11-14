# 2026-11-14 — #563 Microsoft multiple ad groups at create

**Update** — Gap 12 of the Microsoft Advertising gap review (#563), as the owner decided on
2026-11-14: `microsoftConfig.adGroups`, up to five ad groups under one campaign, each with its own
name suffix, keywords, negative keywords, remarketing lists and ad copy. Without it the create
builds today's single ad group unchanged.

- Each per-group field has its single-group twin's rules and flag; negatives go on the ad group
  (EntityType `AdGroup`); lists are read before anything is created and need
  `global_marketing_ops`. The single-group keyword, copy and list fields are refused alongside
  `adGroups`.
- Partial rules: the first group failing is today's failure; a later one failing leaves the
  earlier groups (no rollback), the rest are still built, and the campaign is `created_degraded`
  with a step naming every group not created.
- Every group is recorded as `adGroups` in the result; the top-level ad group fields are the
  first group's.
- Pause/resume address every created group (resume opens them before the campaign gate, pause
  closes the gate first), the bid lever re-bids every group, and keyword actions may name any
  group, one per request. The monitor and reports are campaign-level and unchanged.
- Behind `MICROSOFT_AD_GROUPS_ENABLED` (chart `"false"`): off, a 400 before any job, re-checked in
  Dispatch.

Not verified live: no Microsoft call was made.
