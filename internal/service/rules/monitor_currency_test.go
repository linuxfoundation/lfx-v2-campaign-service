// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The placeholder-budget and high-CPC rules are sized in the account's currency (#454): a non-USD
// account earns one of these items only where the same budget or CPC in USD would.
func TestMonitorMoneyRules_SizedInAccountCurrency(t *testing.T) {
	type platform struct {
		name     string
		active   string
		evaluate func(rows []model.AccountCampaignMetrics, currency string) []model.AccountMonitorActionItem
	}
	platforms := []platform{
		{"google", "ENABLED", func(rows []model.AccountCampaignMetrics, currency string) []model.AccountMonitorActionItem {
			_, items := EvaluateGoogleMonitor(rows, 7, currency)
			return items
		}},
		{"microsoft", "Active", func(rows []model.AccountCampaignMetrics, currency string) []model.AccountMonitorActionItem {
			_, items := EvaluateMicrosoftMonitor(rows, 7, currency)
			return items
		}},
	}
	has := func(items []model.AccountMonitorActionItem, prefix string) bool {
		for _, it := range items {
			if strings.HasPrefix(it.Issue, prefix) {
				return true
			}
		}
		return false
	}
	const placeholder, highCpc = "Daily budget is ", "Avg CPC is "

	cases := []struct {
		name        string
		currency    string
		budgetDay   float64
		spend       float64 // over 20 clicks
		wantBudget  bool
		wantHighCpc bool
	}{
		// USD keeps today's thresholds: <= 1/day, > 5 per click.
		{"USD placeholder and high CPC", "USD", 1, 20 * 6, true, true},
		{"USD real budget, cheap clicks", "USD", 10, 20 * 4, false, false},
		// JPY: ¥100/day (about $0.7) is a placeholder, ¥1,000/day (about $7) is not; a ¥50 click
		// (about $0.34) is cheap, a ¥1,000 click (about $7) is not.
		{"JPY placeholder", "JPY", 100, 20 * 50, true, false},
		{"JPY real budget", "JPY", 1000, 20 * 50, false, false},
		{"JPY high CPC", "JPY", 5000, 20 * 1000, false, true},
		// KWD: 1/day is about $3, a real budget; 1 per click (about $3) is not high, 2 (about $6.5) is.
		{"KWD 1/day is a real budget", "KWD", 1, 20 * 1, false, false},
		{"KWD high CPC", "KWD", 10, 20 * 2, false, true},
		// A currency that cannot be sized fires neither — except a zero budget, a placeholder in
		// every currency.
		{"unknown currency", "", 1, 20 * 6, false, false},
		{"unlisted currency", "ARS", 1, 20 * 10000, false, false},
		{"zero budget in an unknown currency", "", 0, 20 * 6, true, false},
	}
	for _, p := range platforms {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				one := 1.0
				row := model.AccountCampaignMetrics{
					PlatformCampaignID: "1", Name: "c", Status: p.active, IsSearchChannel: true,
					BudgetDay: tc.budgetDay, Spend: tc.spend, Clicks: 20, Impressions: 400, Ctr: 5,
					Conversions: &one,
				}
				items := p.evaluate([]model.AccountCampaignMetrics{row}, tc.currency)
				if got := has(items, placeholder); got != tc.wantBudget {
					t.Errorf("placeholder-budget item = %v, want %v; items = %+v", got, tc.wantBudget, items)
				}
				if got := has(items, highCpc); got != tc.wantHighCpc {
					t.Errorf("high-CPC item = %v, want %v; items = %+v", got, tc.wantHighCpc, items)
				}
			})
		}
	}
}

// X's placeholder-budget rule is the same rule, sized the same way (#454). X never calls a zero
// daily budget a placeholder: a zero there means the line items carry a total budget instead.
func TestTwitterPlaceholderBudget_SizedInAccountCurrency(t *testing.T) {
	for _, tc := range []struct {
		currency  string
		budgetDay float64
		want      bool
	}{
		{"USD", 1, true},
		{"JPY", 100, true},
		{"JPY", 1000, false},
		{"KWD", 1, false},
		{"", 1, false},
	} {
		_, items := EvaluateTwitterMonitor([]model.AccountCampaignMetrics{
			xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = tc.budgetDay; m.Spend = 7 * tc.budgetDay }),
		}, &xFirstDay, &xLastDay, tc.currency)
		if got := hasItem(items, model.MonitorPriorityHigh, "placeholder"); got != tc.want {
			t.Errorf("%s %v/day: placeholder item = %v, want %v; items = %+v", tc.currency, tc.budgetDay, got, tc.want, items)
		}
	}
}

// The helpers keep each rule's boundary in a non-USD currency: the placeholder budget is
// INCLUSIVE and the high CPC EXCLUSIVE, at the USD value times the currency's units per dollar
// (JPY's 147 keeps both products exact in float64).
func TestMonitorMoneyHelpers_BoundaryInAccountCurrency(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"placeholder at 1 USD in JPY", isPlaceholderBudget(147, "JPY"), true},
		{"placeholder just above 1 USD in JPY", isPlaceholderBudget(148, "JPY"), false},
		{"CPC at 5 USD in JPY", isHighAvgCpc(735, "JPY"), false},
		{"CPC just above 5 USD in JPY", isHighAvgCpc(736, "JPY"), true},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// Every table entry is a well-formed ISO 4217 code with a positive figure: a mistyped key or a zero
// would silently switch both rules off for that currency. The currencies #454 names are present.
func TestMonitorUnitsPerUSD_Integrity(t *testing.T) {
	for code, units := range monitorUnitsPerUSD {
		if model.NormalizeCurrencyCode(code) != code {
			t.Errorf("key %q is not an ISO 4217 code", code)
		}
		if !(units > 0) {
			t.Errorf("%s units per USD = %v, want > 0", code, units)
		}
	}
	for _, code := range []string{"USD", "EUR", "GBP", "JPY", "KWD", "BHD"} {
		if _, ok := monitorUnitsPerUSD[code]; !ok {
			t.Errorf("%s missing from monitorUnitsPerUSD", code)
		}
	}
}

// A currency the money rules cannot be sized in is said so in the response: one account-wide LOW
// item, never a silent gap and never a USD guess (#454 review).
func TestMoneyRulesSkipped_IsVisible(t *testing.T) {
	type platform struct {
		name     string
		active   string
		evaluate func(rows []model.AccountCampaignMetrics, currency string) []model.AccountMonitorActionItem
	}
	platforms := []platform{
		{"google", "ENABLED", func(rows []model.AccountCampaignMetrics, c string) []model.AccountMonitorActionItem {
			_, items := EvaluateGoogleMonitor(rows, 7, c)
			return items
		}},
		{"microsoft", "Active", func(rows []model.AccountCampaignMetrics, c string) []model.AccountMonitorActionItem {
			_, items := EvaluateMicrosoftMonitor(rows, 7, c)
			return items
		}},
		{"twitter", "ACTIVE", func(rows []model.AccountCampaignMetrics, c string) []model.AccountMonitorActionItem {
			_, items := EvaluateTwitterMonitor(rows, &xFirstDay, &xLastDay, c)
			return items
		}},
	}
	skipped := func(items []model.AccountMonitorActionItem) []model.AccountMonitorActionItem {
		var out []model.AccountMonitorActionItem
		for _, it := range items {
			if strings.Contains(it.Issue, "checks were skipped") {
				out = append(out, it)
			}
		}
		return out
	}
	for _, p := range platforms {
		measured := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "c", Status: p.active,
			BudgetDay: 1, Spend: 7, Impressions: 400, Clicks: 20, Ctr: 5}
		failed := measured
		failed.FetchFailed = true
		for _, tc := range []struct {
			name     string
			rows     []model.AccountCampaignMetrics
			currency string
			want     string // substring of the one skipped item, "" for none
		}{
			{"absent currency", []model.AccountCampaignMetrics{measured, measured}, "", "could not be read"},
			{"unlisted currency", []model.AccountCampaignMetrics{measured}, "ARS", "(ARS)"},
			{"sized currency", []model.AccountCampaignMetrics{measured}, "USD", ""},
			{"no measured row", []model.AccountCampaignMetrics{failed}, "", ""},
			{"no rows", nil, "", ""},
		} {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				got := skipped(p.evaluate(tc.rows, tc.currency))
				if tc.want == "" {
					if len(got) != 0 {
						t.Errorf("skipped items = %+v, want none", got)
					}
					return
				}
				if len(got) != 1 || !strings.Contains(got[0].Issue, tc.want) ||
					got[0].Priority != model.MonitorPriorityLow || got[0].CampaignID != "" {
					t.Errorf("skipped items = %+v, want one account-wide LOW item naming %q", got, tc.want)
				}
			})
		}
	}
}
