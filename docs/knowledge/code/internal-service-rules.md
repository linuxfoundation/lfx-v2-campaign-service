---
type: "Code Concept"
title: "Pacing and Action-Item Rules (internal/service/rules)"
description: "One implementation of campaign pacing and operator action items, replacing four UI copies that disagreed on thresholds three ways. Pacing is spend against what a flight expects BY NOW, computed over the same period the spend figure covers, with incomputable kept distinct from zero."
resource: "internal/service"
---

# Pacing and Action-Item Rules (`internal/service/rules`)

## Overview

`internal/service/rules` derives two things from a campaign's measured state: a **pacing** band
(is this campaign spending what its flight expects by now?) and a list of **action items**
(what should an operator look at, and what should they do?).

**This package (`pacing.go`/`actions.go`) is not the only rule engine in this
directory.** The six `monitor_*.go` files (`monitor_google.go`,
`monitor_linkedin.go`, `monitor_meta.go`, `monitor_reddit.go`,
`monitor_microsoft.go`, `monitor_twitter.go`) back the account-scoped `/account-monitor`
endpoints, and a seventh, `monitor_hubspot.go`, backs HubSpot's project-scoped email monitor
(see the HubSpot section at the end). The first four were ported from the LFX One BFF's own four historically
divergent rule engines; `monitor_microsoft.go` was written here, on the same shared helpers,
for Microsoft's report-backed monitor (daily budgets only, so Google's daily pacing model;
a shared-budget campaign arrives `PacingUnknown` and is neither paced nor called a
placeholder budget). `monitor_twitter.go` (`EvaluateTwitterMonitor`) is the second
written-here engine, for X's report-backed monitor. It paces a DAILY budget first —
`BudgetDay` × the days of the saved report's window `[first day, the midnight after the last
day)` a line item is scheduled on, the convention the Reddit daily branch settled — and otherwise a
TOTAL budget prorated over the scheduled days (which then needs every range closed), else
`unknownPacingRow`. "Scheduled" means `AccountCampaignMetrics.FlightRanges`, the union of the
line items, whenever it is set (`twitterCoveredDays` / `twitterScheduledDays`), never the
envelope `StartDate..EndDate`, which counts the gap between two line items as scheduled and so
raised a false zero-delivery HIGH for a window inside it
(`TestEvaluateTwitterMonitor_FlightRangesNotEnvelope`). Its rules reuse siblings' values under
named constants (Google's placeholder budget ≤ 1/day, sized in the account's currency since #454 — see Currency below — and the shared 0.3%/1000 low-CTR pair), add
a HIGH zero-delivery item gated on at least one scheduled day in the window, print amounts as
"12.50 in account currency" with no currency symbol (X's figures are in the account's own
currency, which the read does not learn; every ad-platform monitor now prints money this way
through the shared `accountMoney`, #435) and a currency-neutral placeholder-budget remedy
(the wording the Google and Microsoft monitors also use since #448's follow-up), suppress the
0% underspend
item that would restate it (Reddit's rule), and carry no clicks-without-conversions rule —
X conversions are never reported. It takes no `now`: the window is the saved report's own
first and last day IN THE ACCOUNT'S TIMEZONE (`windowStart`/`windowEnd`, from
`ReportedAccountRead.MetricsWindowStart/End`), the same calendar the stats jobs and the
line items' flight dates use. A UTC-clock "today" disagreed with them for part of every day —
on a US/Pacific account each evening it counted a flight starting the next local day as
scheduled and raised a false zero-delivery HIGH (`TestEvaluateTwitterMonitor_EveningInAccountTimezone`).
With no window, pacing and the zero-delivery rule are skipped; the window-free rules still run.
Microsoft's engine takes no date (daily budget × days, no flights), so it is unaffected.

They were originally kept unmerged, bug for bug, so an OLD-vs-NEW differential
diff against the still-live BFF stayed a meaningful faithfulness check. That
diff is no longer the plan of record, so the duplication no longer buys
anything: the four now share **one** pacing ladder, `pacingLabelFor` in
`monitor_shared.go`, and the deliberately-ported defects are being fixed under
their own tickets instead of frozen.

The two read paths (brief view and account monitors) also share the **ladder** — its
implementation and, since D2, its numbers.

## One pacing ladder: 50 / 90 / 100 everywhere (D2)

Placing a percentage on a band lives in **one** place: `PacingLadder.Label` in
`internal/service/rules/ladder.go`. Bands are half-open upward — `pct < Underspending`
is underspending, up to and including `Constrained` is normal, up to and including
`Overspending` is constrained, above that is overspending. **One** ladder feeds it,
`StandardLadder()` = 50 / 90 / 100: below 50 underspending, 50–90 normal, above 90 up to and
including exactly on plan (100) constrained, above 100 overspending. It is used by
`ComputePacing` → `Evaluate` (the single-campaign brief view, via `DefaultThresholds`;
`Thresholds` is an alias of `PacingLadder`) and by `pacingLabelFor` → every `EvaluateXMonitor`.

**Product decision D2 (2026-10-13): the pacing ladder is 50/90/100 everywhere.** Before it the
brief view ran its own `BriefViewLadder` (50 / 100 / 130, matching lfx-self-serve's
`CAMPAIGN_PACING_THRESHOLDS`, under which exactly on plan was normal) and the monitors ran
`AccountMonitorLadder` (50 / 90 / 100); both were replaced by `StandardLadder`. The visible
shift is on the brief path only: values above 90% through 100% of plan moved from `normal` to
`constrained` (exactly 90% stays `normal`; exactly 100% — on plan — is in this band), and values
above 100% through 130% moved from `constrained` to `overspending` — so a brief campaign exactly
on plan now raises a `budget_constrained` item. Changing the ladder moves both read paths' alert bands at once; do
not add a second ladder, band switch, priority rank or unknown-pacing builder without a product
decision behind it.

What stays per caller, deliberately:

- **Rounding.** Google, Microsoft, Meta, Reddit and X `math.Round` the percentage before it
  reaches the ladder; LinkedIn and the brief view do not. So the same raw 49.99% is
  underspending on LinkedIn and on the brief view, and normal on Google.
- **NaN.** `Label` places NaN in overspending (the brief view's historical switch, unreachable
  there because `ComputePacing` refuses a non-finite percentage). The account monitor's
  historical switch placed NaN in normal, and `pacingLabelFor` keeps that with an explicit
  guard — a platform-reported NaN spend survives `math.Round` and reaches it.
- **Unknown pacing.** Two contracts, so two builders, one each: `UnknownPacing()` for the brief
  view (label `unknown`, not computable) and `unknownPacingRow` for the account monitor (the
  enum has no unknown member, so a placeholder `normal` label with `PacingUnknown` set).
- **Priority rank.** Only the account monitor sorts its items (`priorityRank`/`sortByPriority`
  in `monitor_shared.go`); the brief view's `Evaluate` returns items in rule order.

`ladder_pin_test.go` places every boundary (49.99/50/50.01, 89.99/90/90.01, 99.99/100/100.01,
the brief view's former 129.99/130/130.01 edge, plus points either side of each rounding
half-way mark), zero spend, no budget and a failed fetch through every caller's own entry
point, and states each caller's own outcome; `TestComputePacing_D2LadderBoundaries` adds
49.9/50/89.9/90/90.1/100/100.1 on the brief path.

It exists because the UI carried four separate implementations of this logic — LinkedIn, Reddit,
Meta and campaign-metrics — which disagreed three ways on the underspending floor, and Reddit
disagreed with *itself*: it labelled at 50 and alerted at 40, so a campaign could be shown as
healthy while raising an alert. Consolidating into one package behind one set of thresholds is
the point; the alternative was picking one of the four and silently moving every other
platform's alerting.

## Pacing is against the plan-to-date, not the whole budget

`Pct` is spend as a percentage of what the campaign should have spent **by now**. A campaign
three days into a thirty-day flight is expected to have spent a tenth of its budget, so
comparing against the full budget reports every healthy campaign as severely underspending for
most of its life. This is the property the whole rule set rests on.

## `spendDays` is required, and it is not a convenience

`ComputePacing` takes the number of days the spend figure covers as a separate argument. The
only spend this service can read is **window-scoped** — `CampaignMetrics.CostMicros` is cost
over `last_7_days` and friends — while a lifetime `BudgetAmount` describes the whole flight.

Without that argument the two inputs silently describe different periods: a 7-day spend divides
into a 30-day plan and yields a confident *"23% of plan"* for a campaign that is exactly on
track, which then raises an underspending item against it. Both arguments are bare `float64`, so
this is the one error the function cannot detect for itself — the parameter exists to make the
caller state the period rather than assume it.

Expected spend is computed over `min(spendDays, elapsed)`: a window wider than the flight cannot
manufacture plan days that have not yet elapsed.

**A day count is not enough on its own, because it carries no position.** `last_month` is 31 days,
but for a campaign that started last week those days sit almost entirely *before* the flight
began — so a spend of zero over that window is correct and means the campaign did not exist yet.
Paced against an expectation measured from the flight's start it reported underspending against a
campaign with nothing to answer for. `WindowDaysWithinFlight` therefore resolves the window to an
interval and intersects it with the flight; a window that does not overlap at all yields no
pacing rather than a confident zero.

The interval is also **clamped to now**, because a window cannot carry spend from time that has
not happened. `today` at noon is half a day of spend, and counting it as a full day of plan
reports a campaign spending exactly on plan as ~50%. This costs a little accuracy on the rolling
windows, where the platform reports the whole span and only the final day is partial —
`last_7_days` at noon reads ~8% ahead — but that bias is bounded by half a day out of the
window's length and shrinks as the window grows, whereas the unclamped error on `today` is a
factor of two and worst early in the day. Erring toward "spending ahead" is also the safer
direction: it never manufactures an underspending item against a healthy campaign.

**The flight's end date runs through the END of its day.** `end_date` is a `DATE` column, so it
decodes to midnight — the *start* of the final day, not the finish of the flight. Both consumers
normalise it to the FOLLOWING midnight before using it as an exclusive bound (`flightEndInstant`,
shared by `ComputePacing` and `WindowDaysWithinFlight` so the two cannot drift apart). Every
platform adapter already read the column this way — Meta sends `EndDate + "T23:59:59+0000"`, X
adds a day, LinkedIn uses end-of-day — and this package was the outlier.

Using the raw midnight cut the last day off every flight, in two places at once. `daysBetween`
counted a two-day flight as one, so a $100/day campaign that had correctly spent $200 was priced
against a single day of plan and reported 200%, `overspending`. And `WindowDaysWithinFlight`
clamped the window back to an instant already past, returning a zero overlap that `ComputePacing`
reads as incomputable — pacing went `unknown` for the whole of every flight's final day, the day
an operator is most likely to be looking at it. A one-day flight (`start == end`) had zero length
and tripped the inverted-flight guard, so it was never measurable at all.

The **start** needs no such adjustment and deliberately gets none: midnight of the start date is
already the instant the flight opens. A campaign starting today has elapsed a genuine fraction of
a day by noon, which the one-elapsed-day floor below correctly declines to pace.

**Nor is pacing meaningful in a campaign's first day.** A campaign launched a minute into a
30-day $1000 flight is expected to have spent two cents, so a spend of zero is 0% — a
HIGH-priority underspending item against a campaign whose only property is being new. It would be
wrong even with perfect data, and ad platforms report spend with a lag, so the first hours read
as zero regardless of what the campaign is doing. Below one elapsed day, pacing is `unknown`.
Elapsed time is also measured as a FRACTION of a day rather than rounded up, so expected spend
scales smoothly instead of jumping to a full day's plan the instant a flight begins.

## Incomputable is not zero

`Pacing.Computable` is false when there is no budget, no usable flight, or no measurable period.
In that state `Pct` is zero **because nothing was measured**, not because the campaign spent
nothing. A consumer that renders an incomputable pacing as `0%` reports the absence of a budget
as a campaign that failed to spend — the same substitution that turned a failed dashboard read
into a "pause losing campaigns" recommendation. `PacingUnknown` is a distinct label from
`PacingNormal` for the same reason.

## Band boundaries are half-open upward

A value sitting exactly on a threshold lands in the **healthier** band. That matters most at
100% on the brief view: a campaign spending precisely what its flight expects by now is *on
plan*, and labelling it `constrained` would raise a budget item against the only campaign that
needs none.

`PacingLadder` therefore carries the floor and two tops, not a separate `normal` value. The
shared UI constants also define `normal` (90), but on the brief view it names the top of a band
derived from `Constrained` — the healthy band runs up to and including it. Carrying it as a
field would offer a knob a caller could turn with no effect.

## LinkedIn lifetime budget with a daily cap (#584)

`linkedinPacingPct` paces a TOTAL budget prorated over the flight, else a DAILY budget times the
window's days inside it. A campaign carrying BOTH (a lifetime total with a daily cap, as
`linkedInConfig.dailyBudgetUsd` creates) is created with no `runSchedule.end`, and the lifetime
branch reads a missing end as "the flight ends today" — so a week's window of a month-long
campaign would be paced against the whole total and read as badly underspending. Such a row is
therefore paced against the daily cap when it has no end — capped at the total, because a $100
total under a $20 cap stops once the $100 is gone and a week's window can never expect $140 — and
against the smaller of the two expectations when it has one (`linkedinExpectedSpend`,
`linkedinTotalExpected`, `linkedinDailyExpected`; `TestLinkedinPacingPct_LifetimeWithDailyCap`,
`TestLinkedinExpectedSpend_CappedNoEndNeverExceedsTotal`). The metrics carry the window's spend,
not the campaign's lifetime spend, so the cap is the whole total rather than what remains of it:
a campaign that exhausted its total BEFORE the window still reads 0% against that total. A
single-budget row paces as before.

## Currency

The account monitors' money thresholds — the placeholder daily budget (≤ about 1 USD;
Google, Microsoft and X), the high average CPC (> about 5 USD; Google and Microsoft) and
LinkedIn's paused-with-real-budget line (> about 10 USD lifetime, `pausedRealTotalBudgetMinUSD`,
or > about 1 USD a day, the placeholder line read from the other side) — live once in
`monitor_shared.go` (`placeholderBudgetMaxUSD`, `highAvgCpcUSD`, `isPlaceholderBudget`,
`isHighAvgCpc`, `isRealBudget`, `aboveAccountThreshold`) and are sized in the ad account's
currency, which `EvaluateGoogleMonitor`, `EvaluateMicrosoftMonitor` and `EvaluateTwitterMonitor`
take since #454 and `EvaluateLinkedInMonitor` since #547. LinkedIn's currency is the campaign
budgets' `currencyCode` from the same read, so an account with no budgeted campaign reports none;
its skipped item is therefore computed over the measured, budgeted, PAUSED rows only
(`linkedinPausedBudgeted`), not every measured row. `monitorUnitsPerUSD` is
a table of rounded units-per-dollar magnitudes for the currencies whose rate is stable enough
for a fixed figure; high-inflation currencies are left out. The sized threshold and the amount it
is compared with are both rounded to the currency's ISO 4217 minor unit (`moneyAt`,
`monitorMinorUnitDigits`: two decimals by default, 0 for JPY/KRW/CLP/VND, 3 for
KWD/BHD/OMR/JOD), so a float product such as 5 × 1.38 = 6.8999… cannot call a CAD 6.90 click
above a CAD 6.90 threshold. A currency missing from it, or an
absent one, fires neither rule on an amount — Google's and Microsoft's zero daily budget is still
a placeholder in any currency — and the evaluator adds one account-wide LOW item
(`moneyRulesSkippedItem`) saying those checks were skipped, whenever at least one returned row
was measured, so a skipped check is never mistaken for a clean one. Before this, a JPY account tripped the high-CPC rule on a ¥50
click and a KWD account's 1/day budget (about $3) was flagged HIGH as a placeholder.

Pacing is meaningful **per campaign only**. This service performs no FX conversion and platform
cost is reported in each platform's native unit, so `Pct` must never be totalled or averaged
across platforms — the same reason the brief-metrics response refuses a cross-channel cost total.

## The five rules

| Rule | Fires when |
|---|---|
| `zero_delivery` | No impressions **and** no spend, on a **paid-ads** campaign believed to exist upstream **whose flight has begun**. Suppresses the pacing rules below for that campaign |
| `underspending` | Pacing is computable and below the floor |
| `budget_constrained` | Pacing is computable and `constrained` (above 90% up to and including 100% of plan — issue text "Budget constrained") or `overspending` (above 100% — "Spending ahead of plan"); one rule token, `MED`, for both |
| `low_ctr` | CTR below the threshold, over enough impressions to mean anything |
| `no_conversions` | Zero **measured** conversions over enough clicks (50) to mean anything — a non-nil count, so never on a platform that cannot report conversions or on an incomplete LinkedIn/Microsoft read. Never on a zero-click window (the click floor excludes it); a window with no delivery at all is `zero_delivery`'s finding |

Two guards carry most of the weight. **Zero delivery requires both signals** — impressions alone
is an unbilled serve, spend alone is a billing entry with no serve, and flagging either trains
operators to ignore the rule.

It also **waits for the flight**, via the same one-elapsed-day floor the pacing rules use
(`DeliveryExpected`): a campaign dispatched days before its start date has delivered nothing for
the same reason it has spent nothing, and the rule would otherwise tell an operator to check
targeting and creative approval for a campaign whose only property is being early.

It is also **paid-ads only**, and it **suppresses the pacing item**. The email channel bills
nothing per send and its adapter always reports zero cost, while mapping opens onto impressions —
so a delivered email nobody opened is numerically identical to a campaign that never served, and
the rule would tell an operator to check targeting and approval for an email delivered exactly as
intended. And a campaign that never started is trivially at 0% of plan, so emitting both items
gives the operator two HIGH findings with opposite remedies: one says no budget change will fix
this, the other says to adjust the budget. **Low CTR needs a delivery floor**, because three impressions and
no clicks is a 0% CTR that says nothing about the creative.

**No conversions needs two floors, not one.** The click floor is the direct analogue of the CTR
rule's impression floor: a campaign with a handful of clicks and no conversions is describing
normal variance, not a broken funnel. It is set lower than the CTR floor because clicks sit a
funnel step further down and arrive one to two orders of magnitude less often — reusing the
impression floor would mean the rule effectively never fired on the campaigns this service runs.

The floor also means `no_conversions` **never fires on a window with zero clicks**, however quiet
the campaign: an empty or no-activity window is `zero_delivery`'s finding (gated on the flight
having begun and on a paid-ads channel), not this rule's. Since #175 such a window is
`conversions_status: no_conversions` with no total on every channel (LinkedIn sets it;
`ResolvedConversionsStatus` reclassifies Google's no-rows zero and a zero-click Microsoft report),
so it cannot make this rule fire. The brief service passes the rule
`ReportedConversions()`, so only a `reported` total ever reaches it — an `incomplete` or
`no_conversions` status cannot fire it even if an adapter set a number beside it
(`TestGetBriefMetrics_ConversionsStatusGatesTheTotalAndTheRule`). That settles #175's second
point: the docs that called a zero-click window "the campaign the rule exists to catch" were
wrong, and #357's behaviour (no firing below the click floor) is kept.

The second floor is the one that matters more: the rule is **gated on the read having produced a
conversion measurement** (`Conversions != nil`), the same way `zero_delivery` is gated on the
channel billing per delivery. `Conversions` is a POINTER on both `model.CampaignMetrics` and
`rules.Input` precisely so absent stays distinguishable from a measured zero. Absent means "not
measured" — the platform cannot report it, or (LinkedIn, Microsoft) the read was incomplete:
LinkedIn withdraws the whole total when any returned element omits `externalWebsiteConversions`,
and Microsoft does so on a missing column or a blank `ConversionsQualified` cell. Neither case may
fire: a platform that cannot report has no measurement to be zero, and a partial read says nothing
about whether the campaign converted. Google Ads, LinkedIn and Microsoft populate it, and Meta
does for a campaign this service created with the conversions objective while
`META_CONVERSIONS_METRICS_ENABLED` is on (it reads the recorded event's action type, #550); X
exposes conversions solely as per-action-type structures with no scalar to read, Meta does
likewise for every other campaign,
Reddit's reporting read does not request a conversion field at all, and the email channel has no
conversion concept.
Flattening a nil to 0 anywhere along the path would flag **every** campaign on X, Reddit and
email, and every Meta campaign outside the #550 read, forever — reporting the absence of measurement as a campaign defect, which is the
failure mode the whole pointer exists to prevent.

`isActive` is an allow-list of the statuses where the campaign is meant to be spending, and it
gates **every** rule — the pacing ones as well as zero delivery.

`Campaign.Status` carries two kinds of value: a provisioning state stamped at create
(`pending` / `created` / `created_degraded`) and a **run state** set by the status toggle
(`active` / `paused`). Both live in the same column, and only some of them mean delivery is
expected. `paused` is a deliberate operator decision, so zero spend is the intended outcome and
an underspending item tells them to fix something they just chose; `pending` has not necessarily
reached the platform at all, so its spend figure is not evidence about pacing.

An earlier version listed only the provisioning states, which produced all three errors at once:
`paused` and `pending` raised underspending, while `active` — the one status where delivery is
unambiguously expected — raised nothing.

## HubSpot email monitor rules (`monitor_hubspot.go`, LFXV2-2665)

`EvaluateHubSpotMonitor(emails)` returns `[]model.AccountMonitorActionItem`, HIGH first via the
shared `sortByPriority`; there is no pacing (an email has no budget or spend) and so no row
output. `HubSpotRates(counters)` derives FRACTIONS — open, click and unsubscribe over delivered,
bounce over sent, spam reports over delivered — each nil on a zero denominator; call it on SUMMED counters for a total.
A finding carries `CampaignID` (the service campaign UUID) and `EmailID` (the HubSpot email id).

Thresholds are heuristics (named constants with their reasons beside them), not HubSpot limits:

| Rule | Priority | Gate |
| --- | --- | --- |
| sent > 0 and delivered = 0 | HIGH | none (text says a send may still be in progress) |
| bounce rate > 5% / > 2% | HIGH / MED | sent ≥ 100 |
| spam reports / delivered ≥ 0.3% and ≥ 5 reports / > 0.1% and ≥ 3 reports | HIGH / MED | delivered ≥ 100 |
| unsubscribe rate > 1% | MED | delivered ≥ 100 |
| open rate < 15% (opens are inflated by image pre-fetch, so this can only under-report) | MED | delivered ≥ 100 |
| click rate < 1% | LOW | delivered ≥ 100 |
