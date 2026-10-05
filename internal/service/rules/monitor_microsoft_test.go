// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// msBaseRow fires no rule at all: active, a real daily budget, 80% paced over a 1-day window,
// no impressions or clicks. Each case below changes only what its rule reads.
func msBaseRow() model.AccountCampaignMetrics {
	return model.AccountCampaignMetrics{
		PlatformCampaignID: "c1", Name: "Campaign", Status: "Active", BudgetDay: 50, Spend: 40,
	}
}

func msFind(items []model.AccountMonitorActionItem, substr string) *model.AccountMonitorActionItem {
	for i := range items {
		if strings.Contains(items[i].Issue, substr) {
			return &items[i]
		}
	}
	return nil
}

func TestMicrosoftBaseRowFiresNothing(t *testing.T) {
	out, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{msBaseRow()}, 1)
	if len(items) != 0 {
		t.Fatalf("base row fired %+v, want nothing — every per-rule case relies on it", items)
	}
	if out[0].PacingPct != 80 || out[0].PacingLabel != model.MonitorPacingNormal || out[0].Metrics.PacingUnknown {
		t.Errorf("base row = %+v, want 80%% normal with known pacing", out[0])
	}
}

// TestMicrosoftActionItems pins every rule, each on both sides of its boundary. Strict (>, <)
// boundaries are exclusive; the placeholder budget's <= 1 is inclusive.
func TestMicrosoftActionItems(t *testing.T) {
	high, med := model.MonitorPriorityHigh, model.MonitorPriorityMed
	tests := []struct {
		name     string
		mutate   func(*model.AccountCampaignMetrics)
		issue    string
		fires    bool
		priority model.MonitorPriority
	}{
		// Suspended
		{"Suspended is HIGH", func(m *model.AccountCampaignMetrics) { m.Status = "Suspended" }, "suspended by Microsoft", true, high},
		{"status match is case-insensitive", func(m *model.AccountCampaignMetrics) { m.Status = "SUSPENDED" }, "suspended by Microsoft", true, high},
		{"Active is not suspended", func(m *model.AccountCampaignMetrics) {}, "suspended by Microsoft", false, high},

		// Budget exhausted
		{"BudgetPaused is HIGH", func(m *model.AccountCampaignMetrics) { m.Status = "BudgetPaused" }, "budget exhausted", true, high},
		{"BudgetAndManualPaused is HIGH", func(m *model.AccountCampaignMetrics) { m.Status = "BudgetAndManualPaused" }, "also paused manually", true, high},
		{"BudgetPaused is not the manual-pause MED", func(m *model.AccountCampaignMetrics) { m.Status = "BudgetPaused" }, "Campaign is paused", false, med},
		{"BudgetAndManualPaused is not the manual-pause MED", func(m *model.AccountCampaignMetrics) { m.Status = "BudgetAndManualPaused" }, "Campaign is paused", false, med},
		{"manual Paused is not budget-exhausted", func(m *model.AccountCampaignMetrics) { m.Status = "Paused" }, "budget exhausted", false, high},

		// Placeholder budget (<= 1, inclusive)
		{"budget exactly 1 is a placeholder", func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1 }, "placeholder", true, high},
		{"budget 1.01 is not a placeholder", func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1.01 }, "placeholder", false, high},
		{"budget 0 non-shared is a placeholder", func(m *model.AccountCampaignMetrics) { m.BudgetDay = 0 }, "placeholder", true, high},
		{"placeholder requires Active", func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1; m.Status = "Paused" }, "placeholder", false, high},
		{"shared budget is never a placeholder", func(m *model.AccountCampaignMetrics) { m.BudgetDay = 0; m.PacingUnknown = true }, "placeholder", false, high},

		// Paused
		{"Paused is MED with spend", func(m *model.AccountCampaignMetrics) { m.Status = "Paused"; m.Spend = 12.34 }, "Campaign is paused — spent $12.34", true, med},

		// Pacing (BudgetDay 50, days 1: Spend == 2*pct/100*50)
		{"49% underspending fires", func(m *model.AccountCampaignMetrics) { m.Spend = 24.5 }, "Only spending 49%", true, med},
		{"50% is not underspending", func(m *model.AccountCampaignMetrics) { m.Spend = 25 }, "Only spending", false, med},
		{"underspending requires Active", func(m *model.AccountCampaignMetrics) { m.Spend = 10; m.Status = "Paused" }, "Only spending", false, med},
		{"91% constrained fires", func(m *model.AccountCampaignMetrics) { m.Spend = 45.5 }, "demand exceeds", true, med},
		{"90% is not constrained", func(m *model.AccountCampaignMetrics) { m.Spend = 45 }, "demand exceeds", false, med},
		{"constrained requires Active", func(m *model.AccountCampaignMetrics) { m.Spend = 48; m.Status = "Paused" }, "demand exceeds", false, med},
		{"101% overspending fires and names 2x", func(m *model.AccountCampaignMetrics) { m.Spend = 50.5 }, "up to 2x the daily budget", true, med},
		{"100% is constrained, not overspending", func(m *model.AccountCampaignMetrics) { m.Spend = 50 }, "up to 2x", false, med},
		{"overspending requires Active", func(m *model.AccountCampaignMetrics) { m.Spend = 80; m.Status = "Paused" }, "up to 2x", false, med},

		// Search CTR (< 2, clicks > 10)
		{"search CTR 1.99 with 11 clicks fires", func(m *model.AccountCampaignMetrics) {
			m.IsSearchChannel, m.Ctr, m.Clicks, m.Impressions = true, 1.99, 11, 553
		}, "Search CTR is", true, med},
		{"search CTR exactly 2 does not fire", func(m *model.AccountCampaignMetrics) {
			m.IsSearchChannel, m.Ctr, m.Clicks, m.Impressions = true, 2, 11, 550
		}, "Search CTR is", false, med},
		{"search CTR with exactly 10 clicks does not fire", func(m *model.AccountCampaignMetrics) {
			m.IsSearchChannel, m.Ctr, m.Clicks, m.Impressions = true, 1, 10, 1000
		}, "Search CTR is", false, med},
		{"search CTR rule ignores non-search", func(m *model.AccountCampaignMetrics) {
			m.Ctr, m.Clicks, m.Impressions = 1, 11, 1100
		}, "Search CTR is", false, med},

		// Non-search CTR (< 0.3, impressions > 1000)
		{"non-search CTR 0.29 with 1001 impressions fires", func(m *model.AccountCampaignMetrics) {
			m.Ctr, m.Impressions, m.Clicks = 0.29, 1001, 3
		}, "Non-search CTR is", true, med},
		{"non-search CTR exactly 0.3 does not fire", func(m *model.AccountCampaignMetrics) {
			m.Ctr, m.Impressions, m.Clicks = 0.3, 2000, 6
		}, "Non-search CTR is", false, med},
		{"non-search CTR with exactly 1000 impressions does not fire", func(m *model.AccountCampaignMetrics) {
			m.Ctr, m.Impressions, m.Clicks = 0.1, 1000, 1
		}, "Non-search CTR is", false, med},
		{"non-search CTR rule ignores search", func(m *model.AccountCampaignMetrics) {
			m.IsSearchChannel, m.Ctr, m.Impressions, m.Clicks = true, 0.1, 5000, 5
		}, "Non-search CTR is", false, med},

		// Clicks without conversions (> 20, measured 0)
		{"21 clicks with measured 0 conversions fires", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Conversions = 21, 500, floatPtr(0)
		}, "0 conversions", true, med},
		{"exactly 20 clicks does not fire", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Conversions = 20, 500, floatPtr(0)
		}, "0 conversions", false, med},
		{"unmeasured conversions (nil) do not fire", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions = 100, 500
		}, "0 conversions", false, med},
		{"a fractional conversion is not zero", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Conversions = 100, 500, floatPtr(0.5)
		}, "0 conversions", false, med},

		// Avg CPC (> 5, clicks > 10)
		{"avg CPC 5.09 with 11 clicks fires", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Spend = 11, 500, 56
		}, "Avg CPC is $5.09", true, med},
		{"avg CPC exactly 5 does not fire", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Spend = 11, 500, 55
		}, "Avg CPC is", false, med},
		{"avg CPC with exactly 10 clicks does not fire", func(m *model.AccountCampaignMetrics) {
			m.Clicks, m.Impressions, m.Spend = 10, 500, 60
		}, "Avg CPC is", false, med},

		// Impressions without clicks
		{"1 impression and 0 clicks fires", func(m *model.AccountCampaignMetrics) { m.Impressions = 1 }, "0 clicks", true, med},
		{"0 impressions does not fire", func(m *model.AccountCampaignMetrics) {}, "0 clicks", false, med},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := msBaseRow()
			tc.mutate(&row)
			_, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{row}, 1)
			got := msFind(items, tc.issue)
			if !tc.fires {
				if got != nil {
					t.Errorf("rule fired, want it not to: %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("no item containing %q among %+v", tc.issue, items)
			}
			if got.Priority != tc.priority {
				t.Errorf("priority = %q, want %q", got.Priority, tc.priority)
			}
			if got.CampaignID != "c1" || got.CampaignName != "Campaign" {
				t.Errorf("item = %+v, want it attributed to campaign c1", *got)
			}
		})
	}
}

// TestEvaluateMicrosoftMonitor_FetchFailedRow pins that an untrusted row is returned through
// fetchFailedRow with no action items — even one whose fields would otherwise fire HIGH rules.
func TestEvaluateMicrosoftMonitor_FetchFailedRow(t *testing.T) {
	out, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "Failed", Status: "Suspended", Impressions: 10, FetchFailed: true},
	}, 7)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1 — a FetchFailed row is still returned", len(out))
	}
	r := out[0]
	if !r.Metrics.FetchFailed || !r.Metrics.PacingUnknown || r.PacingPct != 0 || r.PacingLabel != model.MonitorPacingNormal {
		t.Errorf("row = %+v, want fetchFailedRow's shape: FetchFailed, PacingUnknown, 0%%, normal placeholder", r)
	}
	if len(items) != 0 {
		t.Errorf("FetchFailed row produced items %+v, want none", items)
	}
}

// TestEvaluateMicrosoftMonitor_SharedBudget pins the shared-budget contract: the dispatcher sets
// PacingUnknown because this campaign's share of the pool is unknowable. The row must not be
// paced, and no budget-amount rule may fire — but budget-independent rules still do.
func TestEvaluateMicrosoftMonitor_SharedBudget(t *testing.T) {
	for _, budget := range []float64{0, 50} {
		row := model.AccountCampaignMetrics{
			PlatformCampaignID: "1", Name: "Shared", Status: "Active", BudgetDay: budget,
			PacingUnknown: true, Spend: 500, Impressions: 300,
		}
		out, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{row}, 7)
		if !out[0].Metrics.PacingUnknown || out[0].PacingPct != 0 || out[0].PacingLabel != model.MonitorPacingNormal {
			t.Errorf("budget %v: row = %+v, want unknown pacing", budget, out[0])
		}
		for _, banned := range []string{"placeholder", "Only spending", "demand exceeds", "up to 2x"} {
			if it := msFind(items, banned); it != nil {
				t.Errorf("budget %v: shared-budget row fired a budget rule: %+v", budget, *it)
			}
		}
		if msFind(items, "0 clicks") == nil {
			t.Errorf("budget %v: budget-independent rule did not fire on a shared-budget row; items = %+v", budget, items)
		}
	}
}

// TestEvaluateMicrosoftMonitor_ZeroBudgetNotShared pins the other side: a 0 budget on a
// non-shared active campaign is unknown pacing AND the HIGH placeholder finding, never an
// underspend complaint.
func TestEvaluateMicrosoftMonitor_ZeroBudgetNotShared(t *testing.T) {
	out, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "No Budget", Status: "Active", Spend: 25},
	}, 7)
	if !out[0].Metrics.PacingUnknown || out[0].PacingPct != 0 {
		t.Errorf("row = %+v, want unknown pacing", out[0])
	}
	it := msFind(items, "placeholder")
	if it == nil || it.Priority != model.MonitorPriorityHigh {
		t.Errorf("want a HIGH placeholder item, got %+v", items)
	}
	if msFind(items, "Only spending") != nil {
		t.Errorf("underspend item fired for a budget-less campaign: %+v", items)
	}
}

// TestEvaluateMicrosoftMonitor_ZeroDaysIsUnknown: no window means no expected spend.
func TestEvaluateMicrosoftMonitor_ZeroDaysIsUnknown(t *testing.T) {
	out, _ := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{msBaseRow()}, 0)
	if !out[0].Metrics.PacingUnknown {
		t.Errorf("row = %+v, want unknown pacing for days=0", out[0])
	}
}

// TestEvaluateMicrosoftMonitor_PacingRounding pins Google's daily model: expected = BudgetDay *
// days, and pct is math.Round'd BEFORE it is placed on the ladder.
func TestEvaluateMicrosoftMonitor_PacingRounding(t *testing.T) {
	tests := []struct {
		name      string
		budget    float64
		days      int
		spend     float64
		wantPct   float64
		wantLabel model.MonitorPacingLabel
	}{
		{"49.6 rounds up to 50, normal", 10, 1, 4.96, 50, model.MonitorPacingNormal},
		{"49.4 rounds down to 49, underspending", 10, 1, 4.94, 49, model.MonitorPacingUnderspending},
		{"90.4 rounds to 90, normal", 10, 1, 9.04, 90, model.MonitorPacingNormal},
		{"100.4 rounds to 100, constrained", 10, 1, 10.04, 100, model.MonitorPacingConstrained},
		{"100.6 rounds to 101, overspending", 10, 1, 10.06, 101, model.MonitorPacingOverspending},
		{"expected scales with days", 10, 7, 35, 50, model.MonitorPacingNormal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{
				{PlatformCampaignID: "1", Status: "Active", BudgetDay: tc.budget, Spend: tc.spend},
			}, tc.days)
			if out[0].PacingPct != tc.wantPct || out[0].PacingLabel != tc.wantLabel {
				t.Errorf("got %v%% %q, want %v%% %q", out[0].PacingPct, out[0].PacingLabel, tc.wantPct, tc.wantLabel)
			}
		})
	}
}

// TestEvaluateMicrosoftMonitor_UnderspendTextUsesDays pins the expected-spend figure in the
// item text against the requested window (the bug Google's text once had with a fixed 30).
func TestEvaluateMicrosoftMonitor_UnderspendTextUsesDays(t *testing.T) {
	_, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Status: "Active", BudgetDay: 10, Spend: 20},
	}, 7)
	if it := msFind(items, "Only spending 29%"); it == nil || !strings.Contains(it.Issue, "$70.00 expected") {
		t.Errorf("want an underspend item reading 29%% and $70.00 expected, got %+v", items)
	}
}

// TestEvaluateMicrosoftMonitor_SortsHighBeforeMed pins that items across rows are ordered by the
// shared priorityRank, HIGH first, stable within a band.
func TestEvaluateMicrosoftMonitor_SortsHighBeforeMed(t *testing.T) {
	_, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "paused", Status: "Paused", BudgetDay: 50},
		{PlatformCampaignID: "noclicks", Status: "Active", BudgetDay: 50, Spend: 40, Impressions: 10},
		{PlatformCampaignID: "suspended", Status: "Suspended", BudgetDay: 50},
		{PlatformCampaignID: "budget", Status: "BudgetPaused", BudgetDay: 50},
	}, 1)
	want := []string{"suspended", "budget", "paused", "noclicks"}
	got := itemIDs(items)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A budget-paused campaign on a SHARED budget has no per-campaign amount, so its HIGH item names
// the shared budget rather than reporting "$0.00/day" — an unknown allowance is not a measured
// zero. An individually budgeted campaign keeps its amount in the message.
func TestMicrosoftBudgetPausedSharedBudgetDoesNotClaimZero(t *testing.T) {
	shared := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "s", Status: "BudgetPaused", PacingUnknown: true}
	_, items := EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{shared}, 7)
	if len(items) == 0 || !strings.Contains(items[0].Issue, "shared budget") || strings.Contains(items[0].Issue, "$0.00") {
		t.Errorf("shared-budget items = %+v, want the shared budget named and no $0.00", items)
	}
	own := model.AccountCampaignMetrics{PlatformCampaignID: "2", Name: "o", Status: "BudgetPaused", BudgetDay: 25}
	_, items = EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{own}, 7)
	if len(items) == 0 || !strings.Contains(items[0].Issue, "$25.00/day") {
		t.Errorf("own-budget items = %+v, want the daily amount named", items)
	}
}
