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
	switch window {
	case "today", "yesterday":
		return 1
	case "last_7_days":
		return 7
	case "last_14_days":
		return 14
	case "last_30_days":
		return 30
	case "this_month":
		// Days elapsed INCLUDING today, matching how the platforms report a month-to-date
		// figure: on the 1st this is 1, not 0.
		return float64(now.Day())
	case "last_month":
		// The length of the previous calendar month, which is 28, 29, 30 or 31. Day 0 of this
		// month is the last day of the previous one.
		return float64(time.Date(now.Year(), now.Month(), 0, 0, 0, 0, 0, now.Location()).Day())
	default:
		return 0
	}
}
