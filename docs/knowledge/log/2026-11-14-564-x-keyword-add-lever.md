# 2026-11-14 — #564 X positive keyword add lever

**Update** — gap 12 of the X Ads gap review (#564): `add-keyword-targeting`,
`POST /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keyword-targeting/additions`,
the counterpart of `remove-keyword-targeting` for X.

- Body `{"keywords": [...]}`, 1-20 distinct keywords after the create-time rules (#574): trimmed,
  1-80 characters, no control character, case-folded duplicates dropped.
- One `BROAD_KEYWORD` / `EQ` targeting criterion per keyword on the recorded line item
  (`twitter.Client.CreateKeywordCriterion`, sharing the negative add's POST and echo check).
- Idempotent: a live broad keyword already on the line item is `ALREADY_PRESENT` with its id, not
  sent. The line item may hold at most 60 criteria, counting the ones it already has (409 over).
- Serialized per line item with the Postgres advisory lock Reddit's targeting writes use, now bound
  to the X dispatcher too; kept 15 s after an UNCONFIRMED POST.
- Role gating is the removal's: `campaign_manager` through `/briefs/**`; a new parity row pins the
  path. Any platform but X is 400.
- New default-off flag `TWITTER_KEYWORD_ADD_ENABLED` (constants, chart values, README,
  deployment.md).

Not verified live: no X call was made. Before enabling, add one positive criterion to a paused test
line item and confirm the echo carries `id`, `line_item_id`, `targeting_type`, `targeting_value`
and (if present) `operator_type` `EQ`.
