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
// Ported from lfx-self-serve's reddit-ads.service.ts (the pacing calc inside getRedditAnalytics,
// and buildRedditActionItems).
//
// The pacing LABEL comes from pacingLabelFor (monitor_shared.go). On the BFF side Reddit's
// 50/90/100 are local literals rather than the shared CAMPAIGN_PACING_THRESHOLDS Meta and
// LinkedIn read — linuxfoundation/lfx-self-serve#3019 — so a change to that constant moves
// those two and leaves Reddit behind. Here there is one ladder and no such gap.
//
// The underspend ACTION ITEM is keyed off the pacing LABEL, as it is on the other three
// platforms. The BFF fired it at a hardcoded pacingPct < 40 — a different number from the
// label's own < 50 boundary — so a campaign pacing at 45% was labelled "underspending" and
// never alerted on: the row and the alert disagreed for the whole 40-49% band, and only the
// row was visible. linuxfoundation/lfx-self-serve#3021. One boundary now decides both.
// 0.3 / 1000 is the low-CTR pair Google and LinkedIn also use; only Meta differs, and for a
// stated reason — see monitor_linkedin.go's const block. The clicks-without-conversions floor
// is genuinely Reddit's own: 100 clicks, against LinkedIn's 50 and Meta's 20, because the click
// volumes these campaigns run at differ by that much.
const (
	redditLowCtrPct           = 0.3
	redditMinImpressions      = 1000
	redditClicksNoConversions = 100
)

// EvaluateRedditMonitor mirrors getRedditAnalytics' per-campaign pacing calc and
// buildRedditActionItems.
func EvaluateRedditMonitor(rows []model.AccountCampaignMetrics, days int, now time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}

		// A Reddit campaign has a pacing percentage only when it has a budget this read can pace:
		// a lifetime (LIFETIME_SPEND) budget with a parseable flight to prorate it against, or a
		// daily (DAILY_SPEND) budget scheduled for some part of the window. Anything else — no
		// budget, an unrecognised goal_type, a lifetime budget with no start date — and there is
		// nothing to pace, so the row says so through unknownPacingRow rather than carrying
		// redditPacingPct's 0 fallback into the ladder, where < 50 reads as "underspending" — a
		// campaign reported as failing to spend a budget it does not have.
		//
		// The empty-StartDate case (Reddit reported no parseable start_time — see
		// internal/platform/reddit/monitor.go) used to be handled here on its own, which left the
		// budget half of the same condition open: a campaign with a real flight and no budget
		// still reached the ladder and was mislabelled. Both are the same fact about the same row.
		//
		// The zero-delivery/CTR/no-conversion action items do not depend on pacing, so they still
		// run; the underspend item is keyed off the label, which here is not underspending.
		pacingPct, computable := redditPacingPct(m, days, now)
		if !computable {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, redditActionItems(row.Metrics, 0, row.PacingLabel)...)
			continue
		}

		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, redditActionItems(m, pacingPct, label)...)
	}

	sortByPriority(items)
	return out, items
}

// redditPacingPct computes a Reddit campaign's pacing percentage from whichever budget its
// goal_type gave it (internal/platform/reddit's AccountCampaignRow.TotalBudget doc comment):
//
//   - LIFETIME_SPEND (TotalBudget): the BFF's schedule-based branch, ported as it was — the
//     total prorated across the flight, expected = TotalBudget / flightDays × elapsedDays.
//     Needs a parseable start date; without one there is no flight to prorate against.
//   - DAILY_SPEND (BudgetDay): expected = BudgetDay × the days of the report window the
//     campaign was scheduled for — the window [today-(days-1), now], clipped to the flight
//     where Reddit reported one (end date inclusive). This is LinkedIn's daily-budget branch
//     (monitor_linkedin.go), and Google/Meta's BudgetDay × days whenever the flight covers the
//     whole window. A flight that does not overlap the window at all has no expected spend,
//     so pacing is not computable rather than measured against a day the campaign was not
//     scheduled to run.
//
// The BFF had a dailyBudget × days branch too, but it was dead: reddit-ads.service.ts
// hardcoded dailyBudget to 0 and read goal_value as a lifetime total whatever goal_type said,
// so a DAILY_SPEND campaign's per-day cap was prorated across its whole flight as if it were
// the lifetime budget and a campaign spending exactly its cap read as heavily overspending.
//
// The second return is whether the percentage means anything. It is false when the campaign has
// no budget this read can pace (absent or unrecognised goal_type included), or a lifetime budget
// with no parseable start date — see EvaluateRedditMonitor for what the caller does with that.
// The BFF had no such signal and simply fell through to 0.
func redditPacingPct(m model.AccountCampaignMetrics, days int, now time.Time) (float64, bool) {
	start := parseMonitorDate(m.StartDate)
	end := parseMonitorDate(m.EndDate)
	if m.TotalBudget > 0 {
		if start.IsZero() {
			return 0, false
		}
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
		utcNow := now.UTC()
		windowStart := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
		effectiveStart := windowStart
		if start.After(effectiveStart) {
			effectiveStart = start
		}
		effectiveEnd := utcNow
		// EndDate is a calendar day the flight still runs on, so the flight ends when that
		// day does.
		if !end.IsZero() && end.AddDate(0, 0, 1).Before(effectiveEnd) {
			effectiveEnd = end.AddDate(0, 0, 1)
		}
		if !effectiveEnd.After(effectiveStart) {
			return 0, false
		}
		scheduledDays := maxFloat(1, math.Ceil(effectiveEnd.Sub(effectiveStart).Hours()/24))
		expected := m.BudgetDay * scheduledDays
		if expected > 0 {
			return math.Round(m.Spend / expected * 100), true
		}
	}
	return 0, false
}

func redditActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	if m.Impressions == 0 && m.Clicks == 0 && m.Status == "ACTIVE" {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Campaign %q has zero impressions and zero clicks — ads may not be delivering", m.Name),
			"Check ad group targeting, bid amount, and creative approval status in Reddit Ads Manager")
	}
	// The underspend item stays keyed off the label (#3021), but NOT when the zero-delivery
	// item above has already fired for this row: a campaign that served nothing paces at 0%,
	// and reporting that as a second HIGH item ("Underspending at 0%") says the same thing
	// twice. The BFF avoided the duplicate with `pacingPct > 0`; this guard is narrower on
	// purpose. A CPC campaign can serve impressions, take no clicks and so spend nothing —
	// 0% pacing with delivery — and `pacingPct > 0` would leave that genuinely underspending
	// campaign with no alert at all, since the zero-delivery item does not fire for it either.
	zeroDelivery := m.Impressions == 0 && m.Clicks == 0
	if label == model.MonitorPacingUnderspending && m.Status == "ACTIVE" && !(zeroDelivery && pacingPct == 0) {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Underspending at %.0f%% of budget — $%.2f spent", pacingPct, m.Spend),
			"Broaden targeting (add subreddits/interests), increase bid, or expand geographic targeting")
	}
	if m.Impressions > redditMinImpressions && m.Ctr < redditLowCtrPct && m.Status == "ACTIVE" {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR at %.2f%% — %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Refresh ad creative, test different headlines, or narrow targeting to more relevant subreddits")
	}
	// This rule is dormant on Reddit today, and correctly so. The dispatcher leaves Conversions
	// nil because this read never asks Reddit for conversions, so the nil guard below is what
	// stops the rule claiming "0 conversions" about something nobody measured. It used to
	// receive a hardcoded non-nil 0 on every row and therefore fired for every campaign past
	// the click floor (linuxfoundation/lfx-self-serve#3020). The rule stays rather than being
	// deleted: it is correct as written, and lights up on its own the day a real conversions
	// read lands.
	if m.Clicks > redditClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks but 0 conversions — traffic is not converting", m.Clicks),
			"Verify Reddit pixel is firing correctly, check landing page relevance, and review conversion event setup")
	}
	return items
}
