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

// MonitorFailureClass is WHY an account-monitor row is FetchFailed, as a small closed set so it
// can be logged and used as a metric label without carrying upstream text (#220). The platform
// client that saw the failure sets it; the service layer logs one summary per request and counts
// rows per class (service.reportUnmeasuredRows). It is not part of the API response.
//
// A FetchFailed row whose client set no class — Google Ads, whose client leaves classification to
// its owner — is counted as MonitorFailureOther; one whose client set a value outside this set is
// counted as MonitorFailureUnknown (NormalizeMonitorFailureClass).
type MonitorFailureClass string

const (
	// MonitorFailureAuth: the platform refused the credential or the token for this read
	// (401/403, or a refused token refresh).
	MonitorFailureAuth MonitorFailureClass = "auth"
	// MonitorFailureThrottle: the platform rate-limited the read (429 after the client's own
	// bounded retry).
	MonitorFailureThrottle MonitorFailureClass = "throttle"
	// MonitorFailureUpstream5xx: the platform could not answer (a 5xx, or its token endpoint
	// unavailable).
	MonitorFailureUpstream5xx MonitorFailureClass = "upstream_5xx"
	// MonitorFailureDecode: the platform answered but the row's data could not be parsed or
	// attributed (an unparseable spend/budget/flight value, a report row for another campaign, an
	// id this service will not interpolate).
	MonitorFailureDecode MonitorFailureClass = "decode"
	// MonitorFailureNoReport: report-backed platforms (Microsoft Ads, X) only — no saved report
	// has finished for this account and window yet, so the row has no metrics to read. Expected
	// on a first read rather than a fault, so it is counted but never logged as a warning.
	MonitorFailureNoReport MonitorFailureClass = "no_report"
	// MonitorFailureOther: anything else — a transport error, a deadline, a status outside the
	// classes above, or a row whose client did not classify it.
	MonitorFailureOther MonitorFailureClass = "other"
	// MonitorFailureUnknown is never set by a client: it is what NormalizeMonitorFailureClass
	// makes of a NON-EMPTY class outside this set — a client bug or drift — so it is counted
	// apart from MonitorFailureOther, where it would be indistinguishable from an honest
	// "unclassified".
	MonitorFailureUnknown MonitorFailureClass = "unknown"
)

// NormalizeMonitorFailureClass maps c onto the closed set: the empty class an unclassified
// FetchFailed row carries becomes MonitorFailureOther, and any other value outside the set becomes
// MonitorFailureUnknown. Callers use the result as a log value and metric label, so it must never
// pass an arbitrary string through.
func NormalizeMonitorFailureClass(c MonitorFailureClass) MonitorFailureClass {
	switch c {
	case MonitorFailureAuth, MonitorFailureThrottle, MonitorFailureUpstream5xx,
		MonitorFailureDecode, MonitorFailureNoReport, MonitorFailureOther:
		return c
	case "":
		return MonitorFailureOther
	default:
		return MonitorFailureUnknown
	}
}

// FlightRange is one contiguous span of scheduled calendar days: StartDate through EndDate,
// both inclusive, as YYYY-MM-DD in the platform's own (account-local) days. EndDate is empty for
// an open-ended span, which can only be the last range of a set.
type FlightRange struct {
	StartDate string
	EndDate   string
}

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
	// FlightRanges is X only: the days the campaign is actually scheduled on, as the union of its
	// line items' flights — sorted, disjoint, non-adjacent ranges of account-local calendar days.
	// StartDate/EndDate above are only the envelope of these (earliest start, latest end), which
	// counts any gap BETWEEN line items as scheduled; a rule engine that pacing- or
	// delivery-judges a window must use the ranges when they are set. Empty when the platform
	// does not report per-line-item flights or the campaign has none.
	FlightRanges []FlightRange
	// MetricsWindowStart/MetricsWindowEnd are Meta only: the Insights `time_range` (YYYY-MM-DD,
	// both inclusive) the row's Spend covers — the bounds the read SENT, the same on every row of
	// one read. Meta's rule engine paces against exactly these days rather than re-deriving a
	// window from a clock of its own, which a request crossing UTC midnight would put one day
	// away from the spend. Empty for every other platform.
	MetricsWindowStart string
	MetricsWindowEnd   string
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
	// FetchFailed marks a row some part of whose upstream data could not be trusted. WHICH part is
	// not recorded, and it is NOT always the metrics. The shapes it takes include: a per-campaign
	// metrics call that failed outright (Reddit's per-campaign report fetch), in which case the
	// numeric metrics fields are left at their zero value; a metrics row that came back but would
	// not parse — Meta's row in its one account-level insights read, whose metrics are then zero
	// while a campaign-level budget set earlier from the campaign list, if the campaign has one,
	// may still be real (the ad-set budget fallback is skipped for such a row, so a budget held on
	// ad sets is not applied to this row and reads as absent), and a Google Ads GAQL row.
	// Google is expected to
	// return one aggregated row per campaign, but the client sums any repeated rows for a campaign
	// rather than rely on that, so if one of several rows fails, the others' contribution stays:
	// real but incomplete delivery. If the campaign's only row fails, Impressions, Clicks and Spend
	// stay zero and Conversions stays nil (absent, not a measured zero). Zero or partial, those
	// metrics are unsuitable for pacing and action-item evaluation. Further shapes: a
	// present-but-unparseable budget read alongside otherwise-good metrics (Google Ads'
	// campaign_budget.amount_micros, LinkedIn's dailyBudget/totalBudget, Meta's
	// daily_budget/lifetime_budget, Microsoft Ads' and X's budget fields), in which case
	// Impressions/Clicks/Spend may be genuinely non-zero while only BudgetDay/TotalBudget is
	// untrusted; an unreadable flight (X); and an unparseable spend alone while impressions and
	// clicks are real (LinkedIn's costInLocalCurrency). Treat that as examples, not a closed set:
	// every platform client sets this flag and each has its own partial cases, so a text that
	// enumerates them by platform goes stale — which is what #219 is about. Whatever the cause,
	// this is the status marker the migration spec calls for: a consumer can tell "the platform
	// authoritatively reported zero" from "this service could not read this row's data" — the same
	// distinguishability CampaignSettingsReadback's `unknown` verdict exists to preserve. Renderers
	// MUST check this before treating any of this row's fields, zero or not, as a fully trusted
	// reading.
	FetchFailed bool
	// FailureClass is why FetchFailed is set, from the platform client that saw the failure;
	// empty when FetchFailed is false, and empty on a Google Ads row (see MonitorFailureClass).
	// Internal only: logged and counted by the service layer, never returned in the response.
	FailureClass MonitorFailureClass
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
	// EmailID is set by the HubSpot email monitor only: the HubSpot marketing-email id the item
	// is about. There CampaignID is this service's campaign UUID (an A/B variant shares its
	// parent's), so EmailID is what joins a finding to its row. Empty on every ad monitor.
	EmailID string
	// CampaignName is the campaign's platform-side name, carried alongside CampaignID so a
	// renderer never needs to re-join against the row list.
	CampaignName string
	Priority     MonitorPriority
	Issue        string
	Action       string
}

// AccountMonitorTotals is the aggregate the monitor response reports next to
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
	// UnmeasuredCampaigns is how many of the CampaignCount rows are FetchFailed (#218). The sums
	// above include those rows at whatever they carry — zero, or partial delivery, for a row whose
	// metrics could not be fully read — so a non-zero count says the totals may understate the
	// account: by up to this many campaigns' delivery. It is an upper bound, not an exact count of
	// rows missing from the sum, because FetchFailed also marks a row whose budget or flight alone
	// was unreadable while its metrics were read and summed (Google Ads, LinkedIn, Meta, Microsoft
	// Ads, X). PacingUnknown alone does NOT count a row: every FetchFailed row also carries it (the
	// rule engines' fetchFailedRow), but most PacingUnknown rows — no budget, no flight — had their
	// delivery read in full. 0 means every row in the sum was fully read.
	UnmeasuredCampaigns int
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

// AccountMonitorRead is what a live-read platform's dispatcher hands the account monitor: the
// campaign rows, and the ad account's currency when the platform reported one in the reads the
// monitor already makes (service.AccountMonitorReader).
type AccountMonitorRead struct {
	// Rows carries every campaign the read returned, with the same contract as
	// AccountMetricsReader.ListAccountCampaignMetrics' result: non-nil, a fetch failure marked
	// on its row rather than zeroed.
	Rows []AccountCampaignMetrics
	// Currency is the ad account's ISO 4217 code — the currency every Spend, BudgetDay and
	// TotalBudget in Rows is denominated in, unconverted — or "" when the platform did not
	// report one this service can vouch for. Always passed through NormalizeCurrencyCode.
	Currency string
}

// NormalizeCurrencyCode returns code when it is shaped like an ISO 4217 alphabetic code — exactly
// three ASCII upper-case letters — and "" otherwise. Nothing is trimmed or case-folded: a value a
// platform sent in any other shape is not a code this service can vouch for, and the account
// monitor's currency is absent rather than guessed. It does not check the code against the ISO
// 4217 list: a well-shaped code a platform reports is passed through.
func NormalizeCurrencyCode(code string) string {
	if len(code) != 3 {
		return ""
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return ""
		}
	}
	return code
}
