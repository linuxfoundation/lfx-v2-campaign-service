# 2026-10-15 — Mixed Meta Insights currencies leave the monitor's budgets unknown

**Fix** — a review thread on #316 (LFXV2-2665).

- The insights-versus-account currency guard read only the currency every row agreed on. With a
  USD account and one USD plus one JPY Insights row, the rows' own disagreement withheld that
  currency (`""`), so the guard saw no disagreement and the budgets were still converted at USD's
  scale and paced against JPY-labelled spend.
- `metaAccountInsights` now keeps every readable code any row carried (`readable`), separately
  from the publishable label, and `conflictsWith` trips the guard when any of them is not the
  account read's currency: no campaign budget is converted and no ad-set fallback is read.
- `TestListAccountCampaigns_InsightsCurrencyMismatchLeavesBudgetsUnknown` gains mixed-row cases
  (USD+JPY and JPY+USD against a USD account) over both budget sources with settled timestamps;
  restoring the agreed-currency-only guard fails both.
- Updated [internal/platform/meta](../code/internal-platform-meta.md) and
  [Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md).
