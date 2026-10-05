// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The window for days=7 at this instant is [2026-09-29, 2026-10-06) — today inclusive, closed by
// the exclusive midnight after today.
var xNow = time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)

func xRow(mut func(*model.AccountCampaignMetrics)) model.AccountCampaignMetrics {
	m := model.AccountCampaignMetrics{PlatformCampaignID: "c1", Name: "n", Status: "ACTIVE", Impressions: 500, Clicks: 10}
	mut(&m)
	return m
}

func TestTwitterPacing(t *testing.T) {
	cases := []struct {
		name       string
		row        model.AccountCampaignMetrics
		pct        float64
		computable bool
	}{
		{"daily, whole window", xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10; m.Spend = 70 }), 100, true},
		{"daily, open flight from before the window", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Spend = 35
			m.StartDate = "2026-01-01"
		}), 50, true},
		{"daily, flight starts today: one day", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Spend = 10
			m.StartDate = "2026-10-05"
		}), 100, true},
		{"daily, flight ends on the window's first day (inclusive)", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Spend = 10
			m.StartDate = "2026-09-01"
			m.EndDate = "2026-09-29"
		}), 100, true},
		{"daily, flight ended before the window", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.StartDate = "2026-09-01"
			m.EndDate = "2026-09-28"
		}), 0, false},
		{"daily, flight starts after the window", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.StartDate = "2026-10-06"
		}), 0, false},
		{"daily wins over total", xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.TotalBudget = 100000
			m.Spend = 70
			m.StartDate = "2026-09-01"
			m.EndDate = "2026-10-30"
		}), 100, true},
		// 300 over a 60-day flight is 5/day; 7 window days expect 35.
		{"total, prorated over the flight", xRow(func(m *model.AccountCampaignMetrics) {
			m.TotalBudget = 300
			m.Spend = 35
			m.StartDate = "2026-09-01"
			m.EndDate = "2026-10-30"
		}), 100, true},
		{"total, open-ended flight", xRow(func(m *model.AccountCampaignMetrics) {
			m.TotalBudget = 300
			m.StartDate = "2026-09-01"
		}), 0, false},
		{"total, no line items", xRow(func(m *model.AccountCampaignMetrics) { m.TotalBudget = 300 }), 0, false},
		{"no budget", xRow(func(m *model.AccountCampaignMetrics) { m.Spend = 5 }), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pct, ok := twitterPacingPct(tc.row, 7, xNow)
			if ok != tc.computable || pct != tc.pct {
				t.Errorf("pacing = (%v, %v), want (%v, %v)", pct, ok, tc.pct, tc.computable)
			}
		})
	}
}

// The window ends at the midnight after today, so the answer does not drift across the day:
// identical spend reads the same at 00:00 and at 23:59.
func TestTwitterPacing_StableAcrossTheDay(t *testing.T) {
	row := xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10; m.Spend = 70 })
	for _, now := range []time.Time{
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 5, 23, 59, 0, 0, time.UTC),
	} {
		if pct, ok := twitterPacingPct(row, 7, now); !ok || pct != 100 {
			t.Errorf("at %v pacing = (%v, %v), want (100, true)", now, pct, ok)
		}
	}
}

func xItems(t *testing.T, rows ...model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	t.Helper()
	return EvaluateTwitterMonitor(rows, 7, xNow)
}

func hasItem(items []model.AccountMonitorActionItem, p model.MonitorPriority, substr string) bool {
	for _, it := range items {
		if it.Priority == p && strings.Contains(it.Issue, substr) {
			return true
		}
	}
	return false
}

func TestEvaluateTwitterMonitor_Rules(t *testing.T) {
	t.Run("zero delivery while scheduled is HIGH, and not repeated as underspending", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10; m.Impressions = 0; m.Clicks = 0 }))
		if !hasItem(items, model.MonitorPriorityHigh, "delivered nothing") {
			t.Errorf("items = %+v, want the zero-delivery item", items)
		}
		if hasItem(items, model.MonitorPriorityMed, "Underspending") {
			t.Errorf("items = %+v, the 0%% underspend restates zero delivery", items)
		}
	})
	t.Run("zero delivery outside the flight is not a finding", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Impressions, m.Clicks = 0, 0
			m.StartDate, m.EndDate = "2026-08-01", "2026-08-31"
		}))
		if len(items) != 0 {
			t.Errorf("items = %+v, want none for a campaign not scheduled in the window", items)
		}
	})
	t.Run("delivery without spend still underspends", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10 }))
		if !hasItem(items, model.MonitorPriorityMed, "Underspending at 0%") {
			t.Errorf("items = %+v, want the underspend item", items)
		}
	})
	t.Run("pacing ladder boundaries", func(t *testing.T) {
		for spend, label := range map[float64]model.MonitorPacingLabel{
			34: model.MonitorPacingUnderspending, 35: model.MonitorPacingNormal, 63: model.MonitorPacingNormal,
			64: model.MonitorPacingConstrained, 70: model.MonitorPacingConstrained, 71: model.MonitorPacingOverspending,
		} {
			rows, _ := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10; m.Spend = spend }))
			if rows[0].PacingLabel != label {
				t.Errorf("spend %v of 70: label %q, want %q", spend, rows[0].PacingLabel, label)
			}
		}
	})
	t.Run("placeholder budget is inclusive at 1", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1; m.Spend = 7 }))
		if !hasItem(items, model.MonitorPriorityHigh, "placeholder") {
			t.Errorf("items = %+v, want the placeholder item at 1/day", items)
		}
		_, items = xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1.01; m.Spend = 7 }))
		if hasItem(items, model.MonitorPriorityHigh, "placeholder") {
			t.Errorf("items = %+v, want no placeholder item above 1/day", items)
		}
	})
	t.Run("paused", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.Status = "PAUSED"; m.BudgetDay = 10; m.Spend = 12 }))
		if !hasItem(items, model.MonitorPriorityMed, "paused — spent $12.00") {
			t.Errorf("items = %+v, want the paused item", items)
		}
		if hasItem(items, model.MonitorPriorityMed, "Underspending") {
			t.Errorf("items = %+v, a paused campaign is not underspending", items)
		}
	})
	t.Run("low ctr floor is exclusive", func(t *testing.T) {
		_, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.Impressions = 1001; m.Ctr = 0.29 }))
		if !hasItem(items, model.MonitorPriorityMed, "Low CTR") {
			t.Errorf("items = %+v, want the low-CTR item", items)
		}
		_, items = xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.Impressions = 1000; m.Ctr = 0.29 }))
		if hasItem(items, model.MonitorPriorityMed, "Low CTR") {
			t.Errorf("items = %+v, want none at exactly 1000 impressions", items)
		}
	})
	t.Run("fetch failed rows are listed, unknown, and silent", func(t *testing.T) {
		rows, items := xItems(t, xRow(func(m *model.AccountCampaignMetrics) { m.FetchFailed = true; m.BudgetDay = 1 }))
		if len(rows) != 1 || !rows[0].Metrics.PacingUnknown || len(items) != 0 {
			t.Errorf("rows=%+v items=%+v, want one unknown row and no items", rows, items)
		}
	})
	t.Run("HIGH sorts before MED", func(t *testing.T) {
		_, items := xItems(t,
			xRow(func(m *model.AccountCampaignMetrics) { m.PlatformCampaignID = "a"; m.Status = "PAUSED" }),
			xRow(func(m *model.AccountCampaignMetrics) { m.PlatformCampaignID = "b"; m.Impressions, m.Clicks = 0, 0 }),
		)
		if len(items) < 2 || items[0].Priority != model.MonitorPriorityHigh {
			t.Errorf("items = %+v, want HIGH first", items)
		}
	})
}
