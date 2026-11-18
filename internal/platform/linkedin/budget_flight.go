// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"math"
	"strconv"
	"time"
)

// FIXED-SCHEDULE LIFETIME FLOOR (#633).
//
// A campaign with a totalBudget and a runSchedule that has an END is a lifetime budget on a
// fixed schedule. LinkedIn spreads that total evenly over the flight and holds the derived
// daily amount to its $10 daily minimum, so the total must be at least $10 × flight days. The
// create succeeds below that, but the campaign cannot be activated: prod campaign 930195766,
// $100 over 18 Nov–18 Dec, derived $3.23/day and was flagged "Daily Budget needs to be at
// least: $10.00". The flat $100 lifetime minimum alone does not catch it.
//
// HOW DAYS ARE COUNTED. Start day through end day, INCLUSIVE, in UTC. The campaign's own day
// boundaries ARE UTC: this client sends runSchedule.start as 00:00 UTC of the start day (or
// now+startTimeBuffer once that day has begun) and runSchedule.end as 23:59:59.999 UTC of the
// end day (toMs), so each UTC calendar day the schedule touches is one day of the flight. That
// reproduces LinkedIn's own figure for 930195766: 18 Nov through 18 Dec is 31 days, and
// $100 / 31 = $3.23. A start already nudged into today counts today as a whole day, which is
// the stricter reading and matches how Campaign Manager showed a same-day start.
//
// The check is against the ROUNDED wire value, like every other minimum in this package, and
// the minimum it names is max($100, $10 × days), so one sentence states the real floor for the
// given dates whichever rule binds.
//
// NOT APPLIED to a lifetime budget with a daily cap (#584): that campaign runs on a continuous
// schedule (no end on the campaign) and sends its own dailyBudget, which ValidateDailyCap holds
// to $10.

// flightDaysUTC returns the number of UTC calendar days from the day holding startMs through
// the day holding endMs, inclusive, or 0 when end precedes start.
func flightDaysUTC(startMs, endMs int64) int {
	start := utcDay(time.UnixMilli(startMs))
	end := utcDay(time.UnixMilli(endMs))
	if end.Before(start) {
		return 0
	}
	// Both are UTC midnights, which have no DST, so the division is exact.
	return int(end.Sub(start)/(24*time.Hour)) + 1
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// MinFixedScheduleLifetimeUSD is the smallest lifetime total LinkedIn will activate on a fixed
// schedule of the given number of days: max($100, $10 × days).
func MinFixedScheduleLifetimeUSD(days int) float64 {
	return math.Max(minLifetimeBudgetUSD, minDailyBudgetUSD*float64(days))
}

// ValidateFixedScheduleLifetime refuses a fixed-schedule lifetime total below LinkedIn's
// minimum for its flight (see the block comment above). startMs and endMs are the
// runSchedule instants the campaign carries, or will carry; startMs may be the nudged
// now+buffer start. A non-finite, non-positive or sub-cent total, or a schedule whose end
// precedes its start, is left to ValidateBudgetAmount and validateSchedule, which own those
// refusals and their sentences.
func ValidateFixedScheduleLifetime(totalUSD float64, startMs, endMs int64) error {
	if math.IsNaN(totalUSD) || math.IsInf(totalUSD, 0) || totalUSD <= 0 {
		return nil
	}
	days := flightDaysUTC(startMs, endMs)
	if days == 0 {
		return nil
	}
	wire := strconv.FormatFloat(totalUSD, 'f', budgetWireDecimals, 64)
	rounded, err := strconv.ParseFloat(wire, 64)
	if err != nil || wire == "0.00" {
		return nil
	}
	floor := MinFixedScheduleLifetimeUSD(days)
	if rounded >= floor {
		return nil
	}
	return invalidBudgetAmount("lifetime budget %s is below LinkedIn's minimum of $%.0f for a %d-day flight from %s through %s (UTC): LinkedIn spreads a fixed-schedule lifetime budget over every day of the flight and needs at least $%.0f a day, with a $%.0f lifetime minimum",
		wire, floor, days, utcDay(time.UnixMilli(startMs)).Format("2006-01-02"), utcDay(time.UnixMilli(endMs)).Format("2006-01-02"),
		minDailyBudgetUSD, minLifetimeBudgetUSD)
}

// ValidateFixedScheduleLifetimeDates is ValidateFixedScheduleLifetime for a create request's
// YYYY-MM-DD dates, for a caller (the dispatch preflight) that checks before any client exists.
// It derives the instants the way toMs will send them: the start day's 00:00 UTC, or now once
// that has passed, and 23:59:59.999 UTC of the end day. Dates that do not parse, or an end
// already past, are left to validateSchedule.
func ValidateFixedScheduleLifetimeDates(totalUSD float64, startDate, endDate string, now time.Time) error {
	if !dateRE.MatchString(startDate) || !dateRE.MatchString(endDate) {
		return nil
	}
	start, err := time.ParseInLocation("2006-01-02", startDate, time.UTC)
	if err != nil {
		return nil
	}
	end, err := time.ParseInLocation("2006-01-02", endDate, time.UTC)
	if err != nil {
		return nil
	}
	nowMs := now.UTC().UnixMilli()
	startMs := start.UnixMilli()
	if startMs <= nowMs {
		startMs = nowMs
	}
	endMs := end.Add(24*time.Hour - time.Millisecond).UnixMilli()
	if endMs <= startMs {
		return nil
	}
	return ValidateFixedScheduleLifetime(totalUSD, startMs, endMs)
}
