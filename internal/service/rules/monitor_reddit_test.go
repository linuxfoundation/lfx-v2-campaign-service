// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
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
	pct, computable := redditPacingPct(m, 10, now)
	// totalFlightDays=20, elapsedDays=10, expected = 100/20*10 = 50, spend/expected*100 = 90.
	if pct != 90 || !computable {
		t.Errorf("pct, computable = %v, %v; want 90, true", pct, computable)
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
	pct, computable := redditPacingPct(m, 5, now)
	if pct != 0 || computable {
		t.Errorf("pct, computable = %v, %v; want 0, false — redditPacingPct has no "+
			"BudgetDay*days branch, it is dead code omitted from this port, not merely "+
			"unreached here, and a daily budget alone gives nothing to pace against", pct, computable)
	}
}

// TestRedditUnderspend_AlertMatchesTheLabel is the regression test for
// linuxfoundation/lfx-self-serve#3021. The pacing label's underspending boundary is < 50, but
// the underspend ACTION ITEM used to fire at a separate hardcoded < 40. A campaign pacing at
// 45% was therefore labelled "underspending" on its row and alerted on nowhere — the whole
// 40-49% band showed the problem and withheld the call to action.
//
// The item is now keyed off the label, as it is on the other three platforms, so one boundary
// decides both.
func TestRedditUnderspend_AlertMatchesTheLabel(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	// Pick a flight where TotalBudget/totalFlightDays == 1, so expected == elapsedDays and a
	// 45% pacing reading is just Spend == 45 with a 100-day flight ending at `now`.
	rows := []model.AccountCampaignMetrics{
		{
			PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
			TotalBudget: 100, StartDate: "2026-03-07", EndDate: "2026-06-15", // exactly 100 days, now == end
			Spend: 45,
			// Nonzero impressions/clicks (below every other rule's own floor) so this row
			// exercises ONLY the underspend rule under test, not the separate
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
	mustContainIssue(t, items, "Underspending at 45%", model.MonitorPriorityHigh)
}

// TestEvaluateRedditMonitor_BudgetlessCampaignWithAFlightIsPacingUnknown covers the half of the
// no-budget case Reddit's original guard left open. That guard keyed only off an empty
// StartDate, so a campaign with a perfectly good flight and no TotalBudget still fell through
// to redditPacingPct's 0 and was labelled "underspending" — the same defect the other three
// platforms carried, reached by a different route. Both halves are now one condition.
func TestEvaluateRedditMonitor_BudgetlessCampaignWithAFlightIsPacingUnknown(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{
			PlatformCampaignID: "1", Name: "No Budget", Status: "ACTIVE",
			StartDate: "2026-06-05", EndDate: "2026-06-25", // a real flight
			Spend: 25, Impressions: 900, Clicks: 5,
		},
	}
	out, items := EvaluateRedditMonitor(rows, 30, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if !out[0].Metrics.PacingUnknown {
		t.Errorf("row = %+v, want PacingUnknown=true — a flight is not a budget", out[0])
	}
	if out[0].PacingLabel == model.MonitorPacingUnderspending {
		t.Errorf("label = %q; a campaign with no budget cannot be underspending one", out[0].PacingLabel)
	}
	if out[0].PacingPct != 0 {
		t.Errorf("PacingPct = %v, want 0 (never computed)", out[0].PacingPct)
	}
	for _, it := range items {
		if strings.Contains(it.Issue, "Underspending") {
			t.Errorf("emitted an underspend item for a budget-less campaign: %q", it.Issue)
		}
	}
}

// TestRedditClicksNoConversions_DormantWhileUnmeasured is the rules half of
// linuxfoundation/lfx-self-serve#3020. The dispatcher used to hand every Reddit row a non-nil
// 0 conversions, so this rule fired for every campaign past the 100-click floor and could
// never be satisfied any other way — a real nonzero count could not reach it.
//
// Reddit's Conversions is now nil, so the rule is dormant rather than wrong. It is kept, not
// deleted: the logic is correct as written and lights up on its own the day a real conversions
// read lands, which the second subtest pins by supplying the measurement the dispatcher does
// not yet have.
func TestRedditClicksNoConversions_DormantWhileUnmeasured(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c", Clicks: 101}

	t.Run("unmeasured conversions fire nothing", func(t *testing.T) {
		items := redditActionItems(base, 0, model.MonitorPacingNormal)
		for _, it := range items {
			if strings.Contains(it.Issue, "0 conversions") {
				t.Errorf("fired %q for a row nobody measured conversions on", it.Issue)
			}
		}
	})
	t.Run("a measured zero still fires", func(t *testing.T) {
		m := base
		m.Conversions = floatPtr(0)
		items := redditActionItems(m, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "0 conversions", model.MonitorPriorityMed)
	})
}

// TestRedditActionItems exercises the remaining independent rules.
func TestRedditActionItems(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("active zero impressions and zero clicks is HIGH", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := redditActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "zero impressions and zero clicks", model.MonitorPriorityHigh)
	})
	t.Run("active low CTR above min impressions is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		row.Impressions = 1001
		row.Ctr = 0.1
		items := redditActionItems(row, 50, model.MonitorPacingNormal)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("inactive campaign does not fire the no-delivery rule (status-gated)", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		items := redditActionItems(row, 0, model.MonitorPacingNormal)
		for _, it := range items {
			t.Errorf("expected no items for a PAUSED zero-delivery row, got: %+v", it)
		}
	})
}

// TestEvaluateRedditMonitor_SkipsFetchFailedRows mirrors
// TestEvaluateGoogleMonitor_SkipsFetchFailedRows: a FetchFailed row's zero-value metrics must
// not be run through pacing/action-item evaluation, but the row itself must still be returned.
func TestEvaluateRedditMonitor_SkipsFetchFailedRows(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "Failed Fetch", Status: "ACTIVE", TotalBudget: 500, FetchFailed: true},
	}
	out, items := EvaluateRedditMonitor(rows, 30, now)
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

// TestEvaluateRedditMonitor_EmptyStartDate_SetsPacingUnknown pins the round-23 review fix: a row
// whose StartDate is empty (Reddit reported no parseable start_time — see
// internal/platform/reddit/monitor.go) is a genuinely different case from FetchFailed — the
// metrics are real, only the flight window is unknown — so it must get PacingUnknown=true and the
// placeholder MonitorPacingNormal label rather than falling through to redditPacingPct's
// pacingPct==0 branch, which would mislabel it "underspending" against a fabricated 0% pace.
// Zero-delivery/CTR action items must still fire since they don't depend on the flight window.
func TestEvaluateRedditMonitor_EmptyStartDate_SetsPacingUnknown(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{
			PlatformCampaignID: "1", Name: "No Flight Window", Status: "ACTIVE",
			TotalBudget: 500, StartDate: "", Impressions: 0, Clicks: 0,
		},
	}
	out, items := EvaluateRedditMonitor(rows, 30, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(out), out)
	}
	if !out[0].Metrics.PacingUnknown {
		t.Errorf("row = %+v, want PacingUnknown=true for an empty StartDate", out[0])
	}
	if out[0].Metrics.FetchFailed {
		t.Errorf("row = %+v, want FetchFailed=false — an empty StartDate is not a fetch failure", out[0])
	}
	if out[0].PacingLabel != model.MonitorPacingNormal {
		t.Errorf("PacingLabel = %q, want the documented placeholder %q", out[0].PacingLabel, model.MonitorPacingNormal)
	}
	if out[0].PacingPct != 0 {
		t.Errorf("PacingPct = %v, want 0 (unset) since it was never computed against a flight", out[0].PacingPct)
	}
	// Zero impressions/clicks on an ACTIVE campaign must still fire — it doesn't depend on StartDate.
	if len(items) != 1 {
		t.Fatalf("got %d action items, want 1 (the zero-delivery item): %+v", len(items), items)
	}
	if items[0].Priority != model.MonitorPriorityHigh {
		t.Errorf("action item priority = %q, want %q", items[0].Priority, model.MonitorPriorityHigh)
	}
}
