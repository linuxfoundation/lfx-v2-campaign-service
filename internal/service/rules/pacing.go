// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package rules turns a campaign's measured performance into the action items an operator
// should act on.
//
// It exists because the same concepts were implemented four times in the UI's BFF —
// linkedin-ads, meta-ads, reddit-ads and campaign-metrics — with thresholds that drifted apart
// accidentally rather than by design. At the time this package was written those four disagreed
// on the underspending floor three ways (40, 50, and a shared constant of 50 that only one of
// them read), and one file used 50 for its label and 40 for the alert on that same label, so a
// campaign at 45% was called underspending while no action item said so.
//
// The thresholds here are therefore ONE set with per-platform overrides only where a reason is
// stated. An override with no stated reason is drift, not configuration.
package rules

import (
	"math"
	"time"
)

// Thresholds are the pacing boundaries, as a percentage of expected spend.
//
// Values match `CAMPAIGN_PACING_THRESHOLDS` in lfx-self-serve's shared constants, which is the
// set Meta already used and the one the other three should have. Deliberately NOT LinkedIn's
// hardcoded 40/90/105: those were never written down anywhere shared, and treating the outlier
// as the standard would silently move every other platform's alerting.
// Two boundaries, not three. The shared constants carry a `normal` value (90) as well, but it
// names the top of a band that labelFor derives from Constrained — the healthy band runs up TO
// and including Constrained, so a third field would be a knob a caller could turn with no
// effect, which is worse than not offering it.
type Thresholds struct {
	// Underspending is the floor: below this share of expected spend, the campaign is not
	// delivering the budget it was given.
	Underspending float64
	// Constrained is the top of the healthy band, inclusive: a campaign exactly on plan sits
	// here. Above it the campaign is outrunning its plan.
	Constrained float64
	// Overspending is the point above which overspend stops being a warning. ABSOLUTE, matching
	// the shared constants' own `overspending: 130` — deriving it as a multiple of Constrained
	// silently moves it whenever Constrained is overridden, which is the one thing a
	// per-platform override must not do to a boundary nobody asked to change.
	Overspending float64
}

// DefaultThresholds is the shared set.
//
// One set for every platform. Per-platform overrides are not implemented: the four UI
// implementations differed with no stated reason, which is drift rather than a platform
// characteristic. If a platform genuinely needs different bands, add the override here WITH the
// reason — do not reintroduce a silent divergence.
var DefaultThresholds = Thresholds{Underspending: 50, Constrained: 100, Overspending: 130}

// PacingLabel is the band a campaign's spend falls into.
type PacingLabel string

const (
	// PacingUnknown is the label when no budget is recorded, so there is nothing to pace
	// against. Distinct from Normal: "we cannot say" is not "on track", and rendering a
	// budget-less campaign as healthy is the failure this label exists to prevent.
	PacingUnknown       PacingLabel = "unknown"
	PacingUnderspending PacingLabel = "underspending"
	PacingNormal        PacingLabel = "normal"
	PacingConstrained   PacingLabel = "constrained"
	PacingOverspending  PacingLabel = "overspending"
)

// Flight is the window a campaign is scheduled to run over.
type Flight struct {
	Start *time.Time
	End   *time.Time
}

// Pacing is the result of comparing spend against a flight-prorated expectation.
type Pacing struct {
	// Pct is spend as a percentage of what the campaign should have spent BY NOW, not of the
	// whole budget. A campaign three days into a thirty-day flight is expected to have spent
	// a tenth, so comparing against the full budget would report every healthy campaign as
	// severely underspending for most of its life.
	Pct float64
	// Label is the band Pct falls into, or PacingUnknown when there is no budget to pace
	// against.
	Label PacingLabel
	// Computable records whether a pacing figure could be derived at all. When false, Pct is
	// zero because nothing was measured — not because the campaign spent nothing.
	Computable bool
}

// BudgetKind distinguishes a lifetime cap from a per-day allowance. The proration differs: a
// lifetime budget is spread across the whole flight, while a daily budget is multiplied by the
// days elapsed.
type BudgetKind string

const (
	BudgetLifetime BudgetKind = "lifetime"
	BudgetDaily    BudgetKind = "daily"
)

const millisPerDay = float64(24 * time.Hour / time.Millisecond)

// minElapsedDaysForPacing is how much of a flight must have run before spend-against-plan says
// anything. One full day: below that the expected figure is a rounding artefact, and platform
// reporting lag means the measured spend is not trustworthy either.
const minElapsedDaysForPacing = 1.0

// ComputePacing derives spend-against-plan for one campaign.
//
// `now` is injected rather than read from the clock so the arithmetic is testable at a fixed
// instant; date-bearing logic whose tests depend on the wall clock passes or fails by when it
// is run.
//
// Returns Computable=false when there is no budget, no usable flight, or spend cannot be
// compared — the caller must not render an incomputable result as 0%, which reads as a campaign
// that spent nothing.
//
// SPEND AND BUDGET MUST COVER THE SAME PERIOD. This is the caller's obligation and the one
// mistake this function cannot detect: both arguments are bare float64, so a 7-day spend
// divided by a 30-day plan type-checks perfectly and yields a confident "23% of plan" for a
// campaign that is exactly on track. `spendDays` is therefore required — see below.
//
// It is also the caller's job to keep spend and budget in ONE currency. This service performs
// no FX conversion and platform cost is reported in each platform's native unit, so a pacing
// figure is only meaningful per campaign; never total or average Pct across platforms.
func ComputePacing(spend float64, spendDays float64, budget float64, kind BudgetKind, flight Flight, now time.Time, t Thresholds) Pacing {
	// budget gets the SAME finiteness test as spend, not just `<= 0`. NaN and +Inf both fail a
	// `<= 0` comparison, so an unreadable budget would sail through: NaN renders as the literal
	// "NaN%" in an operator-facing issue, and +Inf is worse because it is silent — expected
	// spend becomes infinite, Pct becomes 0, and the campaign raises a HIGH-priority
	// underspending item reading "0% of expected spend" that is indistinguishable from a real
	// one. Computable exists to stop exactly this.
	if budget <= 0 || math.IsNaN(budget) || math.IsInf(budget, 0) ||
		math.IsNaN(spend) || math.IsInf(spend, 0) {
		return Pacing{Label: PacingUnknown}
	}
	// spendDays is how many days of spend the `spend` figure actually covers. It exists
	// because the only spend this service can read is WINDOW-scoped (last_7_days and
	// friends), while a lifetime budget describes the whole flight. Without it the two
	// arguments silently describe different periods.
	if spendDays <= 0 || math.IsNaN(spendDays) || math.IsInf(spendDays, 0) {
		return Pacing{Label: PacingUnknown}
	}

	// An absent start with a PRESENT end has no flight to prorate across. Defaulting start to
	// now makes the flight begin this instant, and daysBetween then floors elapsed at one day —
	// so a 30-day window of spend is compared against a single day of plan and a campaign
	// exactly on plan reports 500% overspending. start_date is nullable in the schema
	// (migration 000002), so this is a storable state, not a hypothesis.
	//
	// This is the same defect as the future-dated flight below, arriving through the other
	// door: the now.After(start) guard cannot catch it, because start was just set TO now.
	if flight.Start == nil && flight.End != nil {
		return Pacing{Label: PacingUnknown}
	}
	// The mirror case, and it is NOT symmetric — it applies to lifetime budgets only.
	//
	// A lifetime budget is a total to spread across a flight, so prorating it needs the flight's
	// LENGTH. With no end date there is no length: `end` defaults to now, `total` collapses to
	// `elapsed`, and the whole budget is treated as due today. A campaign ten days into an
	// open-ended $1000 budget having spent $100 then reads 10% and raises a HIGH-priority
	// underspending item, telling an operator to raise spend on a campaign that may be pacing
	// perfectly for a flight that has not been given an end.
	//
	// A DAILY budget is unaffected and stays computable: its rate is explicit, so days elapsed
	// is all the arithmetic needs and the missing end date costs nothing.
	//
	// end_date is nullable in the schema (migration 000002), so this is storable, not theoretical.
	if kind == BudgetLifetime && flight.End == nil {
		return Pacing{Label: PacingUnknown}
	}
	// An absent start is not an error: a campaign created without one begins when it begins.
	// Without an end, the flight is open-ended and "expected by now" is measured to now — and
	// with BOTH absent the zero-length check below returns Unknown anyway.
	start := now
	if flight.Start != nil {
		start = *flight.Start
	}
	// The flight's end DATE means "through the end of that day", so it is normalised to the
	// FOLLOWING midnight before being used as an exclusive bound — see flightEndInstant. Using
	// the raw midnight cut the final day off every flight: `total` lost a day, and `elapsed` was
	// clamped to a moment already past.
	end := now
	if e := flightEndInstant(flight.End); e != nil {
		end = *e
	}
	// A flight that has not begun has no plan-to-date to compare against. Without this the
	// elapsed floor of one day invents a day of expected spend, and a campaign scheduled to
	// start next week raises a HIGH-priority underspending item for not having spent yet.
	// !After, not Before: a campaign starting exactly NOW has elapsed zero days, which
	// daysBetween floors to one, so it would be measured against a full day of plan it has had
	// no time to spend. A strict Before lets that boundary case through and reports a campaign
	// that started this second as either overspending or underspending.
	if !now.After(start) {
		return Pacing{Label: PacingUnknown}
	}
	if !end.After(start) {
		// A zero or inverted flight has no days to prorate across. Reporting 0% here would
		// claim the campaign underspent; it means the schedule is unusable.
		return Pacing{Label: PacingUnknown}
	}

	// Expected spend is computed over the SAME number of days the spend figure covers, not
	// over the whole elapsed flight. A campaign 20 days into a flight whose spend was read
	// over a 7-day window must be compared against 7 days of plan; comparing it against 20
	// days of plan reports a healthy campaign as spending a third of what it should.
	// elapsedDays, not daysBetween: rounding the elapsed period up to a whole day invents plan
	// the campaign has had no time to spend. `total` below still uses daysBetween, where the
	// floor is protecting a divisor rather than inflating a numerator.
	elapsed := elapsedDays(start, minTime(now, end))

	// Below a day, pacing is arithmetically computable and operationally meaningless. A campaign
	// launched a minute into a 30-day $1000 flight is expected to have spent two CENTS, so a
	// spend of zero is 0% — a HIGH-priority underspending item against a campaign whose only
	// property is being new.
	//
	// It would be wrong even with perfect data, and the data is not perfect: ad platforms report
	// spend with a lag, so the first hours of any campaign read as zero regardless of what it is
	// actually doing. Every campaign would raise this item every time it launched, which is how
	// an alert becomes noise operators learn to skip.
	//
	// Unknown rather than a forced `normal`: nothing has been measured yet, and saying "on plan"
	// would be the same substitution in the other direction.
	if elapsed < minElapsedDaysForPacing {
		return Pacing{Label: PacingUnknown}
	}
	// Always positive, so it needs no guard: daysBetween floors at 1 and spendDays > 0 is
	// enforced above. The campaign-has-not-started case that would otherwise land here is
	// caught earlier by the !now.After(start) check, which returns Unknown rather than letting
	// the one-day floor invent a day of expected spend.
	measured := math.Min(spendDays, elapsed)
	var expected float64
	switch kind {
	case BudgetDaily:
		expected = budget * measured
	default:
		// Also always positive — same daysBetween floor — and end.After(start) is established
		// above, so this cannot divide by zero.
		total := daysBetween(start, end)
		expected = (budget / total) * measured
	}
	// No `expected <= 0` guard: a positive budget times a positive measured period cannot reach
	// zero by any path a `<=` test would catch. It CAN underflow to a denormal that stays
	// strictly positive, which such a guard would miss anyway — the non-finite check on the
	// result below is what actually covers that.

	pct := (spend / expected) * 100
	// Backstop. The input guards above cover unreadable arguments, but arithmetic can still
	// produce a non-finite result from finite ones: a denormal `expected` (a tiny budget spread
	// across a long flight) underflows toward zero while staying strictly positive, so the
	// `expected <= 0` test passes and the division overflows to +Inf. A non-finite percentage is
	// not a measurement whatever produced it.
	if math.IsNaN(pct) || math.IsInf(pct, 0) {
		return Pacing{Label: PacingUnknown}
	}
	return Pacing{Pct: pct, Label: labelFor(pct, t), Computable: true}
}

// labelFor maps a percentage onto its band.
//
// Boundaries are half-open UPWARD: a value sitting exactly on a threshold lands in the healthier
// band. That matches the shared constants' own comment ("pacingPct < 50 → underspending") and,
// more importantly, makes exactly-on-plan mean on plan — 100% is a campaign spending precisely
// what the flight expects by now, and labelling that `constrained` would raise a budget item
// against the only campaign that needs none.
//
// Normal therefore extends THROUGH Constrained: the constrained band starts above it. An earlier
// version used `pct <= t.Constrained` for constrained, which put exactly-100% there.
func labelFor(pct float64, t Thresholds) PacingLabel {
	switch {
	case pct < t.Underspending:
		return PacingUnderspending
	case pct <= t.Constrained:
		return PacingNormal
	case pct <= t.Overspending:
		return PacingConstrained
	default:
		return PacingOverspending
	}
}

// daysBetween counts partial days as whole ones, matching the UI's Math.ceil.
//
// The one-day floor applies to the flight's TOTAL length, where it is a divisor and a zero would
// make expected spend infinite. It must NOT be applied to elapsed time — see elapsedDays.
func daysBetween(from, to time.Time) float64 {
	d := math.Ceil(float64(to.Sub(from)/time.Millisecond) / millisPerDay)
	return math.Max(1, d)
}

// elapsedDays is how much of the flight has actually passed, as a FRACTION of a day where less
// than one has elapsed.
//
// It exists because rounding elapsed time up to a whole day manufactures expected spend that the
// campaign has had no time to spend. A campaign launched one minute ago was measured against a
// full day of plan, reported 0% and raised a HIGH-priority underspending item against itself —
// the same defect as the future-dated flight and the nil start date, arriving through a third
// door. The !now.After(start) guard pins only the instant of launch; this covers the 24 hours
// after it.
//
// Not floored at one, deliberately: the caller establishes now > start before calling, so the
// result is always positive, and expected spend scales smoothly from the first minute.
func elapsedDays(from, to time.Time) float64 {
	return float64(to.Sub(from)/time.Millisecond) / millisPerDay
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// flightEndInstant converts a flight's end DATE into the instant the flight actually stops.
//
// `end_date` is a DATE column (migration 000002) and means "through the END of that day", so the
// midnight time.Time it decodes to is the START of the final day, not the finish of the flight.
// Every platform adapter already reads it that way — Meta sends `EndDate + "T23:59:59+0000"`
// (internal/platform/meta/client.go), X adds a day and asks for the following midnight
// (internal/platform/twitter/metrics.go), LinkedIn uses end-of-day. This package was the outlier:
// it used the raw midnight as an EXCLUSIVE bound, so the final day of every flight was treated as
// having already finished.
//
// The damage was not a rounding error. A two-day flight (Aug 17 -> Aug 18) measured one day, so a
// $100/day campaign that had correctly spent $200 read as 200% and labelled `overspending`; and
// WindowDaysWithinFlight clamped `today` to a midnight already past, returning 0 overlap and
// making pacing `unknown` for the whole of every flight's last day — the day an operator is most
// likely to be looking.
//
// The following midnight, matching X's AddDate(0, 0, 1): an exclusive bound at the next midnight
// covers the final day exactly, and unlike 23:59:59 it loses no part of the last second.
//
// nil in, nil out. An absent end is open-ended, not a flight ending at the epoch, and both
// callers must keep treating it as "still running".
//
// One function for both call sites deliberately. ComputePacing and WindowDaysWithinFlight
// consume the same column for the same purpose, and this defect existed because they each
// reasoned about it separately; two copies could drift apart again the same way.
func flightEndInstant(end *time.Time) *time.Time {
	if end == nil {
		return nil
	}
	e := end.AddDate(0, 0, 1)
	return &e
}
