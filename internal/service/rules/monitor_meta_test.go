// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestMetaPacingPct_ScheduleBranch pins the TotalBudget+start-date branch: the lifetime budget is
// spread evenly over the flight (EndDate inclusive) and the expectation covers only the days of
// the requested window the flight ran on, because m.Spend covers only that window.
func TestMetaPacingPct_ScheduleBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{
		TotalBudget: 200,
		StartDate:   "2026-06-05",
		EndDate:     "2026-06-24", // 20-day flight, 06-05..06-24 inclusive
		Spend:       100,
	}
	pct, computable := metaPacingPct(m, 10, now)
	// window 06-06..06-15 = 10 flight days; expected = 200/20*10 = 100; spend 100 → 100%.
	if !computable {
		t.Fatalf("computable = false, want true")
	}
	if pct != 100 {
		t.Errorf("pct = %v, want 100", pct)
	}
}

// TestMetaPacingPct_LifetimePacesTheWindowNotTheFlightToDate pins the PR #316 review fix: spend
// covers only the requested window, so the expectation must too. A 9,000 lifetime budget over a
// 90-day flight, read on day 60 with days=7, spending exactly 100/day, is on plan: 700 of 700.
// Expecting the flight-to-date plan (~5,900) instead reported ~12% — a false underspend.
func TestMetaPacingPct_LifetimePacesTheWindowNotTheFlightToDate(t *testing.T) {
	m := model.AccountCampaignMetrics{
		TotalBudget: 9000,
		StartDate:   "2026-06-01",
		EndDate:     "2026-08-29", // 90 days inclusive
		Spend:       700,
	}
	now := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC) // flight day 60
	pct, computable := metaPacingPct(m, 7, now)
	if !computable || pct != 100 {
		t.Fatalf("pct = %v (computable %v), want 100: 700 spent against 9000/90*7 = 700 expected", pct, computable)
	}
	rows, items := EvaluateMetaMonitor([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 10000, Clicks: 100, TotalBudget: 9000, StartDate: "2026-06-01", EndDate: "2026-08-29", Spend: 700}}, 7, now)
	if rows[0].PacingLabel == model.MonitorPacingUnderspending {
		t.Errorf("an on-plan lifetime campaign is labelled underspending: %+v", rows[0])
	}
	for _, it := range items {
		if strContains(it.Issue, "Underspending") {
			t.Errorf("unexpected underspending item: %q", it.Issue)
		}
	}

	// Half the plan in the window is 50%, and the text names the window's plan, not the whole
	// lifetime budget.
	m.Spend = 350
	if pct, _ := metaPacingPct(m, 7, now); pct != 50 {
		t.Errorf("pct = %v, want 50", pct)
	}
	_, items = EvaluateMetaMonitor([]model.AccountCampaignMetrics{{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		Impressions: 10000, Clicks: 100, TotalBudget: 9000, StartDate: "2026-06-01", EndDate: "2026-08-29", Spend: 300}}, 7, now)
	mustContainIssue(t, items, "(300.00 in account currency of 700.00 in account currency)", model.MonitorPriorityMed)
}

// TestMetaPacingPct_LifetimeWindowEdges: a window that only partly overlaps the flight expects
// only the overlapping days; one that does not overlap it at all is not computable.
func TestMetaPacingPct_LifetimeWindowEdges(t *testing.T) {
	m := model.AccountCampaignMetrics{TotalBudget: 1000, StartDate: "2026-07-27", EndDate: "2026-08-05", Spend: 400} // 10 days
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	// window 07-24..07-30, flight from 07-27: 4 overlapping days → expected 400.
	if pct, ok := metaPacingPct(m, 7, now); !ok || pct != 100 {
		t.Errorf("partial overlap: pct = %v (ok %v), want 100", pct, ok)
	}
	notStarted := m
	notStarted.StartDate, notStarted.EndDate = "2026-08-10", "2026-08-20"
	if _, ok := metaPacingPct(notStarted, 7, now); ok {
		t.Error("a flight that starts after the window was paced; its zero spend is correct, not underspending")
	}
	ended := m
	ended.StartDate, ended.EndDate = "2026-06-01", "2026-07-10"
	if _, ok := metaPacingPct(ended, 7, now); ok {
		t.Error("a flight that ended before the window was paced")
	}
	openEnded := model.AccountCampaignMetrics{TotalBudget: 300, StartDate: "2026-07-01", Spend: 70}
	// open-ended: the flight runs through today, 07-01..07-30 = 30 days; window 7 days → 70.
	if pct, ok := metaPacingPct(openEnded, 7, now); !ok || pct != 100 {
		t.Errorf("open-ended: pct = %v (ok %v), want 100", pct, ok)
	}
}

// TestMetaPacingPct_FlatDailyBudgetBranch pins the BudgetDay*days fallback used when no
// TotalBudget/start-date schedule is present.
func TestMetaPacingPct_FlatDailyBudgetBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{BudgetDay: 10, Spend: 45}
	pct, computable := metaPacingPct(m, 5, now)
	// expected = 10*5 = 50, spend/expected*100 = 90.
	if !computable {
		t.Fatalf("computable = false, want true")
	}
	if pct != 90 {
		t.Errorf("pct = %v, want 90", pct)
	}
}

// TestMetaPacingPct_NoBudgetIsNotComputable pins the fix for the defect an earlier revision of
// this test merely observed: metaPacingPct's second return was hardcoded false on every path,
// including the no-budget fallback, so the caller's guard on it was dead and a genuinely
// unmeasurable row was indistinguishable from a real 0% reading.
//
// The flag now means "computable", and a campaign with no budget information says false.
func TestMetaPacingPct_NoBudgetIsNotComputable(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{} // no budget info at all
	pct, computable := metaPacingPct(m, 10, now)
	if pct != 0 {
		t.Errorf("pct = %v, want 0 for a campaign with no budget information", pct)
	}
	if computable {
		t.Errorf("computable = true for a campaign with no budget; there is nothing to pace against")
	}
}

// TestMetaLabel_OverspendingBandNeverUsesThe130Member pins the same BFF-side oddity
// LinkedIn's port reproduces: Meta's label switch only ever produces "overspending" for
// pacingPct > constrained (100) — the shared CAMPAIGN_PACING_THRESHOLDS' 130 "overspending"
// member is never actually compared against. A pacingPct of 105 must already read as
// overspending, not merely constrained.
func TestMetaLabel_OverspendingBandNeverUsesThe130Member(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "c", BudgetDay: 100, Spend: 105}, // pct = 105
	}
	out, _ := EvaluateMetaMonitor(rows, 1, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if out[0].PacingLabel != model.MonitorPacingOverspending {
		t.Errorf("label = %q, want overspending at 105%% pacing (never compares against the 130 threshold)", out[0].PacingLabel)
	}
}

// TestMetaActionItems exercises metaActionItems' independent rules.
func TestMetaActionItems(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("active with zero impressions and zero spend is HIGH", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "no delivery", model.MonitorPriorityHigh)
	})
	t.Run("low CTR above min impressions is MED", func(t *testing.T) {
		row := base
		row.Ctr = 0.1
		row.Impressions = 501
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("clicks above floor with 0 conversions is MED", func(t *testing.T) {
		row := base
		row.Clicks = 21
		row.Conversions = floatPtr(0)
		items := metaActionItems(row, 0, model.MonitorPacingNormal, 30)
		mustContainIssue(t, items, "0 conversions", model.MonitorPriorityMed)
	})
	t.Run("active underspending is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending, 30)
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityMed)
	})
	t.Run("active underspending funded by BudgetDay uses budgetDay*days as the denominator", func(t *testing.T) {
		// Pins a round-22 review fix: the message used to hardcode m.TotalBudget as the
		// denominator, which is 0 for a campaign funded by BudgetDay instead — producing a
		// self-contradicting "X.XX of 0.00" message.
		row := base
		row.Status = "ACTIVE"
		row.BudgetDay = 10
		row.Spend = 60
		// Through the evaluator, so the denominator is the one pacing itself used.
		_, items := EvaluateMetaMonitor([]model.AccountCampaignMetrics{row}, 30, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityMed)
		found := false
		for _, it := range items {
			if strContains(it.Issue, "Underspending") {
				found = true
				if !strContains(it.Issue, "of 300.00 in account currency") {
					t.Errorf("issue = %q, want it to report 300.00 in account currency (budgetDay=10 * days=30), not 0.00", it.Issue)
				}
			}
		}
		if !found {
			t.Fatalf("no Underspending issue found in %+v", items)
		}
	})
	t.Run("money in action-item text is in account currency, never dollars", func(t *testing.T) {
		// Meta reports spend and budgets in the AD ACCOUNT's currency; a JPY or KRW account's
		// amounts used to be printed with "$" (PR #316 review).
		row := base
		row.Status = "ACTIVE"
		row.BudgetDay = 3000
		row.Spend = 9000
		row.Clicks = 100
		row.Conversions = floatPtr(0)
		items := metaActionItems(row, 10, model.MonitorPacingUnderspending, 90000)
		zero := base
		zero.Status = "ACTIVE"
		zero.Impressions, zero.Spend = 0, 0
		items = append(items, metaActionItems(zero, 0, model.MonitorPacingNormal, 30)...)
		if len(items) < 3 {
			t.Fatalf("want the conversions, underspending and no-delivery items, got %+v", items)
		}
		for _, it := range items {
			if strContains(it.Issue, "$") {
				t.Errorf("issue %q prints a dollar sign for an account-currency amount", it.Issue)
			}
		}
		mustContainIssue(t, items, "9000.00 in account currency spent", model.MonitorPriorityMed)
		mustContainIssue(t, items, "(9000.00 in account currency of 90000.00 in account currency)", model.MonitorPriorityMed)
	})
	t.Run("active constrained is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 95, model.MonitorPacingConstrained, 30)
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("paused underspending does not fire (status-gated)", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending, 30)
		for _, it := range items {
			if it.Issue == "" {
				continue
			}
			if it.Priority == model.MonitorPriorityMed && strContains(it.Issue, "Underspending") {
				t.Fatalf("underspending item fired for a PAUSED campaign, want status==ACTIVE gate: %+v", it)
			}
		}
	})
}

func strContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestEvaluateMetaMonitor_SkipsFetchFailedRows mirrors
// TestEvaluateGoogleMonitor_SkipsFetchFailedRows: a FetchFailed row's zero-value metrics must
// not be run through pacing/action-item evaluation, but the row itself must still be returned.
func TestEvaluateMetaMonitor_SkipsFetchFailedRows(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "Failed Fetch", Status: "ACTIVE", TotalBudget: 500, FetchFailed: true},
	}
	out, items := EvaluateMetaMonitor(rows, 30, now)
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
