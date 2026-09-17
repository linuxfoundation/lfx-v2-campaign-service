// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fetchFailedRow builds the row all four EvaluateXMonitor loops (monitor_google.go,
// monitor_linkedin.go, monitor_meta.go, monitor_reddit.go) return, unevaluated, for a campaign
// whose per-campaign metrics fetch failed upstream (m.FetchFailed == true).
//
// A FetchFailed row's numeric fields are left at their platform-reported zero value (see
// AccountCampaignMetrics.FetchFailed's doc comment) — running the pacing/action-item calc
// against that placeholder zero would fabricate a bogus "underspending" label and a
// HIGH-priority action item for a campaign this port never actually measured. The row is still
// returned in the campaigns array (the caller sees it and its FetchFailed flag), just with
// pacing/action-item evaluation skipped. PacingLabel keeps its zero-value "normal" placeholder —
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
