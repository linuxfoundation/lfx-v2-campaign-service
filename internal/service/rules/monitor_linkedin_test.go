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

// TestLinkedinActionItems_UnderspendingAndConstrained exercise the two label-driven rules and
// the CTR/conversions/paused rules in isolation.
func TestLinkedinActionItems_UnderspendingAndConstrained(t *testing.T) {
	base := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c"}

	t.Run("underspending label is HIGH", func(t *testing.T) {
		items := linkedinActionItems(base, 10, model.MonitorPacingUnderspending)
		mustContainIssue(t, items, "Underspending", model.MonitorPriorityHigh)
	})
	t.Run("constrained label is MED", func(t *testing.T) {
		items := linkedinActionItems(base, 95, model.MonitorPacingConstrained)
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("overspending label also fires the constrained MED rule", func(t *testing.T) {
		// linkedinActionItems' constrained rule is `label == constrained || label ==
		// overspending` — both share the same MED item, matching the ported "the 130
		// overspending member is dead" oddity: overspending never gets its own message here.
		items := linkedinActionItems(base, 150, model.MonitorPacingOverspending)
		mustContainIssue(t, items, "Budget constrained", model.MonitorPriorityMed)
	})
	t.Run("low CTR is MED", func(t *testing.T) {
		row := base
		row.Ctr = 0.1
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "Low CTR", model.MonitorPriorityMed)
	})
	t.Run("clicks without conversions is MED", func(t *testing.T) {
		row := base
		row.Clicks = 51
		row.Conversions = floatPtr(0)
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal)
		mustContainIssue(t, items, "Clicks without conversions", model.MonitorPriorityMed)
	})
	t.Run("paused with real budget is LOW", func(t *testing.T) {
		row := base
		row.Status = "PAUSED"
		row.TotalBudget = 20
		items := linkedinActionItems(row, 0, model.MonitorPacingNormal)
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
	out, items := EvaluateLinkedInMonitor(rows, 30, now)
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
