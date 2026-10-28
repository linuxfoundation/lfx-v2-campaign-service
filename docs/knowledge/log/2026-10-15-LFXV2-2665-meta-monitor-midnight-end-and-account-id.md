# 2026-10-15 — LFXV2-2665: Meta monitor reads a midnight flight end as the day before, and checks whose currency it converts in

**Fix** — Two PR #316 review findings on the Meta account monitor. (1) A flight end — the
campaign list's `stop_time` and an ad set's `end_time` — now goes through `meta.endDateOnly`,
which maps a stop at exactly `00:00:00` (in the timestamp's own offset) to the previous date: that
midnight is the instant the date begins, so the flight's last inclusive day is the one before. An
externally managed campaign stopping at `"2026-10-11T00:00:00+0000"` used to read as ending
2026-10-11, so `rules.metaExpectedSpend` counted 11 flight days instead of 10 and an on-plan
lifetime campaign paced ~110% (a false overspend). `CreateCampaign`'s `23:59:59` ends are
unchanged. (2) `meta.accountCurrency` now requires the answer's `id` to name the requested ad
account (`act_` prefix ignored, `sameMetaAccount`); an answer naming another account, or none, is
`errAccountCurrencyWrongAccount`. The account monitor reads it as currency unknown — budgets left
unconverted, pacing unknown, the published `currency` withheld — instead of scaling a USD
account's budgets at a foreign currency's offset; the ad-set read fails on it, as it does on a
missing currency. See [internal/platform/meta](../code/internal-platform-meta.md).
