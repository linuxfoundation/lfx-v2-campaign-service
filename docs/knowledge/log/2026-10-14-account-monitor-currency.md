# 2026-10-14 — LFXV2-2665: the account monitor reports the ad account's currency

**Update** — `AccountMonitor` gains an optional `currency` (ISO 4217, `^[A-Z]{3}$`): the ad
account's currency, in which every spend and budget amount in the response is denominated,
unconverted; absent when the platform didn't report it. The UI could only show monitor amounts
without a symbol before. Sources: Meta, the insights read's `account_currency` (no extra call);
LinkedIn, the budgets' `currencyCode` in the campaign list (no extra call) — the account's own
local currency, since the monitor's spend is Ad Analytics `costInLocalCurrency`, and absent when
the budgets disagree; X, the account read the monitor already makes for the timezone (cached, no
extra call); Reddit, one `GET /ad_accounts/{id}`; Microsoft, one Customer Management
`Account/Query`, whose `CurrencyCode` is read directly as a three-letter ISO 4217 code (the
withdrawn or non-ISO legacy codes in Microsoft's value set are withheld). Google Ads is
owned separately and leaves it unset. Every currency read is best-effort and identity-checked,
a malformed or disagreeing code is dropped, and a failed read leaves the field absent rather
than failing the monitor. New capabilities `service.AccountMonitorReader` (live) and
`service.AccountCurrencyReader` (report-backed, upstream operation `read_account_currency`);
every non-Google published example carries `currency`. See
[Account-Monitor Endpoints § Currency](../architecture/account-monitor-endpoints.md#currency).

Review round: LinkedIn's monitor now reads spend as Ad Analytics `costInLocalCurrency` instead of
`costInUsd`, so spend, budgets and pacing share the account's own currency and `currency` names
it on any account (a EUR account used to pace USD spend against EUR budgets). **For a USD account
the numbers are unchanged.** Budgets that disagree about the currency leave `currency` absent and
mark those campaigns' pacing unknown. Meta publishes its currency only when the minor-unit offset
is 100 — its monitor budgets still divide by 100, so a JPY/KRW account's would be mislabelled —
until #316's currency-aware conversion merges. The Reddit account read now runs beside the report
fan-out on its own 3s budget with no 429 retry, and Reddit's rows-only metrics reader makes no
account read; the Microsoft/X currency read is capped at 3s, Microsoft's `Account/Query` is not
retried on a 429 and a successful answer is cached on the client per account for 6 hours.

PR #338 review: the currency fields are now decoded RAW, on their own — a number, object, array,
boolean or `null` in X's `currency`, Meta's `account_currency` or a LinkedIn budget `currencyCode`
leaves the currency absent (and that LinkedIn campaign's pacing unknown) instead of failing the
shared decode, which on X took the timezone and the whole monitor with it. Meta's insights pages
and LinkedIn's campaign-list pages are checked with `identityjson` before decoding, so a repeated
or case-variant key can no longer collapse to its last value and be published; an untrusted page
withholds the currency but keeps the rows, unlike the audience reads, which fail on one.

PR #338 review: Microsoft's v13 `CurrencyCode` value set is three-letter codes, not names, so the
earlier mapping of names such as `USDollar` to ISO codes was wrong — it turned every real answer
into an absent currency and cached that for 6 hours. The returned code is now published when it is
a current ISO 4217 code from that set; the withdrawn or non-ISO AZM, BGL, BYB, EEK, LTL, LVL, ROL,
RUR, SIT, SKK, VEF, YUN and ZWD are withheld, and HRK is kept as the accurate label for an
unconverted account. An answer with no recognised code is cached for only 5 minutes.

PR #338 review: Reddit's ad-account `currency` is now decoded RAW, on its own
(`reddit.monitorAccountCurrencyField`), like the other four platforms' currency fields — a number,
object, array, boolean or `null` there leaves the currency absent without failing the account
decode, and the answer's account-id check still runs.
