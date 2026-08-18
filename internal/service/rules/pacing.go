// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package rules turns a campaign's measured performance into the action items an operator
// should act on.
//
// It exists because the same five concepts were implemented four times in the UI's BFF —
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
type Thresholds struct {
	// Underspending is the floor: below this share of expected spend, the campaign is not
	// delivering the budget it was given.
	Underspending float64
	// Normal is the top of the healthy band.
	Normal float64
	// Constrained is the point above which the campaign is outrunning its plan.
	Constrained float64
}

// DefaultThresholds is the shared set. Per-platform overrides go through PlatformThresholds.
var DefaultThresholds = Thresholds{Underspending: 50, Normal: 90, Constrained: 100}

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
	if budget <= 0 || math.IsNaN(spend) || math.IsInf(spend, 0) {
		return Pacing{Label: PacingUnknown}
	}
	// spendDays is how many days of spend the `spend` figure actually covers. It exists
	// because the only spend this service can read is WINDOW-scoped (last_7_days and
	// friends), while a lifetime budget describes the whole flight. Without it the two
	// arguments silently describe different periods.
	if spendDays <= 0 || math.IsNaN(spendDays) || math.IsInf(spendDays, 0) {
		return Pacing{Label: PacingUnknown}
	}

	// An absent start is not an error: a campaign created without one begins when it begins.
	// Without an end, the flight is open-ended and "expected by now" is measured to now.
	start := now
	if flight.Start != nil {
		start = *flight.Start
	}
	end := now
	if flight.End != nil {
		end = *flight.End
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
	elapsed := daysBetween(start, minTime(now, end))
	measured := math.Min(spendDays, elapsed)
	if measured <= 0 {
		// The window covers no part of the flight — a campaign that has not started, or one
		// whose window predates it. There is nothing to compare, and 0% would read as a
		// campaign that failed to spend.
		return Pacing{Label: PacingUnknown}
	}
	var expected float64
	switch kind {
	case BudgetDaily:
		expected = budget * measured
	default:
		total := daysBetween(start, end)
		if total <= 0 {
			return Pacing{Label: PacingUnknown}
		}
		expected = (budget / total) * measured
	}
	if expected <= 0 {
		return Pacing{Label: PacingUnknown}
	}

	pct := (spend / expected) * 100
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
	case pct <= t.Constrained*overspendFactor:
		return PacingConstrained
	default:
		return PacingOverspending
	}
}

// overspendFactor is how far past plan a campaign runs before it is overspending rather than
// merely constrained. 1.3 reproduces the shared constants' 130 against a 100 cap, keeping the
// four bands the UI already renders.
const overspendFactor = 1.3

// daysBetween counts partial days as whole ones, matching the UI's Math.ceil, and never returns
// less than one: a campaign in its first hours has one day of expectation, not zero, and a zero
// would make every early campaign's expected spend zero and its pacing incomputable.
func daysBetween(from, to time.Time) float64 {
	d := math.Ceil(float64(to.Sub(from)/time.Millisecond) / millisPerDay)
	return math.Max(1, d)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
