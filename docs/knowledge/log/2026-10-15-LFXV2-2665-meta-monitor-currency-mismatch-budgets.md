# 2026-10-15 — A Meta currency mismatch leaves the monitor's budgets unknown

**Fix** — a review thread on #316 (LFXV2-2665).

- When the insights rows' `account_currency` was usable but disagreed with the validated
  `?fields=currency` read, `ListAccountCampaignsWithCurrency` only withheld the published
  currency; the budgets were still converted in the account-currency read and reached pacing.
  Spend is reported in the insights currency, so a `"1000"` daily budget converted as USD 10 could
  be paced against JPY spend and raise a false alert.
- That disagreement now leaves the budget currency unknown for the whole read: no campaign-level
  budget is converted and no ad-set fallback is read, so pacing is unknown, while spend,
  impressions and clicks are kept and the currency stays withheld. Insights with no usable code
  (no rows, malformed, rows disagreeing) is not a disagreement.
- `TestListAccountCampaigns_InsightsCurrencyMismatchLeavesBudgetsUnknown` covers both budget
  sources (CBO campaign budget, ABO ad-set fallback) with settled, readable
  `created_time`/`updated_time`, so only the mismatch can make the budgets unknown, plus an
  agreeing-currency control. Removing the gate fails both mismatch cases.
- Updated [internal/platform/meta](../code/internal-platform-meta.md) and
  [Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md).
