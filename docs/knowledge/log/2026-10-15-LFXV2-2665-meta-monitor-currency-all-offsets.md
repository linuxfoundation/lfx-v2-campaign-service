# 2026-10-15 — LFXV2-2665: Meta monitor publishes the currency for every offset its budgets convert

**Update** — The Meta account monitor's `currency` is no longer limited to currencies whose
minor-unit offset is 100. Budgets are converted with the account currency's own offset (the
`?fields=currency` read, `minorToWhole`), so `meta.monitorPublishableCurrency` now publishes the
insights rows' `account_currency` for every code in `currencyMinorUnitOffset` — the offset-1
zero-decimal currencies (JPY, KRW, CLP, VND, IDR, HUF, …) as well as the offset-100 ones — when it
equals the account-currency read the budgets were converted in. It still withholds (and logs) a
code outside that map, whose budgets are left unconverted (no three-decimal currency such as KWD
is in it), and a code the two reads disagree on. The `currency` description in `design/connection.go`,
`docs/api-catalog.md` and the Meta concept files are updated. See
[Account-Monitor Endpoints § Currency](../architecture/account-monitor-endpoints.md#currency).
