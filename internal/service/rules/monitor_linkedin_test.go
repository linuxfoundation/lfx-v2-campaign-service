// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestLinkedinPacingPct_IsUnrounded pins the property called out at length in
// model.AccountMonitorRow.PacingPct's doc comment: LinkedIn, uniquely among the four
// platforms, does NOT round its pacing percentage. A spend/expected ratio chosen to produce a
// non-integral percentage must come back with its fractional part intact.
func TestLinkedinPacingPct_IsUnrounded(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{
		BudgetDay: 3, // flightDays=1 (days=1, no schedule dates) => expected = 3*1 = 3
		Spend:     1,
	}
	pct := linkedinPacingPct(m, 1, now)
	want := 1.0 / 3.0 * 100 // = 33.333...
	if pct == 33 {
		t.Fatalf("pacingPct = %v looks rounded; LinkedIn's pacing percentage must stay fractional", pct)
	}
	if diff := pct - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("pacingPct = %v, want %v (unrounded)", pct, want)
	}
}

// TestLinkedinPacingPct_DaylightSavingDoesNotShiftTheWindow pins the window's day count to the
// calendar, whatever zone the caller's clock carries, in both the daily and the total-budget
// branch. With `now` in a zone with daylight saving, AddDate(0, 0, -29) used to land 29 days and
// one hour back for the 30 days after a fall-back transition (Los Angeles from 2026-11-01, Berlin
// from 2026-10-25, Sydney from 2026-04-05), and math.Ceil turned that hour into a 30th day: 600
// against 100/day read 20% rather than 600/2900 ≈ 20.69%. Every day of 2026, at several local
// hours, in each zone.
func TestLinkedinPacingPct_DaylightSavingDoesNotShiftTheWindow(t *testing.T) {
	// The total-budget row's flight (1095 days, so 100/day) spans all of 2026, which makes the
	// 30-day window the effective one, as in the daily row.
	rows := map[string]model.AccountCampaignMetrics{
		"daily": {BudgetDay: 100, Spend: 600},
		"total": {TotalBudget: 109500, Spend: 600, StartDate: "2025-01-01", EndDate: "2028-01-01"},
	}
	want := 600.0 / 2900 * 100
	for _, name := range []string{"UTC", "America/Los_Angeles", "Europe/Berlin", "Australia/Sydney"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("LoadLocation(%q): %v", name, err)
		}
		for d := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); d.Year() == 2026; d = d.AddDate(0, 0, 1) {
			for _, hour := range []int{0, 9, 16, 23} {
				now := d.Add(time.Duration(hour) * time.Hour)
				for branch, m := range rows {
					if got := linkedinPacingPct(m, 30, now); got < want-1e-9 || got > want+1e-9 {
						t.Errorf("%s %s: pacing = %v%%, want %v%%", branch, now.Format(time.RFC3339), got, want)
					}
				}
			}
		}
	}
}

// TestLinkedinActionItems_UnderspendingAndConstrained exercise the two label-driven rules and
// the CTR/conversions/paused rules in isolation.
func TestLinkedinActionItems_UnderspendingAndConstrained(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("underspending label is HIGH", func(t *testing.T) {
		items := linkedinActionItems(base, 10, model.MonitorPacingUnderspending, "USD")
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityHigh)
	})
	t.Run("constrained label is MED", func(t *testing.T) {
		items := linkedinActionItems(base, 95, model.MonitorPacingConstrained, "USD")
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("overspending label also fires the constrained MED rule", func(t *testing.T) {
		// linkedinActionItems' constrained rule is `label == constrained || label ==
		// overspending` — both share the same MED item, matching the ported "the 130
		// overspending member is dead" oddity: overspending never gets its own message here.
		items := linkedinActionItems(base, 150, model.MonitorPacingOverspending, "USD")
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("low CTR is MED", func(t *testing.T) {
		row := base
		row.Ctr = 0.1
		row.Impressions = 5000
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal, "USD")
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("clicks without conversions is MED", func(t *testing.T) {
		row := base
		row.Clicks = 51
		row.Conversions = floatPtr(0)
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal, "USD")
		mustContainIssue(t, items, "Clicks without conversions", model.MonitorPriorityMed)
	})
	t.Run("paused with real budget is LOW", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		row.TotalBudget = 20
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal, "USD")
		mustContainIssue(t, items, "PAUSED with real budget", model.MonitorPriorityLow)
	})
}

func mustContainIssue(t *testing.T, items []model.AccountMonitorActionItem, substr string, wantPriority model.MonitorPriority) {
	t.Helper()
	for _, it := range items {
		if strings.Contains(it.Issue, substr) {
			if it.Priority != wantPriority {
				t.Errorf("priority = %q, want %q for issue %q", it.Priority, wantPriority, it.Issue)
			}
			return
		}
	}
	t.Errorf("no action item contained %q among %+v", substr, items)
}

// TestEvaluateLinkedInMonitor_SkipsFetchFailedRows mirrors
// TestEvaluateGoogleMonitor_SkipsFetchFailedRows: a FetchFailed row's zero-value metrics must
// not be run through pacing/action-item evaluation, but the row itself must still be returned.
func TestEvaluateLinkedInMonitor_SkipsFetchFailedRows(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "Failed Fetch", Status: "ACTIVE", BudgetDay: 50, FetchFailed: true},
	}
	out, items := EvaluateLinkedInMonitor(rows, 30, now, "USD")
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1 — a FetchFailed row must still be returned: %+v", len(out), out)
	}
	if !out[0].Metrics.FetchFailed || !out[0].Metrics.PacingUnknown {
		t.Errorf("FetchFailed row = %+v, want FetchFailed=true and PacingUnknown=true — a failed fetch must not report a computed pacing verdict", out[0])
	}
	// PacingLabel keeps its zero-value "normal" placeholder rather than going unset — pacing_label
	// is a required enum with no "unknown" member; PacingUnknown=true is the signal not to trust it.
	if out[0].PacingLabel != model.MonitorPacingNormal {
		t.Errorf("FetchFailed row PacingLabel = %q, want the documented placeholder %q", out[0].PacingLabel, model.MonitorPacingNormal)
	}
	if len(items) != 0 {
		t.Errorf("FetchFailed row produced action items, want none: %+v", items)
	}
}

// TestLinkedinLowCtr_ZeroCtrIsTheWorstCaseNotAnExemption is the regression test for the guard
// this port carried from linkedin-ads.service.ts: `ctr > 0 && ctr < 0.3`. A 0% CTR is the worst
// possible case of the condition the rule exists to detect, and it was the one case excluded.
//
// LinkedIn is also the only one of the four platforms with no "impressions but no clicks" rule
// (googleActionItems has one), so nothing else caught it either: a campaign with half a million
// impressions and not one click produced no action item at all, while one at 0.29% produced a
// MED. Both halves are asserted here, since the rule is only correct with both.
func TestLinkedinLowCtr_ZeroCtrIsTheWorstCaseNotAnExemption(t *testing.T) {
	served := model.AccountCampaignMetrics{
		PlatformCampaignID: "c1", Name: "c", Impressions: 500000, Clicks: 0, Ctr: 0,
	}
	items := linkedinActionItems(served, 0, model.MonitorPacingNormal, "USD")
	mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
}

// TestLinkedinLowCtr_NeedsVolumeBeforeItMeansAnything pins the other half. Removing the `ctr > 0`
// exclusion without an impressions floor would fire "Low CTR: 0.00%" on every campaign that has
// not been served yet — turning the fix above into a false alert on every new campaign. Meta,
// Reddit and Google all gate this rule on volume; LinkedIn did not, and now does.
func TestLinkedinLowCtr_NeedsVolumeBeforeItMeansAnything(t *testing.T) {
	for _, impressions := range []int64{0, 1, linkedinMinImpressions} {
		row := model.AccountCampaignMetrics{
			PlatformCampaignID: "c1", Name: "c", Impressions: impressions, Ctr: 0,
		}
		for _, it := range linkedinActionItems(row, 0, model.MonitorPacingNormal, "USD") {
			if strings.Contains(it.Issue, "Low CTR") {
				t.Errorf("impressions = %d: emitted %q; the floor is exclusive (> %d), and a "+
					"campaign with no delivery has no CTR to judge",
					impressions, it.Issue, linkedinMinImpressions)
			}
		}
	}
}

// A row the dispatcher marked PacingUnknown (its budget's currency disagrees with another budget
// on the account) is not paced and raises no pacing item, even though it has a budget and spend.
func TestEvaluateLinkedInMonitor_CurrencyMismatchIsUnknownPacing(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		BudgetDay: 100, Spend: 1, Impressions: 10, Clicks: 1, PacingUnknown: true}
	rows, items := EvaluateLinkedInMonitor([]model.AccountCampaignMetrics{m}, 30, now, "USD")
	if len(rows) != 1 || !rows[0].Metrics.PacingUnknown || rows[0].PacingPct != 0 {
		t.Fatalf("row = %+v, want unknown pacing", rows)
	}
	for _, it := range items {
		if strings.Contains(it.Issue, "pacing") {
			t.Errorf("pacing item %q raised for a row whose pacing is unknown", it.Issue)
		}
	}
	m.PacingUnknown = false
	rows, _ = EvaluateLinkedInMonitor([]model.AccountCampaignMetrics{m}, 30, now, "USD")
	if rows[0].Metrics.PacingUnknown || rows[0].PacingPct == 0 {
		t.Errorf("row = %+v, want it paced once the mismatch flag is clear", rows[0])
	}
}
