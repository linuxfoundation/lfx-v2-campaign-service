// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// BudgetType is the pacing model for a campaign's budget.
type BudgetType string

// Budget types.
const (
	BudgetDaily    BudgetType = "daily"
	BudgetLifetime BudgetType = "lifetime"
)

// Campaign is one platform's campaign, subordinate to a brief. A brief drives
// many campaigns (one per platform), discriminated by Platform and sharing
// BriefID. The row is updated in place (not recreated) when a brief changes
// after campaigns exist.
// VariantDefault is the variant for every provider that does not sub-divide into
// campaign types, and the value every pre-000021 row was backfilled to. Six of the
// seven providers use it permanently: Meta's and Reddit's `objective` configures a
// single campaign rather than multiplying it, and LinkedIn/X/Microsoft/HubSpot have
// no such concept at all.
const VariantDefault = "default"

// VariantInvalid is the slot for a request whose variant could NOT be determined —
// today, a config envelope that fails to decode. It exists so such a request cannot be
// filed under a slot a real campaign occupies.
//
// The idempotency lookup runs BEFORE the dispatcher, so a request routed to an occupied
// slot is answered by reusing that row and never reaches the dispatcher that would have
// reported what was actually wrong with it. Mapping "we don't know" onto 'default' meant
// a malformed create against a brief with an existing Search campaign returned success.
//
// No create path ever writes this value, so the lookup is guaranteed to miss and the
// dispatch proceeds to its real error. The leading underscore keeps it outside the
// namespace any provider's channel/objective string could produce, so a future platform
// channel literally named "invalid" still cannot collide with it.
const VariantInvalid = "_invalid"

// AdoptableVariants lists the slots a platform's adopt endpoint can bind a campaign into.
//
// Only Google sub-divides today: its briefs can hold a Search campaign (VariantDefault) and
// a Demand Gen one simultaneously. Every other provider has exactly one slot, because its
// `objective`/`channel` configures a single campaign rather than multiplying it.
//
// It exists so the adopt pre-check can answer "is there any slot left?" WITHOUT guessing
// which one this campaign will occupy — that is only known once the platform reports what
// the campaign is. A pre-check that guessed VariantDefault refused a Demand Gen adoption
// onto a brief that merely had a Search campaign.
//
// A provider absent from this map has no adopt support, and callers must treat an empty
// result as "cannot pre-decide" rather than as "no slots".
func AdoptableVariants(p Provider) []string {
	if p == ProviderGoogleAds {
		return []string{VariantDefault, "demand-gen"}
	}
	return []string{VariantDefault}
}

// NormalizeVariant maps an empty variant to VariantDefault.
//
// Empty means "this caller does not sub-divide", which is true of every provider
// but Google and of every call site written before 000021. Normalizing at the
// boundary keeps that out of the query layer: the column is NOT NULL, and a bare ""
// would silently become a THIRD slot alongside 'default' — so a brief could hold
// two google-ads campaigns that both mean "the only one", which is exactly the
// duplicate the slot key exists to prevent.
func NormalizeVariant(v string) string {
	if v == "" {
		return VariantDefault
	}
	return v
}

type Campaign struct {
	ID        string
	ProjectID string
	BriefID   string
	JobID     *string // creation job that produced this row (soft ref; no FK)
	Platform  Provider
	// Variant is the sub-division of Platform this campaign is: which of that
	// platform's campaign types it represents. Google has several (search,
	// demand-gen, performance-max next) and its UI offers them as simultaneous
	// checkboxes, so one brief can hold more than one google-ads campaign; every
	// other provider uses VariantDefault.
	//
	// Part of the campaign's identity, not its config: (BriefID, Platform, Variant)
	// is the slot key the dispatch claim arbitrates on (migration 000022), which is
	// what stops a retry creating a second paid campaign.
	Variant            string
	PlatformCampaignID string // ID returned by the ad platform
	CampaignName       string
	Status             string
	BudgetAmount       *float64
	BudgetType         *BudgetType
	StartDate          *time.Time
	EndDate            *time.Time
	ConfigSnapshot     json.RawMessage
	Result             json.RawMessage
	Version            int64
	// CreatedBy / UpdatedBy name the human behind the write, with the same three
	// causes for nil as CampaignBrief.CreatedBy — read that doc first, it is the
	// canonical statement and is not repeated here.
	//
	// What is DIFFERENT for campaigns, and is the whole reason this took its own
	// change: the write does not happen on the request goroutine. Dispatch runs on
	// the orchestrator's root context after the request has returned, so reading the
	// actor at the point of the INSERT would yield nil for every campaign ever
	// created. Orchestrator.Start captures it from the request context and threads it
	// down. A campaign row therefore attributes to whoever asked for the DISPATCH,
	// which is the person who authorized the spend — not to whoever happened to be
	// authenticated when some later goroutine got around to writing the row.
	//
	// The nil case is ordinary rather than exceptional: Orchestrator.Start captures
	// the actor with attributedActor, which returns nil — after logging it — whenever
	// the request carried no authenticated principal. A NULL on such a row is correct,
	// and it means "not recorded", never "nobody".
	CreatedBy *Actor
	UpdatedBy *Actor
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Campaign.Status is a plain string that carries TWO kinds of value: a provisioning state
// stamped by the create/dispatch flow (pending / created / created_degraded) and a run state
// set by the status toggle (active / paused).
//
// Run states — the two a caller can toggle a live campaign between (match the design enum,
// mapped to each platform's own vocabulary by its dispatcher):
const (
	CampaignRunActive = "active"
	CampaignRunPaused = "paused"
)

// IsCampaignRunStatus reports whether status is one of the two RUN states (active/paused) a
// caller sets via the platform status toggle — as opposed to a provisioning state. The DB-only
// update path uses it to refuse a run-state change that would bypass the ad platform.
func IsCampaignRunStatus(status string) bool {
	return status == CampaignRunActive || status == CampaignRunPaused
}

// Provisioning states — stamped during creation. Mirrors the dispatch package's
// campaignStatusCreated/CreatedDegraded and the orchestrator's "pending" placeholder. The
// status toggle keys off these: it is safe to toggle a fully-created campaign, but a
// "pending" (ambiguous orphan) or "created_degraded" (a sub-step still needs reconciliation)
// campaign must NOT be ACTIVATED — doing so would put an incomplete campaign in front of an
// audience. The service allows one exception in the other direction, which is why it is
// spelled out here and not in CampaignStatusToggleable: a 'created_degraded' campaign
// definitely exists upstream and may be spending, so it may be PAUSED, and the service
// preserves the marker rather than overwriting it with the run state.
const (
	CampaignStatusPending         = "pending"
	CampaignStatusCreated         = "created"
	CampaignStatusCreatedDegraded = "created_degraded"
	// CampaignStatusDeleted is the terminal SOFT-DELETE state. The row is retained
	// (it holds platform_campaign_id, the only local pointer to a campaign that may
	// still exist and still be spending upstream) but is invisible to reads and
	// excluded from the (brief_id, platform) partial unique index, which frees the
	// slot for a re-dispatch. Deleting never touches the ad platform — see the
	// service layer.
	CampaignStatusDeleted = "deleted"
	// CampaignStatusGroupCreated and CampaignStatusUnconfirmed are the RETAINED PARTIAL
	// orphan statuses: the upstream create did not complete (only a sub-resource such as
	// the campaign group exists, or the outcome is ambiguous). The orchestrator preserves
	// them on the row so it records WHAT went wrong, and treats them as non-reusable so a
	// retry re-attempts the create. Mirrored from the service package's
	// partialOrphanStatuses / the dispatch package's literals, which are unexported;
	// declared here so packages that cannot import those (postgres) can still reason
	// about them. Drift-guarded by TestPartialOrphanStatusValues in internal/dispatch.
	CampaignStatusGroupCreated = "group_created"
	CampaignStatusUnconfirmed  = "unconfirmed"
)

// CampaignStatusToggleable reports whether a campaign in the given status may have its run
// state set FREELY, in either direction: only a cleanly-created campaign (or one already in a
// run state) is safe. A pending/degraded/other provisioning state is not — see the
// provisioning-state constants.
//
// It is deliberately direction-blind, and false for created_degraded. The service's one
// pause-only exception for that status is expressed at the call site (ToggleCampaignStatus)
// rather than by giving this predicate a direction parameter, because every other caller asks
// the direction-free question and a `toggleable(status, direction)` shape would invite one of
// them to pass the wrong direction and silently gain the exception.
func CampaignStatusToggleable(status string) bool {
	switch status {
	case CampaignStatusCreated, CampaignRunActive, CampaignRunPaused:
		return true
	default:
		return false
	}
}

// CampaignStatusNeedsReconciliation reports whether a campaign's status is a marker that an
// operator or a resume pass still has to resolve, rather than a settled outcome.
//
// The three statuses here all mean "the local row and the ad platform may disagree, and this
// string is the only record of how":
//
//   - pending: a bare dispatch claim. Either an in-flight dispatch owns it, or a dispatch
//     died mid-flight and may have created a campaign upstream that has no local id.
//   - group_created / unconfirmed: a partial orphan — a sub-resource exists, or the create
//     outcome is ambiguous.
//
// Acting on such a row OVERWRITES that marker and so destroys the signal. This is the same
// doctrine CampaignStatusToggleable enforces for the run-state toggle; it is a separate
// predicate because the two answer different questions and legitimately differ on
// created_degraded, which is fully created upstream (safe to retire, safe to PAUSE
// immediately, and safe to resume only after reconciliation).
func CampaignStatusNeedsReconciliation(status string) bool {
	switch status {
	case CampaignStatusPending, CampaignStatusGroupCreated, CampaignStatusUnconfirmed:
		return true
	default:
		return false
	}
}

// CampaignStatusDeletable reports whether a campaign in the given status is a settled,
// complete record that a soft-delete can safely retire: nothing about what happened
// upstream is lost by overwriting the row with 'deleted'.
//
// Deliberately a whitelist, not "!CampaignStatusNeedsReconciliation(status)": the
// campaigns.status column is unconstrained TEXT, so a status this predicate has never
// seen — a typo, a future addition, upstream drift — must fail CLOSED (treated as not
// yet safe to delete) rather than silently pass as deletable. The complement form fails
// OPEN on exactly that input, which is the defect this function exists to avoid.
func CampaignStatusDeletable(status string) bool {
	switch status {
	case CampaignStatusCreated, CampaignStatusCreatedDegraded, CampaignRunActive, CampaignRunPaused:
		return true
	default:
		return false
	}
}

// MetricsWindow is a platform-agnostic reporting window for a live metrics read. It is a
// closed vocabulary (not a platform-defined literal) so the API surface never leaks one
// platform's dialect — each MetricsReader adapter maps these values to its own platform's
// query vocabulary (e.g. Google Ads' GAQL DURING literals, Meta's Insights date_preset).
type MetricsWindow string

// Metrics windows in the platform-agnostic API vocabulary. A MetricsReader adapter may
// support only a subset of these and report ErrMetricsWindowUnsupported for unsupported values.
const (
	MetricsWindowToday      MetricsWindow = "today"
	MetricsWindowYesterday  MetricsWindow = "yesterday"
	MetricsWindowLast7Days  MetricsWindow = "last_7_days"
	MetricsWindowLast14Days MetricsWindow = "last_14_days"
	MetricsWindowLast30Days MetricsWindow = "last_30_days"
	MetricsWindowThisMonth  MetricsWindow = "this_month"
	MetricsWindowLastMonth  MetricsWindow = "last_month"
)

// IsValidMetricsWindow reports whether w is one of the closed set of supported windows. The
// Goa HTTP layer already enforces the enum on requests that arrive over HTTP, but the service
// layer validates independently — the same defense-in-depth as
// IsCampaignRunStatus/CampaignStatusToggleable — so a direct/test caller can't pass an
// unmapped value through to a platform adapter.
func IsValidMetricsWindow(w MetricsWindow) bool {
	switch w {
	case MetricsWindowToday, MetricsWindowYesterday, MetricsWindowLast7Days, MetricsWindowLast14Days,
		MetricsWindowLast30Days, MetricsWindowThisMonth, MetricsWindowLastMonth:
		return true
	default:
		return false
	}
}

// CampaignMetrics is a platform-agnostic, live read-through performance
// snapshot for one campaign. It is never persisted — a MetricsReader
// dispatcher call populates it fresh on every read, the same way
// StatusToggler's ToggleStatus call is always live rather than DB-cached.
//
// Window records what was ASKED FOR, and what a platform does with it is the
// platform's business. Most ad platforms scope the counters to it. HubSpot does
// not: the span selects which email is in scope BY SEND DATE and then returns
// that email's totals to date, so `today` and `last_30_days` on an email sent
// this morning return identical numbers. A consumer that renders these as
// "opens during <Window>" is therefore wrong for at least one channel — the
// honest label is the window that was requested, not a period the counters
// cover.
type CampaignMetrics struct {
	CampaignID  string
	Window      MetricsWindow
	Impressions int64
	Clicks      int64
	CostMicros  int64
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64
	// Conversions is the count of desired actions attributed to this campaign over Window,
	// and is a POINTER because "this platform cannot tell us" and "this platform measured
	// zero" are different facts that a plain int64 cannot hold apart.
	//
	// nil means THIS READ PRODUCED NO COMPLETE CONVERSION MEASUREMENT; a non-nil 0 is a
	// measurement. Two distinct situations reach nil, and a consumer must treat them the
	// same way, which is why the contract is stated over the READ rather than the platform:
	//
	//   - the channel reports no campaign-level conversion count at all (Meta, X, Reddit,
	//     HubSpot — see the four entries below), and
	//   - a conversion-capable channel returned a response this client could not complete:
	//     LinkedIn when any element it returned omitted externalWebsiteConversions (one
	//     omission withdraws the whole total), and Microsoft when the ConversionsQualified
	//     cell is blank because the account has no Universal Event Tracking.
	//
	// Reading nil as "this platform cannot measure conversions" would therefore be wrong for
	// the second group, and a consumer that special-cased the channel on that basis would
	// misclassify a LinkedIn campaign whose response was merely incomplete.
	//
	// Only three of the seven adapters set it, and which three is a statement about the
	// VENDOR APIS, verified field by field against each vendor's published reference rather
	// than inferred from what this repo's clients happen to request today:
	//
	//   - Google Ads populates it from metrics.conversions, which the field reference types
	//     as DOUBLE, not an integer. Google credits FRACTIONAL conversions under data-driven
	//     and position-based attribution, so a campaign can genuinely hold 0.4 of a
	//     conversion. That fraction is carried through as-is: this field is a float64 for
	//     exactly that reason. Rounding it to a whole number here would report a campaign
	//     holding 0.4 conversions as having produced ZERO, which the no_conversions rule
	//     then reads as a finding — a fabricated measurement in the opposite direction to
	//     the nil-versus-zero distinction this pointer exists to protect.
	//     metrics.conversions is ALWAYS in the SELECT list, and proto3 JSON omits
	//     default-valued fields, so an ABSENT conversions member on a Google row is the
	//     encoding of a measured 0.0 — not "unmeasured". The adapter therefore materialises
	//     a non-nil zero there, the same way parseMetricInt already treats an omitted
	//     impressions/clicks value as a measured 0.
	//   - LinkedIn populates it from externalWebsiteConversions (typed `long` in the Ads
	//     Reporting schema), which must be named in the request's `fields` list: LinkedIn
	//     returns only impressions and clicks by default, so an unnamed metric comes back
	//     absent rather than zero. The client always names it, so a well-formed empty
	//     `elements` array is an ANSWERED zero-activity window and materialises a non-nil
	//     zero, matching Google's no-rows branch. Nil is reserved for the case where an
	//     element LinkedIn DID return omitted the metric: that is missing data about
	//     activity that happened, and it withdraws the whole total.
	//   - Microsoft populates it from the ConversionsQualified report column, NOT from
	//     Conversions. Microsoft's own column reference marks `Conversions` deprecated as of
	//     2022, directs callers to ConversionsQualified, and warns the legacy column's values
	//     "may be inaccurate" — so reading the obvious-looking column would have been the
	//     wrong number, not merely an older one. It is typed `double` for the same
	//     fractional-attribution reason Google's is, and its fraction is likewise preserved.
	//     Unlike Google's, the column is only present for accounts using Universal Event
	//     Tracking, so an ABSENT column here really does mean unmeasured and stays nil.
	//
	// The other four leave it nil, and each for a reason that is a property of the platform
	// rather than an unfinished task here:
	//
	//   - Meta reports conversions inside the Insights `actions` array as {action_type,
	//     value} objects, with no scalar campaign-level conversions field. Collapsing that
	//     array into one number requires choosing WHICH action types count as a conversion,
	//     which is a per-advertiser configuration decision this service has no input for.
	//   - X splits conversions across per-event-type metrics (conversion_purchases,
	//     conversion_sign_ups, and so on), each a JSON object rather than a count, and only
	//     under the WEB_CONVERSION/MOBILE_CONVERSION metric groups. Same problem as Meta:
	//     there is no single number to read.
	//   - Reddit's v3 reporting contract has no public documentation at all (see the banner
	//     on reddit.GetCampaignMetrics), so a conversions field name here would be a guess
	//     dressed as a measurement.
	//   - HubSpot stages marketing emails, whose statistics endpoint returns a counter
	//     vocabulary with no conversion counter in it. An email send has no campaign-level
	//     conversion concept to report.
	//
	// A nil here must never be rendered as 0 by a consumer. That substitution is the whole
	// reason for the pointer: the conversions rule in internal/service/rules refuses to fire
	// on a nil precisely so a platform that cannot measure conversions is never reported as
	// a campaign that failed to earn any.
	Conversions *float64
	// Email carries the counters that only an email channel has, and is nil for every ad
	// platform. It exists because the four fields above cannot express an email send
	// without lying: delivery, bounces and unsubscribes have no ad-platform analogue at
	// all, and a consumer that needs them would otherwise have to infer them from numbers
	// that do not contain them.
	//
	// "Only an email channel has" is about the fields inside EmailMetrics, not about the
	// four above: an email send also populates Impressions and Clicks, deliberately, so a
	// cross-channel view can total them without special-casing the channel. Email is the
	// overflow for what does not fit, not a separate parallel result.
	Email *EmailMetrics
}

// EmailMetrics is the email-channel counter set, carried alongside CampaignMetrics'
// platform-agnostic fields rather than replacing them.
//
// The two overlapping fields are mapped deliberately. Opens populate Impressions because
// an open is the same event an ad impression is — the recipient rendered the creative —
// and Clicks is a click in both channels, so a cross-channel click total is genuinely
// correct. CostMicros is where the analogy STOPS: HubSpot charges nothing per send, so an
// email campaign reports 0, and 0 here means "this platform bills no per-send cost", NOT
// "this campaign was free". Dividing a blended cost by a blended conversion count across
// email and paid channels therefore produces a cost-per-acquisition that is wrong in a
// direction that always flatters the campaign.
type EmailMetrics struct {
	// Sent is emails handed to the delivery pipeline; Delivered is those the receiving
	// server accepted. Sent-minus-Delivered is not the same as Bounces (a message can be
	// dropped or suppressed before it is ever attempted), so both are reported rather than
	// leaving a consumer to subtract.
	Sent      int64
	Delivered int64
	// Opens and Clicks duplicate CampaignMetrics.Impressions and .Clicks. The duplication
	// is intentional: a consumer reading this struct should not have to know which
	// ad-shaped field the email channel happens to have been mapped onto.
	Opens        int64
	Clicks       int64
	Bounces      int64
	Unsubscribes int64
}

// JobStatus is the status vocabulary shared by campaign_jobs and the API's
// JobCreateResponse/JobPollResponse.
type JobStatus string

// Job statuses. 'partial' = some platforms succeeded, some failed.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobPartial   JobStatus = "partial"
	JobFailed    JobStatus = "failed"
)

// AllJobStatuses is the WHOLE job status vocabulary, in the order the constants above
// declare it. It exists so tests that must reason over every status can iterate this
// instead of restating the list.
//
// A test that hand-copies the vocabulary silently stops covering a status added later —
// it keeps agreeing with its own copy while the new status goes unclassified. The one
// that matters is the retention prune: its allow-list decides which rows get DELETED, and
// those rows are the audit trail of real ad spend. Deriving from here means adding a
// status to this list is what forces the classification to be made deliberately.
//
// Must stay in step with the campaign_jobs status CHECK constraint (migration 000002).
var AllJobStatuses = []JobStatus{
	JobQueued, JobRunning, JobSucceeded, JobPartial, JobFailed,
}

// Terminal reports whether the job has reached a final state.
func (s JobStatus) Terminal() bool {
	switch s {
	case JobSucceeded, JobPartial, JobFailed:
		return true
	default:
		return false
	}
}

// CampaignJob is the async multi-platform dispatch record. One job per brief
// submission dispatches to multiple Campaign rows (one per platform).
type CampaignJob struct {
	ID        string
	BriefID   string
	Status    JobStatus
	Result    json.RawMessage
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

// PlatformCampaignRef is what a platform lookup reports about a campaign that ALREADY
// exists upstream. It is the evidence adoption binds on, so it is deliberately the
// smallest set of fields that answers "is this really the campaign you named": the id we
// can prove the platform returned, and the name an operator recognises it by.
//
// It carries NO live status, deliberately. Campaign.Status is this service's own lifecycle
// vocabulary that CampaignStatusDeletable and CampaignStatusNeedsReconciliation switch on;
// a platform's ENABLED/PAUSED is a different axis, and writing one into the other yields a
// row both predicates default-deny — undeletable and never reconciled. Nor is the upstream
// run state readable anywhere else here: the metrics read carries impressions, clicks, cost
// and CTR and no run-state field at all. It is only ever SET, by the status toggle, which
// persists this service's own active/paused on the row once the platform confirms — so the
// row records the last toggle this service made, and a change in the platform's own console
// is visible only there. Whether a campaign is
// adoptable at all is the ADAPTER's decision, in the platform's own vocabulary: a lookup
// returns nil for a removed or otherwise unbindable campaign rather than handing the
// service a status string it would have to learn every dialect to interpret.
//
// A nil *PlatformCampaignRef with a nil error means the campaign is genuinely ABSENT —
// the platform answered, and there is no such campaign. That is a load-bearing
// distinction, not a Go convention: the adopt handler returns 404 for absence and 503
// for "we could not verify", and conflating them would let an unverifiable answer read
// as a definitive "no such campaign".
type PlatformCampaignRef struct {
	// ID is the platform's own campaign id, echoed back from the platform's response
	// rather than from the request, so a filter the platform failed to honour cannot
	// pass as a match.
	ID string
	// Name is the campaign name as the platform holds it.
	Name string
	// Result is the platform-shaped provenance blob to persist on the adopted row, built
	// by the ADAPTER because only it knows the shape its own guards read back. Google Ads
	// puts the resolved customer id here, which is what keeps googleAdsCreationCustomerID's
	// account-mismatch check effective for adopted rows: without it the row's Result is
	// empty, the guard reads "unknown", and a later repointing of the project's connection
	// would let the same numeric id address a DIFFERENT customer's campaign.
	Result json.RawMessage
	// Variant is the slot this upstream campaign belongs in, as established from what the
	// PLATFORM reports the campaign actually is — not assumed from the adopt request, which
	// names no campaign type at all.
	//
	// Adoption previously filed every Google campaign under VariantDefault regardless of
	// type. Adopting a Demand Gen campaign therefore left the 'demand-gen' slot empty, and
	// the next Demand Gen dispatch for that brief saw a free slot and created a SECOND paid
	// campaign. An adapter that cannot establish the variant must fail rather than return
	// a guess: an empty value here is a bug in the adapter, and the service layer rejects
	// it rather than defaulting.
	Variant string
}

// ProjectCampaignScope is ONE campaign a project owns upstream, as the authorization scope
// for an otherwise account-wide platform read.
//
// It pairs the id with its provenance deliberately. A platform_campaign_id is a bare numeric
// unique only WITHIN the customer it was created under, so an id on its own cannot be scoped
// safely: Google Ads is one customer shared across every foundation, and UpdateGoogleAds can
// re-point a project's connection between create and read. Handing the adapter a bare id list
// would let a stale id address a DIFFERENT customer's campaign of the same number — the very
// invariant ReadMetrics and the keyword mutation already fail closed on. Carrying Result means
// the adapter can apply that same check rather than trusting the id.
type ProjectCampaignScope struct {
	// PlatformCampaignID is the upstream campaign id, as recorded when it was dispatched.
	PlatformCampaignID string
	// Result is the platform-shaped provenance blob the row recorded at create time. For
	// Google Ads it carries the creating customer id, read back by
	// googleAdsCreationCustomerID. EMPTY means "unknown" — a row written before provenance
	// tracking existed — and per the service-wide convention a READ may proceed on it.
	Result json.RawMessage
}
