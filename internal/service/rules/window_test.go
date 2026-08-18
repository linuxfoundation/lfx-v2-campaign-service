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
