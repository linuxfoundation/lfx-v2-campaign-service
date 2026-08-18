// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
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
type adGroupCreate struct {
	Name             string            `json:"name"`
	Campaign         string            `json:"campaign"`
	Status           string            `json:"status"`
	Type             string            `json:"type"`
	TargetingSetting *targetingSetting `json:"targetingSetting,omitempty"`
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
// finalURL/headlines/descriptions/adGroupName/keywords/audienceSegments are
// precomputed by precomputeAdGroupAdInputs BEFORE CreateCampaign's first
// mutate, so this method performs no local validation of its own — by the
// time it runs, the campaign already exists and there is nothing left to
// reject before sending. If both keywords and audienceSegments are empty
// (GA-4 targeting is optional), the ad group/ad are created with no
// criteria, same as pre-GA-4 behavior.
//
// res is mutated in place (AdGroupName/AdGroupID/AdID/Steps) so the caller's
// existing partial-result plumbing (campaignNamePartial-derived) carries
// whatever was created even when this returns an error.
func (c *Client) createAdGroupAndAd(ctx context.Context, campaignResource, campaignID, finalURL string, headlines, descriptions []string, adGroupName string, keywords []Keyword, audienceSegments []string, res *CampaignResult) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad group creation aborted before any request (context already done): %w", ctxErr)
	}

	adGroupCreateVal := adGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   StatusPaused,
		Type:     adGroupTypeSearchStandard,
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
	// Set the ad group name into the result before sending the mutate, so that on failure
	// (duplicate, ambiguous) the partial result still carries the deterministic name for
	// reconciliation.
	res.AdGroupName = adGroupName
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
	res.AdGroupID = adGroupID
	res.Steps = append(res.Steps, fmt.Sprintf("Ad group created: %s (PAUSED, %s)", adGroupID, adGroupTypeSearchStandard))

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad creation aborted after ad group %s created (context done before ad create; the ad group has no ad yet): %w", adGroupID, ctxErr)
	}

	adReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  StatusPaused,
		Ad: adCreate{
			FinalUrls: []string{finalURL},
			ResponsiveSearchAd: &responsiveSearchAd{
				Headlines:    textAssets(headlines),
				Descriptions: textAssets(descriptions),
			},
		},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		// Ads carry no unique name/duplicate-error code (Google allows duplicate ad
		// content within an ad group), so a definite 4xx here is a straightforward
		// rejection, not a possible prior-attempt collision.
		return fmt.Errorf("google-ads ad creation failed (ad group %s created): %w", adGroupID, err)
	}
	adResource, _, err := firstResourceName(adResp)
	if err != nil {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; 2xx with no/malformed resource name — an ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
	}
	// adGroupAdID validates the resource KIND but not the account; without this, a
	// wrong-account adGroupAds resource would still pass adGroupAdID and could be
	// accepted as this ad. requireNumericID=false: the trailing segment is the
	// composite "{adGroupId}~{adId}" shape, validated by adGroupAdID below.
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID == "" {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	// The adGroupAd resourceName's ad-group-id half must match the ad group this
	// ad was created under — a mismatch means the response doesn't describe the
	// ad this call just created (a malformed/substituted resourceName), so the
	// returned adID cannot be trusted enough to persist.
	if returnedAdGroupID != adGroupID {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
	}
	res.AdID = adID
	res.Steps = append(res.Steps, fmt.Sprintf("Responsive search ad created: %s (PAUSED, %d headlines, %d descriptions)", adID, len(headlines), len(descriptions)))

	if len(keywords) == 0 && len(audienceSegments) == 0 {
		return nil
	}
	keywordIDs, audienceIDs, err := c.createAdGroupTargeting(ctx, adGroupResource, adGroupID, keywords, audienceSegments)
	if err != nil {
		return err
	}
	res.KeywordCriteriaIDs = keywordIDs
	res.AudienceCriteriaIDs = audienceIDs
	res.Steps = append(res.Steps, fmt.Sprintf("Keyword/audience targeting attached: %d keyword(s), %d audience segment(s)", len(keywordIDs), len(audienceIDs)))
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
