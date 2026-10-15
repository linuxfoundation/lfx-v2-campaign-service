# 2026-10-15 — LFXV2-2665: Meta monitor merges the published currency with per-currency budgets

**Note** — Merging main (#338's monitor `currency`) into this branch keeps both behaviours in
`meta.Client.ListAccountCampaignsWithCurrency`: budgets are converted with the account currency's
own offset from the `?fields=currency` read (`minorToWhole`), and the published `currency` is
still the insights rows' `account_currency`, withheld unless its offset is 100
(`monitorPublishableCurrency`). The Insights read now returns its `since`/`until` window on
`metaAccountInsights`. Tests expecting only two upstream reads now expect the third
(account-currency) read. Lifting the offset-100 restriction is a separate change. See
[Account-Monitor Endpoints § Currency](../architecture/account-monitor-endpoints.md#currency).
