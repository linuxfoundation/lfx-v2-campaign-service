// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"math"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// These tests pin, CALLER BY CALLER, which band a pacing percentage lands in at every boundary of
// both ladders, end to end through each caller's own entry point. They were written against the
// code as it stood before the two ladder implementations were merged into one, and must not move:
// a failure here means a refactor moved an operator-facing alert band.
//
// The callers do not all see the same number. Google, Microsoft, Meta, Reddit and X ROUND the
// percentage before placing it on the ladder, LinkedIn does not, and the brief view does not; so
// the same raw 49.99% is underspending on LinkedIn and normal on Google. Each table states its
// caller's own outcome rather than one shared expectation.

// boundaryPcts is every raw percentage the tables below place: each ladder boundary of both
// ladders (50, 90, 100, 130), a hair either side of it, and points either side of each
// half-way mark the rounding callers resolve differently.
var boundaryPcts = []float64{
	0,
	49.4, 49.6, 49.99, 50, 50.01,
	89.99, 90, 90.01, 90.4, 90.6,
	99.99, 100, 100.01, 100.4, 100.6,
	129.99, 130, 130.01,
}

const (
	under = "underspending"
	norm  = "normal"
	cons  = "constrained"
	over  = "overspending"
)

// roundedMonitorWant is the account-monitor ladder (50/90/100) applied to math.Round(pct).
var roundedMonitorWant = map[float64]string{
	0:    under,
	49.4: under, 49.6: norm, 49.99: norm, 50: norm, 50.01: norm,
	89.99: norm, 90: norm, 90.01: norm, 90.4: norm, 90.6: cons,
	99.99: cons, 100: cons, 100.01: cons, 100.4: cons, 100.6: over,
	129.99: over, 130: over, 130.01: over,
}

// unroundedMonitorWant is the account-monitor ladder (50/90/100) applied to the raw pct.
var unroundedMonitorWant = map[float64]string{
	0:    under,
	49.4: under, 49.6: under, 49.99: under, 50: norm, 50.01: norm,
	89.99: norm, 90: norm, 90.01: cons, 90.4: cons, 90.6: cons,
	99.99: cons, 100: cons, 100.01: over, 100.4: over, 100.6: over,
	129.99: over, 130: over, 130.01: over,
}

// briefViewWant is the brief-view ladder (50/100/130) applied to the raw pct.
var briefViewWant = map[float64]string{
	0:    under,
	49.4: under, 49.6: under, 49.99: under, 50: norm, 50.01: norm,
	89.99: norm, 90: norm, 90.01: norm, 90.4: norm, 90.6: norm,
	99.99: norm, 100: norm, 100.01: cons, 100.4: cons, 100.6: cons,
	129.99: cons, 130: cons, 130.01: over,
}

// pinNow is a fixed instant at noon UTC, so day-counting callers see whole days.
var pinNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// Every monitor fixture below expects 1000 units of spend over the window, so spend = pct * 10.
func pinSpend(pct float64) float64 { return pct * 10 }

func pinMonitorRow(spend float64) model.AccountCampaignMetrics {
	return model.AccountCampaignMetrics{
		PlatformCampaignID: "c1", Name: "Pinned campaign", Status: "ACTIVE",
		Impressions: 5000, Clicks: 100, Spend: spend, BudgetDay: 100,
	}
}

type monitorCaller struct {
	name string
	want map[float64]string
	// eval runs the caller over one row and returns that row.
	eval func(m model.AccountCampaignMetrics) model.AccountMonitorRow
}

func pinOneRow(t *testing.T, rows []model.AccountMonitorRow) model.AccountMonitorRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	return rows[0]
}

func monitorCallers(t *testing.T) []monitorCaller {
	// X: a 10-day saved-report window, no line items, so the daily budget covers all 10 days.
	xFirst := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	xLast := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	return []monitorCaller{
		{"google", roundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateGoogleMonitor([]model.AccountCampaignMetrics{m}, 10)
			return pinOneRow(t, rows)
		}},
		{"microsoft", roundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{m}, 10)
			return pinOneRow(t, rows)
		}},
		{"meta", roundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateMetaMonitor([]model.AccountCampaignMetrics{m}, 10, pinNow)
			return pinOneRow(t, rows)
		}},
		{"reddit", roundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateRedditMonitor([]model.AccountCampaignMetrics{m}, 10, pinNow)
			return pinOneRow(t, rows)
		}},
		{"x", roundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateTwitterMonitor([]model.AccountCampaignMetrics{m}, &xFirst, &xLast)
			return pinOneRow(t, rows)
		}},
		// LinkedIn's daily branch counts ceil(days-1) whole days back from now, so days=11 is a
		// 10-day expectation.
		{"linkedin", unroundedMonitorWant, func(m model.AccountCampaignMetrics) model.AccountMonitorRow {
			rows, _ := EvaluateLinkedInMonitor([]model.AccountCampaignMetrics{m}, 11, pinNow)
			return pinOneRow(t, rows)
		}},
	}
}

// TestPin_MonitorCallers_LadderBoundaries places every boundary percentage through every
// account-monitor caller and checks the band, that the row is paced, and the reported pct.
func TestPin_MonitorCallers_LadderBoundaries(t *testing.T) {
	for _, c := range monitorCallers(t) {
		for _, pct := range boundaryPcts {
			row := c.eval(pinMonitorRow(pinSpend(pct)))
			want, ok := c.want[pct]
			if !ok {
				t.Fatalf("%s: no expectation for %v", c.name, pct)
			}
			if string(row.PacingLabel) != want {
				t.Errorf("%s at %v%%: label %q, want %q (reported pct %v)", c.name, pct, row.PacingLabel, want, row.PacingPct)
			}
			if row.Metrics.PacingUnknown {
				t.Errorf("%s at %v%%: PacingUnknown set on a budgeted row", c.name, pct)
			}
			wantPct := pct
			if c.name != "linkedin" {
				wantPct = math.Round(pct)
			}
			if math.Abs(row.PacingPct-wantPct) > 1e-9 {
				t.Errorf("%s at %v%%: reported pct %v, want %v", c.name, pct, row.PacingPct, wantPct)
			}
		}
	}
}

// TestPin_MonitorCallers_NoBudgetIsUnknownRow pins the unknown-pacing row on every caller: a
// campaign with no budget is PacingUnknown, carries the placeholder normal label, and a zero pct.
func TestPin_MonitorCallers_NoBudgetIsUnknownRow(t *testing.T) {
	for _, c := range monitorCallers(t) {
		m := pinMonitorRow(250)
		m.BudgetDay = 0
		row := c.eval(m)
		if !row.Metrics.PacingUnknown || row.PacingLabel != model.MonitorPacingNormal || row.PacingPct != 0 {
			t.Errorf("%s no budget: got unknown=%v label=%q pct=%v, want unknown=true label=normal pct=0",
				c.name, row.Metrics.PacingUnknown, row.PacingLabel, row.PacingPct)
		}
	}
}

// TestPin_MonitorCallers_FetchFailedIsUnknownRow pins fetchFailedRow on every caller.
func TestPin_MonitorCallers_FetchFailedIsUnknownRow(t *testing.T) {
	for _, c := range monitorCallers(t) {
		m := pinMonitorRow(250)
		m.FetchFailed = true
		row := c.eval(m)
		if !row.Metrics.PacingUnknown || row.PacingLabel != model.MonitorPacingNormal || row.PacingPct != 0 {
			t.Errorf("%s fetch failed: got unknown=%v label=%q pct=%v, want unknown=true label=normal pct=0",
				c.name, row.Metrics.PacingUnknown, row.PacingLabel, row.PacingPct)
		}
	}
}

// TestPin_MonitorCallers_ZeroSpendIsUnderspending pins that a budgeted campaign spending nothing
// is a measured 0% underspend, not an unknown row.
func TestPin_MonitorCallers_ZeroSpendIsUnderspending(t *testing.T) {
	for _, c := range monitorCallers(t) {
		row := c.eval(pinMonitorRow(0))
		if row.Metrics.PacingUnknown || row.PacingLabel != model.MonitorPacingUnderspending || row.PacingPct != 0 {
			t.Errorf("%s zero spend: got unknown=%v label=%q pct=%v, want unknown=false label=underspending pct=0",
				c.name, row.Metrics.PacingUnknown, row.PacingLabel, row.PacingPct)
		}
	}
}

// TestPin_MonitorLadder_NaN pins where a NaN percentage lands on the account-monitor ladder:
// normal, because every comparison against NaN is false and the monitor's switch falls through
// to its default. Reachable from a caller only if a platform reports a NaN spend; see the
// Google case below.
func TestPin_MonitorLadder_NaN(t *testing.T) {
	if got := pacingLabelFor(math.NaN()); got != model.MonitorPacingNormal {
		t.Errorf("pacingLabelFor(NaN) = %q, want normal", got)
	}
	row := pinOneRow(t, func() []model.AccountMonitorRow {
		rows, _ := EvaluateGoogleMonitor([]model.AccountCampaignMetrics{pinMonitorRow(math.NaN())}, 10)
		return rows
	}())
	if row.PacingLabel != model.MonitorPacingNormal {
		t.Errorf("google NaN spend: label %q, want normal", row.PacingLabel)
	}
}

// TestPin_MonitorLadder_Infinities pins the two infinities on the account-monitor ladder.
func TestPin_MonitorLadder_Infinities(t *testing.T) {
	if got := pacingLabelFor(math.Inf(1)); got != model.MonitorPacingOverspending {
		t.Errorf("pacingLabelFor(+Inf) = %q, want overspending", got)
	}
	if got := pacingLabelFor(math.Inf(-1)); got != model.MonitorPacingUnderspending {
		t.Errorf("pacingLabelFor(-Inf) = %q, want underspending", got)
	}
}

// briefPinFlight is 30 days in and 30 to go, so a 10-day daily-budget expectation is 100 * 10.
func briefPinFlight() Flight {
	start := pinNow.AddDate(0, 0, -30)
	end := pinNow.AddDate(0, 0, 30)
	return Flight{Start: &start, End: &end}
}

// TestPin_BriefView_LadderBoundaries places every boundary percentage through ComputePacing with
// the brief-view thresholds, unrounded.
func TestPin_BriefView_LadderBoundaries(t *testing.T) {
	for _, pct := range boundaryPcts {
		got := ComputePacing(pinSpend(pct), 10, 100, BudgetDaily, briefPinFlight(), pinNow, DefaultThresholds)
		if !got.Computable {
			t.Errorf("brief at %v%%: not computable", pct)
			continue
		}
		if string(got.Label) != briefViewWant[pct] {
			t.Errorf("brief at %v%%: label %q, want %q (pct %v)", pct, got.Label, briefViewWant[pct], got.Pct)
		}
		if math.Abs(got.Pct-pct) > 1e-9 {
			t.Errorf("brief at %v%%: pct %v", pct, got.Pct)
		}
	}
}

// TestPin_BriefView_NoBudgetIsUnknown pins the brief view's unknown value: label unknown, not
// computable, zero pct.
func TestPin_BriefView_NoBudgetIsUnknown(t *testing.T) {
	got := ComputePacing(250, 10, 0, BudgetDaily, briefPinFlight(), pinNow, DefaultThresholds)
	if got != (Pacing{Label: PacingUnknown}) {
		t.Errorf("no budget: got %+v, want {Label: unknown}", got)
	}
}

// TestPin_BriefView_ActionItemsAtBoundaries pins which pacing action item (rule and priority)
// the brief view raises at each boundary, and its message.
func TestPin_BriefView_ActionItemsAtBoundaries(t *testing.T) {
	cases := []struct {
		pct      float64
		rule     string
		priority Priority
		issue    string
	}{
		{49.99, "underspending", PriorityHigh, "Underspending — 50% of expected spend for this point in the flight"},
		{50, "", "", ""},
		{100, "", "", ""},
		{100.01, "budget_constrained", PriorityMedium, "Spending ahead of plan — 100% of expected spend for this point in the flight"},
		{130, "budget_constrained", PriorityMedium, "Spending ahead of plan — 130% of expected spend for this point in the flight"},
		{130.01, "budget_constrained", PriorityMedium, "Spending ahead of plan — 130% of expected spend for this point in the flight"},
	}
	for _, c := range cases {
		p := ComputePacing(pinSpend(c.pct), 10, 100, BudgetDaily, briefPinFlight(), pinNow, DefaultThresholds)
		items := Evaluate(Input{
			CampaignID: "c1", Platform: "google_ads", Status: model.CampaignRunActive,
			Impressions: 5000, Clicks: 100, Spend: pinSpend(c.pct), CTRPct: 2,
			Pacing: p, DeliveryExpected: true, BillsPerDelivery: true,
		})
		if c.rule == "" {
			if len(items) != 0 {
				t.Errorf("brief at %v%%: got %+v, want no items", c.pct, items)
			}
			continue
		}
		if len(items) != 1 || items[0].Rule != c.rule || items[0].Priority != c.priority || items[0].Issue != c.issue {
			t.Errorf("brief at %v%%: got %+v, want one %s/%s %q", c.pct, items, c.rule, c.priority, c.issue)
		}
	}
}
