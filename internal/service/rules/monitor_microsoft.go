// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// See monitor_google.go's package-level comment for why the per-platform monitor rule engines
// are separate from pacing.go/actions.go's single-campaign path.
//
// Unlike its four siblings, this one is NOT a port: the BFF never had a Microsoft Advertising
// monitor. Its rule set is modelled on Google's (monitor_google.go), the closest sibling — both
// are search-first platforms funded by a DAILY budget — and every threshold below that is the
// same rule as Google's carries Google's value, so an operator reading both monitors side by
// side sees one standard, not two. What genuinely differs is Microsoft's status vocabulary
// (it reports budget exhaustion and policy suspension as statuses of their own) and its shared
// budgets, which have no Google equivalent on this path.
//
// The shared pieces — pacingLabelFor, unknownPacingRow, fetchFailedRow, priorityRank/
// sortByPriority — come from monitor_shared.go. Do not copy them here: four private copies of
// the ladder and the rank is the defect linuxfoundation/lfx-self-serve#3019 records.
//
// Unlike Google, this engine does NOT apply the "zz" scratch-name filter (isScratchCampaignName).
// That filter is a BFF behaviour Google's port inherited; nothing asked for it on a platform
// the BFF never monitored, and a filtered campaign is invisible — no row, no item.

// Microsoft Advertising v13's own campaign Status literals, compared case-insensitively.
// "Deleted" is filtered out by the dispatcher before rows reach here.
const (
	msStatusActive                = "active"
	msStatusPaused                = "paused"
	msStatusBudgetPaused          = "budgetpaused"
	msStatusBudgetAndManualPaused = "budgetandmanualpaused"
	msStatusSuspended             = "suspended"
)

// Thresholds. Every one is the same rule as Google's and carries Google's value (which, for the
// low-CTR pair, LinkedIn and Reddit share too — see monitor_linkedin.go's const block).
const (
	// msPlaceholderBudgetMax is the daily budget at or below which an active campaign is treated
	// as carrying a placeholder: a 1/day budget buys too few clicks to read anything from.
	// INCLUSIVE — Google's `BudgetDay <= 1`.
	msPlaceholderBudgetMax = 1.0
	// msSearchLowCtrPct is the search CTR benchmark; search ads below 2% are mismatched to intent.
	msSearchLowCtrPct = 2.0
	// msSearchMinClicks is the click volume (EXCLUSIVE) below which a search CTR is noise.
	msSearchMinClicks = 10
	// msLowCtrPct is the non-search (Audience network) CTR benchmark.
	msLowCtrPct = 0.3
	// msMinImpressions is the impression volume (EXCLUSIVE) at which a non-search CTR means anything.
	msMinImpressions = 1000
	// msClicksNoConversions is the click volume (EXCLUSIVE) above which zero measured conversions
	// is a broken funnel rather than variance. Google's 20: same daily-budget search click volumes.
	msClicksNoConversions = 20
	// msHighAvgCpc is the average CPC (EXCLUSIVE) above which clicks are too expensive for an
	// event/awareness campaign.
	msHighAvgCpc = 5.0
	// msHighCpcMinClicks is the click volume (EXCLUSIVE) below which an average CPC is noise.
	msHighCpcMinClicks = 10
)

// msAmountPrefix prefixes every amount in an item's text. "$" for parity with the four sibling
// monitors, but the figures are in the ACCOUNT'S OWN CURRENCY as Microsoft reports it — no FX
// conversion happens anywhere on this path — so on a non-USD account the symbol is nominal.
const msAmountPrefix = "$"

func msMoney(v float64) string { return fmt.Sprintf("%s%.2f", msAmountPrefix, v) }

// EvaluateMicrosoftMonitor computes each row's pacing percentage/label and the account's action
// items for a Microsoft Advertising account.
//
// Pacing follows Google's daily model exactly (EvaluateGoogleMonitor): expected spend is
// BudgetDay * days and pacingPct = round(spend / expected * 100), placed on the shared ladder.
// That is the right model rather than Meta/LinkedIn/Reddit's flight-prorated one because
// Microsoft v13 campaigns carry DAILY budgets only — TotalBudget is always 0 and there are no
// flight dates to prorate against.
//
// A row is reported with unknown pacing (unknownPacingRow) when:
//   - FetchFailed: the dispatcher had no trustworthy metrics/budget for it. Returned via
//     fetchFailedRow with NO action items, as on every sibling.
//   - PacingUnknown arrives already set: the campaign draws on a SHARED budget, a pool spanning
//     several campaigns, so this campaign's own share is unknowable. BudgetDay is 0 there by
//     construction, not a placeholder, so the placeholder-budget rule must not fire either.
//     The budget-independent rules still run.
//   - expected <= 0: no budget (or no window) to pace against. The budget-independent rules
//     still run, and so does the placeholder-budget rule — a 0 budget on an active,
//     non-shared campaign IS the placeholder finding, at HIGH.
func EvaluateMicrosoftMonitor(rows []model.AccountCampaignMetrics, days int) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)

	for _, m := range rows {
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}

		expectedSpend := m.BudgetDay * float64(days)
		if m.PacingUnknown || expectedSpend <= 0 {
			row := unknownPacingRow(m)
			out = append(out, row)
			// Pass m, NOT row.Metrics: unknownPacingRow sets PacingUnknown on its copy, which would
			// make a plain zero-budget row indistinguishable from a shared-budget one and silence
			// the placeholder rule exactly where it should fire. m.PacingUnknown is still the
			// dispatcher's own shared-budget signal here.
			items = append(items, microsoftActionItems(m, 0, row.PacingLabel, days)...)
			continue
		}

		pacingPct := math.Round(m.Spend / expectedSpend * 100)
		label := pacingLabelFor(pacingPct)
		row := model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label}
		out = append(out, row)
		items = append(items, microsoftActionItems(m, pacingPct, label, days)...)
	}

	sortByPriority(items)
	return out, items
}

// microsoftActionItems evaluates every rule independently for one row. m must be the row as the
// dispatcher sent it, so m.PacingUnknown means "shared budget" and nothing else. label is a real
// pacing verdict only on the paced path; on the unknown path the caller passes the placeholder
// normal label, so the three pacing rules cannot fire.
func microsoftActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, days int) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	status := strings.ToLower(m.Status)
	active := status == msStatusActive
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}

	// HIGH — the campaign is not serving, or cannot meaningfully serve.
	if status == msStatusSuspended {
		add(model.MonitorPriorityHigh,
			"Campaign suspended by Microsoft — ads are not serving",
			"Review policy and billing status in Microsoft Advertising (Tools > Policy center, Billing) and resolve the suspension")
	}
	if status == msStatusBudgetPaused || status == msStatusBudgetAndManualPaused {
		issue := fmt.Sprintf("Campaign paused by Microsoft — budget exhausted (%s/day)", msMoney(m.BudgetDay))
		if status == msStatusBudgetAndManualPaused {
			issue += ", and also paused manually"
		}
		add(model.MonitorPriorityHigh, issue,
			"Raise the daily budget, or check whether an account-level budget or monthly cap has been reached")
	}
	// Shared-budget rows carry BudgetDay 0 by construction; that is not a placeholder.
	if !m.PacingUnknown && active && m.BudgetDay <= msPlaceholderBudgetMax {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Budget is %s/day — this is a placeholder and won't generate meaningful traffic", msMoney(m.BudgetDay)),
			"Set a real daily budget (typically $10-50/day for events) before expecting results")
	}

	// MED
	if status == msStatusPaused {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Campaign is paused — spent %s before pause", msMoney(m.Spend)),
			"Check if this was intentionally paused or if the event is still upcoming and needs reactivation")
	}
	if label == model.MonitorPacingUnderspending && active {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Only spending %.0f%% of %s/day budget — %s spent vs %s expected",
				pacingPct, msMoney(m.BudgetDay), msMoney(m.Spend), msMoney(m.BudgetDay*float64(days))),
			"Broaden targeting (locations, audiences), add broad match keywords, or increase bids to win more auctions")
	}
	if label == model.MonitorPacingConstrained && active {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Spending %.0f%% of budget — demand exceeds %s/day cap, ads stop showing mid-day", pacingPct, msMoney(m.BudgetDay)),
			"Increase daily budget to capture missed impressions, or narrow targeting to focus spend on highest-value audiences")
	}
	if label == model.MonitorPacingOverspending && active {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Spending %.0f%% of budget — %s spent vs %s expected at %s/day; Microsoft can deliver up to 2x the daily budget on a single day",
				pacingPct, msMoney(m.Spend), msMoney(m.BudgetDay*float64(days)), msMoney(m.BudgetDay)),
			"Confirm the overdelivery is acceptable; lower the daily budget or bids if the window's total spend must stay on plan")
	}
	if m.IsSearchChannel && m.Ctr < msSearchLowCtrPct && m.Clicks > msSearchMinClicks {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Search CTR is %.2f%% (benchmark: 2%%+) - %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Improve headline relevance to search intent, add negative keywords to filter irrelevant queries")
	}
	if !m.IsSearchChannel && m.Ctr < msLowCtrPct && m.Impressions > msMinImpressions {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Non-search CTR is %.2f%% (benchmark: 0.3%%+) - %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Refresh creative assets, check for audience overlap across campaigns, or narrow audience targeting")
	}
	if m.Clicks > msClicksNoConversions && m.Conversions != nil && *m.Conversions == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d clicks (%s spent) but 0 conversions — traffic is not converting", m.Clicks, msMoney(m.Spend)),
			"Verify the UET tag and conversion goals are firing correctly, check landing page load speed, and review if the CTA matches the ad promise")
	}
	if m.Clicks > msHighCpcMinClicks {
		if avgCpc := m.Spend / float64(m.Clicks); avgCpc > msHighAvgCpc {
			add(model.MonitorPriorityMed,
				fmt.Sprintf("Avg CPC is %s — spending %s for only %d clicks", msMoney(avgCpc), msMoney(m.Spend), m.Clicks),
				"Switch to a Target CPA or Maximize Clicks bid strategy, add long-tail keywords with lower competition")
		}
	}
	if m.Impressions > 0 && m.Clicks == 0 {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("%d impressions but 0 clicks — ads are showing but no one is clicking", m.Impressions),
			"Rewrite ad copy to be more compelling, ensure headlines match search intent, test different CTAs")
	}
	return items
}
