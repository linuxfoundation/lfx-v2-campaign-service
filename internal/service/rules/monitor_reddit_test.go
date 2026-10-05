// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestRedditPacingPct_ScheduleBranch pins the LIFETIME_SPEND branch of redditPacingPct: a
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

// TestRedditPacingPct_DailyBudget pins the DAILY_SPEND branch: expected spend is BudgetDay ×
// the days of the report window the campaign was scheduled for, the window being
// [today-(days-1), now] clipped to the flight (end date inclusive). It used to be absent — the
// BFF hardcoded dailyBudget to 0 and paced every goal_value as a lifetime total, so a daily
// cap was prorated across the whole flight and a campaign spending exactly its cap read as
// heavily overspending.
func TestRedditPacingPct_DailyBudget(t *testing.T) {
	// Mid-day, so the partial final day counts as a whole day — matching Google/Meta's
	// BudgetDay × days and the report window, and one day more than LinkedIn's instant-anchored
	// range (see redditPacingPct).
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		m              model.AccountCampaignMetrics
		days           int
		wantPct        float64
		wantComputable bool
	}{
		{
			// Window 06-09 00:00 .. 06-15 12:00 = 6.5 days -> 7; expected 70; 63/70 = 90%.
			name: "no flight paces against the whole window",
			m:    model.AccountCampaignMetrics{BudgetDay: 10, Spend: 63},
			days: 7, wantPct: 90, wantComputable: true,
		},
		{
			// Flight starts 06-13: 06-13 00:00 .. 06-15 12:00 = 2.5 days -> 3; expected 30.
			name: "flight starting inside the window clips its start",
			m:    model.AccountCampaignMetrics{BudgetDay: 10, Spend: 30, StartDate: "2026-06-13"},
			days: 7, wantPct: 100, wantComputable: true,
		},
		{
			// Flight ends 06-10 (inclusive -> 06-11 00:00): 06-09 .. 06-11 = 2 days; expected 20.
			name: "flight ending inside the window clips its end, end day inclusive",
			m:    model.AccountCampaignMetrics{BudgetDay: 10, Spend: 10, StartDate: "2026-05-01", EndDate: "2026-06-10"},
			days: 7, wantPct: 50, wantComputable: true,
		},
		{
			name: "flight entirely before the window has nothing to pace",
			m:    model.AccountCampaignMetrics{BudgetDay: 10, Spend: 0, StartDate: "2026-05-01", EndDate: "2026-06-01"},
			days: 7, wantPct: 0, wantComputable: false,
		},
		{
			name: "flight starting after now has nothing to pace",
			m:    model.AccountCampaignMetrics{BudgetDay: 10, Spend: 0, StartDate: "2026-06-20"},
			days: 7, wantPct: 0, wantComputable: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pct, computable := redditPacingPct(tt.m, tt.days, now)
			if pct != tt.wantPct || computable != tt.wantComputable {
				t.Errorf("pct, computable = %v, %v; want %v, %v", pct, computable, tt.wantPct, tt.wantComputable)
			}
		})
	}
}

// TestEvaluateRedditMonitor_DailyBudgetAtItsCapIsNotOverspending pins the user-visible half of
// the goal_type fix: a DAILY_SPEND campaign spending its daily cap every day of the window is on
// plan, where pacing the same goal_value as a lifetime total over a 30-day flight called it
// overspending several times over.
func TestEvaluateRedditMonitor_DailyBudgetAtItsCapIsNotOverspending(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	rows := []model.AccountCampaignMetrics{{
		PlatformCampaignID: "1", Name: "daily", Status: "ACTIVE",
		BudgetDay: 10, StartDate: "2026-06-01", EndDate: "2026-06-30",
		Spend: 63, Impressions: 5000, Clicks: 50,
	}}
	out, _ := EvaluateRedditMonitor(rows, 7, now)
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if out[0].Metrics.PacingUnknown || out[0].PacingPct != 90 || out[0].PacingLabel != model.MonitorPacingNormal {
		t.Errorf("row = %+v; want pacing 90%% normal against 7 days of a $10/day cap", out[0])
	}
}

// TestRedditUnderspend_NotDuplicatedAtZeroDelivery pins the underspend item's zero-delivery
// guard. An ACTIVE campaign that served nothing paces at 0% and already gets the HIGH
// zero-delivery item; the #3021 fix (underspend keyed off the label) made it fire a second HIGH
// "Underspending at 0%" item for the same condition. The BFF's `pacingPct > 0` guard prevented
// that but also silenced a CPC campaign with impressions, no clicks and so no spend, which the
// zero-delivery item does not cover either — that case must still alert.
func TestRedditUnderspend_NotDuplicatedAtZeroDelivery(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	base := model.AccountCampaignMetrics{
		PlatformCampaignID: "1", Name: "c", Status: "ACTIVE",
		TotalBudget: 100, StartDate: "2026-06-01", EndDate: "2026-06-30",
	}

	t.Run("zero delivery fires one HIGH item, not two", func(t *testing.T) {
		out, items := EvaluateRedditMonitor([]model.AccountCampaignMetrics{base}, 7, now)
		if out[0].PacingPct != 0 || out[0].PacingLabel != model.MonitorPacingUnderspending {
			t.Fatalf("row = %+v; setup expected 0%% underspending", out[0])
		}
		mustContainIssue(t, items, "zero impressions and zero clicks", model.MonitorPriorityHigh)
		for _, it := range items {
			if strings.Contains(it.Issue, "Underspending") {
				t.Errorf("emitted %q beside the zero-delivery item for the same condition", it.Issue)
			}
		}
	})
	t.Run("impressions with no spend still alerts as underspending", func(t *testing.T) {
		m := base
		m.Impressions = 500
		_, items := EvaluateRedditMonitor([]model.AccountCampaignMetrics{m}, 7, now)
		mustContainIssue(t, items, "Underspending at 0%", model.MonitorPriorityHigh)
	})
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
