// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
	"time"

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
		// 5 × 1.38 is 6.8999… in float64: CAD 6.90 is the threshold itself, not above it.
		{"CPC at 5 USD in CAD", isHighAvgCpc(6.90, "CAD"), false},
		{"CPC just above 5 USD in CAD", isHighAvgCpc(6.91, "CAD"), true},
		{"CPC at 5 USD in CAD, from a division", isHighAvgCpc(138.0/20, "CAD"), false},
		// The compared amount is rounded too: a float remainder far below a cent is not above it.
		{"CPC a float remainder above 5 USD in CAD", isHighAvgCpc(6.90+1e-9, "CAD"), false},
		{"placeholder a float remainder above 1 USD in EUR", isPlaceholderBudget(0.87+1e-9, "EUR"), true},
		// 1 × 0.87 at EUR's cents; KWD at its three decimals.
		{"placeholder at 1 USD in EUR", isPlaceholderBudget(0.87, "EUR"), true},
		{"placeholder just above 1 USD in EUR", isPlaceholderBudget(0.88, "EUR"), false},
		{"placeholder at 1 USD in KWD", isPlaceholderBudget(0.307, "KWD"), true},
		{"placeholder just above 1 USD in KWD", isPlaceholderBudget(0.308, "KWD"), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// Every minor-unit override names a currency the table sizes, with an ISO 4217 digit count.
func TestMonitorMinorUnitDigits_Integrity(t *testing.T) {
	for code, digits := range monitorMinorUnitDigits {
		if _, ok := monitorUnitsPerUSD[code]; !ok {
			t.Errorf("%s has a minor unit but no units-per-USD entry", code)
		}
		if digits != 0 && digits != 3 {
			t.Errorf("%s minor unit = %d, want 0 or 3 (2 is the default and needs no entry)", code, digits)
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
			if strings.Contains(it.Issue, " skipped") {
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

// On Google and Microsoft a zero daily budget is still flagged in a currency that cannot be sized,
// so the skipped item must not claim the placeholder check was skipped outright (#454 review).
func TestMoneyRulesSkipped_NamesTheZeroBudgetException(t *testing.T) {
	zeroBudget := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "c", Status: "ENABLED", Impressions: 400, Clicks: 20, Ctr: 5}
	_, items := EvaluateGoogleMonitor([]model.AccountCampaignMetrics{zeroBudget}, 7, "")
	var placeholder, skipped bool
	for _, it := range items {
		placeholder = placeholder || strings.HasPrefix(it.Issue, "Daily budget is ")
		if strings.Contains(it.Issue, " skipped") {
			skipped = strings.Contains(it.Issue, "on non-zero amounts") && strings.Contains(it.Issue, "zero daily budget is still flagged")
		}
	}
	if !placeholder || !skipped {
		t.Errorf("placeholder=%v skipped-names-exception=%v, want both; items = %+v", placeholder, skipped, items)
	}
	ms := zeroBudget
	ms.Status = "Active"
	_, items = EvaluateMicrosoftMonitor([]model.AccountCampaignMetrics{ms}, 7, "")
	found := false
	for _, it := range items {
		found = found || strings.Contains(it.Issue, "zero daily budget is still flagged")
	}
	if !found {
		t.Errorf("microsoft skipped item does not name the zero-budget exception; items = %+v", items)
	}
	_, items = EvaluateTwitterMonitor([]model.AccountCampaignMetrics{xRow(func(m *model.AccountCampaignMetrics) { m.BudgetDay = 1 })}, &xFirstDay, &xLastDay, "")
	for _, it := range items {
		if strings.Contains(it.Issue, "still flagged") {
			t.Errorf("X flags no zero budget, but its skipped item says so: %q", it.Issue)
		}
	}
}

// LinkedIn's paused-with-real-budget rule is sized in the account's currency too (#454 follow-up,
// #547): 10 lifetime or 1 a day in USD terms, each EXCLUSIVE, compared at the currency's minor
// unit, and never fired on an amount in a currency that cannot be sized.
func TestLinkedInPausedRealBudget_SizedInAccountCurrency(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		currency    string
		total, day  float64
		wantPaused  bool
		wantSkipped bool
	}{
		// USD keeps today's thresholds: > 10 lifetime, > 1 a day.
		{"USD total above 10", "USD", 10.01, 0, true, false},
		{"USD total at 10", "USD", 10, 0, false, false},
		{"USD daily above 1", "USD", 0, 1.01, true, false},
		{"USD daily at 1", "USD", 0, 1, false, false},
		// JPY: ¥200/day (about $1.4) is real, ¥100/day (about $0.7) is not — in dollars both were
		// real. ¥1,000 lifetime (about $6.8) is not; ¥1,471 (just above $10) is.
		{"JPY 100/day is not real", "JPY", 0, 100, false, false},
		{"JPY 200/day is real", "JPY", 0, 200, true, false},
		{"JPY 1000 lifetime is not real", "JPY", 1000, 0, false, false},
		{"JPY 1471 lifetime is real", "JPY", 1471, 0, true, false},
		{"JPY 1470 lifetime is at the line", "JPY", 1470, 0, false, false},
		// KWD: 1/day is about $3, a real budget the dollar comparison (> 1) missed.
		{"KWD 1/day is real", "KWD", 0, 1, true, false},
		{"KWD 0.307/day is at the line", "KWD", 0, 0.307, false, false},
		// CAD 13.80 lifetime is 10 × 1.38 at cents, the threshold itself, not above it.
		{"CAD at 10 USD lifetime", "CAD", 13.80, 0, false, false},
		{"CAD just above 10 USD lifetime", "CAD", 13.81, 0, true, false},
		// The compared amount is rounded too: a float remainder far below a cent is not above it.
		{"CAD a float remainder above 10 USD lifetime", "CAD", 13.80 + 1e-9, 0, false, false},
		// Unsizable currencies: no USD guess, and the account-wide LOW skipped item instead.
		{"absent currency", "", 500, 50, false, true},
		{"unlisted currency", "ARS", 500, 50, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "c", Status: "PAUSED",
				TotalBudget: tc.total, BudgetDay: tc.day}
			_, items := EvaluateLinkedInMonitor([]model.AccountCampaignMetrics{row}, 30, now, tc.currency)
			if got := hasItem(items, model.MonitorPriorityLow, "PAUSED with real budget"); got != tc.wantPaused {
				t.Errorf("paused item = %v, want %v; items = %+v", got, tc.wantPaused, items)
			}
			var skipped []model.AccountMonitorActionItem
			for _, it := range items {
				if strings.Contains(it.Issue, "paused-with-real-budget check was skipped") {
					skipped = append(skipped, it)
				}
			}
			if tc.wantSkipped != (len(skipped) == 1) || len(skipped) > 1 {
				t.Errorf("skipped items = %+v, want one: %v", skipped, tc.wantSkipped)
			}
			if len(skipped) == 1 && (skipped[0].Priority != model.MonitorPriorityLow || skipped[0].CampaignID != "") {
				t.Errorf("skipped item = %+v, want account-wide LOW", skipped[0])
			}
		})
	}
}

// LinkedIn reads its currency off the campaign budgets, so an account whose campaigns carry none
// reports no currency for a reason that skipped nothing: no skipped item there, nor for active or
// fetch-failed paused rows — only a measured, budgeted, PAUSED campaign is one the rule examines.
func TestLinkedInMoneyRuleSkipped_OnlyWhenAPausedBudgetWasThere(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	active := model.AccountCampaignMetrics{PlatformCampaignID: "1", Name: "a", Status: "ACTIVE", BudgetDay: 50, Spend: 10}
	pausedNoBudget := model.AccountCampaignMetrics{PlatformCampaignID: "2", Name: "b", Status: "PAUSED"}
	pausedFailed := model.AccountCampaignMetrics{PlatformCampaignID: "3", Name: "c", Status: "PAUSED", TotalBudget: 500, FetchFailed: true}
	_, items := EvaluateLinkedInMonitor([]model.AccountCampaignMetrics{active, pausedNoBudget, pausedFailed}, 30, now, "")
	for _, it := range items {
		if strings.Contains(it.Issue, " skipped") {
			t.Errorf("unexpected skipped item %+v; items = %+v", it, items)
		}
	}
}
