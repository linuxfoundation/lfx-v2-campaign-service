// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"math"
	"sort"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The account-monitor pacing ladder is AccountMonitorLadder (50/90/100) in ladder.go, placed by
// the same PacingLadder.Label the brief view uses with BriefViewLadder.
//
// These monitors used to carry four private copies — googlePacing*, linkedinPacing*, metaPacing*
// and redditPacing* — carrying identical numbers under names that disagreed about what the
// numbers MEANT: Google called its 100 "overspending" and its 90 "constrainedFrom", while the
// other three called 100 "constrained" and 90 "normal". Same ladder, four vocabularies, so the
// copies read as four different rules and any future edit to one looked local. They became one
// const block and switch here, and that switch then became AccountMonitorLadder, so the ladder
// arithmetic now exists once for both read paths.
//
// On the BFF side the split is real and is what linuxfoundation/lfx-self-serve#3019 records:
// meta-ads.service.ts and linkedin-ads.service.ts read the shared CAMPAIGN_PACING_THRESHOLDS,
// while campaign-metrics.service.ts (Google) and reddit-ads.service.ts hardcode the same numbers
// locally, so an edit to the shared constant moves two platforms and silently leaves two behind.

// pacingLabelFor places a prorated spend percentage on AccountMonitorLadder.
//
// It answers for the NUMBER only. Whether a campaign has a pacing figure worth placing at all is
// each platform's own question — the guards differ because the platforms report budget
// differently, not because the bands do — so a caller with no trustworthy budget must not reach
// here at all. See each EvaluateXMonitor for the guard it applies first.
//
// Deliberately AccountMonitorLadder, NOT BriefViewLadder: which is correct is open (D2), and
// routing the account-monitor path through the brief view's ladder would move every platform's
// alerting bands as a side effect of a refactor.
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
	switch AccountMonitorLadder.Label(pct) {
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
// A metrics-fetch-failed row's numeric fields are left at their platform-reported zero value
// (see AccountCampaignMetrics.FetchFailed's doc comment) — running the pacing/action-item calc
// against that placeholder zero would fabricate a bogus "underspending" label and a
// HIGH-priority action item for a campaign this port never actually measured. A Google
// budget-only-failed row is different: its impressions/clicks/spend may be genuinely non-zero,
// only BudgetDailyUSD is untrusted, but the pacing calc needs BudgetDailyUSD, so the whole row
// is still routed through here rather than partially evaluated. Either way the row is still
// returned in the campaigns array, with pacing/action-item evaluation skipped — the caller sees
// it, and its FetchFailed flag. PacingLabel keeps its zero-value "normal" placeholder —
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
