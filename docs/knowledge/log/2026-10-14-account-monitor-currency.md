# 2026-10-14 — LFXV2-2665: the account monitor reports the ad account's currency

**Update** — `AccountMonitor` gains an optional `currency` (ISO 4217, `^[A-Z]{3}$`): the ad
account's currency, in which every spend and budget amount in the response is denominated,
unconverted; absent when the platform didn't report it. The UI could only show monitor amounts
without a symbol before. Sources: Meta, the insights read's `account_currency` (no extra call);
LinkedIn, the budgets' `currencyCode` in the campaign list (no extra call), published only as
`USD` because LinkedIn's spend is `costInUsd`; X, the account read the monitor already makes for
the timezone (cached, no extra call); Reddit, one `GET /ad_accounts/{id}`; Microsoft, one
Customer Management `Account/Query` with its `CurrencyCode` name mapped to ISO. Google Ads is
owned separately and leaves it unset. Every currency read is best-effort and identity-checked,
a malformed or disagreeing code is dropped, and a failed read leaves the field absent rather
than failing the monitor. New capabilities `service.AccountMonitorReader` (live) and
`service.AccountCurrencyReader` (report-backed, upstream operation `read_account_currency`);
every non-Google published example carries `currency`. See
[Account-Monitor Endpoints § Currency](../architecture/account-monitor-endpoints.md#currency).
