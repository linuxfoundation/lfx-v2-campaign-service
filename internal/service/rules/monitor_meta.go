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
// Meta is the one platform whose low-CTR pair differs from the other three's 0.3 / 1000: its CTR
// baseline is higher, so 0.5% is the comparable "low", and its delivery reaches a judgeable
// volume sooner, so the floor is 500 rather than 1000. See monitor_linkedin.go's const block.
// Its clicks-without-conversions floor is the lowest of the four for the same reason the others
// differ — the click volumes do.
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
		expected, _ := metaExpectedSpend(m, days, now)
		if !computable {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, metaActionItems(row.Metrics, 0, row.PacingLabel, expected)...)
			continue
		}

		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, metaActionItems(m, pacingPct, label, expected)...)
	}

	sortByPriority(items)
	return out, items
}

// metaPacingPct computes the pacing percentage, spend over metaExpectedSpend.
func metaPacingPct(m model.AccountCampaignMetrics, days int, now time.Time) (float64, bool) {
	expected, ok := metaExpectedSpend(m, days, now)
	if !ok {
		return 0, false
	}
	return math.Round(m.Spend / expected * 100), true
}

// metaExpectedSpend is the spend the campaign's plan expects over the requested window:
// schedule-based when a total budget and a start time are both known, else a flat
// dailyBudget*days expectation. ok=false (expected 0) when nothing can be expected.
//
// The second return says whether the figure is COMPUTABLE. It used to be an `unknown` flag that
// no return path ever set, so the caller's guard on it was dead and a campaign with neither
// budget reached the ladder carrying 0 — reported as underspending. Now the no-budget path says
// so, and the caller reports PacingUnknown.
//
// THE LIFETIME BRANCH PACES THE REQUESTED WINDOW, not the flight to date (PR #316 review). m.Spend
// is the Insights spend over the last `days` days only (fetchAccountCampaignInsights' time_range),
// so the expectation must cover the same span: the lifetime budget spread evenly over the flight,
// times the days of the window the flight actually ran on. It used to be the expectation since
// the flight STARTED — a 9,000 / 90-day campaign on day 60 read with days=7 compared ~700 of
// window spend against ~5,900 of flight-to-date plan and reported 12%, a false underspend on
// every lifetime campaign past its first week. This is LinkedIn's linkedinPacingPct proration
// (budget / flight days × window days), in calendar days: the window is the `days` UTC days
// ending today inclusive (the Insights range), and EndDate is a day the flight still runs on, so
// the flight ends when that day does. An open-ended flight runs through today. A window the
// flight does not overlap (it has not started, or ended before the window) is not computable: a
// zero spend there is correct, not underspending.
func metaExpectedSpend(m model.AccountCampaignMetrics, days int, now time.Time) (float64, bool) {
	start := parseMonitorDate(m.StartDate)
	end := parseMonitorDate(m.EndDate)
	if m.TotalBudget > 0 && !start.IsZero() {
		utcNow := now.UTC()
		today := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC)
		windowStart := today.AddDate(0, 0, -(days - 1))
		windowEnd := today.AddDate(0, 0, 1) // exclusive: today is in the window
		flightEnd := windowEnd
		if !end.IsZero() {
			flightEnd = end.AddDate(0, 0, 1)
		}
		if !flightEnd.After(start) {
			return 0, false
		}
		totalFlightDays := math.Round(flightEnd.Sub(start).Hours() / 24)
		effStart, effEnd := start, flightEnd
		if windowStart.After(effStart) {
			effStart = windowStart
		}
		if windowEnd.Before(effEnd) {
			effEnd = windowEnd
		}
		if !effEnd.After(effStart) {
			return 0, false
		}
		windowDays := math.Round(effEnd.Sub(effStart).Hours() / 24)
		expected := m.TotalBudget / totalFlightDays * windowDays
		if expected > 0 {
			return expected, true
		}
		return 0, false
	}
	if m.BudgetDay > 0 {
		expected := m.BudgetDay * float64(days)
		if expected > 0 {
			return expected, true
		}
	}
	return 0, false
}

// metaMoney renders an amount for action-item text. Meta reports spend and budgets in the AD
// ACCOUNT's currency (JPY, KRW, EUR … as well as USD) and this service performs no FX
// conversion, so a "$" would misstate a yen or won amount as dollars. The currency CODE is not
// carried on the row, so the text says "in account currency", as the X monitor's xMoney does.
func metaMoney(v float64) string { return fmt.Sprintf("%.2f in account currency", v) }

// expected is metaExpectedSpend's figure for the window — the denominator the underspending text
// reports, so the text and the percentage describe the same span.
func metaActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, expected float64) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	if m.Status == "ACTIVE" && m.Impressions == 0 && m.Spend == 0 {
		add(model.MonitorPriorityHigh,
			"Campaign active but no delivery — 0 impressions and nothing spent",
			"Check ad set targeting, budget, and creative approval status in Meta Ads Manager")
	}
	if m.Ctr < metaLowCtrPct && m.Impressions > metaMinImpressions {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR: %.2f%% across %d impressions", m.Ctr, m.Impressions),
			"Refresh creative assets, test new ad formats, or narrow audience targeting")
	}
	if m.Clicks > metaClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks (%s spent) but 0 conversions", m.Clicks, metaMoney(m.Spend)),
			"Verify Meta Pixel / Conversions API is firing; check landing page and CTA alignment")
	}
	if label == model.MonitorPacingUnderspending && m.Status == "ACTIVE" {
		// The denominator is the window's expected spend, the same figure pacingPct was computed
		// from. It used to be m.TotalBudget — the WHOLE lifetime budget, while pacingPct (and the
		// spend) cover only the window — and before that "of 0.00" for a daily-funded campaign.
		denominator := expected
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Underspending: %.0f%% of budget used (%s of %s)", pacingPct, metaMoney(m.Spend), metaMoney(denominator)),
			"Broaden audience targeting or increase bid cap to improve delivery")
	}
	if (label == model.MonitorPacingConstrained || label == model.MonitorPacingOverspending) && m.Status == "ACTIVE" {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Budget %s: %.0f%% of budget used", label, pacingPct),
			"Increase daily budget or narrow targeting to focus spend on highest-value audiences")
	}
	return items
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
