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
// Ported from lfx-self-serve's meta-ads.service.ts (buildCampaignMetrics, buildMetaActionItems),
// using the SHARED CAMPAIGN_PACING_THRESHOLDS the BFF's packages/shared/src/constants exports
// (underspending 50 / normal 90 / constrained 100 / overspending 130) — but, ported verbatim,
// Meta's own label logic never actually compares against the `overspending` (130) member: its
// overspend band starts at >constrained (100), exactly as linkedin-ads.service.ts's does. That
// makes the 130 threshold dead for both platforms' own labeling, which is themselves a BFF-side
// oddity this port reproduces rather than "fixes".
const (
	metaPacingUnderspending = 50
	metaPacingNormal        = 90
	metaPacingConstrained   = 100
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

		pacingPct, unknown := metaPacingPct(m, days, now)
		label := model.MonitorPacingNormal
		if !unknown {
			switch {
			case pacingPct < metaPacingUnderspending:
				label = model.MonitorPacingUnderspending
			case pacingPct > metaPacingConstrained:
				label = model.MonitorPacingOverspending
			case pacingPct > metaPacingNormal:
				label = model.MonitorPacingConstrained
			}
		}
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, metaActionItems(m, pacingPct, label)...)
	}

	sortByPriority(items, metaPriorityRank)
	return out, items
}

// metaPacingPct ports buildCampaignMetrics' pacing branch: schedule-based when a total budget
// and a start time are both known, else a flat dailyBudget*days expectation, else 0 (unknown
// treated as pacingPct 0 / label "normal", exactly as the BFF's `pacingPct = 0` default did —
// this is NOT the same as model.AccountCampaignMetrics.PacingUnknown, which this port reserves
// for rows the dispatcher could not schedule-bound at all).
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
			return math.Round(m.Spend / expected * 100), false
		}
		return 0, false
	}
	if m.BudgetDay > 0 {
		expected := m.BudgetDay * float64(days)
		if expected > 0 {
			return math.Round(m.Spend / expected * 100), false
		}
	}
	return 0, false
}

func metaActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel) []model.AccountMonitorActionItem {
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
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Underspending: %.0f%% of budget used ($%.2f of $%.2f)", pacingPct, m.Spend, m.TotalBudget),
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
