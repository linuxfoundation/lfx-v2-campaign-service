// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"math"
	"testing"
	"time"
)

// A fixed instant. Date arithmetic tested against the wall clock passes or fails by WHEN it is
// run, so every case here pins now explicitly.
var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func day(offset int) *time.Time {
	t := testNow.AddDate(0, 0, offset)
	return &t
}

// Pacing is spend against what should have been spent BY NOW, not against the whole budget.
//
// This is the property the whole rule set rests on: comparing to the full budget reports every
// healthy campaign as severely underspending for most of its flight, which is the failure mode
// the UI's own implementations were careful to avoid and which a naive port would reintroduce.
func TestComputePacing_ProratesAcrossTheFlight(t *testing.T) {
	// Day 10 of a 20-day flight with $1000 lifetime: expected-by-now is $500.
	flight := Flight{Start: day(-10), End: day(10)}

	onPlan := ComputePacing(500, 10, 1000, BudgetLifetime, flight, testNow, DefaultThresholds)
	if !onPlan.Computable {
		t.Fatal("pacing should be computable with a budget and a usable flight")
	}
	if math.Abs(onPlan.Pct-100) > 1 {
		t.Errorf("halfway through the flight having spent half the budget = %.1f%%, want ~100%%", onPlan.Pct)
	}
	if onPlan.Label != PacingNormal {
		t.Errorf("label = %q, want normal", onPlan.Label)
	}

	// The same $500 against the FULL budget would read as 50% — the naive comparison.
	if math.Abs(onPlan.Pct-50) < 1 {
		t.Error("pacing was computed against the whole budget rather than the elapsed share")
	}
}

func TestComputePacing_Labels(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(10)} // expected-by-now = half the budget
	cases := map[string]struct {
		spend float64
		want  PacingLabel
	}{
		"far below plan":        {100, PacingUnderspending}, // 20%
		"just below the floor":  {245, PacingUnderspending}, // 49%
		"exactly on the floor":  {250, PacingNormal},        // 50% — boundary is half-open UP
		"healthy":               {400, PacingNormal},        // 80%
		"top of normal":         {450, PacingNormal},        // 90%
		"exactly on plan":       {500, PacingNormal},        // 100% — on plan is NOT constrained
		"ahead of plan":         {600, PacingConstrained},   // 120%
		"at the overspend edge": {650, PacingConstrained},   // 130%
		"overspending":          {700, PacingOverspending},  // 140%
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ComputePacing(tc.spend, 10, 1000, BudgetLifetime, flight, testNow, DefaultThresholds)
			if got.Label != tc.want {
				t.Errorf("spend %.0f -> %.0f%% labelled %q, want %q", tc.spend, got.Pct, got.Label, tc.want)
			}
		})
	}
}

// A daily budget is multiplied by days elapsed, not divided across the flight. Getting this
// backwards makes a daily-budget campaign look wildly over or under depending on flight length.
func TestComputePacing_DailyBudgetMultipliesElapsedDays(t *testing.T) {
	flight := Flight{Start: day(-4), End: day(26)} // 4 days elapsed of a 30-day flight
	// spendDays is 4, matching the elapsed days, so this exercises the spendDays == elapsed
	// case directly rather than relying on the min() cap to rescue a larger value. The
	// spendDays < elapsed case has its own test below.
	//
	// $100/day for 4 days = $400 expected. Spending exactly that is on plan.
	got := ComputePacing(400, 4, 100, BudgetDaily, flight, testNow, DefaultThresholds)
	if !got.Computable || math.Abs(got.Pct-100) > 1 {
		t.Errorf("daily pacing = %.1f%% (computable=%v), want ~100%%", got.Pct, got.Computable)
	}
	// The same numbers read as a LIFETIME budget: $100 spread over 30 days is $3.33/day, so 4
	// days expects $13.33 and $400 spent is ~3000%. Asserted as a VALUE, not as "far from the
	// daily figure" — a loose separation check still passes under substantial corruption of
	// either arm.
	asLifetime := ComputePacing(400, 4, 100, BudgetLifetime, flight, testNow, DefaultThresholds)
	if math.Abs(asLifetime.Pct-3000) > 10 {
		t.Errorf("lifetime pacing = %.1f%%, want ~3000%% ($100 over 30 days = $13.33 expected by day 4)", asLifetime.Pct)
	}
}

// Incomputable is NOT zero. A campaign with no budget has no pacing, and rendering that as 0%
// would report it as severely underspending — a finding about spend derived from the absence of
// a budget.
func TestComputePacing_IncomputableStatesAreNotZeroPercent(t *testing.T) {
	cases := map[string]struct {
		budget float64
		flight Flight
	}{
		"no budget recorded":           {0, Flight{Start: day(-10), End: day(10)}},
		"negative budget":              {-100, Flight{Start: day(-10), End: day(10)}},
		"flight ends before it starts": {1000, Flight{Start: day(10), End: day(-10)}},
		"zero-length flight":           {1000, Flight{Start: day(0), End: day(0)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ComputePacing(250, 10, tc.budget, BudgetLifetime, tc.flight, testNow, DefaultThresholds)
			if got.Computable {
				t.Errorf("pacing reported computable at %.1f%%; there is nothing to pace against", got.Pct)
			}
			if got.Label != PacingUnknown {
				t.Errorf("label = %q, want unknown — 'we cannot say' is not 'on track'", got.Label)
			}
		})
	}
}

// A campaign in its first hours has one day of expectation, not zero. A zero would make expected
// spend zero and pacing incomputable for every campaign on its launch day.
func TestComputePacing_PartialFirstDayCountsAsOne(t *testing.T) {
	start := testNow.Add(-2 * time.Hour)
	end := testNow.AddDate(0, 0, 30)
	got := ComputePacing(10, 10, 3000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("a campaign hours into its flight must still have computable pacing")
	}
}

func TestComputePacing_RejectsNonFiniteSpend(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(10)}
	for name, spend := range map[string]float64{"NaN": math.NaN(), "Inf": math.Inf(1)} {
		t.Run(name, func(t *testing.T) {
			if got := ComputePacing(spend, 10, 1000, BudgetLifetime, flight, testNow, DefaultThresholds); got.Computable {
				t.Errorf("%s spend produced a computable pacing of %.1f%%", name, got.Pct)
			}
		})
	}
}

// The defect this parameter exists to prevent: the only spend this service can read is
// window-scoped, and a lifetime budget describes the whole flight. Comparing a 7-day spend
// against 20 days of plan reports an on-track campaign as spending a third of what it should —
// a confident number that is simply about a different period.
func TestComputePacing_ComparesLikeForLikePeriods(t *testing.T) {
	// Day 20 of a 40-day, $4000 flight: $100/day of plan. The campaign is exactly on plan,
	// and we read 7 days of its spend: $700.
	flight := Flight{Start: day(-20), End: day(20)}

	got := ComputePacing(700, 7, 4000, BudgetLifetime, flight, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("a 7-day spend against a lifetime budget is computable — it prorates to 7 days of plan")
	}
	if math.Abs(got.Pct-100) > 1 {
		t.Errorf("on-plan campaign read over 7 days = %.1f%%, want ~100%%", got.Pct)
	}
	if got.Label != PacingNormal {
		t.Errorf("label = %q, want normal — this campaign is exactly on plan", got.Label)
	}

	// Had expected-spend been computed over the full 20 elapsed days ($2000), the same $700
	// would read as 35% and raise an underspending item against a healthy campaign.
	if got.Label == PacingUnderspending {
		t.Error("expected spend was computed over the elapsed flight rather than the measured window")
	}
}

// A window wider than the flight cannot manufacture plan days that have not elapsed.
func TestComputePacing_MeasuredPeriodIsCappedByElapsedFlight(t *testing.T) {
	// Day 3 of a flight, but a 30-day window. Only 3 days of plan exist.
	flight := Flight{Start: day(-3), End: day(27)}
	got := ComputePacing(300, 30, 1000, BudgetDaily, flight, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("want computable")
	}
	// 3 days elapsed x $1000/day = $3000 expected; $300 spent = 10%.
	if math.Abs(got.Pct-10) > 1 {
		t.Errorf("pct = %.1f%%, want ~10%% (expected spend capped at the 3 elapsed days)", got.Pct)
	}
}

func TestComputePacing_UnusableMeasuredPeriod(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(10)}
	for name, days := range map[string]float64{
		"zero":     0,
		"negative": -7,
		"nan":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ComputePacing(500, days, 1000, BudgetLifetime, flight, testNow, DefaultThresholds); got.Computable {
				t.Errorf("spendDays=%v produced a computable %.1f%% — it describes no period", days, got.Pct)
			}
		})
	}
}

// The daily arm needs its own like-for-like case. The capped-period test above has
// spendDays > elapsed, so measured and elapsed are equal there and it cannot tell the two
// apart — a daily arm computing over the elapsed flight passes it.
func TestComputePacing_DailyBudgetUsesTheMeasuredWindow(t *testing.T) {
	// Day 20 of a flight at $100/day. A campaign exactly on plan, read over 7 days: $700.
	flight := Flight{Start: day(-20), End: day(20)}

	got := ComputePacing(700, 7, 100, BudgetDaily, flight, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("want computable")
	}
	// 7 measured days x $100 = $700 expected, so $700 spent is 100%. Computed over the 20
	// elapsed days it would be $2000 expected and read as 35% — underspending.
	if math.Abs(got.Pct-100) > 1 {
		t.Errorf("pct = %.1f%%, want ~100%% (expected spend must cover the 7 measured days, not 20 elapsed)", got.Pct)
	}
	if got.Label != PacingNormal {
		t.Errorf("label = %q, want normal", got.Label)
	}
}

// An unreadable budget must not become a measurement. NaN and +Inf both fail a `<= 0` test, so
// without an explicit finiteness check they reach the arithmetic: NaN renders as "NaN%" in an
// operator-facing issue, and +Inf is worse because it is SILENT — expected spend goes infinite,
// Pct lands on 0, and the campaign raises a HIGH-priority underspending item that looks exactly
// like a real one.
func TestComputePacing_NonFiniteBudgetIsNotAMeasurement(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(10)}
	for name, budget := range map[string]float64{
		"nan":       math.NaN(),
		"+infinite": math.Inf(1),
		"-infinite": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []BudgetKind{BudgetLifetime, BudgetDaily} {
				got := ComputePacing(500, 10, budget, kind, flight, testNow, DefaultThresholds)
				if got.Computable {
					t.Errorf("%s budget (%s) produced a computable %.1f%% labelled %q", name, kind, got.Pct, got.Label)
				}
			}
		})
	}
}

// A campaign that has not started cannot be behind on spend. The elapsed floor of one day would
// otherwise invent a day of expected spend and flag a campaign scheduled to begin next week.
func TestComputePacing_FutureDatedFlightIsNotUnderspending(t *testing.T) {
	flight := Flight{Start: day(5), End: day(30)}
	got := ComputePacing(0, 7, 1000, BudgetLifetime, flight, testNow, DefaultThresholds)
	if got.Computable {
		t.Errorf("a flight starting in 5 days produced a computable %.1f%% labelled %q", got.Pct, got.Label)
	}
	if got.Label == PacingUnderspending {
		t.Error("a campaign that has not started was labelled underspending")
	}
}

// Finite inputs can still produce a non-finite result: a denormal expected-spend underflows
// toward zero while staying strictly positive, so the `expected <= 0` test passes and the
// division overflows.
func TestComputePacing_UnderflowDoesNotEscapeAsAMeasurement(t *testing.T) {
	start := testNow.AddDate(-273, 0, 0)
	end := testNow.AddDate(0, 0, 10)
	got := ComputePacing(500, 10, 1e-300, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds)
	if got.Computable {
		t.Errorf("underflowed expected-spend produced a computable %v labelled %q", got.Pct, got.Label)
	}
}

// The overspend edge is absolute, not a multiple of Constrained. Deriving it would move a
// boundary nobody asked to change whenever a platform overrides the constrained cap.
func TestThresholds_OverspendEdgeIsIndependentOfConstrained(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(10)} // expected-by-now = half the budget
	// Constrained raised to 120, overspending left at 130. A campaign at 135% must still be
	// overspending; a derived edge would have moved it to 156 and labelled this constrained.
	custom := Thresholds{Underspending: 50, Constrained: 120, Overspending: 130}
	got := ComputePacing(675, 10, 1000, BudgetLifetime, flight, testNow, custom) // 135%
	if got.Label != PacingOverspending {
		t.Errorf("135%% against an absolute 130 edge = %q, want overspending (the edge tracked Constrained)", got.Label)
	}
}

// A nil start with a present end has no flight to prorate across. Defaulting the start to `now`
// makes the flight begin this instant, and the one-day elapsed floor then compares a 30-day
// window of spend against a single day of plan — an on-plan campaign reports 500% overspending.
//
// start_date is nullable in the schema, so this is a storable state. It is the same defect as
// the future-dated flight, arriving through the other door: the now.Before(start) guard cannot
// catch it, because start was just set TO now.
func TestComputePacing_NilStartWithAnEndIsNotAMeasurement(t *testing.T) {
	end := testNow.AddDate(0, 0, 10)
	got := ComputePacing(500, 30, 1000, BudgetLifetime, Flight{Start: nil, End: &end}, testNow, DefaultThresholds)
	if got.Computable {
		t.Errorf("a flight with no start produced a computable %.1f%% labelled %q", got.Pct, got.Label)
	}
	if got.Label == PacingOverspending {
		t.Error("a campaign with no recorded start was reported as overspending")
	}

	// Both absent stays incomputable too, via the zero-length flight check.
	if both := ComputePacing(500, 30, 1000, BudgetLifetime, Flight{}, testNow, DefaultThresholds); both.Computable {
		t.Errorf("a flight with no dates at all produced a computable %.1f%%", both.Pct)
	}

	// And a present start still measures, or the guard would have disabled pacing wholesale.
	start := testNow.AddDate(0, 0, -10)
	ok := ComputePacing(500, 10, 1000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds)
	if !ok.Computable || math.Abs(ok.Pct-100) > 5 {
		t.Errorf("a normal flight = %.1f%% (computable=%v), want ~100%%", ok.Pct, ok.Computable)
	}
}

// A campaign starting exactly NOW has elapsed zero days, which daysBetween floors to one. A
// strict `now.Before(start)` lets that boundary through and measures the campaign against a full
// day of plan it has had no time to spend.
func TestComputePacing_FlightStartingExactlyNowIsNotMeasurable(t *testing.T) {
	end := testNow.AddDate(0, 0, 10)
	for name, spend := range map[string]float64{"with spend": 500, "no spend yet": 0} {
		t.Run(name, func(t *testing.T) {
			got := ComputePacing(spend, 7, 1000, BudgetLifetime, Flight{Start: &testNow, End: &end}, testNow, DefaultThresholds)
			if got.Computable {
				t.Errorf("a flight starting this instant produced a computable %.1f%% labelled %q", got.Pct, got.Label)
			}
		})
	}
	// One second in, it IS measurable — the guard must not disable pacing for every campaign
	// whose flight has merely begun.
	start := testNow.Add(-time.Second)
	if got := ComputePacing(500, 7, 1000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds); !got.Computable {
		t.Error("a flight that started one second ago is not measurable; the guard is too broad")
	}
}
