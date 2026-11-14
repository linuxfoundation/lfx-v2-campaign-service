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
// The low-CTR rule's two numbers are shared with Google and Reddit rather than being a third
// LinkedIn-specific pair: all three platforms call 0.3% low, and 1000 impressions is the volume
// at which a CTR figure means anything. Meta is the one that genuinely differs (0.5 / 500) —
// its CTR baseline is higher and its delivery reaches that volume sooner. The
// clicks-without-conversions floors differ per platform because the click volumes do.
const (
	linkedinLowCtrPct           = 0.3
	linkedinMinImpressions      = 1000
	linkedinClicksNoConversions = 50
)

// EvaluateLinkedInMonitor mirrors getLinkedInAnalytics' campaignMetrics.map pacing calc and its
// action-item loop.
//
// DEVIATION FROM THE BFF: linkedin-ads.service.ts's action-item loop's FIRST branch is "this
// campaign has zero ad creatives and is ACTIVE" (a separate per-campaign creative-analytics
// fetch this port's dispatcher does not make — see internal/dispatch/linkedin.go's
// ListAccountCampaignMetrics doc comment) — that HIGH "no creatives" rule is not ported.
//
// The BFF's MED-vs-MEDIUM sort-key mismatch is NOT ported: items are ordered by the shared
// priorityRank (linuxfoundation/lfx-self-serve#3018), as on the other three platforms.
//
// currency is the ad account's ISO 4217 code as the read reported it (the campaign budgets'
// currencyCode, see linkedin.Client.ListAccountCampaignsWithCurrency), or "". It sizes the
// paused-with-real-budget rule in the account's own currency; see monitorUnitsPerUSD (#454).
func EvaluateLinkedInMonitor(rows []model.AccountCampaignMetrics, days int, now time.Time, currency string) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
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
		//
		// A row the dispatcher already marked PacingUnknown — its budget's currency disagrees
		// with another budget on the account, so it cannot be paced against the account's spend
		// (linkedin.Client.ListAccountCampaignsWithCurrency) — takes the same unknown row.
		if m.PacingUnknown || (m.TotalBudget <= 0 && m.BudgetDay <= 0) {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, linkedinActionItems(row.Metrics, 0, row.PacingLabel, currency)...)
			continue
		}

		pacingPct := linkedinPacingPct(m, days, now)
		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, linkedinActionItems(m, pacingPct, label, currency)...)
	}

	// Only over the rows the money rule would have looked at — a measured PAUSED campaign with a
	// budget. Unlike Google, Microsoft and X, LinkedIn's currency comes from the budgets
	// themselves, so an account whose campaigns carry none reports no currency for a reason that
	// skipped nothing; the item there would be noise.
	if it, ok := moneyRulesSkippedItem(currency, "paused-with-real-budget check was skipped", linkedinPausedBudgeted(out)); ok {
		items = append(items, it)
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

	// A campaign carrying BOTH budgets is a lifetime total with a daily cap (#584), and LinkedIn
	// pairs those with a continuous schedule — no runSchedule.end. Without an end the lifetime
	// branch below takes the flight as ending today, so a week's window of a month-long campaign
	// would be paced against the WHOLE total and read as badly underspending. With no end the
	// daily cap is the only figure that says what the window should have spent; with one, the
	// campaign can spend no more than the smaller of the two expectations.
	var expected float64
	switch {
	case m.TotalBudget > 0 && m.BudgetDay > 0 && schedEnd.IsZero():
		expected = linkedinDailyExpected(m, rangeStart, schedStart, schedEnd, now)
	case m.TotalBudget > 0 && m.BudgetDay > 0:
		expected = math.Min(linkedinTotalExpected(m, rangeStart, schedStart, schedEnd, now),
			linkedinDailyExpected(m, rangeStart, schedStart, schedEnd, now))
	case m.TotalBudget > 0:
		expected = linkedinTotalExpected(m, rangeStart, schedStart, schedEnd, now)
	case m.BudgetDay > 0:
		expected = linkedinDailyExpected(m, rangeStart, schedStart, schedEnd, now)
	}
	if expected > 0 {
		return m.Spend / expected * 100
	}
	return 0
}

// linkedinTotalExpected is the lifetime branch: the total spread evenly over the flight, times
// the days of the window that fall inside it.
func linkedinTotalExpected(m model.AccountCampaignMetrics, rangeStart, schedStart, schedEnd, now time.Time) float64 {
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
	return m.TotalBudget / totalFlightDays * windowDays
}

// linkedinDailyExpected is the daily branch: the daily budget times the days of the window that
// fall inside the flight.
func linkedinDailyExpected(m model.AccountCampaignMetrics, rangeStart, schedStart, schedEnd, now time.Time) float64 {
	effectiveStart := schedStart
	if effectiveStart.IsZero() || rangeStart.After(effectiveStart) {
		effectiveStart = rangeStart
	}
	effectiveEnd := schedEnd
	if effectiveEnd.IsZero() || now.Before(effectiveEnd) {
		effectiveEnd = now
	}
	flightDays := maxFloat(1, math.Ceil(effectiveEnd.Sub(effectiveStart).Hours()/24))
	return m.BudgetDay * flightDays
}

// linkedinPausedBudgeted is the subset of rows the paused-with-real-budget rule would examine
// had the currency been sizable: measured (not FetchFailed), PAUSED, with a budget. It applies
// the measured filter itself rather than leaning on moneyRulesSkippedItem's, so it returns
// exactly the set its callers' comments describe.
func linkedinPausedBudgeted(rows []model.AccountMonitorRow) []model.AccountMonitorRow {
	var out []model.AccountMonitorRow
	for _, r := range rows {
		if !r.Metrics.FetchFailed && r.Metrics.Status == "PAUSED" && (r.Metrics.TotalBudget > 0 || r.Metrics.BudgetDay > 0) {
			out = append(out, r)
		}
	}
	return out
}

func linkedinActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, currency string) []model.AccountMonitorActionItem {
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
	// Gated on an impressions floor, like Meta/Google/Reddit, rather than on the BFF's
	// `ctr > 0` — which excluded a 0% CTR, the WORST case, from the rule meant to catch it.
	// LinkedIn has no "impressions but no clicks" rule to catch it instead (Google does), so a
	// campaign with a half-million impressions and zero clicks emitted nothing at all while one
	// at 0.29% got a MED item. The floor has to land in the same change: without it, removing
	// `ctr > 0` would fire "Low CTR: 0.00%" on every campaign that has not been served yet.
	if m.Ctr < linkedinLowCtrPct && m.Impressions > linkedinMinImpressions {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR: %.2f%%", m.Ctr),
			"Refresh ad copy or images; review audience targeting")
	}
	if m.Clicks > linkedinClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			"Clicks without conversions",
			"Audit LinkedIn Insight Tag on registration landing page")
	}
	// Sized in the account's currency (isRealBudget, #454): 10 lifetime or 1 a day in USD terms.
	// Before, those were compared as dollars, so a ¥2/day JPY campaign counted as real budget
	// and a KWD account's 1/day (about $3) did not.
	// A row the dispatcher marked PacingUnknown has a budget whose own currency could not be
	// trusted (unreadable, or disagreeing with the account's other budgets), so the account's
	// currency does not size it either.
	if m.Status == "PAUSED" && !m.PacingUnknown && isRealBudget(m.TotalBudget, m.BudgetDay, currency) {
		add(model.MonitorPriorityLow,
			"Campaign is PAUSED with real budget",
			"Confirm intentional pause or activate")
	}
	return items
}
