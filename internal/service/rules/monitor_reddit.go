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
// Reddit's pacing literals (50/90/100 below) are LOCAL to reddit-ads.service.ts, not read from
// the shared CAMPAIGN_PACING_THRESHOLDS constant Meta/LinkedIn use — they happen to hold the
// same numeric values as that constant's underspending/normal/constrained members today, which
// is a coincidence worth noting, not a shared source of truth: a future edit to one will not
// move the other, on either side of this port.
const (
	redditPacingUnderspending = 50
	redditPacingNormal        = 90
	redditPacingConstrained   = 100
	// redditUnderspendActionFloor is the migration spec's bug (b): the underspend ACTION ITEM
	// fires at pacingPct < 40, a DIFFERENT number from the pacingLabel's own <50 boundary above.
	// KNOWN BUG, ported verbatim — see follow-up ticket: a campaign pacing at, say, 45% carries
	// PacingLabel == "underspending" (per the 50 boundary) but never gets the HIGH "Underspending
	// at ..." action item (which needs <40) — the label and the alert disagree for the 40-49%
	// band. reddit-ads.service.ts's own alert issue text says "Underspending at {pacingPct}%",
	// not a literal "50" — so this is a NUMERIC threshold mismatch (40 vs 50), not, as one
	// description of this bug puts it, a case where the alert text itself says "50"; that literal
	// claim does not reproduce against the current BFF source and is not what this port
	// reproduces. What is real, and is preserved here, is the 40-vs-50 threshold gap itself.
	redditUnderspendActionFloor = 40
	redditLowCtrPct             = 0.3
	redditMinImpressions        = 1000
	redditClicksNoConversions   = 100
)

// EvaluateRedditMonitor mirrors getRedditAnalytics' per-campaign pacing calc and
// buildRedditActionItems.
func EvaluateRedditMonitor(rows []model.AccountCampaignMetrics, days int, now time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		// See monitor_google.go's identical guard: a FetchFailed row's zero-value metrics must
		// not be run through pacing/action-item evaluation, which would fabricate a finding
		// against data this port never actually read.
		if m.FetchFailed {
			out = append(out, model.AccountMonitorRow{Metrics: m, PacingLabel: model.MonitorPacingNormal})
			continue
		}

		pacingPct := redditPacingPct(m, days, now)
		label := model.MonitorPacingNormal
		switch {
		case pacingPct < redditPacingUnderspending:
			label = model.MonitorPacingUnderspending
		case pacingPct > redditPacingConstrained:
			label = model.MonitorPacingOverspending
		case pacingPct > redditPacingNormal:
			label = model.MonitorPacingConstrained
		}
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, redditActionItems(m, pacingPct)...)
	}

	sortByPriority(items, redditPriorityRank)
	return out, items
}

// redditPacingPct ports the schedule-based branch (totalBudget>0 && schedStart set) of
// getRedditAnalytics; Reddit campaigns carry no daily budget (dailyBudget is hardcoded to 0
// upstream — see the dispatcher), so the dailyBudget*days branch is DEAD CODE here exactly as
// it is dead in reddit-ads.service.ts (dailyBudget is always 0 there too), and is omitted.
func redditPacingPct(m model.AccountCampaignMetrics, _ int, now time.Time) float64 {
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
			return math.Round(m.Spend / expected * 100)
		}
	}
	return 0
}

func redditActionItems(m model.AccountCampaignMetrics, pacingPct float64) []model.AccountMonitorActionItem {
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
	if pacingPct > 0 && pacingPct < redditUnderspendActionFloor && m.Status == "ACTIVE" {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Underspending at %.0f%% of budget — $%.2f spent", pacingPct, m.Spend),
			"Broaden targeting (add subreddits/interests), increase bid, or expand geographic targeting")
	}
	if m.Impressions > redditMinImpressions && m.Ctr < redditLowCtrPct && m.Status == "ACTIVE" {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR at %.2f%% — %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Refresh ad creative, test different headlines, or narrow targeting to more relevant subreddits")
	}
	// c.conversions is hardcoded to 0 for every Reddit row (see the dispatcher and
	// model.AccountCampaignMetrics.Conversions' doc comment) — so this rule, ported verbatim,
	// fires for every Reddit campaign whose Clicks exceed the threshold: it can never be
	// satisfied otherwise, because conversions can never be observed as nonzero. KNOWN BUG,
	// ported verbatim — see follow-up ticket.
	if m.Clicks > redditClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks but 0 conversions — traffic is not converting", m.Clicks),
			"Verify Reddit pixel is firing correctly, check landing page relevance, and review conversion event setup")
	}
	return items
}

func redditPriorityRank(p model.MonitorPriority) int {
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
