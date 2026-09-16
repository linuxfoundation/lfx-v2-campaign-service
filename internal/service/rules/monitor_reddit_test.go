// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestRedditPacingPct_ScheduleBranch pins the only implemented branch of redditPacingPct: a
// TotalBudget/StartDate schedule prorated across the flight, capped at the flight length.
func TestRedditPacingPct_ScheduleBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{
		TotalBudget: 100,
		StartDate:   "2026-06-05", // 10 days elapsed
		EndDate:     "2026-06-25", // 20-day flight
		Spend:       45,
	}
	pct := redditPacingPct(m, 10, now)
	// totalFlightDays=20, elapsedDays=10, expected = 100/20*10 = 50, spend/expected*100 = 90.
	if pct != 90 {
		t.Errorf("pct = %v, want 90", pct)
	}
}

// TestRedditPacingPct_NoDailyBudgetBranch pins that redditPacingPct has NO BudgetDay*days
// fallback at all: Reddit campaigns always carry BudgetDay==0 upstream (the plan's own note
// that dailyBudget is hardcoded to 0), and the ported function returns 0 rather than
// evaluating a dead branch, matching reddit-ads.service.ts exactly. A row with BudgetDay set
// but no TotalBudget/StartDate schedule must still pace at 0, not at spend/(BudgetDay*days).
func TestRedditPacingPct_NoDailyBudgetBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{BudgetDay: 10, Spend: 45} // no TotalBudget/StartDate
	pct := redditPacingPct(m, 5, now)
	if pct != 0 {
		t.Errorf("pct = %v, want 0 — redditPacingPct has no BudgetDay*days branch, "+
			"it is dead code omitted from this port, not merely unreached here", pct)
	}
}

// TestRedditUnderspendGap_LabelVsActionItemMismatch is the migration spec's documented bug
// (b): the pacingLabel's underspending boundary is <50, but the underspend ACTION ITEM only
// fires below 40 — a genuinely different number. A campaign paced at 45% must be labeled
// "underspending" yet get NO underspend action item, because 45 is not < 40.
//
// follow-up: do not unify these two thresholds to close the 40-49% gap here — that fix is
// tracked separately, and normalizing this test would defeat the differential verification
// this migration depends on. See redditUnderspendActionFloor's doc comment in monitor_reddit.go.
func TestRedditUnderspendGap_LabelVsActionItemMismatch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	// Pick a flight where TotalBudget/totalFlightDays == 1, so expected == elapsedDays and a
	// 45% pacing reading is just Spend == 45 with a 100-day (or longer) flight ending at `now`.
	rows := []model.AccountCampaignMetrics{
		{
			PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
			TotalBudget: 100, StartDate: "2026-03-07", EndDate: "2026-06-15", // exactly 100 days, now == end
			Spend: 45,
			// Nonzero impressions/clicks (below every other rule's own floor) so this row
			// exercises ONLY the underspend-gap rule under test, not the separate
			// zero-impressions/zero-clicks no-delivery HIGH rule.
			Impressions: 500,
			Clicks:      10,
		},
	}
	out, items := EvaluateRedditMonitor(rows, 90, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if out[0].PacingPct != 45 {
		t.Fatalf("pacingPct = %v, want 45 (test setup didn't land on the intended boundary)", out[0].PacingPct)
	}
	if out[0].PacingLabel != model.MonitorPacingUnderspending {
		t.Errorf("label = %q, want underspending — the label's own boundary is <50", out[0].PacingLabel)
	}
	for _, it := range items {
		if it.Priority == model.MonitorPriorityHigh {
			t.Errorf("an underspend action item fired at 45%% pacing, want none: %+v — "+
				"the action item's floor is <40, a different number from the label's <50", it)
		}
	}
}

// TestRedditConversionsHardcodedZero_ClicksNoConversionsAlwaysFires pins the KNOWN BUG:
// Conversions is hardcoded to a non-nil 0 for every Reddit row upstream, so the "clicks
// without conversions" rule fires unconditionally once Clicks crosses the 100-click floor —
// it can never be satisfied any other way, because a real nonzero conversion count can never
// reach this code path.
//
// follow-up: once Reddit's dispatcher populates real Conversions data, revisit whether this
// rule should require Conversions == nil (unmeasured) rather than compare against a literal 0
// that source never varies today.
func TestRedditConversionsHardcodedZero_ClicksNoConversionsAlwaysFires(t *testing.T) {
	m := model.AccountCampaignMetrics{
		PlatformCampaignID: "c1", Name: "c",
		Clicks: 101, Conversions: floatPtr(0),
	}
	items := redditActionItems(m, 0)
	mustContainIssue(t, items, "0 conversions", model.MonitorPriorityMed)
}

// TestRedditActionItems exercises the remaining independent rules.
func TestRedditActionItems(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("active zero impressions and zero clicks is HIGH", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := redditActionItems(row, 0)
		mustContainIssue(t, items, "zero impressions and zero clicks", model.MonitorPriorityHigh)
	})
	t.Run("active low CTR above min impressions is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		row.Impressions = 1001
		row.Ctr = 0.1
		items := redditActionItems(row, 50)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("inactive campaign does not fire the no-delivery rule (status-gated)", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		items := redditActionItems(row, 0)
		for _, it := range items {
			t.Errorf("expected no items for a PAUSED zero-delivery row, got: %+v", it)
		}
	})
}

// TestRedditPriorityRank_IsNotBuggy pins that Reddit's rank function, unlike LinkedIn's,
// correctly maps every priority, including MED, to its documented slot.
func TestRedditPriorityRank_IsNotBuggy(t *testing.T) {
	items := []model.AccountMonitorActionItem{
		{CampaignID: "a", Priority: model.MonitorPriorityLow},
		{CampaignID: "b", Priority: model.MonitorPriorityMed},
		{CampaignID: "c", Priority: model.MonitorPriorityHigh},
	}
	sortByPriority(items, redditPriorityRank)
	want := []string{"c", "b", "a"}
	got := itemIDs(items)
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("sorted order = %v, want HIGH, MED, LOW", got)
		}
	}
}
