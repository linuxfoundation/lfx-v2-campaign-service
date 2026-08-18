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
100%: a campaign spending precisely what its flight expects by now is *on plan*, and labelling
it `constrained` would raise a budget item against the only campaign that needs none.

`Thresholds` therefore carries two boundaries, not three. The shared UI constants also define a
`normal` value (90), but it names the top of a band that is derived from `Constrained` — the
healthy band runs up to and including it. Carrying it as a field would offer a knob a caller
could turn with no effect.

## Currency

Pacing is meaningful **per campaign only**. This service performs no FX conversion and platform
cost is reported in each platform's native unit, so `Pct` must never be totalled or averaged
across platforms — the same reason the brief-metrics response refuses a cross-channel cost total.

## The four rules

| Rule | Fires when |
|---|---|
| `zero_delivery` | No impressions **and** no spend, on a campaign believed to exist upstream |
| `underspending` | Pacing is computable and below the floor |
| `budget_constrained` | Pacing is computable and above plan |
| `low_ctr` | CTR below the threshold, over enough impressions to mean anything |

Two guards carry most of the weight. **Zero delivery requires both signals** — impressions alone
is an unbilled serve, spend alone is a billing entry with no serve, and flagging either trains
operators to ignore the rule.

It is also **paid-ads only**, and it **suppresses the pacing item**. The email channel bills
nothing per send and its adapter always reports zero cost, while mapping opens onto impressions —
so a delivered email nobody opened is numerically identical to a campaign that never served, and
the rule would tell an operator to check targeting and approval for an email delivered exactly as
intended. And a campaign that never started is trivially at 0% of plan, so emitting both items
gives the operator two HIGH findings with opposite remedies: one says no budget change will fix
this, the other says to adjust the budget. **Low CTR needs a delivery floor**, because three impressions and
no clicks is a 0% CTR that says nothing about the creative.

`isActive` is an allow-list of the statuses where the service believes the campaign reached the
platform. A pending claim has not necessarily been created upstream, so a zero-delivery item
against it would report a dispatch problem as a targeting one.
