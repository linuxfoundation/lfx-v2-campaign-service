// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// This file and its three siblings monitor_linkedin.go / monitor_meta.go / monitor_reddit.go
// back the ACCOUNT-MONITOR read path — see internal/domain/model/monitor.go — which is distinct
// from the single-campaign metrics path that pacing.go's Thresholds/ComputePacing and actions.go's
// Evaluate serve.
//
// The four were originally ported from the BFF bug-for-bug, each with its own private copy of the
// rules, so that the port could be differentially diffed against the still-live BFF before
// cutover. That diff is no longer the plan of record, which removes the reason to preserve the
// duplication — so what the four genuinely share now lives in monitor_shared.go, and the five
// deliberately-ported defects are being fixed under their own tickets rather than frozen.
//
// What stays separate, and deliberately: these four still do NOT route through
// Thresholds/Evaluate. That path runs a different ladder (50/100/130, with `Constrained` as an
// inclusive top) against a different input shape, so routing the monitor through it would move
// every operator-facing alerting band as a side effect. Merging the two read paths is its own
// decision, on its own ticket, under linuxfoundation/lfx-self-serve#2519 — not a side effect of
// deduplicating the four.
//
// Ported from lfx-self-serve's campaign-metrics.service.ts (resolveDateRange,
// parseCampaignMetrics, generateActionItems).
//
// fetchFailedRow, used below, is a cross-platform helper shared with the other three
// monitor_*.go files — see monitor_shared.go.

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
		// Campaigns named with the operator's "zz" scratch prefix are hidden from this view.
		// See isScratchCampaignName for what counts and why the BFF's own test is not used
		// directly.
		if isScratchCampaignName(m.Name) {
			continue
		}

		// internal/platform/googleads.ListAccountCampaigns sets FetchFailed when a campaign's
		// GAQL metrics fields fail to parse (round-19 review) OR when its budget is present
		// but unparseable alongside otherwise-good metrics (round-24/25 review) — see
		// fetchFailedRow's doc comment in monitor_shared.go. Either way this row (metrics
		// possibly real, possibly zero-value) is excluded here rather than risk fabricating a
		// pacing/action-item finding against an untrusted field. This is a deliberately blanket
		// exclusion, unlike monitor_reddit.go's empty-StartDate branch (which still runs the
		// metrics-only action items): budgetOK failing here means BudgetDailyUSD specifically is
		// untrusted, but most of googleActionItems' own rules read status/impressions/clicks/
		// spend, not the budget, and could in principle still fire. Left blanket for now — no
		// evidence yet that a real budget-parse failure has ever coincided with an actionable
		// delivery issue on the same row — rather than partially evaluating a row this port has
		// never had to before (round-26 review).
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}

		// No daily budget means no plan to pace against, so there is no percentage to report.
		// This port used to fall through to pacingPct = 0 here, which the ladder reads as
		// "underspending" — so every budget-less campaign was reported as failing to spend a
		// budget it does not have, with an action item reading "Only spending 0% of $0.00/day
		// budget — $0.00 spent vs $0.00 expected". Reddit already routes this case through
		// PacingUnknown; Google and Meta did not (Meta's own guard was dead code, never taken).
		//
		// The real signal for this campaign is not lost: the BudgetDay <= 1 rule in
		// googleActionItems still fires, and says the accurate thing — that the budget is a
		// placeholder — at HIGH rather than burying it in a pacing complaint at MED.
		expectedSpend := m.BudgetDay * float64(days)
		if expectedSpend <= 0 {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, googleActionItems(row.Metrics, 0, row.PacingLabel, days)...)
			continue
		}

		pacingPct := math.Round(m.Spend / expectedSpend * 100)
		label := pacingLabelFor(pacingPct)

		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)

		items = append(items, googleActionItems(m, pacingPct, label, days)...)
	}

	sortByPriority(items)
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

// isScratchCampaignName reports whether a campaign name uses the operator convention of
// prefixing throwaway campaigns with "zz" so they sort last and can be ignored.
//
// Ported from getMonitorData's `.filter((c) => !c.name.toLowerCase().startsWith('zz'))`, but
// NOT that test verbatim. Two bare letters is not a convention, it is a coincidence waiting to
// happen: `startsWith('zz')` silently drops any campaign whose name merely begins with them,
// and a dropped campaign is invisible here — no row, no action items, no indication anything
// was filtered. An operator looking for a campaign that is quietly missing from the monitor
// has nothing to go on.
//
// The prefix must therefore be followed by a separator (or be the whole name) to count. That
// keeps every name the convention actually produces — "zz-test", "ZZ_old_scratch", "zz 2026
// draft" — and stops the filter reaching a name that simply starts with the same two letters.
func isScratchCampaignName(name string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "zz") {
		return false
	}
	rest := n[len("zz"):]
	if rest == "" {
		return true
	}
	next, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLetter(next) && !unicode.IsDigit(next)
}
