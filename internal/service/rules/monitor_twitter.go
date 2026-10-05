// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// See monitor_google.go's package-level comment for why the per-platform monitor rule engines
// are separate from pacing.go/actions.go's single-campaign path.
//
// Like Microsoft's, this one is NOT a port: the BFF never had an X monitor. Every threshold is a
// value a sibling already uses, under its own name here so a reader sees which sibling's rule it
// is. The shared pieces — pacingLabelFor, unknownPacingRow, fetchFailedRow, sortByPriority — come
// from monitor_shared.go and must not be copied.
//
// No clicks-without-conversions rule: X reports conversions only per event type, under metric
// groups the monitor does not request, and reports nothing at all for an account with no
// conversion tag — indistinguishable from a measured zero — so the dispatcher leaves Conversions
// nil on every row and such a rule could never fire honestly.

// X's own entity_status literals (https://docs.x.com/x-ads-api/campaign-management). DRAFT is
// excluded by the dispatcher's with_draft=false, and deleted campaigns by with_deleted=false.
const (
	xStatusActive = "ACTIVE"
	xStatusPaused = "PAUSED"
)

const (
	// xPlaceholderBudgetMax is Google's and Microsoft's placeholder rule: a daily budget at or
	// below 1 (INCLUSIVE) buys too little delivery to read anything from.
	xPlaceholderBudgetMax = 1.0
	// xLowCtrPct / xMinImpressions are the low-CTR pair Google (non-search), LinkedIn, Reddit
	// and Microsoft (non-search) share: below 0.3% once impressions exceed 1000 (EXCLUSIVE).
	// Meta's higher pair is specific to Meta's CTR baseline; X's promoted posts sit with the
	// other social placements, not with Meta.
	xLowCtrPct      = 0.3
	xMinImpressions = 1000
)

// xAmountPrefix prefixes every amount in an item's text. "$" for parity with the sibling
// monitors, but the figures are in the ACCOUNT'S OWN CURRENCY as X reports it (*_local_micro) —
// nothing on this path converts — so on a non-USD account the symbol is nominal.
const xAmountPrefix = "$"

func xMoney(v float64) string { return fmt.Sprintf("%s%.2f", xAmountPrefix, v) }

// EvaluateTwitterMonitor computes each row's pacing percentage/label and the account's action
// items for an X Ads account.
//
// windowStart / windowEnd are the calendar window the metrics were measured over: the saved
// report's first and last day IN THE ACCOUNT'S TIMEZONE, as UTC-midnight dates
// (model.ReportedAccountRead.MetricsWindowStart/End). The rules evaluate on those days, never on
// a "today" derived from the service's clock: X's stats jobs and the line items' flight dates are
// both in the account's timezone, so a UTC "today" would disagree with them by a day for part of
// every day on any non-UTC account (see twitterWindow).
//
// Pacing (twitterPacingPct):
//   - a DAILY budget is paced as BudgetDay × the days of the report window the flight covers;
//   - otherwise a TOTAL budget is prorated over the flight and paced over the days of the window
//     the flight covers;
//   - otherwise, or when either computation has nothing to measure against, the row is
//     unknownPacingRow.
//
// With no window (nil or unusable bounds) the window-dependent judgements — pacing and the
// zero-delivery rule — are skipped: every row is unknownPacingRow and only the window-free rules
// run. In practice the orchestrator supplies a window whenever a finished report exists, and
// without one every row is FetchFailed anyway.
//
// A FetchFailed row (unreadable budget or flight, or no finished report yet) is returned via
// fetchFailedRow with no action items, as on every sibling.
func EvaluateTwitterMonitor(rows []model.AccountCampaignMetrics, windowStart, windowEnd *time.Time) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
	out := make([]model.AccountMonitorRow, 0, len(rows))
	items := make([]model.AccountMonitorActionItem, 0)
	win, hasWindow := twitterWindow(windowStart, windowEnd)

	for _, m := range rows {
		if m.FetchFailed {
			out = append(out, fetchFailedRow(m))
			continue
		}
		var pacingPct float64
		computable := false
		if hasWindow {
			pacingPct, computable = twitterPacingPct(m, win)
		}
		if !computable {
			row := unknownPacingRow(m)
			out = append(out, row)
			items = append(items, twitterActionItems(m, 0, row.PacingLabel, win, hasWindow)...)
			continue
		}
		label := pacingLabelFor(pacingPct)
		out = append(out, model.AccountMonitorRow{Metrics: m, PacingPct: pacingPct, PacingLabel: label})
		items = append(items, twitterActionItems(m, pacingPct, label, win, hasWindow)...)
	}

	sortByPriority(items)
	return out, items
}

// xWindow is a report window as UTC-midnight instants, [start, end) with end EXCLUSIVE — the
// convention the Reddit monitor settled on, so a whole-window daily-budget flight is
// BudgetDay × days exactly.
type xWindow struct{ start, end time.Time }

// twitterWindow turns the report's first and last calendar day (account-local days carried as
// UTC-midnight dates) into [first day, the midnight after the last day). Because the flight
// dates (twitterFlight) are account-local calendar days carried the same way, comparing the two
// is a comparison of the account's own days — no timezone is needed here and none is guessed.
// Reports false for a missing, zero or inverted window.
func twitterWindow(firstDay, lastDay *time.Time) (xWindow, bool) {
	if firstDay == nil || lastDay == nil || firstDay.IsZero() || lastDay.IsZero() {
		return xWindow{}, false
	}
	f, l := firstDay.UTC(), lastDay.UTC()
	start := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.UTC)
	last := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	if last.Before(start) {
		return xWindow{}, false
	}
	return xWindow{start: start, end: last.AddDate(0, 0, 1)}, true
}

// twitterFlight returns the flight as UTC instants, [StartDate, the midnight after EndDate);
// either bound is zero when unknown (EndDate empty means open-ended).
func twitterFlight(m model.AccountCampaignMetrics) (start, end time.Time) {
	start = parseMonitorDate(m.StartDate)
	if e := parseMonitorDate(m.EndDate); !e.IsZero() {
		end = e.AddDate(0, 0, 1)
	}
	return start, end
}

// windowFlightDays is the number of whole days of [ws, we) the flight [fs, fe) covers. A zero
// flight bound is open on that side.
func windowFlightDays(ws, we, fs, fe time.Time) float64 {
	if !fs.IsZero() && fs.After(ws) {
		ws = fs
	}
	if !fe.IsZero() && fe.Before(we) {
		we = fe
	}
	if !we.After(ws) {
		return 0
	}
	return math.Ceil(we.Sub(ws).Hours() / 24)
}

// twitterPacingPct returns the pacing percentage and whether it means anything.
//
// Daily budget first. X campaigns can carry both; the daily cap is the per-day plan the account's
// serving actually follows, and it needs no flight end — while a total budget prorated over a
// flight assumes even delivery X does not promise. Only a campaign with no daily budget is paced
// on its total.
//
//   - Daily: expected = BudgetDay × days of the window the flight covers. A flight with no known
//     start (no line items) is treated as covering the window from its start: a campaign with no
//     line item cannot serve, so the zero-delivery rule — not pacing — is what speaks to it, and
//     a missing start cannot be allowed to shrink the expectation to nothing.
//   - Total: needs BOTH flight bounds (a lifetime budget over an open-ended or unscheduled flight
//     has no per-day share). expected = TotalBudget / flight days × days of the window the
//     flight covers.
//
// No overlap between the flight and the window means the campaign was not scheduled to spend in
// it, so there is nothing to compare its spend against: not computable.
func twitterPacingPct(m model.AccountCampaignMetrics, w xWindow) (float64, bool) {
	ws, we := w.start, w.end
	fs, fe := twitterFlight(m)
	switch {
	case m.BudgetDay > 0:
		covered := windowFlightDays(ws, we, fs, fe)
		if covered <= 0 {
			return 0, false
		}
		return math.Round(m.Spend / (m.BudgetDay * covered) * 100), true
	case m.TotalBudget > 0:
		if fs.IsZero() || fe.IsZero() || !fe.After(fs) {
			return 0, false
		}
		flightDays := math.Ceil(fe.Sub(fs).Hours() / 24)
		covered := windowFlightDays(ws, we, fs, fe)
		if covered <= 0 {
			return 0, false
		}
		return math.Round(m.Spend / (m.TotalBudget / flightDays * covered) * 100), true
	default:
		return 0, false
	}
}

// twitterScheduledInWindow reports whether the campaign's flight overlaps the window, treating
// an unknown start as "scheduled" (see twitterPacingPct) — the gate for the zero-delivery rule,
// so a campaign whose flight ended before the window, or starts after it, is not reported as
// failing to deliver.
func twitterScheduledInWindow(m model.AccountCampaignMetrics, w xWindow) bool {
	fs, fe := twitterFlight(m)
	return windowFlightDays(w.start, w.end, fs, fe) > 0
}

// twitterActionItems evaluates every rule independently for one row. label is a real verdict
// only on the paced path; on the unknown path the caller passes the placeholder normal label, so
// the pacing rules cannot fire. hasWindow false skips the zero-delivery rule, which needs the
// window to know whether the campaign was scheduled at all.
func twitterActionItems(m model.AccountCampaignMetrics, pacingPct float64, label model.MonitorPacingLabel, w xWindow, hasWindow bool) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: m.PlatformCampaignID, CampaignName: m.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}
	active := m.Status == xStatusActive
	zeroDelivery := m.Impressions == 0 && m.Clicks == 0

	// HIGH
	if active && zeroDelivery && hasWindow && twitterScheduledInWindow(m, w) {
		add(model.MonitorPriorityHigh,
			"Campaign is active but delivered nothing in the window — 0 impressions and 0 clicks",
			"Check the line items are active and scheduled, the promoted posts are approved, and the funding instrument has budget in X Ads Manager")
	}
	if active && m.BudgetDay > 0 && m.BudgetDay <= xPlaceholderBudgetMax {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Budget is %s/day — this is a placeholder and won't generate meaningful traffic", xMoney(m.BudgetDay)),
			"Set a real daily budget (typically $10-50/day for events) before expecting results")
	}

	// MED
	if m.Status == xStatusPaused {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Campaign is paused — spent %s in the window", xMoney(m.Spend)),
			"Check if this was intentionally paused or if the event is still upcoming and needs reactivation")
	}
	// Not when the zero-delivery item above already fired for a campaign that spent nothing:
	// "Underspending at 0%" restates it. A campaign with delivery but no spend (impressions, no
	// billable clicks) still gets this item — the Reddit monitor's settled rule.
	if label == model.MonitorPacingUnderspending && active && !(zeroDelivery && pacingPct == 0) {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Underspending at %.0f%% of budget — %s spent in the window", pacingPct, xMoney(m.Spend)),
			"Broaden targeting (keywords, follower look-alikes, locations) or raise the bid so the line items win more auctions")
	}
	if (label == model.MonitorPacingConstrained || label == model.MonitorPacingOverspending) && active {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Budget %s: %.0f%% of budget used", label, pacingPct),
			"Increase the budget to capture missed impressions, or narrow targeting to focus spend on the highest-value audiences")
	}
	if m.Impressions > xMinImpressions && m.Ctr < xLowCtrPct {
		add(model.MonitorPriorityMed,
			fmt.Sprintf("Low CTR at %.2f%% — %d clicks from %d impressions", m.Ctr, m.Clicks, m.Impressions),
			"Refresh the promoted posts' creative, test a different call to action, or narrow targeting")
	}
	return items
}
