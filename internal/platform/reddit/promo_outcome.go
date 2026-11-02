// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
)

// PromoOutcome is the outcome class of the promoted-post leg of CreateCampaign: Step 1.5, which
// authors a post from an ImageURL, and Step 4, which creates the ad from that post or from a
// caller-supplied PostURL.
//
// Every way that leg can end is ONE row of promoOutcomes. The row is the single source of the
// AdWarning and the runbook step text. Its Shape and Persisted columns are DESCRIPTIVE: they
// record the result shape CreateCampaign returns for the class (its own switch on the outcome
// implements it) and the status the dispatcher persists, and TestReddit_PromoOutcomeTable keeps
// the columns and that behaviour in sync. #173 is why: four review rounds on PR #158 each fixed
// one arm of a switch and broke another, because the guidance was assembled as prose at each
// step, by whoever remembered which facts were settled at that line. Classification
// (classifyAuthorFailure, classifyAdFailure) and rendering (PromoOutcomeRow.step and stepf) are
// now separate; a change to the text of one class cannot reach another, and the dispatcher's
// table-driven test pins every row.
type PromoOutcome string

// The outcome classes. "Unconfirmed" means the request may have reached Reddit and the object
// may exist: a 5xx, a 429, a 3xx on a mutating request, a transport error that is not proven
// pre-send, or a 2xx whose body carried no id. "Rejected" is any other apiError, a definite
// 4xx. "Pre-send" is a failure proven to have happened before Reddit received anything (token
// refresh, request build, a refused dial).
const (
	// PromoNotRequestedNoVariants: no PostURL, no ImageURL, no variants. Nothing was asked of
	// this leg, so the result is clean.
	PromoNotRequestedNoVariants PromoOutcome = "not_requested_no_variants"
	// PromoNotRequestedVariants: no PostURL or ImageURL, but variants are listed for the
	// operator to create by hand. Still clean: no ad was requested of this client.
	PromoNotRequestedVariants PromoOutcome = "not_requested_variants"
	// PromoAdCreated: the ad was created and its id decoded.
	PromoAdCreated PromoOutcome = "ad_created"

	// PromoAuthorCanceledInFlight: the caller cancelled while the authoring POST was in flight.
	// The post may exist and no campaign was created.
	PromoAuthorCanceledInFlight PromoOutcome = "author_canceled_in_flight"
	// PromoAuthorCanceledBeforeSend: the caller cancelled before the authoring POST was sent.
	// Nothing exists.
	PromoAuthorCanceledBeforeSend PromoOutcome = "author_canceled_before_send"
	// PromoAuthorUnconfirmed: authoring may have reached Reddit; the campaign continues with no ad.
	PromoAuthorUnconfirmed PromoOutcome = "author_unconfirmed"
	// PromoAuthorRejected: Reddit rejected the post (a definite 4xx); the campaign continues.
	PromoAuthorRejected PromoOutcome = "author_rejected"
	// PromoAuthorPreSend: authoring failed before Reddit received it; the campaign continues.
	PromoAuthorPreSend PromoOutcome = "author_pre_send"

	// PromoAdCanceledInFlight: the caller cancelled while the ad POST was in flight.
	PromoAdCanceledInFlight PromoOutcome = "ad_canceled_in_flight"
	// PromoAdCanceledBeforeSend: the caller cancelled before the ad POST was sent.
	PromoAdCanceledBeforeSend PromoOutcome = "ad_canceled_before_send"
	// PromoAdUnconfirmed: the ad POST may have reached Reddit (5xx, 429, mutating 3xx, transport).
	PromoAdUnconfirmed PromoOutcome = "ad_unconfirmed"
	// PromoAdNoID: the ad POST returned a 2xx with no ad id; the ad may exist.
	PromoAdNoID PromoOutcome = "ad_no_id"
	// PromoAdRejected: Reddit rejected the ad (a definite 4xx).
	PromoAdRejected PromoOutcome = "ad_rejected"
	// PromoAdPreSend: the ad POST failed before Reddit received it.
	PromoAdPreSend PromoOutcome = "ad_pre_send"
)

// ResultShape is what CreateCampaign returns for an outcome. The dispatcher's persisted status
// is a function of the shape alone (RedditDispatcher.Dispatch), which is why the table records
// both.
type ResultShape string

const (
	// ShapeNilError is (nil, err): nothing was or may have been created. The dispatcher returns
	// a pre-create error, the claim is released and no row is written.
	ShapeNilError ResultShape = "nil_result_with_error"
	// ShapePartialError is (partial, err): something may exist. The claim is retained and the
	// orchestrator persists the partial as a pending orphan for reconciliation.
	ShapePartialError ResultShape = "partial_result_with_error"
	// ShapeDegraded is (result, nil) with a non-empty AdWarning: created_degraded.
	ShapeDegraded ResultShape = "result_with_ad_warning"
	// ShapeClean is (result, nil) with no AdWarning: created.
	ShapeClean ResultShape = "result"
)

// The status the dispatcher persists for each shape. Kept beside the table so a reader sees the
// whole consequence of a row in one place; the dispatcher's own constants are the authority and
// its table-driven test asserts these agree with what it actually persists.
const (
	PersistedNotCreated      = "not_created (claim released, no row)"
	PersistedPending         = "pending"
	PersistedCreatedDegraded = "created_degraded"
	PersistedCreated         = "created"
)

// PromoOutcomeRow is one row of the outcome table.
type PromoOutcomeRow struct {
	Outcome   PromoOutcome
	Shape     ResultShape
	Persisted string
	// AdWarning is the exact CampaignResult.AdWarning, "" for none. For the three non-fatal
	// authoring classes it is extended at Step 4 by campaignContextWarning (see below).
	AdWarning string
	// Step is the runbook step appended for the outcome, "" for none. StepWithStatus, when
	// set, replaces it when the error carried an HTTP status, which it formats with %d.
	Step           string
	StepWithStatus string
	// StepFormat, when set, is the row's step as a format whose arguments are NOT an HTTP
	// status (the variant count for PromoNotRequestedVariants, the post id for PromoAdNoID).
	// It is rendered only by stepf; step() panics on such a row, so routing it through the
	// status path fails loudly in tests instead of emitting a raw verb.
	StepFormat string
	// CampaignContext marks the classes whose shortfall happened BEFORE the paid campaign
	// existed (Step 1.5 runs ahead of Step 2). Their runbook cannot name the campaign at the
	// point of failure, because it does not exist yet; Step 4 renders it once both the campaign
	// and the ad group are known to exist, and only then (#158 round 3).
	CampaignContext bool
}

// Rendered at Step 4 for a CampaignContext row, the only point at which both ids are decoded.
const (
	campaignContextStep    = "Campaign %s and ad group %s were created (PAUSED) with no ad -- add the ad to THIS campaign in Reddit Ads Manager rather than creating a new one"
	campaignContextWarning = " (the campaign and ad group were created and are PAUSED)"
)

var promoOutcomeRows = []PromoOutcomeRow{
	{
		Outcome: PromoNotRequestedNoVariants, Shape: ShapeClean, Persisted: PersistedCreated,
		Step: "No ad variants or post URL provided -- add ads manually in Reddit Ads Manager",
	},
	{
		// The variant lines that follow the step are rendered by CreateCampaign. StepFormat's
		// %d is the variant COUNT.
		Outcome: PromoNotRequestedVariants, Shape: ShapeClean, Persisted: PersistedCreated,
		StepFormat: "%d ad variant(s) ready -- create ads in Reddit Ads Manager with these headlines (set/replace the shown utm_* params on your registration URL, keeping all its other query parameters; drop any trailing '/' from the path so it matches the automated destination):",
	},
	{
		// The step names the ad, the post and the click URL; CreateCampaign renders it.
		Outcome: PromoAdCreated, Shape: ShapeClean, Persisted: PersistedCreated,
	},

	{
		Outcome: PromoAuthorCanceledInFlight, Shape: ShapePartialError, Persisted: PersistedPending,
		Step: "Promoted-post authoring is UNCONFIRMED (the request reached Reddit but the outcome is unknown) -- no campaign was created; check Reddit Ads Manager for a post with this headline BEFORE authoring it again to avoid a duplicate, and DELETE it if it exists and is not attached to an ad",
	},
	{
		Outcome: PromoAuthorCanceledBeforeSend, Shape: ShapeNilError, Persisted: PersistedNotCreated,
	},
	{
		Outcome: PromoAuthorUnconfirmed, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning:       "promoted-post authoring is UNCONFIRMED (the request may have reached Reddit) and no ad was created — verify in Reddit Ads Manager BEFORE authoring the post again to avoid a duplicate, and DELETE any post that is not attached to an ad",
		Step:            "Promoted-post authoring UNCONFIRMED (the request may have reached Reddit) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
		StepWithStatus:  "Promoted-post authoring UNCONFIRMED (HTTP %d) -- continuing without an ad; verify whether the post exists before authoring it again, and DELETE it if it is not attached to an ad",
		CampaignContext: true,
	},
	{
		Outcome: PromoAuthorRejected, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning:       "promoted-post authoring FAILED (Reddit rejected the post) and no ad was created — author the post and add the ad in Reddit Ads Manager",
		StepWithStatus:  "Promoted-post authoring failed: Reddit rejected the post (HTTP %d) -- continuing without an ad; author the post and add the ad in Reddit Ads Manager",
		CampaignContext: true,
	},
	{
		Outcome: PromoAuthorPreSend, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning:       "promoted-post authoring FAILED before it reached Reddit; no post exists and no ad was created — author the post and add the ad in Reddit Ads Manager",
		Step:            "Promoted-post authoring failed before the request reached Reddit -- continuing without an ad; author the post and add the ad in Reddit Ads Manager",
		CampaignContext: true,
	},

	{
		Outcome: PromoAdCanceledInFlight, Shape: ShapePartialError, Persisted: PersistedPending,
		AdWarning: "promoted-post ad is UNCONFIRMED: the create was interrupted in flight and the ad MAY exist — verify in Reddit Ads Manager before recreating",
	},
	{
		Outcome: PromoAdCanceledBeforeSend, Shape: ShapePartialError, Persisted: PersistedPending,
	},
	{
		Outcome: PromoAdUnconfirmed, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning:      "promoted-post ad creation is UNCONFIRMED (campaign and ad group created, PAUSED); the ad request may have reached Reddit — verify whether the ad exists BEFORE creating it manually to avoid a duplicate",
		Step:           "Ad creation UNCONFIRMED (request may have reached Reddit) -- verify in Reddit Ads Manager before adding manually",
		StepWithStatus: "Ad creation UNCONFIRMED (HTTP %d) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		// Step is a FORMAT whose %s is the post id: CreateCampaign renders it with fmt.Sprintf
		// and never through step(), which would return the raw format.
		Outcome: PromoAdNoID, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning: "promoted-post ad creation is UNCONFIRMED (2xx response with no ad id); the ad may already exist — verify in Reddit Ads Manager BEFORE creating it manually to avoid a duplicate",
		// StepFormat's %s is the post id.
		StepFormat: "Ad creation UNCONFIRMED (2xx with no ad ID, post: %s) -- verify in Reddit Ads Manager before adding manually",
	},
	{
		Outcome: PromoAdRejected, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning:      "promoted-post ad creation FAILED (campaign and ad group created, PAUSED); Reddit rejected the ad — add it manually in Reddit Ads Manager",
		StepWithStatus: "Ad creation failed: Reddit rejected the ad (HTTP %d) -- add ad manually in Reddit Ads Manager",
	},
	{
		Outcome: PromoAdPreSend, Shape: ShapeDegraded, Persisted: PersistedCreatedDegraded,
		AdWarning: "promoted-post ad creation FAILED before it reached Reddit (campaign and ad group created, PAUSED); the ad was not created — add it manually in Reddit Ads Manager",
		Step:      "Ad creation failed before the request reached Reddit -- add ad manually in Reddit Ads Manager",
	},
}

var promoOutcomes = func() map[PromoOutcome]PromoOutcomeRow {
	m := make(map[PromoOutcome]PromoOutcomeRow, len(promoOutcomeRows))
	for _, r := range promoOutcomeRows {
		if _, dup := m[r.Outcome]; dup {
			panic("reddit: duplicate promo outcome row " + string(r.Outcome))
		}
		m[r.Outcome] = r
	}
	return m
}()

// PromoOutcomes returns a copy of the outcome table, in declaration order. It is exported so the
// dispatcher's table-driven test can drive every row end to end and check the Persisted column
// against the status it actually writes; nothing in production reads it.
func PromoOutcomes() []PromoOutcomeRow {
	return append([]PromoOutcomeRow(nil), promoOutcomeRows...)
}

// promoRow returns the row for an outcome. Every outcome a classifier can return has a row, so a
// miss is a programming error.
func promoRow(o PromoOutcome) PromoOutcomeRow {
	r, ok := promoOutcomes[o]
	if !ok {
		panic("reddit: no promo outcome row for " + string(o))
	}
	return r
}

// step renders the row's runbook step. status is the HTTP status when the error carried one
// (hasStatus), and is used only when the row has a StepWithStatus.
func (r PromoOutcomeRow) step(status int, hasStatus bool) string {
	if r.StepFormat != "" {
		panic("reddit: promo outcome row " + string(r.Outcome) + " has a StepFormat; render it with stepf")
	}
	if hasStatus && r.StepWithStatus != "" {
		return fmt.Sprintf(r.StepWithStatus, status)
	}
	return r.Step
}

// stepf renders a StepFormat row with its own arguments. A row without a StepFormat is a
// programming error: its step goes through step().
func (r PromoOutcomeRow) stepf(args ...any) string {
	if r.StepFormat == "" {
		panic("reddit: promo outcome row " + string(r.Outcome) + " has no StepFormat; render it with step")
	}
	return fmt.Sprintf(r.StepFormat, args...)
}

// classifyAuthorFailure maps an authoring (Step 1.5) error to its outcome class.
//
// ORDER IS LOAD-BEARING, and is pinned by the dispatcher's table-driven test:
//  1. a caller cancellation AND an ambiguous error first: the post may exist, so the claim must
//     be retained (a nil result releases it and a retry would author a duplicate);
//  2. then any other caller cancellation: nothing was sent;
//  3. then ambiguity BEFORE errors.As(*apiError): an apiError carrying a 5xx, a 429 or a
//     mutating 3xx is both, and testing errors.As first told the operator to author again,
//     the duplicate this classification exists to prevent (#158 round 2);
//  4. then a definite 4xx; anything else is proven pre-send.
func classifyAuthorFailure(ctx context.Context, err error) PromoOutcome {
	return classifyPromoFailure(ctx, err, PromoAuthorCanceledInFlight, PromoAuthorCanceledBeforeSend,
		PromoAuthorUnconfirmed, PromoAuthorRejected, PromoAuthorPreSend)
}

// classifyAdFailure maps an ad-create (Step 4) error to its outcome class, by the same rule and
// in the same order as classifyAuthorFailure.
func classifyAdFailure(ctx context.Context, err error) PromoOutcome {
	return classifyPromoFailure(ctx, err, PromoAdCanceledInFlight, PromoAdCanceledBeforeSend,
		PromoAdUnconfirmed, PromoAdRejected, PromoAdPreSend)
}

func classifyPromoFailure(ctx context.Context, err error, canceledInFlight, canceledBeforeSend, unconfirmed, rejected, preSend PromoOutcome) PromoOutcome {
	ambiguous := createOutcomeAmbiguous(err)
	if ctx.Err() != nil {
		if ambiguous {
			return canceledInFlight
		}
		return canceledBeforeSend
	}
	if ambiguous {
		return unconfirmed
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return rejected
	}
	return preSend
}

// errStatus extracts the HTTP status for a runbook step. It never selects an outcome.
func errStatus(err error) (int, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode, true
	}
	return 0, false
}
