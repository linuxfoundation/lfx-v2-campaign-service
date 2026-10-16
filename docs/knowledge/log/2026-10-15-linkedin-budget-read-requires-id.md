# 2026-10-15 — LinkedIn's budget read refuses a response naming no campaign

**Fix** — `linkedin.Client.GetCampaignBudget` now refuses a 2xx whose `id` is absent, `null`
or empty, as it already refused one naming a different campaign.

- The echo check was `got != "" && got != campaignID`, so a body with no id was accepted as the
  requested campaign's budget. Its fields then drove the budget lever's pacing guard, where an
  absent `dailyBudget`/`totalBudget` reads as "this campaign does not use this field".
- Sibling precedent: `GetCampaignSettings` reads the same `adAccounts/{acct}/adCampaigns/{id}`
  resource and requires the echo; Reddit's and X's `GetCampaignBudget` and Meta's
  `GetAdSetBid` refuse a missing id with the same `returned no … id instead` wording.
- The refusal is a pure-read failure, so `LinkedInDispatcher` still reports it as definite
  (nothing was written).
- Tests: `TestGetCampaignBudget_RefusesAResponseNamingNoCampaign` (absent, null, empty id).
  Mutation-checked: restoring the `got != ""` short-circuit fails all three cases.
- Concept updated: `code/internal-platform-linkedin.md`.
