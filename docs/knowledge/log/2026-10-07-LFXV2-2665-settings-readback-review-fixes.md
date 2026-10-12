# 2026-10-07 — LFXV2-2665 settings readback: pre-PR review fixes

**Fix** — Six findings from the pre-PR review of the Microsoft/Meta/Reddit/X settings readback:

- **No false 404 on Meta.** Graph 100/33 on `GET /{campaign_id}` also means "this token cannot
  load it", and that read is not account-scoped. It is now an absence only after one
  account-scoped probe (`GET /{act_id}?fields=id`, same token) proves the account loads;
  otherwise 503. No probe is sent after a 200.
- **Actionable 409 for upstream identity contradictions.** Meta/Reddit/X reporting the
  campaign under another account, or a recorded Meta ad set / X line item now in another
  campaign, were wrapped in `ErrCampaignAccountMismatch`, whose "reconnect the original
  account" message is unactionable when the connection already IS that account. New
  `domain.ErrCampaignUpstreamIdentityMismatch`, own 409 arm, fixed re-dispatch message.
- **Nudged-start window tightened** to the creation day or the day after (was ±1 day), so an
  edit to a day before creation reads `diverged`.
- **Reddit** reports `is_campaign_budget_optimization` upstream-only, explaining an `unknown`
  budget when it is off.
- **X** compares the budget only under `budget_optimization` `CAMPAIGN` (a LINE_ITEM total cap
  is not the recorded daily amount) and reports `budget_optimization` upstream-only.
- **api-catalog** states each platform's `unknown_count` floor (created and adopted rows)
  instead of Google's alone; design field list regenerated.
