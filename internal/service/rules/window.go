// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import "time"

// WindowDays is how many days of spend a metrics window covers.
//
// It exists because ComputePacing needs the period its `spend` argument describes, and the
// service reads spend over a named window rather than over the flight. Getting this wrong is
// invisible: every value here is a plausible number of days, so a mistake produces a confident
// pacing figure about the wrong period rather than an error.
//
// The month-relative windows are computed rather than assumed, because their length depends on
// WHEN they are asked about. `this_month` is 1 day on the first of the month and 31 on the last
// day of July; treating it as a constant 30 would understate expected spend by a factor of 30
// early in the month and report a campaign as wildly overspending on day one.
//
// Returns 0 for a window it does not recognise. Callers must treat that as "no pacing" rather
// than substituting a default: a guessed period is exactly the failure this function exists to
// prevent, and ComputePacing rejects a non-positive spendDays for that reason.
func WindowDays(window string, now time.Time) float64 {
	start, end, ok := WindowInterval(window, now)
	if !ok {
		return 0
	}
	return end.Sub(start).Hours() / 24
}

// WindowInterval resolves a metrics window to the actual span of time it covers.
//
// A day COUNT cannot express a window's position, and position is what decides whether the spend
// figure and the flight describe the same period at all. `last_month` is 31 days, but those 31
// days sit entirely BEFORE a flight that started last week — so a spend of zero over that window
// is correct and means the campaign did not exist yet, not that it failed to spend. Paced against
// an expectation measured from the flight's start, it reported underspending against a campaign
// with nothing to answer for.
//
// The third return is false for a window this function does not recognise. Callers must treat
// that as "no pacing" rather than substituting a default.
func WindowInterval(window string, now time.Time) (start, end time.Time, ok bool) {
	day := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}
	switch window {
	case "today":
		return day(now), day(now).AddDate(0, 0, 1), true
	case "yesterday":
		return day(now).AddDate(0, 0, -1), day(now), true
	case "last_7_days":
		// N days ENDING today, inclusive: today-(N-1) .. today+1. Going back a full N from
		// today would span N+1 days.
		return day(now).AddDate(0, 0, -6), day(now).AddDate(0, 0, 1), true
	case "last_14_days":
		// N days ENDING today, inclusive: today-(N-1) .. today+1. Going back a full N from
		// today would span N+1 days.
		return day(now).AddDate(0, 0, -13), day(now).AddDate(0, 0, 1), true
	case "last_30_days":
		// N days ENDING today, inclusive: today-(N-1) .. today+1. Going back a full N from
		// today would span N+1 days.
		return day(now).AddDate(0, 0, -29), day(now).AddDate(0, 0, 1), true
	case "this_month":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return s, day(now).AddDate(0, 0, 1), true
	case "last_month":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
		return s, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// WindowDaysWithinFlight is how many days of a metrics window actually overlap a campaign's
// flight — the period the spend figure and the plan BOTH describe.
//
// This is what ComputePacing's spendDays argument should carry whenever the spend came from a
// named window. WindowDays alone gives the window's LENGTH, which says nothing about where it
// sits: `last_month` is 31 days, but for a campaign that started last week those 31 days lie
// almost entirely before the flight began. Pacing the resulting (correct) zero spend against an
// expectation measured from the flight's start reported underspending against a campaign that
// did not exist for most of the window.
//
// Returns 0 when the window and the flight do not overlap at all, which ComputePacing treats as
// incomputable — the honest answer, since the spend figure describes a period the campaign was
// not running in.
//
// An absent flight bound is open-ended on that side: a campaign with no end date is still
// running, so a window reaching up to now overlaps it.
func WindowDaysWithinFlight(window string, now time.Time, flightStart, flightEnd *time.Time) float64 {
	ws, we, ok := WindowInterval(window, now)
	if !ok {
		return 0
	}
	// Deliberately NOT clamped to `now`. A window's day count is what the platform reports
	// against — `last_7_days` returns seven days of spend whether it is asked at midnight or at
	// noon — so trimming the final partial day would compare seven days of spend against six and
	// a half days of plan and report every campaign as 8% ahead. The flight bounds below are the
	// real constraint, because they decide whether the campaign existed at all.
	if flightStart != nil && flightStart.After(ws) {
		ws = *flightStart
	}
	if flightEnd != nil && flightEnd.Before(we) {
		we = *flightEnd
	}
	if !we.After(ws) {
		return 0
	}
	return we.Sub(ws).Hours() / 24
}
