// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"
	"math"
	"sort"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The account-monitor pacing ladder is StandardLadder (50/90/100) in ladder.go — the same ladder
// the brief view places on (D2), through the same PacingLadder.Label.
//
// These monitors used to carry four private copies — googlePacing*, linkedinPacing*, metaPacing*
// and redditPacing* — carrying identical numbers under names that disagreed about what the
// numbers MEANT: Google called its 100 "overspending" and its 90 "constrainedFrom", while the
// other three called 100 "constrained" and 90 "normal". Same ladder, four vocabularies, so the
// copies read as four different rules and any future edit to one looked local. They became one
// const block and switch here, and that switch then became StandardLadder, so the ladder
// arithmetic now exists once for both read paths.
//
// On the BFF side the split is real and is what linuxfoundation/lfx-self-serve#3019 records:
// meta-ads.service.ts and linkedin-ads.service.ts read the shared CAMPAIGN_PACING_THRESHOLDS,
// while campaign-metrics.service.ts (Google) and reddit-ads.service.ts hardcode the same numbers
// locally, so an edit to the shared constant moves two platforms and silently leaves two behind.

// pacingLabelFor places a prorated spend percentage on StandardLadder.
//
// It answers for the NUMBER only. Whether a campaign has a pacing figure worth placing at all is
// each platform's own question — the guards differ because the platforms report budget
// differently, not because the bands do — so a caller with no trustworthy budget must not reach
// here at all. See each EvaluateXMonitor for the guard it applies first.
//
// It maps onto the account-monitor label enum (model.MonitorPacingLabel), which has no unknown
// member, and keeps this path's historical NaN-is-normal placement — see the guard below.
func pacingLabelFor(pct float64) model.MonitorPacingLabel {
	// NaN is normal on this path, as it always was: the former switch tested `<` and two `>`
	// comparisons, all false for NaN, and fell through to its normal default. PacingLadder.Label
	// tests `<=` and falls through to overspending instead, so without this guard a NaN spend
	// reported by a platform (math.Round keeps NaN) would turn from "normal" into an
	// "overspending" label and its action item. Kept as found; whether NaN should reach the
	// ladder at all is a separate question.
	if math.IsNaN(pct) {
		return model.MonitorPacingNormal
	}
	switch StandardLadder().Label(pct) {
	case PacingUnderspending:
		return model.MonitorPacingUnderspending
	case PacingConstrained:
		return model.MonitorPacingConstrained
	case PacingOverspending:
		return model.MonitorPacingOverspending
	default:
		return model.MonitorPacingNormal
	}
}

// unknownPacingRow is the row for a campaign whose pacing percentage cannot be computed at all —
// no usable budget, or no flight to prorate one against.
//
// It sets PacingUnknown, which model.AccountCampaignMetrics documents as the "absent, not
// defaulted" contract: a consumer must render this as unknown rather than as a number. The label
// is MonitorPacingNormal because the label enum has no unknown member on this path and normal is
// the only band that asserts nothing actionable; PacingUnknown is what carries the meaning, and a
// consumer that reads the label without it will report "on plan" for a campaign nobody can pace.
//
// PacingPct is deliberately left at its zero value rather than carrying a computed 0. The
// distinction is the whole point: 0 means "spent nothing against a real budget", which is a
// finding; unknown means "there is no budget to have spent against", which is not.
func unknownPacingRow(m model.AccountCampaignMetrics) model.AccountMonitorRow {
	m.PacingUnknown = true
	return model.AccountMonitorRow{Metrics: m, PacingLabel: model.MonitorPacingNormal}
}

// fetchFailedRow is unknownPacingRow for one specific cause: the dispatcher could not trust the
// row's own fields. A fetch failure is a reason pacing is unknown, not a separate state.
//
// The Google branch is reachable: internal/platform/googleads.ListAccountCampaigns sets
// FetchFailed on a row whose GAQL metrics fields (impressions/clicks/costMicros) fail to parse,
// and on one whose campaign_budget.amount_micros is present but unparseable alongside
// otherwise-good metrics — see AccountCampaignRow.FetchFailed's doc comment in
// internal/platform/googleads/monitor.go, and TestListAccountCampaigns_MalformedMetrics_
// MarksFetchFailed / TestListAccountCampaigns_MalformedBudget_MarksFetchFailed in
// internal/platform/googleads/monitor_test.go, which pin both causes.
//
// A metrics-fetch-failed row's numeric metrics fields are not necessarily zero. They may be a
// zero placeholder, or partial delivery: for example, where a platform sums repeated upstream
// rows for one campaign (Google Ads, should it return more than one), or where one field fails
// while the others parse (LinkedIn, whose unreadable costInLocalCurrency leaves Spend=0 beside
// real impressions/clicks). See AccountCampaignMetrics.FetchFailed's doc comment. Running the
// pacing/action-item calc against any of these would produce an unreliable pacing label or
// action item for a campaign this port never fully measured. A row whose
// budget alone failed to parse is different — on ANY platform that reports a budget, not just
// Google Ads: its impressions/clicks/spend may be genuinely non-zero and only BudgetDay (or
// TotalBudget) is untrusted, but those are exactly what the pacing calc divides by, so the
// whole row is still routed through here rather than partially evaluated. Either way the row
// is still returned in the campaigns array, with pacing/action-item evaluation skipped — the
// caller sees it, and its FetchFailed flag. PacingLabel keeps its zero-value "normal" placeholder —
// pacing_label is a required enum with no "unknown" member — and PacingUnknown=true is the
// caller's signal not to trust it, the same convention pacing_pct's own "meaningless when
// pacing_unknown is true" doc comment establishes (design/connection.go).
//
// This is a contract-level rule shared by every platform, not a per-platform quirk — kept here,
// alongside the other cross-platform helpers, rather than duplicated across the four
// monitor_*.go files, so a fifth platform ported later cannot silently omit it the way round 2
// of this branch's review found all four had. Living in a per-platform file (it was originally
// in monitor_google.go) misrepresented that: this package's monitor_*.go files are otherwise
// strictly per-platform, mirroring the pacing.go/actions.go/window.go split between shared and
// per-platform logic.
func fetchFailedRow(m model.AccountCampaignMetrics) model.AccountMonitorRow {
	return unknownPacingRow(m)
}

// priorityRank orders action items for display: HIGH first, then MED, then LOW, then anything
// unrecognised. It matches the BFF's generateActionItems sort key (`{ HIGH: 0, MED: 1, LOW: 2 }`
// with a `?? 3` fallback).
//
// This used to be four per-platform copies, of which LinkedIn's was wrong: it spelled the middle
// case "MEDIUM" while model.MonitorPriorityMed is "MED", so no MED item ever matched and every
// one fell through to the unranked bucket — sorting MED items BEHIND LOW ones, which is the
// reverse of the intended order and the exact opposite of what an operator triaging a list
// needs. linuxfoundation/lfx-self-serve#3018.
//
// Keeping one function is most of the fix. Four copies of a four-line switch is how one of them
// got to be wrong for as long as it was: nothing about linkedinPriorityRank looked broken on its
// own, and telling it apart from its three correct siblings meant reading all four side by side.
func priorityRank(p model.MonitorPriority) int {
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

// sortByPriority is the stable sort all four monitors apply to their action items.
// sort.SliceStable matches Array.prototype.sort's stability the BFF relies on, in O(n log n)
// rather than the O(n²) insertion sort this used to run.
func sortByPriority(items []model.AccountMonitorActionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return priorityRank(items[i].Priority) < priorityRank(items[j].Priority)
	})
}

// accountMoney renders an amount for action-item text, shared by every ad-platform monitor so
// none of them can drift back to a symbol (#435). The figures are the connected ad account's
// own, in its currency: no FX conversion happens on any monitor path, and the row does not carry
// the currency code, so a "$" would call a EUR, INR or yen amount dollars. The text names the
// unit instead.
func accountMoney(v float64) string { return fmt.Sprintf("%.2f in account currency", v) }

// The two monitor thresholds that are amounts of money, written in US dollars. Every other
// monitor threshold is a percentage or a count, which no currency changes.
const (
	// placeholderBudgetMaxUSD is the daily budget (INCLUSIVE) at or below which an active
	// campaign is treated as carrying a placeholder: about a dollar a day buys too few clicks to
	// read anything from.
	placeholderBudgetMaxUSD = 1.0
	// highAvgCpcUSD is the average CPC (EXCLUSIVE) above which clicks are too expensive for an
	// event/awareness campaign.
	highAvgCpcUSD = 5.0
)

// monitorUnitsPerUSD is roughly how many units of each currency one US dollar is worth, used only
// to size placeholderBudgetMaxUSD and highAvgCpcUSD in the ad account's own currency (#454).
// Before it, both were compared with the account-currency amount as if it were dollars, so a
// JPY account tripped the high-CPC rule on a ¥50 click and a KWD account's 1/day budget (about
// $3) was flagged as a placeholder.
//
// These are magnitudes, not rates: no amount is converted or shown with them, and the two rules
// they size are heuristics whose USD values are themselves round numbers. Rounded mid-2026
// values; an entry needs revisiting only when its currency moves by a large fraction, which is
// why currencies with persistently high inflation (ARS, TRY, NGN, EGP and the like) are left
// out rather than given a figure that would go stale.
//
// A currency missing from this table, or an account whose currency was not reported (""), has
// no size for either rule, so neither fires on an amount there — see isPlaceholderBudget and
// isHighAvgCpc. A threshold that cannot be sized is not guessed at in dollars: that guess is
// exactly the misfire this table exists to remove.
var monitorUnitsPerUSD = map[string]float64{
	"USD": 1, "PAB": 1,
	"EUR": 0.87, "GBP": 0.75, "CHF": 0.8, "CAD": 1.38, "AUD": 1.53, "NZD": 1.7,
	"SEK": 9.6, "NOK": 10.1, "DKK": 6.4, "PLN": 3.7, "CZK": 21, "HUF": 345, "RON": 4.4,
	"ILS": 3.4, "ZAR": 17.8,
	"JPY": 147, "KRW": 1390, "CNY": 7.15, "HKD": 7.8, "TWD": 30, "SGD": 1.29, "INR": 87,
	"THB": 32.5, "MYR": 4.25, "PHP": 57, "IDR": 16300, "VND": 26000,
	"MXN": 18.7, "BRL": 5.5, "CLP": 950, "COP": 4000, "PEN": 3.6,
	"AED": 3.6725, "SAR": 3.75, "QAR": 3.64, "KWD": 0.307, "BHD": 0.376, "OMR": 0.3845, "JOD": 0.709,
}

// monitorMinorUnitDigits is the ISO 4217 minor unit of each monitorUnitsPerUSD currency that does
// not use two decimals. The money rules compare at this precision (moneyAt), so a threshold sized
// in floating point cannot sit a hair off the amount it means: 5 × 1.38 is 6.8999… in float64,
// which without rounding called a CAD 6.90 click above a CAD 6.90 threshold.
var monitorMinorUnitDigits = map[string]int{
	"JPY": 0, "KRW": 0, "CLP": 0, "VND": 0,
	"KWD": 3, "BHD": 3, "OMR": 3, "JOD": 3,
}

// moneyAt rounds v to currency's minor unit: two decimals unless monitorMinorUnitDigits says
// otherwise.
func moneyAt(v float64, currency string) float64 {
	digits, ok := monitorMinorUnitDigits[currency]
	if !ok {
		digits = 2
	}
	scale := math.Pow10(digits)
	return math.Round(v*scale) / scale
}

// accountThreshold sizes a USD-denominated threshold in currency, the ad account's ISO 4217 code,
// rounded to that currency's minor unit (moneyAt). ok is false when the currency has no entry in
// monitorUnitsPerUSD, "" included.
func accountThreshold(usd float64, currency string) (threshold float64, ok bool) {
	units, ok := monitorUnitsPerUSD[currency]
	if !ok {
		return 0, false
	}
	return moneyAt(usd*units, currency), true
}

// isPlaceholderBudget reports whether a daily budget in currency is a placeholder: at or below
// placeholderBudgetMaxUSD sized in that currency, both compared at the currency's minor unit. A
// budget of zero or less is a placeholder in every currency, so it is one even when the currency
// cannot be sized.
func isPlaceholderBudget(budgetDay float64, currency string) bool {
	if budgetDay <= 0 {
		return true
	}
	limit, ok := accountThreshold(placeholderBudgetMaxUSD, currency)
	return ok && moneyAt(budgetDay, currency) <= limit
}

// isHighAvgCpc reports whether an average CPC in currency is above highAvgCpcUSD sized in that
// currency, both compared at the currency's minor unit — the precision the item prints it at. It
// is never high in a currency that cannot be sized.
func isHighAvgCpc(avgCpc float64, currency string) bool {
	limit, ok := accountThreshold(highAvgCpcUSD, currency)
	return ok && moneyAt(avgCpc, currency) > limit
}

// moneyRulesSkippedGoogleMicrosoft is moneyRulesSkippedItem's subject for Google and Microsoft,
// which still flag a zero daily budget whatever the currency.
const moneyRulesSkippedGoogleMicrosoft = "placeholder-budget and high-CPC checks on non-zero amounts were skipped (a zero daily budget is still flagged)"

// moneyRulesSkippedItem is the account-wide LOW item that says the money-sized rules were not
// run, so a caller can tell "no placeholder budget or expensive click" apart from "these could
// not be checked" (#454 review). It is due when currency cannot be sized (absent, or not in
// monitorUnitsPerUSD) and at least one returned row was measured — a row the rules would have
// looked at. skipped is the issue text's subject, naming the rules the platform runs and what of
// them still ran: on Google and Microsoft a zero daily budget is still flagged in any currency,
// so the text must not say the placeholder check was skipped outright. It never assumes USD
// in their place: that guess is the misfire the sizing removed.
//
// It goes on an absent currency as well as an unlisted one: on Microsoft and X the currency comes
// from a best-effort side read that can time out, and on Google from every row carrying the same
// customer.currency_code, so a USD account can reach here on one request and not the next.
func moneyRulesSkippedItem(currency, skipped string, rows []model.AccountMonitorRow) (model.AccountMonitorActionItem, bool) {
	if _, ok := accountThreshold(1, currency); ok {
		return model.AccountMonitorActionItem{}, false
	}
	measured := false
	for _, r := range rows {
		if !r.Metrics.FetchFailed {
			measured = true
			break
		}
	}
	if !measured {
		return model.AccountMonitorActionItem{}, false
	}
	issue := fmt.Sprintf("The %s — the ad account's currency could not be read, so the thresholds cannot be sized", skipped)
	action := "Reload the monitor to retry the currency read, or review those figures in the ad platform directly"
	if currency != "" {
		issue = fmt.Sprintf("The %s — the ad account's currency (%s) is not one this service sizes the thresholds in", skipped, currency)
		action = "Review those figures in the ad platform directly"
	}
	return model.AccountMonitorActionItem{Priority: model.MonitorPriorityLow, Issue: issue, Action: action}, true
}
