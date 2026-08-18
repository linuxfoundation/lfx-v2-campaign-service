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
	flight := Flight{Start: day(-10), End: day(9)}

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
	flight := Flight{Start: day(-10), End: day(9)} // expected-by-now = half the budget
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
	flight := Flight{Start: day(-4), End: day(25)} // 4 days elapsed of a 30-day flight
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
		"no budget recorded":           {0, Flight{Start: day(-10), End: day(9)}},
		"negative budget":              {-100, Flight{Start: day(-10), End: day(9)}},
		"flight ends before it starts": {1000, Flight{Start: day(10), End: day(-10)}},
		"zero-length flight":           {1000, Flight{Start: day(0), End: day(0)}},
		// The two above both have a start at or after now, so they exit at the
		// !now.After(start) guard and never reach the inverted-flight check. This one has a
		// PAST start, which is the only way to reach it — and it is a storable state:
		// start_date and end_date are plain nullable DATE columns with no CHECK constraint and
		// no service-side ordering validation, so a typo'd end date produces exactly this.
		//
		// A start EQUAL to the end is deliberately absent: end_date means "through the end of
		// that day", so start == end is a valid ONE-DAY flight, not an unusable schedule. It
		// used to land here only because the raw midnight was treated as an exclusive bound.
		// See TestComputePacing_SingleDayFlight.
		"started, but ends before it started": {1000, Flight{Start: day(-10), End: day(-20)}},
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

// A campaign in its first day has no meaningful pacing.
//
// This test previously asserted the opposite — that a partial first day counts as a whole one —
// which is what made a campaign launched minutes ago report 0% and raise a HIGH-priority
// underspending item against itself. Two hours into a 30-day $3000 flight the expected spend is
// $8, and the platform has not reported the real figure yet either.
func TestComputePacing_FirstDayIsNotPaced(t *testing.T) {
	end := testNow.AddDate(0, 0, 30)
	for name, gap := range map[string]time.Duration{
		"one minute": time.Minute,
		"one hour":   time.Hour,
		"two hours":  2 * time.Hour,
		"half a day": 12 * time.Hour,
		"23 hours":   23 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			start := testNow.Add(-gap)
			got := ComputePacing(0, 30, 3000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds)
			if got.Computable {
				t.Errorf("%v into the flight produced a computable %.1f%% labelled %q", gap, got.Pct, got.Label)
			}
			if got.Label == PacingUnderspending {
				t.Errorf("%v into the flight was labelled underspending", gap)
			}
		})
	}

	// A full day in, it IS measured — the floor must not disable pacing wholesale.
	//
	// The flight spans start(-1d) to end(+30d). end_date means "through the end of that day", so
	// the flight runs to the midnight AFTER it and daysBetween counts 32 days — one day of a
	// $3000 plan is therefore $93.75, not the $96.77 that the un-normalised 31-day count gave.
	// Asserted against the real figure: rounding it to a tidier number would either fail against
	// correct code or need a tolerance wide enough to stop binding anything.
	start := testNow.AddDate(0, 0, -1)
	got := ComputePacing(93.75, 30, 3000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("a campaign a full day into its flight must be measured; the floor is too broad")
	}
	if math.Abs(got.Pct-100) > 1 {
		t.Errorf("one day in, one day of plan spent = %.1f%%, want ~100%%", got.Pct)
	}
}

func TestComputePacing_RejectsNonFiniteSpend(t *testing.T) {
	flight := Flight{Start: day(-10), End: day(9)}
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
	flight := Flight{Start: day(-20), End: day(19)}

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
	flight := Flight{Start: day(-3), End: day(26)}
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
	flight := Flight{Start: day(-10), End: day(9)}
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
	flight := Flight{Start: day(-20), End: day(19)}

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
	flight := Flight{Start: day(-10), End: day(9)}
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
	flight := Flight{Start: day(-10), End: day(9)} // expected-by-now = half the budget
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
// the future-dated flight, arriving through the other door: the now.After(start) guard cannot
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
// a strict `now.Before(start)` would let that boundary through, measuring the campaign against a
// full day of plan it has had no time to spend.
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
	// A full day in it IS measurable — the guard must not disable pacing for every campaign
	// whose flight has merely begun. (One second in is NOT measurable, but that is the
	// first-day floor's job, not this guard's — see TestComputePacing_FirstDayIsNotPaced.)
	start := testNow.AddDate(0, 0, -1)
	if got := ComputePacing(500, 7, 1000, BudgetLifetime, Flight{Start: &start, End: &end}, testNow, DefaultThresholds); !got.Computable {
		t.Error("a flight that started a day ago is not measurable; the guard is too broad")
	}
}

// A flight that has already ENDED must stop accruing expected spend at its end date. Without
// clamping, expected spend keeps growing with wall-clock time and a fully-delivered campaign
// drifts into `underspending` the longer it sits finished.
func TestComputePacing_CompletedFlightStopsAccruingPlan(t *testing.T) {
	// A 10-day flight that ended 5 days ago, fully spent. Expected spend is the whole $1000
	// (10 of 10 days elapsed), so $1000 spent is 100% — not 66% as it would be if the plan
	// kept prorating across the 15 days since the start.
	//
	// spendDays is 30, deliberately ABOVE the flight length: `measured` is
	// min(spendDays, elapsed), so a spendDays at or below the flight length caps the result
	// and masks whether `elapsed` was clamped at all. Only a spendDays large enough to let
	// `elapsed` be the binding term can detect a missing minTime.
	flight := Flight{Start: day(-15), End: day(-6)}
	got := ComputePacing(1000, 30, 1000, BudgetLifetime, flight, testNow, DefaultThresholds)
	if !got.Computable {
		t.Fatal("a completed flight is still measurable")
	}
	if math.Abs(got.Pct-100) > 1 {
		t.Errorf("a fully-spent completed flight = %.1f%%, want ~100%% (expected spend must stop at the end date)", got.Pct)
	}
	if got.Label == PacingUnderspending {
		t.Error("a fully-delivered campaign was labelled underspending because the plan kept accruing past its end")
	}
}

// The reported defect, with the exact numbers it was reported with.
//
// `end_date` is a DATE meaning "through the end of that day". Treating the midnight it decodes
// to as an EXCLUSIVE bound cut the final day off every flight, and on a short flight that is not
// a rounding error but a proportional one: a two-day flight measured ONE day, so the plan it was
// plotted against was half the real one.
//
// Aug 17 -> Aug 18 at noon on the 18th, $100/day, $200 spent. The campaign is exactly on plan.
// Before the fix it read 200% and was labelled `overspending` — a HIGH-priority budget item
// raised against a campaign doing precisely what it was told to.
func TestComputePacing_TwoDayFlightOnItsFinalDay(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)

	// The flight is TWO days long, not one. This is the arithmetic the whole defect rests on.
	if got := daysBetween(start, *flightEndInstant(&end)); got != 2 {
		t.Errorf("daysBetween over an Aug 17 -> Aug 18 flight = %v, want 2", got)
	}

	// At NOON on the final day only 1.5 of those 2 days have elapsed, and expected spend is
	// prorated to the elapsed period — so the on-plan spend at this instant is $150, not $200.
	// Asserted as exact equalities: this is integral arithmetic with no rounding in it, and a
	// tolerance would re-admit the very off-by-one-day these cases exist to pin.
	onPlan := ComputePacing(150, 2, 100, BudgetDaily, Flight{Start: &start, End: &end}, now, DefaultThresholds)
	if !onPlan.Computable {
		t.Fatal("a campaign on the final day of its flight must still be measurable")
	}
	if onPlan.Pct != 100 {
		t.Errorf("on-plan two-day flight at noon on day two = %v%%, want exactly 100%%", onPlan.Pct)
	}
	if onPlan.Label != PacingNormal {
		t.Errorf("label = %q, want normal — this campaign spent exactly its plan", onPlan.Label)
	}

	// The reported case, with the reported numbers: $200 spent by noon of the final day. Front-
	// loading the whole two-day budget into the first day and a half IS ahead of plan, and 133%
	// clears the 130 overspend edge, so `overspending` here is a true finding rather than the
	// artefact. What changed is the MAGNITUDE: the un-normalised end priced the entire flight at
	// a single day and reported 200%.
	got := ComputePacing(200, 2, 100, BudgetDaily, Flight{Start: &start, End: &end}, now, DefaultThresholds)
	if math.Abs(got.Pct-400.0/3.0) > 1e-9 {
		t.Errorf("$200 by noon of a two-day flight = %v%%, want 133.33%% (1.5 days x $100 expected)", got.Pct)
	}
	// The specific failure that was reported, named so a regression is unambiguous. Keyed on the
	// PERCENTAGE, not the label: both readings land in the same band here, so a label-only
	// assertion would not have detected the defect at all.
	if got.Pct == 200 {
		t.Error("the flight end was treated as an exclusive midnight: the two-day flight was priced as one")
	}

	// And by the END of the flight the full $200 is exactly the plan.
	atEnd := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	done := ComputePacing(200, 2, 100, BudgetDaily, Flight{Start: &start, End: &end}, atEnd, DefaultThresholds)
	if done.Pct != 100 || done.Label != PacingNormal {
		t.Errorf("a fully-delivered two-day flight = %v%% / %q, want exactly 100%% / normal", done.Pct, done.Label)
	}
}

// A single-day flight (start == end) is a valid schedule, not an unusable one.
//
// end_date runs through the end of its day, so start == end describes a campaign running for
// exactly one day. While the raw midnight was used as an exclusive bound this had zero length
// and tripped the inverted-flight guard, so EVERY one-day campaign was permanently `unknown`.
func TestComputePacing_SingleDayFlight(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	// Ran yesterday, and is now over. A completed one-day flight has one full day of plan.
	dayOf := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	if got := daysBetween(dayOf, *flightEndInstant(&dayOf)); got != 1 {
		t.Errorf("a single-day flight spans %v days, want 1", got)
	}

	got := ComputePacing(100, 1, 100, BudgetDaily, Flight{Start: &dayOf, End: &dayOf}, now, DefaultThresholds)
	if !got.Computable {
		t.Fatal("a one-day flight is a real schedule; it must be measurable")
	}
	if got.Pct != 100 {
		t.Errorf("a fully-spent one-day flight = %v%%, want exactly 100%%", got.Pct)
	}
	if got.Label != PacingNormal {
		t.Errorf("label = %q, want normal", got.Label)
	}

	// Half spent on that same one-day flight is genuinely underspending — the label must still
	// discriminate, not just return `normal` for everything one-day.
	half := ComputePacing(40, 1, 100, BudgetDaily, Flight{Start: &dayOf, End: &dayOf}, now, DefaultThresholds)
	if half.Pct != 40 || half.Label != PacingUnderspending {
		t.Errorf("40%% of a one-day plan = %v%% / %q, want 40%% / underspending", half.Pct, half.Label)
	}
}

// The final day of a flight must be paced, not silently dropped.
//
// A lifetime budget is the case that shows the proportional error: with the end treated as
// exclusive, a 10-day flight prorated its whole budget across 9 days, inflating the daily plan
// by ~11% and pushing an on-plan campaign toward `underspending` on its last day.
func TestComputePacing_FinalDayOfFlightIsPaced(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	// Aug 9 -> Aug 18 inclusive: 10 days. $1000 lifetime = $100/day.
	start := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)

	if got := daysBetween(start, *flightEndInstant(&end)); got != 10 {
		t.Errorf("Aug 9 -> Aug 18 = %v days, want 10", got)
	}

	// Read over a 7-day window: 7 x $100 = $700 expected.
	got := ComputePacing(700, 7, 1000, BudgetLifetime, Flight{Start: &start, End: &end}, now, DefaultThresholds)
	if !got.Computable {
		t.Fatal("the last day of a flight is still inside it")
	}
	if got.Pct != 100 {
		t.Errorf("on-plan campaign on its final day = %v%%, want exactly 100%%", got.Pct)
	}
	if got.Label != PacingNormal {
		t.Errorf("label = %q, want normal", got.Label)
	}
}
