// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestPacingLabelFor_Boundaries pins every boundary of the one ladder all four account monitors
// now share, including which side of each cutoff is inclusive. These four platforms previously
// carried four private copies of this switch under names that disagreed about which band a
// number belonged to, so the boundaries are worth asserting in one place rather than inferring
// them from any single platform's constant names.
func TestPacingLabelFor_Boundaries(t *testing.T) {
	cases := []struct {
		pct  float64
		want model.MonitorPacingLabel
	}{
		{0, model.MonitorPacingUnderspending},
		{49.9, model.MonitorPacingUnderspending},
		{50, model.MonitorPacingNormal}, // the floor is exclusive: < 50, not <= 50
		{90, model.MonitorPacingNormal}, // healthy band top is inclusive
		{90.1, model.MonitorPacingConstrained},
		{100, model.MonitorPacingConstrained}, // exactly on plan is constrained, not overspending
		{100.1, model.MonitorPacingOverspending},
		{130, model.MonitorPacingOverspending}, // the BFF's dead 130 member changes nothing here
	}
	for _, c := range cases {
		if got := pacingLabelFor(c.pct); got != c.want {
			t.Errorf("pacingLabelFor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

// TestUnknownPacingRow_ReportsAbsenceNotZero pins the contract
// model.AccountCampaignMetrics.PacingUnknown documents: absent, not defaulted.
func TestUnknownPacingRow_ReportsAbsenceNotZero(t *testing.T) {
	row := unknownPacingRow(model.AccountCampaignMetrics{PlatformCampaignID: "1", Spend: 25})
	if !row.Metrics.PacingUnknown {
		t.Errorf("PacingUnknown = false, want true")
	}
	if row.PacingPct != 0 {
		t.Errorf("PacingPct = %v, want the zero value — an unknown pacing carries no percentage", row.PacingPct)
	}
	if row.PacingLabel != model.MonitorPacingNormal {
		t.Errorf("PacingLabel = %q, want the %q placeholder the enum forces", row.PacingLabel, model.MonitorPacingNormal)
	}
	if row.Metrics.Spend != 25 {
		t.Errorf("Spend = %v, want 25 — the metrics themselves are real, only the pacing is not", row.Metrics.Spend)
	}
}

// TestBudgetlessCampaignIsPacingUnknown_AllPlatforms is the regression test for the defect this
// package carried on three of its four platforms: a campaign the platform reports with no budget
// at all has nothing to pace against, but Google and Meta fell through to pacingPct = 0, which
// the ladder reads as "underspending" — reporting every budget-less campaign as failing to spend
// a budget it does not have. LinkedIn held it off the ladder but still emitted the row as
// "normal" with PacingUnknown unset, which a consumer reads as "on plan".
//
// Reddit already routed this through PacingUnknown and is included to pin that it still does.
//
// All four must now agree — and Microsoft and X, added later on the same shared helpers, with them:
// PacingUnknown true, and never the underspending label.
func TestBudgetlessCampaignIsPacingUnknown_AllPlatforms(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	// No BudgetDay, no TotalBudget, but real spend — the shape that made a computed 0% look
	// like a genuine reading. Reddit keys its own unknown branch off an empty StartDate, which
	// this row also has.
	budgetless := func(status string) model.AccountCampaignMetrics {
		return model.AccountCampaignMetrics{
			PlatformCampaignID: "1", Name: "No Budget", Status: status, Spend: 25, Impressions: 900, Clicks: 5,
		}
	}

	platforms := []struct {
		name   string
		status string // each port compares against its own platform's status vocabulary
		eval   func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem)
	}{
		{"google", "enabled", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateGoogleMonitor(r, 10)
		}},
		{"meta", "ACTIVE", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateMetaMonitor(r, 10, now)
		}},
		{"linkedin", "ACTIVE", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateLinkedInMonitor(r, 10, now)
		}},
		{"reddit", "ACTIVE", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateRedditMonitor(r, 10, now)
		}},
		// Microsoft has daily budgets only and no flight dates, so like Google its evaluator
		// takes no clock.
		{"microsoft", "Active", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateMicrosoftMonitor(r, 10)
		}},
		// X, the second report-backed platform, paces a daily or a total budget; with neither
		// there is nothing to pace.
		{"x", "ACTIVE", func(r []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return EvaluateTwitterMonitor(r, 10, now)
		}},
	}

	for _, p := range platforms {
		t.Run(p.name, func(t *testing.T) {
			out, items := p.eval([]model.AccountCampaignMetrics{budgetless(p.status)})
			if len(out) != 1 {
				t.Fatalf("got %d rows, want 1", len(out))
			}
			if !out[0].Metrics.PacingUnknown {
				t.Errorf("PacingUnknown = false, want true — there is no budget to pace against")
			}
			if out[0].PacingLabel == model.MonitorPacingUnderspending {
				t.Errorf("PacingLabel = %q; a campaign with no budget cannot be underspending one", out[0].PacingLabel)
			}
			if out[0].PacingPct != 0 {
				t.Errorf("PacingPct = %v, want 0", out[0].PacingPct)
			}
			for _, it := range items {
				if strings.Contains(strings.ToLower(it.Issue), "underspending") ||
					strings.Contains(strings.ToLower(it.Issue), "only spending") {
					t.Errorf("emitted an underspend action item for a budget-less campaign: %q", it.Issue)
				}
			}
		})
	}
}

// TestBudgetlessGoogleCampaignStillRaisesThePlaceholderBudget pins that suppressing the bogus
// underspend item did not silence the accurate one. A budget-less enabled Google campaign is a
// real problem; googleActionItems' BudgetDay <= 1 rule is what says so, at HIGH, and in the
// language of the actual fault rather than as a pacing complaint.
func TestBudgetlessGoogleCampaignStillRaisesThePlaceholderBudget(t *testing.T) {
	_, items := EvaluateGoogleMonitor([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "No Budget", Status: "enabled", Spend: 25},
	}, 10)

	var found bool
	for _, it := range items {
		if it.Priority == model.MonitorPriorityHigh && strings.Contains(it.Issue, "placeholder") {
			found = true
		}
	}
	if !found {
		t.Errorf("no HIGH placeholder-budget item for a budget-less enabled campaign; items = %+v", items)
	}
}

// TestPriorityRank_MedSortsAheadOfLow is the regression test for
// linuxfoundation/lfx-self-serve#3018. LinkedIn's rank function spelled its middle case "MEDIUM"
// while model.MonitorPriorityMed is "MED", so no MED item ever matched: every one fell through
// to the unranked bucket and sorted BEHIND the LOW items — the reverse of the intended order,
// on the list an operator reads top-down to decide what to fix first.
//
// The four platforms now share one rank function, so this asserts it once. The ordering matters
// more than the numbers, so it is asserted through the sort rather than against the ranks.
func TestPriorityRank_MedSortsAheadOfLow(t *testing.T) {
	items := []model.AccountMonitorActionItem{
		{CampaignID: "med-1", Priority: model.MonitorPriorityMed},
		{CampaignID: "high", Priority: model.MonitorPriorityHigh},
		{CampaignID: "low", Priority: model.MonitorPriorityLow},
		{CampaignID: "med-2", Priority: model.MonitorPriorityMed},
		{CampaignID: "odd", Priority: model.MonitorPriority("SOMETHING-ELSE")},
	}
	sortByPriority(items)

	want := []string{"high", "med-1", "med-2", "low", "odd"}
	got := itemIDs(items)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted order = %v, want %v — HIGH, then MED in original relative order, "+
				"then LOW, then anything unrecognised", got, want)
		}
	}
}

// TestPriorityRank_Values pins the individual ranks, since the ordering test above could in
// principle pass while two ranks collided.
func TestPriorityRank_Values(t *testing.T) {
	tests := []struct {
		priority model.MonitorPriority
		want     int
	}{
		{model.MonitorPriorityHigh, 0},
		{model.MonitorPriorityMed, 1},
		{model.MonitorPriorityLow, 2},
		{model.MonitorPriority("UNKNOWN"), 3},
		{model.MonitorPriority("MEDIUM"), 3}, // the misspelling that caused #3018 is not special
	}
	for _, tc := range tests {
		if got := priorityRank(tc.priority); got != tc.want {
			t.Errorf("priorityRank(%q) = %d, want %d", tc.priority, got, tc.want)
		}
	}
}
