// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// See monitor_google.go's package-level comment for why this file is a deliberately
// SEPARATE, unshared rule engine rather than routed through Thresholds/Evaluate.
//
// Ported from lfx-self-serve's meta-ads.service.ts (buildCampaignMetrics, buildMetaActionItems).
// The pacing label comes from pacingLabelFor (monitor_shared.go).
//
// On the BFF side Meta reads the shared CAMPAIGN_PACING_THRESHOLDS, whose fourth member is
// `overspending: 130` — and never compares against it: the overspend band starts above
// `constrained` (100), so 130 is dead for labeling on Meta and LinkedIn both. The shared ladder
// here has no such member, which is why it is three boundaries and not four.
const (
	metaLowCtrPct           = 0.5
	metaMinImpressions      = 500
	metaClicksNoConversions = 20
)

// EvaluateMetaMonitor mirrors buildCampaignMetrics' pacing calc and buildMetaActionItems.
//
// now is the single snapshot time the service layer takes before evaluating every row (see
// GetBriefMetrics's `now := s.now()` convention) — Meta's pacing formula needs "now" to derive
// elapsed flight days.
func EvaluateMetaMonitor(rows []model.AccountCampaignMetrics, days int, now time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}

		// See monitor_google.go's budget-less branch for why this reports unknown rather than a
		// computed 0. Meta had the guard below written already, but metaPacingPct returned false
		// from every one of its four returns, so it was never taken: a budget-less Meta campaign
		// got pacingPct 0 and the "underspending" label, exactly as Google's did.
		pacingPct, computable := metaPacingPct(m, days, now)
		if !computable {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, metaActionItems(row.Metrics, 0, row.PacingLabel, days)...)
			continue
		}

		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, metaActionItems(m, pacingPct, label, days)...)
	}

	sortByPriority(items, metaPriorityRank)
	return out, items
}

// metaPacingPct computes the pacing percentage: schedule-based when a total budget and a start
// time are both known, else against a flat dailyBudget*days expectation.
//
// The second return says whether the figure is COMPUTABLE. It used to be an `unknown` flag that
// no return path ever set, so the caller's guard on it was dead and a campaign with neither
// budget reached the ladder carrying 0 — reported as underspending. Now the no-budget path says
// so, and the caller reports PacingUnknown.
func metaPacingPct(m model.AccountCampaignMetrics, days int, now time.Time) (float64, bool) {
	start := parseMonitorDate(m.StartDate)
	end := parseMonitorDate(m.EndDate)
	if m.TotalBudget > 0 && !start.IsZero() {
		flightEnd := now
		if !end.IsZero() {
			flightEnd = end
		}
		totalFlightDays := maxFloat(1, math.Ceil(flightEnd.Sub(start).Hours()/24))
		elapsedDays := maxFloat(1, math.Ceil(now.Sub(start).Hours()/24))
		expected := m.TotalBudget / totalFlightDays * math.Min(elapsedDays, totalFlightDays)
		if expected > 0 {
			return math.Round(m.Spend / expected * 100), true
		}
		return 0, false
	}
	if m.BudgetDay > 0 {
		expected := m.BudgetDay * float64(days)
		if expected > 0 {
			return math.Round(m.Spend / expected * 100), true
		}
	}
	return 0, false
}

func metaActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, days int) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	if m.Status == "ACTIVE" && m.Impressions == 0 && m.Spend == 0 {
		add(model.MonitorPriorityHigh,
			"Campaign active but no delivery — 0 impressions and $0 spent",
			"Check ad set targeting, budget, and creative approval status in Meta Ads Manager")
	}
	if m.Ctr < metaLowCtrPct && m.Impressions > metaMinImpressions {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR: %.2f%% across %d impressions", m.Ctr, m.Impressions),
			"Refresh creative assets, test new ad formats, or narrow audience targeting")
	}
	if m.Clicks > metaClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks ($%.2f spent) but 0 conversions", m.Clicks, m.Spend),
			"Verify Meta Pixel / Conversions API is firing; check landing page and CTA alignment")
	}
	if label == model.MonitorPacingUnderspending && m.Status == "ACTIVE" {
		// m.TotalBudget is 0 whenever pacingPct was computed from the flat BudgetDay*days
		// branch (metaPacingPct) instead of the schedule-based one — using it unconditionally
		// as the denominator produced a self-contradicting "$X of $0.00" message for every
		// daily-budget-funded campaign. Fall back to the same budgetDay*days expectation
		// metaPacingPct itself used to derive pacingPct in that branch.
		denominator := m.TotalBudget
		if denominator <= 0 {
			denominator = m.BudgetDay * float64(days)
		}
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Underspending: %.0f%% of budget used ($%.2f of $%.2f)", pacingPct, m.Spend, denominator),
			"Broaden audience targeting or increase bid cap to improve delivery")
	}
	if (label == model.MonitorPacingConstrained || label == model.MonitorPacingOverspending) && m.Status == "ACTIVE" {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Budget %s: %.0f%% of budget used", label, pacingPct),
			"Increase daily budget or narrow targeting to focus spend on highest-value audiences")
	}
	return items
}

func metaPriorityRank(p model.MonitorPriority) int {
	switch p {
	case model.MonitorPriorityHigh:
		return 0
	case model.MonitorPriorityMed:
		return 1
	case model.MonitorPriorityLow:
		return 2
	default:
		return 3
	}
}

// parseMonitorDate parses a YYYY-MM-DD date, returning the zero time.Time for an empty or
// unparseable string — the shared "no flight date known" representation the meta/reddit
// pacing formulas both test with IsZero().
func parseMonitorDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
