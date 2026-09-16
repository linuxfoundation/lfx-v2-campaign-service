// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestEvaluateGoogleMonitor_FiltersZZPrefixedCampaigns pins the KNOWN BUG ported verbatim
// from getMonitorData's `.filter((c) => !c.name.toLowerCase().startsWith('zz'))`: ANY
// campaign whose name starts with "zz" (case-insensitive) is silently dropped from both the
// row list and the action-item pass, not just an intentional test/scratch campaign. A real
// operator-named campaign that happens to start with "ZZ" (e.g. an archival naming
// convention) is invisible to this endpoint exactly the same way.
//
// follow-up: unify this with the shared rules package and reconsider the zz-prefix filter
// (see monitor_google.go's package doc comment, follow-up ticket #7 / lfx-self-serve#2519).
func TestEvaluateGoogleMonitor_FiltersZZPrefixedCampaigns(t *testing.T) {
	rows := []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "KubeCon NA 2026", Status: "enabled", BudgetDay: 50, Spend: 50},
		{PlatformCampaignID: "2", Name: "zz-test-scratch", Status: "enabled", BudgetDay: 50, Spend: 50},
		{PlatformCampaignID: "3", Name: "ZZ-archive-test", Status: "enabled", BudgetDay: 50, Spend: 50},
		{PlatformCampaignID: "4", Name: "Zz-Mixed-Case", Status: "enabled", BudgetDay: 50, Spend: 50},
	}
	out, _ := EvaluateGoogleMonitor(rows, 1)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1 — every zz-prefixed campaign (any case) must be dropped, "+
			"including a legitimately-named archival campaign: %+v", len(out), out)
	}
	if out[0].Metrics.PlatformCampaignID != "1" {
		t.Errorf("surviving row = %q, want the one non-zz campaign", out[0].Metrics.PlatformCampaignID)
	}
}

// TestGooglePacingBoundaries pins the three LOCAL literals (50/90/100) at their exact
// boundaries. These are campaign-metrics.service.ts's own literals, NOT this package's shared
// Thresholds — see monitor_google.go's const doc comment — so this test exists specifically to
// catch a future edit that (accidentally or "helpfully") repoints these at the shared
// constants.
func TestGooglePacingBoundaries(t *testing.T) {
	// budgetDay=1, days=1 => expectedSpend=1, so spend == pacingPct/100 directly.
	tests := []struct {
		name      string
		spend     float64
		wantPct   float64
		wantLabel model.MonitorPacingLabel
	}{
		{"just below underspending floor (49%)", 0.49, 49, model.MonitorPacingUnderspending},
		{"exactly on underspending floor (50%) is normal, not underspending", 0.50, 50, model.MonitorPacingNormal},
		{"exactly on overspend-from boundary (90%) is still normal", 0.90, 90, model.MonitorPacingNormal},
		{"just above overspend-from boundary (91%) is constrained", 0.91, 91, model.MonitorPacingConstrained},
		{"exactly on constrained ceiling (100%) is still constrained, not overspending", 1.00, 100, model.MonitorPacingConstrained},
		{"just above constrained ceiling (101%) is overspending", 1.01, 101, model.MonitorPacingOverspending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := []model.AccountCampaignMetrics{
				{PlatformCampaignID: "1", Name: "c", Status: "enabled", BudgetDay: 1, Spend: tc.spend},
			}
			out, _ := EvaluateGoogleMonitor(rows, 1)
			if len(out) != 1 {
				t.Fatalf("got %d rows, want 1", len(out))
			}
			if out[0].PacingPct != tc.wantPct {
				t.Errorf("pacingPct = %v, want %v", out[0].PacingPct, tc.wantPct)
			}
			if out[0].PacingLabel != tc.wantLabel {
				t.Errorf("label = %q, want %q — boundary comparisons in EvaluateGoogleMonitor are strict "+
					"(< and >, never <= or >=), so the label must not flip until the value is strictly past it",
					out[0].PacingLabel, tc.wantLabel)
			}
		})
	}
}

// TestGoogleActionItems exercises each of googleActionItems' independent rules in isolation,
// with every other field held at a value that does not also trigger a different rule, so a
// single fired item can be attributed to the rule under test.
func TestGoogleActionItems(t *testing.T) {
	tests := []struct {
		name         string
		row          model.AccountCampaignMetrics
		wantIssue    string
		wantPriority model.MonitorPriority
	}{
		{
			name:         "limited status is HIGH",
			row:          model.AccountCampaignMetrics{Status: "LIMITED", BudgetDay: 50, Impressions: 10},
			wantIssue:    "Campaign limited by Google",
			wantPriority: model.MonitorPriorityHigh,
		},
		{
			name:         "budgetDay<=1 and enabled is HIGH placeholder budget",
			row:          model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 1},
			wantIssue:    "this is a placeholder",
			wantPriority: model.MonitorPriorityHigh,
		},
		{
			name:         "paused is MED",
			row:          model.AccountCampaignMetrics{Status: "PAUSED", BudgetDay: 50, Spend: 12.34},
			wantIssue:    "Campaign is paused",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name: "search CTR below 2% with clicks>10 is MED",
			row: model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 50, Spend: 50,
				IsSearchChannel: true, Ctr: 1.0, Clicks: 11, Impressions: 1100},
			wantIssue:    "Search CTR is",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name: "display CTR below 0.3% with impressions>1000 is MED",
			row: model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 50, Spend: 50,
				IsSearchChannel: false, Ctr: 0.1, Impressions: 1001},
			wantIssue:    "Display CTR is",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name: "clicks>20 with 0 conversions is MED",
			row: model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 50, Spend: 50,
				Clicks: 21, Conversions: floatPtr(0)},
			wantIssue:    "0 conversions",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name: "avgCpc>5 with clicks>10 is MED",
			row: model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 50, Spend: 60,
				Clicks: 11},
			wantIssue:    "Avg CPC is",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name: "impressions>0 with 0 clicks is MED",
			row: model.AccountCampaignMetrics{Status: "ENABLED", BudgetDay: 50, Spend: 50,
				Impressions: 500, Clicks: 0},
			wantIssue:    "0 clicks",
			wantPriority: model.MonitorPriorityMed,
		},
		{
			name:         "draft is MED",
			row:          model.AccountCampaignMetrics{Status: "DRAFT", BudgetDay: 50},
			wantIssue:    "still in draft",
			wantPriority: model.MonitorPriorityMed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.PlatformCampaignID = "c1"
			tc.row.Name = "Campaign"
			items := googleActionItems(tc.row, 0, model.MonitorPacingNormal, 1)
			found := false
			for _, it := range items {
				if strings.Contains(it.Issue, tc.wantIssue) {
					found = true
					if it.Priority != tc.wantPriority {
						t.Errorf("priority = %q, want %q for issue %q", it.Priority, tc.wantPriority, it.Issue)
					}
				}
			}
			if !found {
				t.Errorf("no action item contained %q among %+v", tc.wantIssue, items)
			}
		})
	}
}

// TestGoogleActionItems_UnderspendingAndConstrainedRequireEnabled pins that the
// underspending/constrained MED rules are gated on status=="enabled" in addition to the
// pacing label, matching campaign-metrics.service.ts's own `status === 'enabled'` guard on
// those two branches (unlike paused/draft, which are status-only).
func TestGoogleActionItems_UnderspendingAndConstrainedRequireEnabled(t *testing.T) {
	row := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "c", Status: "PAUSED", BudgetDay: 50, Spend: 10}
	items := googleActionItems(row, 20, model.MonitorPacingUnderspending, 1)
	for _, it := range items {
		if strings.Contains(it.Issue, "Only spending") {
			t.Fatalf("underspending action item fired for a PAUSED campaign; the BFF gates this rule on status==enabled: %+v", it)
		}
	}
}

// TestGooglePriorityRank_IsNotBuggy pins that Google's rank function (unlike LinkedIn's, see
// monitor_linkedin_test.go) correctly maps every priority, including MED, to its documented
// slot: HIGH:0, MED:1, LOW:2, unknown:3.
func TestGooglePriorityRank_IsNotBuggy(t *testing.T) {
	items := []model.AccountMonitorActionItem{
		{CampaignID: "a", Priority: model.MonitorPriorityLow},
		{CampaignID: "b", Priority: model.MonitorPriorityMed},
		{CampaignID: "c", Priority: model.MonitorPriorityHigh},
	}
	sortByPriority(items, googlePriorityRank)
	want := []string{"c", "b", "a"}
	for i, id := range want {
		if items[i].CampaignID != id {
			t.Fatalf("sorted order = %v, want HIGH, MED, LOW", itemIDs(items))
		}
	}
}

func itemIDs(items []model.AccountMonitorActionItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.CampaignID
	}
	return ids
}

func floatPtr(f float64) *float64 { return &f }
