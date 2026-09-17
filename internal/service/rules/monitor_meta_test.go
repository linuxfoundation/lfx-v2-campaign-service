// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestMetaPacingPct_ScheduleBranch pins the TotalBudget+start-date branch: expected spend is
// prorated by elapsed-vs-total flight days, capped at the flight length.
func TestMetaPacingPct_ScheduleBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{
		TotalBudget: 100,
		StartDate:   "2026-06-05", // 10 days elapsed
		EndDate:     "2026-06-25", // 20-day flight
		Spend:       50,
	}
	pct, unknown := metaPacingPct(m, 10, now)
	// totalFlightDays=20, elapsedDays=10, expected = 100/20*10 = 50, spend/expected*100 = 100.
	if unknown {
		t.Fatalf("unknown = true, want false")
	}
	if pct != 100 {
		t.Errorf("pct = %v, want 100", pct)
	}
}

// TestMetaPacingPct_FlatDailyBudgetBranch pins the BudgetDay*days fallback used when no
// TotalBudget/start-date schedule is present.
func TestMetaPacingPct_FlatDailyBudgetBranch(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{BudgetDay: 10, Spend: 45}
	pct, unknown := metaPacingPct(m, 5, now)
	// expected = 10*5 = 50, spend/expected*100 = 90.
	if unknown {
		t.Fatalf("unknown = true, want false")
	}
	if pct != 90 {
		t.Errorf("pct = %v, want 90", pct)
	}
}

// TestMetaPacingPct_UnknownIsAlwaysFalse observes that metaPacingPct's `unknown` return value
// is hardcoded false on every code path in the current source — including the final
// no-budget-information fallback, which returns (0, false) rather than (0, true). This is
// NOT one of the plan's documented preserved bugs and is not asserted here as a "bug" the way
// LinkedIn's MED/MEDIUM mismatch is; it is flagged in the accompanying report as something
// worth confirming against the BFF source, since a genuinely unmeasurable row is
// indistinguishable from a real 0% pacing reading under the current code.
func TestMetaPacingPct_UnknownIsAlwaysFalse(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	m := model.AccountCampaignMetrics{} // no budget info at all
	pct, unknown := metaPacingPct(m, 10, now)
	if pct != 0 {
		t.Errorf("pct = %v, want 0 for a campaign with no budget information", pct)
	}
	if unknown {
		t.Errorf("unknown = true; metaPacingPct's current source never returns true on any path — " +
			"if this now fails, the vestigial-looking always-false return has changed and the " +
			"report's open item about it should be revisited")
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
		items := metaActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "no delivery", model.MonitorPriorityHigh)
	})
	t.Run("low CTR above min impressions is MED", func(t *testing.T) {
		row := base
		row.Ctr = 0.1
		row.Impressions = 501
		items := metaActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("clicks above floor with 0 conversions is MED", func(t *testing.T) {
		row := base
		row.Clicks = 21
		row.Conversions = floatPtr(0)
		items := metaActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "0 conversions", model.MonitorPriorityMed)
	})
	t.Run("active underspending is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending)
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityMed)
	})
	t.Run("active constrained is MED", func(t *testing.T) {
		row := base
		row.Status = "ACTIVE"
		items := metaActionItems(row, 95, model.MonitorPacingConstrained)
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("paused underspending does not fire (status-gated)", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		items := metaActionItems(row, 30, model.MonitorPacingUnderspending)
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
	if len(items) != 0 {
		t.Errorf("FetchFailed row produced action items, want none: %+v", items)
	}
}
