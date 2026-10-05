// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The 7-day report window used throughout: first day 2026-09-29, last day 2026-10-05 (account-
// local days carried as UTC-midnight dates), i.e. [2026-09-29, 2026-10-06) with the exclusive
// midnight after the last day.
var (
	xFirstDay = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	xLastDay  = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
)

func xTestWindow(t *testing.T) xWindow {
	t.Helper()
	w, ok := twitterWindow(&xFirstDay, &xLastDay)
	if !ok {
		t.Fatal("test window rejected")
	}
	return w
}

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
			pct, ok := twitterPacingPct(tc.row, xTestWindow(t))
			if ok != tc.computable || pct != tc.pct {
				t.Errorf("pacing = (%v, %v), want (%v, %v)", pct, ok, tc.pct, tc.computable)
			}
		})
	}
}

// The rules judge the account's own calendar days, carried in the report's window — not a
// "today" from the service's UTC clock. At 17:00 US/Pacific on Oct 5 it is already 00:00 UTC on
// Oct 6, but the stats job the platform submitted then covers the ACCOUNT's days, Sep 29..Oct 5
// (twitter.accountReportWindow). A campaign whose line items start on the next local day
// (StartDate 2026-10-06) has, correctly, no delivery in that report; a UTC-derived window
// [Sep 30, Oct 7) would count it as scheduled and raise a false HIGH every evening.
func TestEvaluateTwitterMonitor_EveningInAccountTimezone(t *testing.T) {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	now := time.Date(2026, 10, 5, 17, 0, 0, 0, pacific)
	if now.UTC().Day() != 6 {
		t.Fatalf("precondition: %v should already be Oct 6 in UTC", now.UTC())
	}
	// The window the platform derives at that instant: the account's local days.
	local := now.In(pacific)
	last := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	first := last.AddDate(0, 0, -6)

	t.Run("flight starting tomorrow local is not zero delivery", func(t *testing.T) {
		_, items := EvaluateTwitterMonitor([]model.AccountCampaignMetrics{xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Impressions, m.Clicks = 0, 0
			m.StartDate = "2026-10-06"
		})}, &first, &last)
		if hasItem(items, model.MonitorPriorityHigh, "delivered nothing") {
			t.Errorf("items = %+v, want no zero-delivery HIGH for a flight that starts after the report's last local day", items)
		}
	})
	t.Run("daily expected spend counts local days", func(t *testing.T) {
		// Flight starts on the report's last local day (Oct 5): exactly one day of budget is
		// expected. A UTC-derived window would have counted Oct 5 and Oct 6 — two days, 50%.
		rows, _ := EvaluateTwitterMonitor([]model.AccountCampaignMetrics{xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Spend = 10
			m.StartDate = "2026-10-05"
		})}, &first, &last)
		if rows[0].PacingPct != 100 || rows[0].Metrics.PacingUnknown {
			t.Errorf("row = %+v, want 100%% of one local day's budget", rows[0])
		}
		// And a whole-window flight expects all seven local days.
		rows, _ = EvaluateTwitterMonitor([]model.AccountCampaignMetrics{xRow(func(m *model.AccountCampaignMetrics) {
			m.BudgetDay = 10
			m.Spend = 70
		})}, &first, &last)
		if rows[0].PacingPct != 100 {
			t.Errorf("row = %+v, want 100%% of seven local days", rows[0])
		}
	})
}

// Without a window the window-dependent judgements are skipped rather than guessed: pacing is
// unknown and the zero-delivery rule stays silent, while the window-free rules still run.
func TestEvaluateTwitterMonitor_NoWindowSkipsWindowRules(t *testing.T) {
	inverted := xFirstDay.AddDate(0, 0, -1)
	for name, bounds := range map[string][2]*time.Time{
		"nil":      {nil, nil},
		"no end":   {&xFirstDay, nil},
		"inverted": {&xFirstDay, &inverted},
	} {
		t.Run(name, func(t *testing.T) {
			rows, items := EvaluateTwitterMonitor([]model.AccountCampaignMetrics{
				xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 10; m.Impressions, m.Clicks = 0, 0 }),
				xRow(func(m *model.AccountCampaignMetrics) {
					m.PlatformCampaignID = "c2"
					m.Status = "PAUSED"
					m.BudgetDay = 10
				}),
			}, bounds[0], bounds[1])
			for _, r := range rows {
				if !r.Metrics.PacingUnknown {
					t.Errorf("row %s paced without a window: %+v", r.Metrics.PlatformCampaignID, r)
				}
			}
			if hasItem(items, model.MonitorPriorityHigh, "delivered nothing") {
				t.Errorf("items = %+v, want no zero-delivery item without a window", items)
			}
			if !hasItem(items, model.MonitorPriorityMed, "paused") {
				t.Errorf("items = %+v, want the window-free paused item", items)
			}
		})
	}
}

func xItems(t *testing.T, rows ...model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	t.Helper()
	return EvaluateTwitterMonitor(rows, &xFirstDay, &xLastDay)
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
