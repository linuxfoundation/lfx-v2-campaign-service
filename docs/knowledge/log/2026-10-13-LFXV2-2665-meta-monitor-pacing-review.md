# 2026-10-13 — Meta monitor paces spend and plan over the same ad sets and days (LFXV2-2665, PR #316 review)

**Fix** — Fourth round of PR #316 review fixes, all one theme: Meta pacing must compare spend and
expected spend over the SAME ad sets and the SAME days, and where that cannot be established
the row is `pacing_unknown`, never a false alert.

- **Daily pacing counts only the flight's days in the window.** `metaExpectedSpend` expected
  `BudgetDay × days` whatever the flight, so a campaign started 7 days ago at full budget read
  ~23% on `days=30`. It now expects `BudgetDay` on the window's flight days (start and end
  inclusive; an unreported date is the window's edge), and a flight outside the window is unknown.
  On the ad-set path a daily budget is used only when the active ad sets share one window.
- **Lifetime end date inclusive, open-ended lifetime unknown.** The end date is counted
  inclusively (pinned by a last-day-only test), and a lifetime flight with no end date is
  `pacing_unknown`: there is no flight length to prorate over, and reading it as "ends today"
  expected the whole budget at once.
- **One window snapshot.** The service took `now` before the read while the Insights read took
  its own clock reading for `since`/`until`, so a request crossing UTC midnight paced one day's
  plan against the next day's spend. `fetchAccountCampaignInsights` now returns the bounds it
  sent, every row carries them (`model.AccountCampaignMetrics.MetricsWindowStart`/`End`), and
  `rules.EvaluateMetaMonitor(rows)` takes no clock or `days`; a row with no window is unknown.
- **Non-ACTIVE ad sets make the campaign unknown.** Paused ad sets' budgets were dropped from the
  sum while their earlier spend stayed in the campaign Insights (two ad sets at 10/day for a week,
  one then paused: 140 spent against 70 expected, a false overspend). When an ad set stopped is
  not on the read, so its budget cannot be prorated; any ad set not `ACTIVE` now leaves the
  campaign's ad-set budget unknown. Residual: Meta's default edge omits DELETED ad sets.
- **Invalid JSON from the ad-set read no longer fails the monitor.** `do` decoded the 2xx body
  first and returned a `transportError`; the read now takes it undecoded through a `rawBody` sink,
  and a body that is not valid JSON is an `adSetBudgetsUnreadableError` — budgets unknown,
  delivery metrics kept.
- **Flight timestamps are parsed whole.** `dateOnly` validated only the first 10 characters, so
  `"2026-10-01garbage"` was read as a flight date. It now parses the whole value as Graph's
  `2006-01-02T15:04:05-0700` or RFC 3339 and returns the date as written; anything else is "" (an
  unknown date, which leaves an ABO budget unknown). `docs/api-catalog.md` now states that any
  non-ACTIVE ad set makes an ABO campaign `pacing_unknown` and that daily ABO budgets also need a
  shared, readable window.
- **An empty `end_time` is open-ended.** `adSetWindow` read an ad set's `end_time: ""` as a
  malformed flight, so open-ended DAILY ABO campaigns stayed `pacing_unknown`. Empty is now "no
  end", like an absent field and like the campaign list's empty `stop_time`: daily pacing runs to
  the window's edge, and a lifetime budget with no end stays unknown in the rules layer. Only a
  non-empty value that is not a whole Meta timestamp is malformed.
- **Minor units are converted exactly.** The fractional-unit check was decimal, but the encoding
  still multiplied in binary float, so `90071992547253.9` × 100 was written as 9007199254725391.
  `minorUnitsExact` now computes amount×offset with `big.Rat` from the amount's shortest decimal
  form; the exact product decides too large (since the exactness-ceiling bullet below, past 15 digits of minor units),
  too small and fractional, and is what is encoded, for budgets and bids alike. The settings
  readback encodes the recorded amount the same way. The `MetaConfig` create contract in
  `docs/api-catalog.md` now says a fraction of a minor unit is refused at create too, not rounded.
- **Insights spend must be a finite, non-negative amount.** `fetchAccountCampaignInsights`
  accepted any `strconv.ParseFloat` result, so a negative, NaN or ±Inf `spend` could reach
  pacing (now also on the ABO path). `parseMonitorSpend` refuses them and the row is
  `FetchFailed`, as for any per-row parse failure; the counters already refused negatives through
  `parseMetricInt`. The `docs/api-catalog.md` settings-readback entry and
  `code/internal-dispatch.md` now say the recorded budget is encoded exactly, with the legacy
  `round(amount × offset)` fallback only for a fractional row from before #235.
- **A campaign with both budgets is `pacing_unknown`.** `metaExpectedSpend` paced a row with a
  daily AND a lifetime budget on the lifetime figure alone (or dropped it only when that flight
  was unpaceable), while the same mix on ad sets was unknown. Meta documents the two as mutually
  exclusive, so the campaign-list read now keeps neither (0/0, delivery intact, no ad-set
  fallback) and the rules refuse a row with both.
- **Exactness ceiling for minor units.** Amounts reach the encoder already decoded into a
  float64, so digits can be lost before any check (JSON `9007199254740992.4` → `…992`, accepted
  as a whole JPY amount). Budgets and bids — create and update alike — above 999,999,999,999,999
  minor units (15 digits, what a float64 keeps exactly) are refused "too large to represent
  exactly"; this replaces the int64 ceiling. 2^53 was not enough: `90071992547409.91` USD is below
  2^53 cents but decodes to `…409.9`. Documented in `docs/api-catalog.md`.
- **Spend must be a plain decimal.** `parseMonitorSpend` first requires `^[0-9]+(\.[0-9]+)?$`, so
  hex floats, exponents (`1e2` — Meta sends plain decimals), underscores, signs and bare dots make
  the row `FetchFailed`.
- **Published Meta monitor example follows the new pacing.** Merging main's per-platform
  monitor examples (#319): the Meta 200 example in `design/account_monitor_examples.go` is now
  what `EvaluateMetaMonitor` produces over the read's own window (Sep 6–Oct 5): the lifetime row
  spends 2360 of a 2950 window plan (5900 over a 60-day inclusive flight, 30 days in the window)
  for 80%, and the underspending text prints account-currency money, not `$`.
  `TestPublishedAccountMonitorExamplesMatchTheirOperation` stamps that window on the example rows,
  as the dispatcher does. Main's `TestListAccountCampaigns_FailedRowIsDecode` now gives its
  campaigns a campaign-level budget, since a budget-less one would send the read to the ad-set
  fallback its fixture does not serve.
- **A budget edited inside the window is `pacing_unknown`.** Pacing applied today's budget to
  every day of the window, so a mid-window edit raised false under/overspend alerts. Both reads
  now request `updated_time`; a CBO campaign updated, or an ACTIVE ad set updated, on or after the
  day before the window's first day (account-timezone margin) — or with an absent, null or
  malformed `updated_time` — leaves the campaign `pacing_unknown`. Conservative: `updated_time`
  moves on any edit. Meta's ad-account `/activities` edge (budget-update events with old/new
  values) could reconstruct the per-day plan; noted, not built. Both reads also take
  `created_time`: `updated_time` is stamped at creation, so an object created inside the window
  and never edited looked edited and a newly created campaign could never be paced. An
  `updated_time` within 60s of a readable `created_time` is not an edit; an absent or unreadable
  `created_time` keeps the conservative verdict.
- **A `null` campaign-level budget is unreadable.** The campaign-list budgets were decoded into
  strings, so `null` read as absent and the campaign fell back to its ad-set budgets. They are
  decoded raw now: null or non-string → `FetchFailed` (decode), no ad-set fallback.
- **Catalog: the HubSpot account-monitor row restored.** A merge resolution had dropped main's
  `GET .../connection-hubspot/account-monitor?days=` row from `docs/api-catalog.md`; it is back
  verbatim. A row-by-row diff against `origin/main` found no other lost or reverted row.
- **The budget snapshot is the later read, and identity-checked.** `ListAccountCampaigns` read
  the campaign list (budgets + `updated_time`) before Insights, so a CBO budget edited between the
  two calls was invisible; Insights is now read first (it is account-level and needs no campaign
  ids), then the campaign list, currency and ad sets. Every campaign-list page passes
  `identityjson.Check` before decoding — a duplicated `updated_time` or budget used to decode as
  whichever came last — and a refused page fails the read as the currency read's does. The
  dispatcher account-scope tests now expect the Insights path as the first request.
- **An unreadable campaign flight leaves the budget unknown.** The campaign list's
  `start_time`/`stop_time` were decoded into strings, so `null` and a malformed timestamp read as
  a missing flight and a daily CBO budget was paced over the whole window. They are decoded raw
  now: absent is no bound, an empty `stop_time` is "no end", and an empty `start_time`, `null`, a
  non-string or a malformed value leaves the campaign-level budget unknown (not `FetchFailed`),
  as an unreadable ad-set window does.
- **`bidToMinorUnits` overflow guard restored.** A scaled bid past the int64 range is refused
  exactly as `budgetToMinorUnits` refuses it, instead of converting to an implementation-defined
  (possibly negative) minor-unit bid. (First a float comparison against
  `float64(math.MaxInt64)`; now the 15-digit exactness ceiling above, well inside int64.)

No API-shape or migration change (the published Meta monitor example and the generated specs
carrying it did change, see above). See `architecture/account-monitor-endpoints.md` and
`code/internal-platform-meta.md`.
