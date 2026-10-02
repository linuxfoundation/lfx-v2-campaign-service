// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Ad group + responsive search ad creation (GA-3): adGroups:mutate ->
// adGroupAds:mutate. This makes the campaign->ad group->ad hierarchy real,
// matching the shape of the reddit and microsoft adapters. Keyword/audience
// targeting on the resulting ad group is GA-4 (see targeting.go) — this file
// calls into it after the ad is created.
// ---------------------------------------------------------------------------

const (
	// adGroupTypeSearchStandard is the only ad group type this client creates
	// today — the standard type for a Search-network ad group.
	adGroupTypeSearchStandard = "SEARCH_STANDARD"

	// maxAdGroupNameRunes mirrors the campaign's limit, not the budget's:
	// AdGroup.name is bounded at 255 CHARACTERS (StringLengthError.TOO_LONG),
	// counted the same way as Campaign.name, not in UTF-8 bytes like
	// CampaignBudget.name. A multibyte ad-group name that fits in 255
	// characters could be rejected by a byte-based check well before the
	// real limit, even though Google would accept it.
	maxAdGroupNameRunes = 255

	// maxFinalURLBytes bounds the ad's composed FinalUrls (the registration URL with the
	// LFX utm_* params appended). Google Ads' v23 System Limits cap a Final URL at 2,084
	// UTF-8 BYTES (not characters — unlike Campaign.name/AdGroup.name, which are measured
	// in runes), including the required protocol prefix; validated on the COMPOSED url up
	// front so a near-limit registration URL can't pass buildAdFinalURL's syntax check and
	// then be rejected only at adGroupAds:mutate — after the budget, campaign, and ad group
	// already exist, orphaning that paid hierarchy for what is purely a local length failure.
	maxFinalURLBytes = 2084

	// minCPCBid/maxCPCBid bound a caller-supplied ad-group CPC bid, expressed in the
	// ad ACCOUNT's currency (not USD — the account's currency is whatever it was
	// opened with, and this client never converts). The range is the same sanity
	// window the Microsoft adapter uses for the same field (microsoft/targeting.go):
	// a floor low enough that no real bid is refused, and a ceiling that catches the
	// units mistake this field invites — a caller passing micros (2_500_000) instead
	// of currency units (2.50) would otherwise set a bid five orders of magnitude
	// over the intent, against a real budget.
	//
	// Neither bound is a Google platform limit. Google's own ceiling is far higher
	// and its floor is currency-dependent, so this is a broker-side guard, and a
	// deliberately loose one: refusing a bid Google would have accepted is the worse
	// failure of the two.
	//
	// The ceiling is sized for the WEAKEST currency an LF account can be opened in,
	// not for USD, precisely because this value is never converted. A 1_000.0 ceiling
	// reads as generous in dollars and refuses ordinary bids in the zero-decimal and
	// low-unit currencies — ~1_000 JPY is under $7, ~2_000 KRW under $2 — so it would
	// have refused creates Google accepts, on accounts this service already supports.
	// 100_000.0 still catches the mistake the guard exists for (micros start at
	// 1_000_000 for a one-unit bid, an order of magnitude above the ceiling in every
	// currency) while clearing any real bid in any of them.
	minCPCBid = 0.01
	maxCPCBid = 100_000.0

	// errCodeDuplicateAdGroupName is Google's AdGroupError code when an ad group
	// name already exists within the campaign — the ad-group analogue of
	// errCodeDuplicateBudgetName/errCodeDuplicateCampaignName. A retry with the
	// same deterministic name (NameSuffix) hits this instead of double-creating.
	errCodeDuplicateAdGroupName = "DUPLICATE_ADGROUP_NAME"
)

// adGroupCreate is the create payload for adGroups:mutate. TargetingSetting is
// set only when GA-4 attaches audience segments (see createAdGroupAndAd) — see
// targetingSetting's doc comment in campaign.go for why this lives at the ad
// group level rather than the campaign level.
//
// CpcBidMicros is omitempty on purpose, and the zero value genuinely means
// "unset" rather than "bid nothing": a manual-CPC ad group created without it
// inherits whatever Google derives for the campaign, which is the behaviour
// every campaign this client created before the field existed had. Emitting an
// explicit 0 instead would be a different request — a zero bid — so the field
// must disappear entirely when the caller supplies no bid.
type adGroupCreate struct {
	Name             string            `json:"name"`
	Campaign         string            `json:"campaign"`
	Status           string            `json:"status"`
	Type             string            `json:"type"`
	CpcBidMicros     int64             `json:"cpcBidMicros,omitempty"`
	TargetingSetting *targetingSetting `json:"targetingSetting,omitempty"`
}

// validateCPCBid validates a caller-supplied ad-group CPC bid and converts it to
// Google's micros.
//
// 0 means UNSET and is returned as (0, nil) — no default is invented. The
// alternative, picking some house default when the caller says nothing, would
// silently change the bid on every campaign created before this field existed.
//
// The omission itself is done by `json:"cpcBidMicros,omitempty"` on adGroupCreate,
// NOT by any flag this function returns: 0 micros reaches the marshaller and the
// field disappears. minCPCBid is what keeps those two meanings from colliding — an
// accepted bid is at least 0.01, so it never rounds to fewer than 10000 micros and
// can never be mistaken for the unset zero. Lowering minCPCBid far enough to break
// that (below 0.0000005) would need an explicit presence flag threaded to the
// payload instead; the Microsoft adapter's validateCpcBid returns exactly such a
// flag because its own payload is not omitempty-driven.
//
// NaN/Inf are rejected before any range comparison, because every comparison
// against NaN is false — a NaN bid would slip past both bounds and then round to
// a garbage int64.
//
// Deliberately NOT built on ValidateBudgetMicros, despite the shared shape (finite
// check, bound, math.Round to micros). That helper's contract is incompatible in the
// one place that matters: it rejects anything rounding to <= 0 micros, so it turns a
// 0 — this function's "caller supplied no bid" — into an error. Routing through it
// would make every create that omits cpcBid fail, which is every caller predating
// this field. The bounds differ too (0.01..1000 in the account currency, versus a
// budget's 1e9 ceiling with no floor). Only microsPerUnit and the
// round-don't-truncate rule are genuinely shared, and both are already single
// definitions.
func validateCPCBid(bid float64) (micros int64, err error) {
	if bid == 0 {
		return 0, nil
	}
	if math.IsNaN(bid) || math.IsInf(bid, 0) {
		return 0, fmt.Errorf("google-ads: cpc bid must be a finite number, got %v", bid)
	}
	if bid < minCPCBid {
		return 0, fmt.Errorf("google-ads: cpc bid must be at least %.2f in the account currency, got %.4f", minCPCBid, bid)
	}
	if bid > maxCPCBid {
		return 0, fmt.Errorf("google-ads: cpc bid %.2f exceeds the maximum %.0f in the account currency", bid, maxCPCBid)
	}
	// math.Round, not truncation, for the same reason the budget conversion rounds:
	// a bid of 0.07 is 0.07000000000000001 in float64, and truncating micros would
	// bill a cent less than the caller asked for on every such value.
	return int64(math.Round(bid * microsPerUnit)), nil
}

// adGroupStatusUpdate is the update payload for adGroups:mutate (status-only toggle,
// mirrors campaignStatusUpdate).
type adGroupStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// responsiveSearchAd is the ad-type payload for an Ad create.
type responsiveSearchAd struct {
	Headlines    []adTextAsset `json:"headlines"`
	Descriptions []adTextAsset `json:"descriptions"`
}

// adCreate is the "ad" object nested in an adGroupAd create.
type adCreate struct {
	FinalUrls          []string            `json:"finalUrls"`
	ResponsiveSearchAd *responsiveSearchAd `json:"responsiveSearchAd,omitempty"`
}

// adGroupAdCreate is the create payload for adGroupAds:mutate.
type adGroupAdCreate struct {
	AdGroup string   `json:"adGroup"`
	Status  string   `json:"status"`
	Ad      adCreate `json:"ad"`
}

// adGroupAdStatusUpdate is the update payload for adGroupAds:mutate (status-only
// toggle, mirrors campaignStatusUpdate).
type adGroupAdStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// isDuplicateAdGroupNameErr reports whether err is Google's AdGroupError
// DUPLICATE_ADGROUP_NAME rejection on a definite 4xx (excluding 429), mirroring
// isDuplicateBudgetNameErr/isDuplicateCampaignNameErr.
func isDuplicateAdGroupNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateAdGroupName)
}

// adGroupAdID splits an adGroupAd resourceName into its ad group id and ad id.
// Unlike most other Google Ads resources, AdGroupAd uses a COMPOSITE trailing
// segment "{adGroupId}~{adId}" (e.g. "customers/1/adGroupAds/111~222") rather
// than a single numeric id — resourceID's plain last-slash split returns that
// whole "111~222" string, so this further splits on "~". Requires EXACTLY two
// components and BOTH must be a non-empty run of ASCII digits (numericID) — a
// third tilde-separated component (e.g. "111~222~333") or a non-numeric half
// is rejected as malformed rather than silently accepted, since the extra/
// non-numeric text would otherwise be carried into res.AdGroupID/AdID and
// later interpolated into a resourceName path by UpdateAdGroupAndAdStatus.
// ALSO validates that the resource KIND is "adGroupAds" (not e.g. "campaigns"),
// so a malformed resource of a wrong type (e.g. "customers/1/campaigns/111~222")
// is correctly rejected rather than incorrectly accepted as a confirmed AdGroupAd.
// Returns ("", "") if the resource name is empty, the resource kind is not
// "adGroupAds", or the trailing segment isn't in that exact shape. AdGroupCriterion
// (GA-4, targeting.go) uses adGroupCriterionID for the same composite shape.
func adGroupAdID(resourceName string) (adGroupID, adID string) {
	// Validate the full resource path structure: customers/<id>/adGroupAds/<composite-id>
	// Split by "/" to validate the resource kind is "adGroupAds" and not something else.
	// Require EXACTLY 4 segments: extra segments indicate a malformed/substituted response.
	pathParts := strings.Split(resourceName, "/")
	if len(pathParts) != 4 || pathParts[0] != "customers" || pathParts[2] != "adGroupAds" {
		return "", ""
	}
	return compositeResourceID(resourceName)
}

// adGroupCriterionID splits an adGroupCriterion resourceName into its ad group id
// and criterion id. Like AdGroupAd, AdGroupCriterion uses a COMPOSITE trailing
// segment "{adGroupId}~{criterionId}" (e.g. "customers/1/adGroupCriteria/111~222")
// rather than a single numeric id. Requires EXACTLY two components and BOTH must be
// a non-empty run of ASCII digits (numericID) — a third tilde-separated component
// or a non-numeric half is rejected as malformed. ALSO validates that the resource
// KIND is "adGroupCriteria" (not e.g. "campaigns" or "adGroupAds") AND that the
// customer segment is THIS client's current account, mirroring
// validateCampaignResource's own cross-account check — a malformed/substituted
// resourceName naming another customer's adGroupCriteria must not be trusted enough
// to persist. Returns ("", "") if the resource name is empty, the resource kind is
// not "adGroupCriteria", the customer segment doesn't match, or the trailing segment
// isn't in the exact composite shape.
func (c *Client) adGroupCriterionID(resourceName string) (adGroupID, criterionID string) {
	// Validate the full resource path structure: customers/<id>/adGroupCriteria/<composite-id>
	// Split by "/" to validate the resource kind is "adGroupCriteria" and not something else.
	// Require EXACTLY 4 segments, matching adGroupAdID: extra segments indicate a
	// malformed/substituted response and must be rejected, not accepted with the
	// extra segments silently ignored.
	pathParts := strings.Split(resourceName, "/")
	if len(pathParts) != 4 || pathParts[0] != "customers" || pathParts[2] != "adGroupCriteria" {
		return "", ""
	}
	if pathParts[1] != c.account.CustomerID {
		return "", ""
	}
	return compositeResourceID(resourceName)
}

// campaignCriterionID is adGroupCriterionID's sibling for campaignCriteria resource names,
// applying the same four checks: exactly four segments, the "campaignCriteria" kind, THIS
// client's customer id, and the composite "{campaignId}~{criterionId}" shape.
//
// It exists because the campaign path previously used bare resourceID, which returns any
// non-empty trailing segment. That accepted a 2xx naming another ACCOUNT's criterion, a
// different resource KIND, another CAMPAIGN's criterion, and even "garbage/4242" — each
// persisted as a successful geo attachment. A resource name is the only proof of what a record
// IS, so a lenient parse here is an identity claim nobody checked.
func (c *Client) campaignCriterionID(resourceName string) (campaignID, criterionID string) {
	pathParts := strings.Split(resourceName, "/")
	if len(pathParts) != 4 || pathParts[0] != "customers" || pathParts[2] != "campaignCriteria" {
		return "", ""
	}
	if pathParts[1] != c.account.CustomerID {
		return "", ""
	}
	return compositeResourceID(resourceName)
}

// compositeResourceID splits a resourceName's trailing "{parentId}~{id}"
// segment — the shape AdGroupAd and AdGroupCriterion resource names use,
// unlike every single-id resource this package otherwise handles via
// resourceID. Returns ("", "") if the resource name is empty or the trailing
// segment isn't in that shape.
func compositeResourceID(resourceName string) (parentID, id string) {
	trailing := resourceID(resourceName)
	if trailing == "" {
		return "", ""
	}
	parts := strings.Split(trailing, "~")
	if len(parts) != 2 || !numericID(parts[0]) || !numericID(parts[1]) {
		return "", ""
	}
	return parts[0], parts[1]
}

// precomputeAdGroupAdInputs validates and derives everything createAdGroupAndAd
// needs (destination URL, ad copy, ad-group name) WITHOUT sending any request.
// CreateCampaign calls this BEFORE the first (budget) mutate: an invalid
// RegistrationURL, an over-length composed final URL, unusable ad copy,
// over-length ad-group name, or bad keyword/audience-segment (GA-4) input
// must fail before any Google Ads resource is created, not after the
// budget+campaign already committed — surfacing it only inside
// createAdGroupAndAd (which runs after both prior mutates) would orphan a
// real campaign+budget with no ad group/ad for what is purely a local
// input-validation failure.
func precomputeAdGroupAdInputs(in CampaignInput) (finalURL string, headlines, descriptions []string, adGroupName string, keywords []Keyword, audienceSegments []string, err error) {
	finalURL, err = buildAdFinalURL(in.RegistrationURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
	if err != nil {
		return "", nil, nil, "", nil, nil, fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid destination URL): %w", err)
	}
	if n := len(finalURL); n > maxFinalURLBytes {
		return "", nil, nil, "", nil, nil, fmt.Errorf("google-ads ad group/ad creation aborted before any request (composed ad final URL is %d bytes, exceeding the %d limit; shorten the registration URL)", n, maxFinalURLBytes)
	}
	headlines, descriptions, err = composeAdCopy(in.Headlines, in.Descriptions, in.EventName, in.Project)
	if err != nil {
		return "", nil, nil, "", nil, nil, fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid ad copy): %w", err)
	}
	adGroupName = ComposeName("Ad Group", in)
	if err := validateEntityName("ad group", adGroupName, utf8.RuneCountInString(adGroupName), maxAdGroupNameRunes, "characters"); err != nil {
		return "", nil, nil, "", nil, nil, err
	}
	keywords, err = validateKeywords(in.Keywords)
	if err != nil {
		return "", nil, nil, "", nil, nil, fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid keyword input): %w", err)
	}
	audienceSegments, err = validateAudienceSegments(in.AudienceSegments)
	if err != nil {
		return "", nil, nil, "", nil, nil, fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid audience segment input): %w", err)
	}
	return finalURL, headlines, descriptions, adGroupName, keywords, audienceSegments, nil
}

// createAdGroupAndAd extends a just-created campaign with a PAUSED ad group and a
// PAUSED responsive search ad. Both are created with a single mutate call each
// (no idempotency key on either), so the same ambiguous/duplicate classification
// used for the budget/campaign applies. Unlike the budget/campaign duplicate
// branches, a DUPLICATE_ADGROUP_NAME on retry does NOT look up the existing ad
// group's id — it is reported the same way the budget/campaign duplicates are,
// "already exists, reconcile by name" — so a retry after an ambiguous or
// duplicate ad-group outcome does not re-attempt the ad create either (there is
// no id to attach it to). That mirrors this package's existing choice to prefer
// simple create-then-catch over find-then-create for named resources; a
// campaign left in that state needs manual reconciliation, same as a
// duplicate-budget or duplicate-campaign orphan today.
//
// finalURL and every field of plan are resolved by preflightCampaignKind
// (precomputeAdGroupAdInputs, then validateAdGroupPlans) BEFORE CreateCampaign's
// first mutate, so this method performs no local validation of its own — by the
// time it runs, the campaign already exists and there is nothing left to
// reject before sending. If both plan.keywords and plan.audienceSegments are
// empty (GA-4 targeting is optional), the ad group/ad are created with no
// criteria, same as pre-GA-4 behavior.
//
// res is mutated in place (AdGroups/AdGroupName/AdGroupID/AdID/Steps) so the
// caller's existing partial-result plumbing (campaignNamePartial-derived)
// carries whatever was created even when this returns an error.
//
// One plan per ad group: createAdGroupsAndAds loops this over the whole list,
// and a single-group campaign is simply a list of one.
func (c *Client) createAdGroupAndAd(ctx context.Context, campaignResource, campaignID, finalURL string, plan adGroupPlan, res *CampaignResult) error {
	adGroupName := plan.name
	cpcBidMicros := plan.cpcBidMicros
	keywords := plan.keywords
	audienceSegments := plan.audienceSegments

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad group creation aborted before any request (context already done): %w", ctxErr)
	}

	adGroupCreateVal := adGroupCreate{
		Name:         adGroupName,
		Campaign:     campaignResource,
		Status:       StatusPaused,
		Type:         adGroupTypeSearchStandard,
		CpcBidMicros: cpcBidMicros,
	}
	// See targetingSetting's doc comment (campaign.go): GA-4's audience criteria
	// are AdGroupCriterions, so the observation-only setting must be declared
	// here, on the ad group create, not on the campaign create.
	if len(audienceSegments) > 0 {
		adGroupCreateVal.TargetingSetting = &targetingSetting{
			TargetRestrictions: []targetRestriction{{TargetingDimension: "AUDIENCE", BidOnly: true}},
		}
	}
	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupCreateVal}}}
	// Record the ad group name before sending the mutate, so that on failure
	// (duplicate, ambiguous) the partial result still carries the deterministic name for
	// reconciliation. With several groups this is also what says HOW FAR the cascade got:
	// the entry exists for every group that was attempted, with an empty ID for the one
	// that failed and no entry at all for the groups never reached.
	isFirstAdGroup := len(res.AdGroups) == 0
	res.AdGroups = append(res.AdGroups, AdGroupResult{Name: adGroupName})
	group := &res.AdGroups[len(res.AdGroups)-1]
	// The scalar AdGroupName/AdGroupID/AdID predate multi-group support and are kept
	// populated from the FIRST group, so every existing reader — the dispatcher's
	// status toggle, the campaign_settings readback, persisted result blobs — keeps
	// working unchanged on a single-group campaign and still resolves to a real ad
	// group on a multi-group one.
	if isFirstAdGroup {
		res.AdGroupName = adGroupName
	}
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		switch {
		case isDuplicateAdGroupNameErr(err):
			return fmt.Errorf("google-ads ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", adGroupName, err)
		case createOutcomeAmbiguous(err):
			return fmt.Errorf("google-ads ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", adGroupName, err)
		default:
			return fmt.Errorf("google-ads ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return fmt.Errorf("google-ads ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", adGroupName, err)
	}
	// firstResourceName only extracts a trailing id; it does not check resource kind or
	// account. Without this, a malformed/wrong-account 2xx (e.g. a different customer's
	// adGroups resource) would be accepted as confirmed and its id persisted as AdGroupID.
	if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
		return fmt.Errorf("google-ads ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", adGroupName, verr)
	}
	group.ID = adGroupID
	if isFirstAdGroup {
		res.AdGroupID = adGroupID
	}
	// Two variants rather than one with a formatted zero: the step log is read by
	// operators reconciling a campaign against the account, and "CpcBidMicros 0" would
	// read as a zero bid that was set rather than a field that was never sent. Neither
	// variant claims what the presence or absence of a bid does to serving — that is
	// Google's behaviour to state, not this client's, and it has not been verified
	// against a live account from here.
	if cpcBidMicros > 0 {
		res.Steps = append(res.Steps, fmt.Sprintf("Ad group created: %s (PAUSED, %s, CPC bid %.2f in the account currency)",
			adGroupID, adGroupTypeSearchStandard, float64(cpcBidMicros)/microsPerUnit))
	} else {
		res.Steps = append(res.Steps, fmt.Sprintf("Ad group created: %s (PAUSED, %s, no CPC bid set)", adGroupID, adGroupTypeSearchStandard))
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad creation aborted after ad group %s created (context done before ad create; the ad group has no ad yet): %w", adGroupID, ctxErr)
	}

	// Every ad in the group goes in ONE mutate. Google applies a mutate atomically
	// unless partial failure is asked for, so a group either gets all its ads or
	// none — which is the outcome worth having, because a group holding one of the
	// three ads a caller asked for reads as complete in the UI and quietly rotates
	// less copy than the campaign was built to test.
	adOps := make([]mutateOperation, 0, len(plan.ads))
	for _, ad := range plan.ads {
		adOps = append(adOps, mutateOperation{Create: adGroupAdCreate{
			AdGroup: adGroupResource,
			Status:  StatusPaused,
			Ad: adCreate{
				FinalUrls: []string{finalURL},
				ResponsiveSearchAd: &responsiveSearchAd{
					Headlines:    textAssets(ad.headlines),
					Descriptions: textAssets(ad.descriptions),
				},
			},
		}})
	}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), mutateRequest{Operations: adOps}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; %d ad(s) may exist — verify in Google Ads before retrying): %w", adGroupID, len(adOps), err)
		}
		// Ads carry no unique name/duplicate-error code (Google allows duplicate ad
		// content within an ad group), so a definite 4xx here is a straightforward
		// rejection, not a possible prior-attempt collision.
		return fmt.Errorf("google-ads ad creation failed (ad group %s created): %w", adGroupID, err)
	}
	var adResults mutateResponse
	if uErr := json.Unmarshal(adResp, &adResults); uErr != nil || len(adResults.Results) != len(adOps) {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; 2xx with a malformed/short mutate response for %d ad(s) — ads may exist — verify in Google Ads before retrying)", adGroupID, len(adOps))
	}
	adIDs := make([]string, 0, len(adOps))
	for i, r := range adResults.Results {
		adResource := r.ResourceName
		// adGroupAdID validates the resource KIND but not the account; without this, a
		// wrong-account adGroupAds resource would still pass adGroupAdID and could be
		// accepted as this ad. requireNumericID=false: the trailing segment is the
		// composite "{adGroupId}~{adId}" shape, validated by adGroupAdID below.
		if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; ad %d of %d: %w — verify in Google Ads before retrying)", adGroupID, i+1, len(adOps), verr)
		}
		returnedAdGroupID, adID := adGroupAdID(adResource)
		if adID == "" || returnedAdGroupID == "" {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, adResource, i)
		}
		// The adGroupAd resourceName's ad-group-id half must match the ad group this
		// ad was created under — a mismatch means the response doesn't describe the
		// ad this call just created (a malformed/substituted resourceName), so the
		// returned adID cannot be trusted enough to persist.
		if returnedAdGroupID != adGroupID {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
		}
		adIDs = append(adIDs, adID)
	}
	group.AdIDs = adIDs
	// The scalar AdID is the FIRST ad of the FIRST group, for the same
	// compatibility reason AdGroupID is.
	if isFirstAdGroup && len(adIDs) > 0 {
		res.AdID = adIDs[0]
	}
	if len(adIDs) == 1 {
		res.Steps = append(res.Steps, fmt.Sprintf("Responsive search ad created: %s (PAUSED, %d headlines, %d descriptions)", adIDs[0], len(plan.ads[0].headlines), len(plan.ads[0].descriptions)))
	} else {
		res.Steps = append(res.Steps, fmt.Sprintf("Responsive search ads created: %s (PAUSED, %d ads in ad group %s)", strings.Join(adIDs, ", "), len(adIDs), adGroupID))
	}

	if len(keywords) == 0 && len(audienceSegments) == 0 {
		return nil
	}
	keywordIDs, audienceIDs, err := c.createAdGroupTargeting(ctx, adGroupResource, adGroupID, keywords, audienceSegments)
	if err != nil {
		return err
	}
	group.KeywordCriteriaIDs = keywordIDs
	group.AudienceCriteriaIDs = audienceIDs
	if isFirstAdGroup {
		res.KeywordCriteriaIDs = keywordIDs
		res.AudienceCriteriaIDs = audienceIDs
	}
	res.Steps = append(res.Steps, fmt.Sprintf("Keyword/audience targeting attached: %d keyword(s), %d audience segment(s)", len(keywordIDs), len(audienceIDs)))
	return nil
}

// createAdGroupsAndAds runs the ad group + ad cascade once per planned group.
//
// It stops at the FIRST failure rather than carrying on with the remaining
// groups. A failure here is almost never specific to one group — a dead context,
// a revoked token, a rate limit — and pressing on would turn one reconcilable
// partial into several. What was built is already in res.AdGroups, so the caller
// knows exactly which groups exist and which were never attempted.
func (c *Client) createAdGroupsAndAds(ctx context.Context, campaignResource, campaignID, finalURL string, plans []adGroupPlan, res *CampaignResult) error {
	for i, plan := range plans {
		if err := c.createAdGroupAndAd(ctx, campaignResource, campaignID, finalURL, plan, res); err != nil {
			if len(plans) == 1 {
				return err
			}
			return fmt.Errorf("ad group %d of %d (%d created before it): %w", i+1, len(plans), i, err)
		}
	}
	return nil
}

// UpdateAdGroupAndAdStatus toggles an ad group and its ad between ENABLED and
// PAUSED, mirroring UpdateCampaignStatus. Both mutates are sent as idempotent
// (bounded 429 retries are safe: re-applying the same status converges, same
// reasoning as UpdateCampaignStatus). Returns after the FIRST failure without
// attempting the second mutate — the caller (GoogleAdsDispatcher.ToggleStatus, which
// wires this cascade) orders campaign/ad-group/ad calls per the children-first-on-ACTIVATE /
// campaign-first-on-PAUSE contract, so a failed ad group update must not mask
// itself as "ad group ok, ad unknown".
func (c *Client) UpdateAdGroupAndAdStatus(ctx context.Context, adGroupID, adID, status string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	if status != StatusEnabled && status != StatusPaused {
		return fmt.Errorf("google-ads: unsupported ad group/ad status %q (want %s or %s)", status, StatusEnabled, StatusPaused)
	}
	adGroupID = strings.TrimSpace(adGroupID)
	adID = strings.TrimSpace(adID)
	if adGroupID == "" || adID == "" {
		return fmt.Errorf("google-ads: cannot update ad group/ad status: ad group id and ad id must both be set")
	}
	if !numericID(adGroupID) {
		return fmt.Errorf("google-ads: ad group id %q is not numeric", adGroupID)
	}
	if !numericID(adID) {
		return fmt.Errorf("google-ads: ad id %q is not numeric", adID)
	}

	adGroupReq := mutateRequest{Operations: []mutateOperation{{
		Update: adGroupStatusUpdate{
			ResourceName: "customers/" + c.account.CustomerID + "/adGroups/" + adGroupID,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, true); err != nil {
		return fmt.Errorf("google-ads ad group %s status update to %s failed: %w", adGroupID, status, err)
	}

	adReq := mutateRequest{Operations: []mutateOperation{{
		Update: adGroupAdStatusUpdate{
			ResourceName: "customers/" + c.account.CustomerID + "/adGroupAds/" + adGroupID + "~" + adID,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, true); err != nil {
		// The ad group update already succeeded, and the ad update failed.
		// This is a partial cascade: the tree is partially applied. Wrap the error
		// so IsOutcomeUnconfirmed recognizes it as unconfirmed, matching the pattern
		// used by the reddit and twitter cascade clients.
		return &partialCascadeError{stage: "ad", err: err}
	}
	return nil
}

// partialCascadeError marks a cascade that changed the ad group upstream but then
// failed on the ad entity: the run state is PARTIALLY applied. Its Unconfirmed()
// reports true so the caller (via IsOutcomeUnconfirmed) treats it as "may be
// applied — verify before retrying" rather than "not modified"; a retry re-runs
// the idempotent cascade.
type partialCascadeError struct {
	stage string
	err   error
}

func (e *partialCascadeError) Error() string {
	return "google-ads: ad group status changed but the " + e.stage + " update failed (partially applied): " + e.err.Error()
}

func (e *partialCascadeError) Unwrap() error { return e.err }

// Unconfirmed marks the outcome as ambiguous-applied for IsOutcomeUnconfirmed.
func (e *partialCascadeError) Unconfirmed() bool { return true }

// numericID reports whether s is a non-empty run of ASCII digits — the same
// shape check UpdateCampaignStatus applies to a campaign id, reused here so an
// id interpolated into a resourceName can't alter the resource path.
// strconv.ParseUint accepts a leading "+", so explicitly reject every rune outside 0–9.
func numericID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
