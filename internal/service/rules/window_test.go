// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"testing"
	"time"
)

func TestWindowDays_FixedWindows(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	cases := map[string]float64{
		"today":        1,
		"yesterday":    1,
		"last_7_days":  7,
		"last_14_days": 14,
		"last_30_days": 30,
	}
	for window, want := range cases {
		if got := WindowDays(window, now); got != want {
			t.Errorf("WindowDays(%q) = %v, want %v", window, got, want)
		}
	}
}

// The month windows depend on WHEN they are asked about. A constant 30 would understate
// expected spend thirtyfold on the first of the month and report a healthy campaign as wildly
// overspending.
func TestWindowDays_MonthWindowsDependOnTheDate(t *testing.T) {
	cases := []struct {
		name      string
		now       time.Time
		thisMonth float64
		lastMonth float64
	}{
		// August has 31 days, July has 31, February 2026 has 28.
		{"first of the month", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), 1, 31},
		{"mid month", time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), 18, 31},
		{"last day of a 31-day month", time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC), 31, 31},
		{"after a 28-day february", time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), 10, 28},
		{"after a 30-day month", time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC), 5, 30},
		// January's previous month is December of the prior year.
		{"january crosses the year", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), 15, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WindowDays("this_month", tc.now); got != tc.thisMonth {
				t.Errorf("this_month = %v, want %v", got, tc.thisMonth)
			}
			if got := WindowDays("last_month", tc.now); got != tc.lastMonth {
				t.Errorf("last_month = %v, want %v", got, tc.lastMonth)
			}
		})
	}
}

// An unrecognised window must not resolve to a plausible default. A guessed period produces a
// confident pacing figure about the wrong span, which is worse than no pacing at all.
func TestWindowDays_UnknownWindowIsNotGuessed(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for _, w := range []string{"", "last_90_days", "LAST_7_DAYS", "all_time"} {
		if got := WindowDays(w, now); got != 0 {
			t.Errorf("WindowDays(%q) = %v, want 0 — an unknown window has no known length", w, got)
		}
	}
	// And 0 must make pacing incomputable rather than dividing by it.
	flight := Flight{Start: day(-10), End: day(10)}
	if got := ComputePacing(500, WindowDays("all_time", now), 1000, BudgetLifetime, flight, testNow, DefaultThresholds); got.Computable {
		t.Errorf("an unknown window produced a computable %.1f%%", got.Pct)
	}
}

// A window's day COUNT says nothing about where it sits. `last_month` is 31 days, but for a
// campaign that started last week those days lie almost entirely before the flight began — so a
// spend of zero over that window is correct and means the campaign did not exist yet, not that
// it failed to spend.
func TestWindowDaysWithinFlight_WindowBeforeTheFlight(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	// A flight that began on the 13th. `last_month` is all of July — entirely before it.
	start := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if got := WindowDaysWithinFlight("last_month", now, &start, &end); got != 0 {
		t.Errorf("last_month overlapped a flight starting in August by %v days; July precedes it entirely", got)
	}
	// The bare length is still 31 — this is exactly the number that made the old behaviour look
	// reasonable.
	if got := WindowDays("last_month", now); got != 31 {
		t.Errorf("WindowDays(last_month) = %v, want 31", got)
	}

	// And a zero overlap must make pacing incomputable rather than reporting 0%.
	got := ComputePacing(0, WindowDaysWithinFlight("last_month", now, &start, &end), 1000, BudgetLifetime,
		Flight{Start: &start, End: &end}, now, DefaultThresholds)
	if got.Computable {
		t.Errorf("a window preceding the flight produced a computable %.1f%% labelled %q", got.Pct, got.Label)
	}
}

// A window that only PARTLY overlaps the flight contributes only the overlapping days.
func TestWindowDaysWithinFlight_PartialOverlap(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	// last_7_days spans Aug 12..19. A flight starting Aug 15 overlaps 4 of those days.
	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if got := WindowDaysWithinFlight("last_7_days", now, &start, &end); got != 4 {
		t.Errorf("overlap = %v days, want 4 (Aug 15..19 of a window spanning Aug 12..19)", got)
	}
}

// An absent flight bound is open-ended on that side — a campaign with no end date is still
// running, so a window reaching up to now overlaps it fully.
func TestWindowDaysWithinFlight_OpenEndedFlight(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := WindowDaysWithinFlight("last_7_days", now, &start, nil); got != 7 {
		t.Errorf("overlap with an open-ended flight = %v, want the full 7", got)
	}
	// No start either: nothing constrains the window.
	if got := WindowDaysWithinFlight("last_7_days", now, nil, nil); got != 7 {
		t.Errorf("overlap with an unbounded flight = %v, want the full 7", got)
	}
}

// An unrecognised window has no interval, so it has no overlap either.
func TestWindowDaysWithinFlight_UnknownWindow(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := WindowDaysWithinFlight("all_time", now, &start, nil); got != 0 {
		t.Errorf("unknown window returned %v days of overlap", got)
	}
}

// A flight that ENDED inside the window contributes only the days up to its end. Without the
// end clamp a finished campaign keeps accruing plan for days it was no longer running.
func TestWindowDaysWithinFlight_FlightEndsMidWindow(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	// last_7_days spans Aug 12..19; a flight ending Aug 15 covers 3 of those days.
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if got := WindowDaysWithinFlight("last_7_days", now, &start, &end); got != 3 {
		t.Errorf("overlap = %v days, want 3 (Aug 12..15 of a window spanning Aug 12..19)", got)
	}
}
