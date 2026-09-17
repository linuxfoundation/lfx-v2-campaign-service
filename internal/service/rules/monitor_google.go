// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// This file, and its three siblings monitor_linkedin.go / monitor_meta.go / monitor_reddit.go,
// are a deliberately UNPORTED-INTO-`rules` family. This package's Thresholds/ComputePacing/
// Evaluate (pacing.go, actions.go) already unify what the BFF drifted apart on the SINGLE-
// campaign metrics path. The account-monitor endpoints this file backs are a DIFFERENT read
// path — see internal/domain/model/monitor.go — being ported for the express purpose of
// differentially verifying it against the still-live BFF, bug for bug. Routing it through the
// already-unified Thresholds/Evaluate would silently move every threshold this file exists to
// preserve, breaking that diff. Unifying these four is deferred to follow-up ticket #7,
// tracked as part of linuxfoundation/lfx-self-serve#2519; it is a decision to make in the
// open, once, not a side effect of adding this endpoint.
//
// Ported from lfx-self-serve's campaign-metrics.service.ts (resolveDateRange,
// parseCampaignMetrics, generateActionItems).

// googlePacingUnderspending/Constrained/Overspending are campaign-metrics.service.ts's own
// local literals (50/90/100) — NOT this package's shared Thresholds{50,100,130}, and not the
// same 50/90/100 Reddit happens to also hardcode (a coincidence of value, not a shared
// constant on either side). Ported verbatim.
const (
	googlePacingUnderspending = 50
	googlePacingConstrained   = 100
	googlePacingOverspendFrom = 90
)

// EvaluateGoogleMonitor computes each row's pacing percentage/label and the account's action
// items, mirroring campaign-metrics.service.ts's parseCampaignMetrics + generateActionItems.
//
// days is the caller's requested window (7..90, validated by the service layer) — Google's
// port, unlike LinkedIn/Meta/Reddit, computes expected spend as budgetDay*days directly rather
// than from the campaign's own flight dates, exactly as resolveDateRange/parseCampaignMetrics
// do (Google Ads campaigns read here are not flight-scheduled in the BFF's model).
func EvaluateGoogleMonitor(rows []model.AccountCampaignMetrics, days int) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		// zz-prefixed campaign names are filtered out entirely — ported from
		// getMonitorData's `.filter((c) => !c.name.toLowerCase().startsWith('zz'))`. This is a
		// KNOWN BUG/convention, ported verbatim — see follow-up ticket: it silently drops any
		// campaign an operator happened to name starting with "zz" (e.g. a "ZZ-archive-test"
		// campaign), not just the intended test/scratch ones.
		if strings.HasPrefix(strings.ToLower(m.Name), "zz") {
			continue
		}

		// A FetchFailed row's numeric fields are left at their platform-reported zero value
		// (see AccountCampaignMetrics.FetchFailed's doc comment) — running the pacing/action-item
		// calc against that placeholder zero would fabricate a bogus "underspending" label and a
		// HIGH-priority action item for a campaign this port never actually measured. The row is
		// still returned in the campaigns array (the caller sees it and its FetchFailed flag),
		// just with pacing/action-item evaluation skipped. PacingLabel keeps its zero-value
		// "normal" placeholder — pacing_label is a required enum with no "unknown" member — and
		// PacingUnknown=true is the caller's signal not to trust it, the same convention
		// pacing_pct's own "meaningless when pacing_unknown is true" doc comment establishes
		// (design/connection.go).
		if m.FetchFailed {
			m.PacingUnknown = true
			out = append(out, model.AccountMonitorRow{Metrics: m, PacingLabel: model.MonitorPacingNormal})
			continue
		}

		expectedSpend := m.BudgetDay * float64(days)
		pacingPct := 0.0
		if expectedSpend > 0 {
			pacingPct = math.Round(m.Spend / expectedSpend * 100)
		}
		label := model.MonitorPacingNormal
		switch {
		case pacingPct < googlePacingUnderspending:
			label = model.MonitorPacingUnderspending
		case pacingPct > googlePacingConstrained:
			label = model.MonitorPacingOverspending
		case pacingPct > googlePacingOverspendFrom:
			label = model.MonitorPacingConstrained
		}

		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)

		items = append(items, googleActionItems(m, pacingPct, label, days)...)
	}

	sortByPriority(items, googlePriorityRank)
	return out, items
}

// googleStatusLimited/Enabled/Paused/Draft are Google Ads' own run-state literals as
// normalizeCampaignStatus lower-cases them in the BFF (GADS_STATUS_ENUM). This port's
// dispatcher is expected to hand these rules the platform's own status string lower-cased the
// same way, so the comparisons below match campaign-metrics.service.ts's literal comparisons
// against 'limited'/'enabled'/'paused'/'draft'.
const (
	googleStatusLimited = "limited"
	googleStatusEnabled = "enabled"
	googleStatusPaused  = "paused"
	googleStatusDraft   = "draft"
)

func googleActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, days int) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	status := strings.ToLower(m.Status)
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	if status == googleStatusLimited {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Campaign limited by Google — only %d impressions on $%.2f/day budget", m.Impressions, m.BudgetDay),
			"Switch bid strategy to Maximize Clicks, or expand keyword match types to increase eligible auctions")
	}
	if m.BudgetDay <= 1 && status == googleStatusEnabled {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Budget is $%.2f/day — this is a placeholder and won't generate meaningful traffic", m.BudgetDay),
			"Set a real daily budget (typically $10-50/day for events) before expecting results")
	}
	if status == googleStatusPaused {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Campaign is paused — spent $%.2f before pause", m.Spend),
			"Check if this was intentionally paused or if the event is still upcoming and needs reactivation")
	}
	if label == model.MonitorPacingUnderspending && status == googleStatusEnabled {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Only spending %.0f%% of $%.2f/day budget — $%.2f spent vs $%.2f expected", pacingPct, m.BudgetDay, m.Spend, m.BudgetDay*float64(days)),
			"Broaden targeting (locations, audiences), add broad match keywords, or increase bids to win more auctions")
	}
	if label == model.MonitorPacingConstrained && status == googleStatusEnabled {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Spending %.0f%% of budget — demand exceeds $%.2f/day cap, ads stop showing mid-day", pacingPct, m.BudgetDay),
			"Increase daily budget to capture missed impressions, or narrow targeting to focus spend on highest-value audiences")
	}
	if m.IsSearchChannel && m.Ctr < 2 && m.Clicks > 10 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Search CTR is %.2f%% (benchmark: 2%%+) - %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Improve headline relevance to search intent, add negative keywords to filter irrelevant queries")
	}
	if !m.IsSearchChannel && m.Ctr < 0.3 && m.Impressions > 1000 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Display CTR is %.2f%% (benchmark: 0.3%%+) - %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Refresh creative assets, check for audience overlap across campaigns, or narrow placement targeting")
	}
	if m.Clicks > 20 && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks ($%.2f spent) but 0 conversions — traffic is not converting", m.Clicks, m.Spend),
			"Verify conversion tracking is firing correctly, check landing page load speed, and review if the CTA matches the ad promise")
	}
	avgCpc := 0.0
	if m.Clicks > 0 {
		avgCpc = m.Spend / float64(m.Clicks)
	}
	if avgCpc > 5 && m.Clicks > 10 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Avg CPC is $%.2f — spending $%.2f for only %d clicks", avgCpc, m.Spend, m.Clicks),
			"Switch to a Target CPA or Maximize Clicks bid strategy, add long-tail keywords with lower competition")
	}
	if m.Impressions > 0 && m.Clicks == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d impressions but 0 clicks — ads are showing but no one is clicking", m.Impressions),
			"Rewrite ad copy to be more compelling, ensure headlines match search intent, test different CTAs")
	}
	if status == googleStatusDraft {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Campaign is still in draft — $%.2f/day budget allocated but not running", m.BudgetDay),
			"Upload creative assets, review ad groups, publish the campaign (then pause if not ready to go live)")
	}
	return items
}

// googlePriorityRank matches campaign-metrics.service.ts's generateActionItems sort key
// exactly: HIGH:0, MED:1, LOW:2, anything else last.
func googlePriorityRank(p model.MonitorPriority) int {
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

// sortByPriority is a small stable sort shared by all four monitor_*.go files (each with its
// own rank function, since — see monitor_linkedin.go — the rank functions are NOT
// interchangeable).
func sortByPriority(items []model.AccountMonitorActionItem, rank func(model.MonitorPriority) int) {
	// insertion sort: stable, and these lists are always small (one account's worth of
	// campaigns), matching Array.prototype.sort's stability the BFF relies on.
	for i := 1; i < len(items); i++ {
		j := i
		for j > 0 && rank(items[j-1].Priority) > rank(items[j].Priority) {
			items[j-1], items[j] = items[j], items[j-1]
			j--
		}
	}
}
