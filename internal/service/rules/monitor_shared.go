// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fetchFailedRow builds the row all four EvaluateXMonitor loops (monitor_google.go,
// monitor_linkedin.go, monitor_meta.go, monitor_reddit.go) return, unevaluated, for a campaign
// whose pacing inputs cannot be trusted — not only m.FetchFailed == true, but also
// monitor_reddit.go's m.StartDate == "" branch, which reuses this same builder for a campaign
// whose metrics are real but whose flight window is unknown (see that branch's own comment).
//
// The Google branch is reachable: internal/platform/googleads.ListAccountCampaigns sets
// FetchFailed on a row whose GAQL metrics fields (impressions/clicks/costMicros) fail to parse
// (round-19 review), or whose campaign_budget.amount_micros is present but unparseable alongside
// otherwise-good metrics (round-24/25 review) — see AccountCampaignRow.FetchFailed's doc comment
// in internal/platform/googleads/monitor.go and TestListAccountCampaigns_MalformedMetrics_
// MarksFetchFailed / TestListAccountCampaigns_MalformedBudget_MarksFetchFailed in
// internal/platform/googleads/monitor_test.go.
//
// A metrics-fetch-failed row's numeric fields are left at their platform-reported zero value
// (see AccountCampaignMetrics.FetchFailed's doc comment) — running the pacing/action-item calc
// against that placeholder zero would fabricate a bogus "underspending" label and a
// HIGH-priority action item for a campaign this port never actually measured. A Google
// budget-only-failed row is different: its impressions/clicks/spend may be genuinely non-zero,
// only BudgetDailyUSD is untrusted, but the pacing calc needs BudgetDailyUSD, so the whole row
// is still routed through here rather than partially evaluated. Either way the row is still
// returned in the campaigns array, with pacing/action-item evaluation skipped — the caller sees
// it, and, for the two Google causes above, its FetchFailed flag; the Reddit empty-StartDate row
// carries PacingUnknown=true with FetchFailed left unset, since that row's metrics are real and
// only its flight window is unknown. PacingLabel keeps its zero-value "normal" placeholder —
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
	m.PacingUnknown = true
	return model.AccountMonitorRow{Metrics: m, PacingLabel: model.MonitorPacingNormal}
}
