# 2026-10-07 — LFXV2-2665 settings readback for Microsoft, Meta, Reddit and X

**Update** — `get-campaign-settings` (`GET /projects/{projectId}/briefs/{briefId}/campaigns/{id}/settings`)
is now wired for Microsoft Advertising, Meta, Reddit and X as well as Google Ads; LinkedIn and
HubSpot still answer 400. Each of the four `*Dispatcher` types implements `service.SettingsReader`
(`internal/dispatch/{microsoft,meta,reddit,twitter}_settings.go`) over a new pure-read client
method `GetCampaignSettings` per platform, sharing `settings_readback.go`.

- **Per-platform field sets.** Microsoft compares `budget_amount`, `budget_type`,
  `campaign_name` and reports `status`, `budget_explicitly_shared`, `bidding_strategy_type`
  upstream-only (no flight dates — Microsoft campaigns have none). Meta, Reddit and X compare
  `budget_amount`, `budget_type`, `campaign_name`, `start_date`, `end_date` and report `status`
  and `bidding_strategy_type` upstream-only.
- **Units as each create path wrote them.** Microsoft DailyBudget (account-currency decimal,
  only under a daily `BudgetType`); Meta minor units through the account currency's own offset,
  the recorded side encoded as `round(amount × offset)`; Reddit `goal_value` and X
  `*_local_micro` micros, rendered like Google's (full precision when sub-cent). Dates are UTC
  calendar dates; a Meta/Reddit start nudged past UTC midnight at dispatch is shown with both
  sides and `unknown` (new `model.UncomparableSettingsField`), never a false `diverged`.
- **Provenance is Google's rule.** Unknown provenance → 409 before any connection is resolved;
  account mismatch → 409, including an account the platform reports for the campaign, and a
  recorded Meta ad set / X line item that belongs to another campaign. Adopted rows read at
  the campaign level with child-dependent fields absent.
- **Definite vs unknown.** No such campaign → 404; transport, 5xx, 401/403, exhausted 429,
  `identityjson`-refused bodies, a wrong id echoed, non-integer or contradictory amounts → 503.
- Design description and `campaign-settings-field` updated (`make apigen`); api-catalog row,
  `code/internal-dispatch.md`, `code/internal-service.md` and the four platform client concepts
  updated.
