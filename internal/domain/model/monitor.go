// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// This file ports the "monitor" read path from the lfx-self-serve BFF
// (campaign-metrics.service.ts / linkedin-ads.service.ts / meta-ads.service.ts /
// reddit-ads.service.ts) into this service's domain model. It is a faithful PORT, not a
// redesign: the BFF computed rule-engine action items per platform, each with its own
// pacing thresholds (one shared ladder in internal/service/rules/monitor_shared.go), and
// this type is the account-scoped shape that
// carries that computation's inputs and outputs across the dispatcher/orchestrator/service
// boundary the same way CampaignMetrics does for the single-campaign read path.

// MonitorPriority is the three-value priority band the per-platform rule engines assign to a
// generated action item. Kept as a string type (not reusing any Go-side enum) because these
// are the BFF's own wire values. All four platforms order them through one shared
// priorityRank (internal/service/rules/monitor_shared.go); LinkedIn's MED-vs-MEDIUM sort-key
// mismatch, which sorted MED items behind LOW ones, was fixed rather than ported
// (linuxfoundation/lfx-self-serve#3018).
type MonitorPriority string

const (
	MonitorPriorityHigh MonitorPriority = "HIGH"
	MonitorPriorityMed  MonitorPriority = "MED"
	MonitorPriorityLow  MonitorPriority = "LOW"
)

// MonitorPacingLabel is the pacing classification derived from a campaign's spend-vs-budget
// ratio. All four platforms compute it through one shared pacingLabelFor
// (internal/service/rules/monitor_shared.go) over the same 50/90/100 ladder, so this type is
// shared vocabulary AND shared thresholds (linuxfoundation/lfx-self-serve#3019).
type MonitorPacingLabel string

const (
	MonitorPacingNormal        MonitorPacingLabel = "normal"
	MonitorPacingUnderspending MonitorPacingLabel = "underspending"
	MonitorPacingConstrained   MonitorPacingLabel = "constrained"
	MonitorPacingOverspending  MonitorPacingLabel = "overspending"
)

// AccountCampaignMetrics is one campaign row read live from a raw ad account (not from this
// service's own persisted Campaign rows) — the input the monitor rule engines act on.
//
// This is a DIFFERENT read path from CampaignMetrics: CampaignMetrics reads metrics for a
// campaign this service created and tracks; AccountCampaignMetrics reads EVERY campaign
// visible on the connected ad account, whether or not this service ever created it. That is
// why it carries its own name/status/budget fields rather than joining against a stored
// model.Campaign row.
type AccountCampaignMetrics struct {
	// PlatformCampaignID is the id the platform assigned to this campaign.
	PlatformCampaignID string
	// Name is the campaign's platform-side name, unparsed.
	Name string
	// Status is the platform's own status string (e.g. "ENABLED"/"PAUSED" for Google,
	// "ACTIVE"/"PAUSED" for LinkedIn/Reddit, "ACTIVE"/"PAUSED" for Meta), passed through
	// verbatim rather than normalized to this service's own Campaign.Status vocabulary —
	// the monitor rule engines compare against the platform's own literal strings, exactly
	// as the BFF did.
	Status string
	// Spend is total cost in the account's currency over the requested window.
	Spend       float64
	Impressions int64
	Clicks      int64
	// Ctr is Clicks/Impressions * 100, 0 when Impressions is 0.
	Ctr float64
	// Conversions is a pointer for the same reason model.CampaignMetrics.Conversions is: nil
	// means this platform/row could not measure conversions, a non-nil 0 is a measurement.
	// Reddit leaves it nil on every row: its monitor read never asks for conversions, so it
	// has nothing to report. The port originally set a non-nil 0 there, copying the BFF
	// (linuxfoundation/lfx-self-serve#3020) — a measurement claim nothing behind it could
	// support.
	Conversions *float64
	// BudgetDay is the daily budget in the account's currency, 0 when the campaign has none
	// (e.g. a LinkedIn/Meta campaign funded by TotalBudget instead).
	BudgetDay float64
	// TotalBudget is the lifetime/total budget in the account's currency, 0 when the
	// campaign is funded by BudgetDay instead.
	TotalBudget float64
	// StartDate/EndDate are the campaign's flight dates in the platform's own representation,
	// RFC 3339 date-only (YYYY-MM-DD), empty when the platform did not report one.
	StartDate string
	EndDate   string
	// PacingUnknown is true when pacing could not be computed at all: either the flight dates
	// were unavailable (e.g. Google Ads rows this port does not schedule-bound, or a platform
	// row with no runSchedule/start_time), or the campaign has no usable budget to pace
	// against. A rule engine MUST NOT compute a pacing percentage
	// against a fabricated flight window when this is true — it must report pacing as
	// unknown, mirroring the same "absent, not defaulted" contract as
	// CampaignSettingsReadback's `unknown` verdict.
	PacingUnknown bool
	// IsSearchChannel is Google Ads only: true when campaign.advertising_channel_type is
	// SEARCH (false for DEMAND_GEN, PERFORMANCE_MAX, VIDEO, DISPLAY and every other platform). The BFF derives
	// its equivalent "isSearch" flag by parsing the campaign NAME through a naming-convention parser
	// (parseCampaignName(name).adFormat.toLowerCase().includes('search')) — this port reads
	// the channel type field directly instead of re-implementing that naming convention
	// parser. This is a disclosed, deliberate deviation (not a preserved bug): for any
	// campaign actually named per the convention the two derivations agree, and this port
	// avoids depending on an unported string-parsing helper for a boolean that GAQL already
	// answers authoritatively. Always false for LinkedIn/Meta/Reddit rows.
	IsSearchChannel bool
	// FetchFailed marks a row some part of whose upstream data could not be trusted: either a
	// per-campaign metrics call to the platform failed outright (e.g. LinkedIn's per-campaign
	// creative-analytics fetch, or Reddit's per-campaign report fetch), in which case the
	// numeric metrics fields are left at their zero value, or (Google Ads only) a
	// present-but-unparseable campaign_budget.amount_micros was read alongside otherwise-good
	// metrics (round-24/25 review — internal/platform/googleads/monitor.go's microsToUSD), in
	// which case Impressions/Clicks/SpendUSD may be genuinely non-zero while BudgetDailyUSD is
	// the untrusted one. Either way, this is the explicit status marker the migration spec
	// calls for: a consumer can tell "the platform authoritatively reported zero" from "this
	// service could not read this row's data" — the same distinguishability
	// CampaignSettingsReadback's `unknown` verdict exists to preserve. Renderers MUST check
	// this before treating any of this row's fields, zero or not, as a fully trusted reading.
	FetchFailed bool
	// CampaignURL is Google Ads only: a direct link to the campaign in the Google Ads UI,
	// matching the BFF's buildGoogleAdsUrl(campaignId) =
	// "https://ads.google.com/aw/campaigns?campaignId=<id>". Empty for every other platform's
	// rows, and for a Google row with no PlatformCampaignID.
	CampaignURL string
}

// AccountMonitorActionItem is one rule-engine finding for a single campaign (or, on
// platforms whose rule engine emits account-wide items, the whole account). The engines live
// in internal/service/rules/monitor_*.go and share their pacing ladder, priority ordering and
// budget-less handling through monitor_shared.go; the quirks each one carried over from its
// BFF source were fixed rather than preserved — see that package's doc comments and
// docs/knowledge/architecture/account-monitor-endpoints.md for the list. What remains
// genuinely per-platform is the rule set itself and the thresholds that carry a stated reason
// (Meta's higher CTR baseline, the per-platform clicks-without-conversions floors).
type AccountMonitorActionItem struct {
	// CampaignID is the platform campaign id the item is about. Empty for an account-wide
	// item (none of the four ported engines currently emit one, but the field exists so a
	// future account-wide rule does not need a shape change).
	CampaignID string
	// CampaignName is the campaign's platform-side name, carried alongside CampaignID so a
	// renderer never needs to re-join against the row list.
	CampaignName string
	Priority     MonitorPriority
	Issue        string
	Action       string
}

// AccountMonitorTotals is the account-wide aggregate the monitor response reports next to
// the per-campaign rows.
//
// Always the sum of exactly those rows, on every platform (service.monitorTotals). Reddit's
// port originally took its totals from a SEPARATE account-level metrics call, the way
// reddit-ads.service.ts's getRedditAnalytics does, and that call is unfiltered: it covers
// every campaign on the account, including ones the monitor's own status filter excludes from
// the rows. So the totals and the rows described different populations, with nothing in the
// response saying so — and CampaignCount, taken from the row count, already contradicted the
// spend it sat beside. Summing the rows makes the aggregate and the list agree by
// construction. See linuxfoundation/lfx-self-serve#3022.
type AccountMonitorTotals struct {
	Spend       float64
	Impressions int64
	Clicks      int64
	// Conversions is a pointer for the same reason AccountCampaignMetrics.Conversions is, and
	// it has to be: nil means no row in the sum reported a conversion measurement, which is
	// not the same claim as a measured 0. Reddit makes that distinction load-bearing — since
	// linuxfoundation/lfx-self-serve#3020 its rows carry nil rather than a hardcoded 0, so a
	// float64 here would rebuild at the aggregate exactly the false measurement that fix
	// removed from every row.
	Conversions   *float64
	CampaignCount int
}

// AccountMonitorRow wraps one AccountCampaignMetrics with rule-engine output, in the shape
// design/connection.go's AccountMonitor result type returns to the caller. Kept separate
// from AccountCampaignMetrics itself so a dispatcher's ListAccountCampaignMetrics can return
// bare metrics rows (its actual contract — see AccountMetricsReader) while the service layer
// owns attaching each row's action items, exactly the same split GetBriefMetrics uses
// between the metrics fan-out and its own pacing calc.
type AccountMonitorRow struct {
	Metrics AccountCampaignMetrics
	// PacingPct is a percentage (spend / expected-spend * 100). Kept as float64, NOT int:
	// Google/Meta/Reddit's ported formulas all Math.round() this value before comparing it
	// against their thresholds, but LinkedIn's does not — linkedin-ads.service.ts computes
	// `(analytics.spend / expectedSpend) * 100` and uses it unrounded for both the label
	// comparison and the displayed value. Rounding here for every platform would silently
	// change which side of a threshold a fractional LinkedIn pacing percentage falls on, so
	// Google/Meta/Reddit's rule engines round to an integral float64 themselves and
	// LinkedIn's leaves it fractional, exactly as each did upstream.
	PacingPct   float64
	PacingLabel MonitorPacingLabel
}
