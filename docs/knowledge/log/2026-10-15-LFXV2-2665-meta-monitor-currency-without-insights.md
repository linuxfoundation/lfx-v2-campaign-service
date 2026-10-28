# 2026-10-15 — LFXV2-2665: Meta monitor publishes the currency when the window has no insights rows

**Fix** — The Meta account monitor's `currency` was absent for an account with no insights rows
in the window (no delivery yet — e.g. freshly created campaigns), although the budgets' currency
was known from the `?fields=currency` read they are converted in. With no row there is no
`account_currency` to disagree with, so `ListAccountCampaignsWithCurrency` now passes that
account read to `monitorPublishableCurrency` in its place, still subject to the known-offset check
(an unlisted code stays withheld and logged). Rows that omit, garble or disagree on
`account_currency`, an insights page `identityjson` refuses (`metaAccountInsights.noCurrencyEvidence`),
and an insights code that differs from the account read all still withhold it; a failed account
read still fails the monitor read. See
[Account-Monitor Endpoints § Currency](../architecture/account-monitor-endpoints.md#currency).
