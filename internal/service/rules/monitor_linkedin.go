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
// SEPARATE, unshared rule engine.
//
// Ported from lfx-self-serve's linkedin-ads.service.ts (the pacing calc + label logic inside
// getLinkedInAnalytics, and its action-item loop).
//
// The pacing label comes from pacingLabelFor (monitor_shared.go). On the BFF side LinkedIn
// reads the shared CAMPAIGN_PACING_THRESHOLDS — moved off local literals there so the label
// agrees with a shared pacing-bar component — and, like Meta, never compares against that
// constant's fourth member (`overspending: 130`); see monitor_meta.go.
//
// LinkedIn's pacingPct is also NOT rounded, unlike every other platform here — see
// model.AccountMonitorRow.PacingPct's doc comment.
const (
	linkedinLowCtrPct           = 0.3
	linkedinClicksNoConversions = 50
)

// EvaluateLinkedInMonitor mirrors getLinkedInAnalytics' campaignMetrics.map pacing calc and its
// action-item loop.
//
// DEVIATION FROM THE BFF: linkedin-ads.service.ts's action-item loop's FIRST branch is "this
// campaign has zero ad creatives and is ACTIVE" (a separate per-campaign creative-analytics
// fetch this port's dispatcher does not make — see internal/dispatch/linkedin.go's
// ListAccountCampaignMetrics doc comment) — that HIGH "no creatives" rule is not ported. Every
// other rule below IS ported, including the priority-sort bug.
func EvaluateLinkedInMonitor(rows []model.AccountCampaignMetrics, days int, now time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}

		// LinkedIn's guard was the one that worked — a budget-less campaign was already held off
		// the ladder rather than labelled underspending. What it did not do is SAY so: the row
		// went out as MonitorPacingNormal with PacingUnknown unset, which a consumer reads as
		// "on plan". Same treatment as Google and Meta now, for the same reason.
		if m.TotalBudget <= 0 && m.BudgetDay <= 0 {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, linkedinActionItems(row.Metrics, 0, row.PacingLabel)...)
			continue
		}

		pacingPct := linkedinPacingPct(m, days, now)
		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, linkedinActionItems(m, pacingPct, label)...)
	}

	sortByPriority(items)
	return out, items
}

// linkedinPacingPct ports the totalBudget/dailyBudget branches of getLinkedInAnalytics'
// campaignMetrics.map, in day granularity (this port's AccountCampaignMetrics carries
// date-only flight bounds, not the BFF's millisecond runSchedule timestamps — see
// model.AccountCampaignMetrics.StartDate/EndDate). rangeStart is `now - (days-1) days`, the
// same inclusive-of-today convention shared by the Google/Reddit/Meta monitor dispatchers and
// pinned by internal/platform/linkedin/monitor_test.go's
// TestListAccountCampaigns_UsesInjectedClockNotWallClock (a "days=7" window ending on today
// starts 6 days back, not 7) — not `now - days days` as an earlier draft of this comment said.
//
// Deliberately UNROUNDED — see model.AccountMonitorRow.PacingPct.
func linkedinPacingPct(m model.AccountCampaignMetrics, days int, now time.Time) float64 {
	rangeStart := now.AddDate(0, 0, -(days - 1))
	schedStart := parseMonitorDate(m.StartDate)
	schedEnd := parseMonitorDate(m.EndDate)

	if m.TotalBudget > 0 {
		flightStart := schedStart
		if flightStart.IsZero() {
			flightStart = rangeStart
		}
		flightEnd := schedEnd
		if flightEnd.IsZero() {
			flightEnd = now
		}
		totalFlightDays := maxFloat(1, math.Ceil(flightEnd.Sub(flightStart).Hours()/24))
		effectiveStart := flightStart
		if rangeStart.After(effectiveStart) {
			effectiveStart = rangeStart
		}
		effectiveEnd := flightEnd
		if now.Before(effectiveEnd) {
			effectiveEnd = now
		}
		windowDays := maxFloat(1, math.Ceil(effectiveEnd.Sub(effectiveStart).Hours()/24))
		expected := m.TotalBudget / totalFlightDays * windowDays
		if expected > 0 {
			return m.Spend / expected * 100
		}
		return 0
	}
	if m.BudgetDay > 0 {
		effectiveStart := schedStart
		if effectiveStart.IsZero() || rangeStart.After(effectiveStart) {
			effectiveStart = rangeStart
		}
		effectiveEnd := schedEnd
		if effectiveEnd.IsZero() || now.Before(effectiveEnd) {
			effectiveEnd = now
		}
		flightDays := maxFloat(1, math.Ceil(effectiveEnd.Sub(effectiveStart).Hours()/24))
		expected := m.BudgetDay * flightDays
		if expected > 0 {
			return m.Spend / expected * 100
		}
	}
	return 0
}

func linkedinActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	// The BFF's "no creatives" HIGH rule (else-if'd ahead of underspending, see this file's
	// doc comment) is not ported, so underspending is evaluated unconditionally here rather
	// than only on the BFF's else-branch.
	if label == model.MonitorPacingUnderspending {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Underspending — pacing below %d%%", monitorPacingUnderspendingBelow),
			"Check targeting breadth, bid strategy, or budget floor")
	}
	if label == model.MonitorPacingConstrained || label == model.MonitorPacingOverspending {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Budget constrained — pacing above %d%%", monitorPacingHealthyTo),
			"Consider increasing budget if event is in peak registration period")
	}
	if m.Ctr > 0 && m.Ctr < linkedinLowCtrPct {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR: %.2f%%", m.Ctr),
			"Refresh ad copy or images; review audience targeting")
	}
	if m.Clicks > linkedinClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			"Clicks without conversions",
			"Audit LinkedIn Insight Tag on registration landing page")
	}
	if m.Status == "PAUSED" && (m.TotalBudget > 10 || m.BudgetDay > 1) {
		add(model.MonitorPriorityLow,
			"Campaign is PAUSED with real budget",
			"Confirm intentional pause or activate")
	}
	return items
}
