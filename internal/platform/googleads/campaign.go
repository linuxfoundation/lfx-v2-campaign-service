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
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Campaign creation (GA-2): campaignBudget:mutate -> campaigns:mutate
// ---------------------------------------------------------------------------

const (
	// microsPerUnit converts a currency amount to Google Ads "micros"
	// (amount * 1,000,000). Budgets are expressed in micros of the account's
	// currency.
	microsPerUnit = 1_000_000

	// maxBudget caps the budget well below the int64 micros-overflow threshold
	// (math.MaxInt64 / 1e6 ≈ 9.2e12) so the *microsPerUnit conversion can never wrap
	// to a negative value. Mirrors the reddit/twitter clients' budget cap. This is a
	// sanity bound on caller input, NOT the account's minimum — Google enforces a
	// currency-dependent minimum server-side and rejects a too-low budget with a
	// campaignBudgetError, which surfaces as a definite failure.
	maxBudget = 1_000_000_000.0

	// maxBudgetNameBytes / maxCampaignNameRunes bound the composed names. Google Ads
	// v23 applies DIFFERENT limits in DIFFERENT UNITS (verified against the v23 System
	// Limits table + the RPC field references):
	//   - CampaignBudget.name: 1..255 inclusive, in UTF-8 BYTES (trimmed).
	//   - Campaign.name:        up to 256 CHARACTERS (StringLengthError.TOO_LONG).
	// The unit difference matters: a multibyte name hits the budget's byte ceiling
	// sooner than 255 characters, while the campaign limit is counted in characters.
	// Each name is validated against its own limit+unit before any create call, so an
	// over-limit name is rejected up front rather than creating the budget and then
	// failing the campaign mutate (which would orphan the budget). (Names must also be
	// unique per account, and an unbounded caller EventName could produce an oversized
	// payload — both reasons to cap.)
	maxBudgetNameBytes   = 255
	maxCampaignNameRunes = 256

	// advertisingChannelSearch is the only channel type this client creates today.
	advertisingChannelSearch = "SEARCH"

	// euPoliticalAdvertisingNo declares a campaign does NOT contain EU political
	// advertising — required on every v23 campaign create (see campaignCreate).
	euPoliticalAdvertisingNo = "DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING"

	// errCodeDuplicateBudgetName is Google's CampaignBudgetError code when a
	// non-shared campaign BUDGET name already exists.
	errCodeDuplicateBudgetName = "DUPLICATE_NAME"
	// errCodeDuplicateCampaignName is Google's CampaignError code when a CAMPAIGN
	// name already exists — a DIFFERENT code from the budget's DUPLICATE_NAME. Using
	// the wrong one means the campaign-duplicate branch never fires.
	//
	// Because :mutate has no idempotency key, a retried create that reuses a
	// deterministic name fails with the family-appropriate code — callers treat it as
	// "already exists, reconcile by name" rather than a fresh failure. See
	// isDuplicateBudgetNameErr / isDuplicateCampaignNameErr.
	errCodeDuplicateCampaignName = "DUPLICATE_CAMPAIGN_NAME"
)

// CampaignInput is the platform-agnostic request to create a Google Ads campaign:
// a PAUSED search-campaign shell (GA-2), an ad group + responsive search ad (GA-3),
// and keyword/audience targeting on that ad group (GA-4).
type CampaignInput struct {
	// EventName is the human-readable campaign subject, folded into the budget and
	// campaign names. Caller-supplied and otherwise unbounded, so it is trimmed and
	// the composed names are length-capped before any create call.
	EventName string
	// EventSlug is the URL-safe event identifier, used (with EventName/Project as
	// fallbacks) to build the ad's utm_campaign click-through param — see
	// buildAdFinalURL in ad_copy.go.
	EventSlug string
	// Project is folded into the composed name alongside EventName.
	Project string
	// RegistrationURL is the event's destination page. Required for the ad group +
	// ad cascade (GA-3): it becomes the ad's final URL, tagged with UTM params via
	// buildAdFinalURL. Mirrors the reddit/microsoft clients' RegistrationURL field.
	RegistrationURL string
	// Headlines / Descriptions are caller-supplied Responsive Search Ad copy.
	// Optional: composeAdCopy pads missing slots with deterministic placeholders
	// derived from EventName/Project up to the platform minimum (3 headlines, 2
	// descriptions), so a caller may leave these nil.
	Headlines    []string
	Descriptions []string
	// Budget is the campaign daily budget in whole units of the ad ACCOUNT's
	// currency. IMPORTANT: this is NOT a USD amount and the client performs NO
	// foreign-exchange conversion — Google interprets the resulting amountMicros in
	// the account's own currency, so a value of 50 becomes 50 of whatever the account
	// is denominated in (USD, EUR, JPY, …). The caller must supply an amount already
	// denominated in the account currency. Converted to micros (×1,000,000); must be
	// > 0 and <= maxBudget. (Renamed from BudgetUSD, which implied an FX conversion
	// this client does not do — mirrors the meta client's Budget field.)
	Budget float64
	// NameSuffix, when non-empty, is appended to the composed budget/campaign names
	// to make them unique+deterministic per logical campaign. A caller that wants
	// at-most-once retry semantics passes a stable value (e.g. the brief id): a
	// retry then reuses the same name and Google rejects it with DUPLICATE_NAME,
	// which the client reports as UNCONFIRMED-already-exists rather than re-creating.
	NameSuffix string
	// Keywords are positive Search keyword criteria attached to the ad group
	// (GA-4). Optional: an ad group created with none still exists but matches
	// no query, so a caller that wants the campaign to actually serve once
	// enabled must supply at least one. See validateKeywords for the
	// text/match-type/count rules.
	Keywords []Keyword
	// AudienceSegments are EXISTING Google Ads audience resource names (Customer
	// Match "user list" resources the caller has already built elsewhere — this
	// client does not create audiences) attached to the ad group as observation-only
	// criteria (GA-4): see validateAudienceSegments for the accepted resource-name
	// shapes and createAdGroupTargeting for why they're observation-only rather than
	// restrictive.
	AudienceSegments []string
}

// CampaignResult reports what CreateCampaign created. The Google Ads hierarchy is
// campaignBudget -> campaign, so both names and both IDs are surfaced for
// reconcile/cleanup. The NAMES matter on an ambiguous/duplicate failure BEFORE an id
// is known: the budget and campaign have DIFFERENT deterministic names (LFX | Budget
// | … vs LFX | Search Campaign | …), so a caller reconciling a possibly-orphaned
// budget must look it up by CampaignBudgetName — CampaignName would not find it.
type CampaignResult struct {
	Platform           string `json:"platform"`
	AccountLabel       string `json:"accountLabel,omitempty"`
	CampaignName       string `json:"campaignName"`
	CampaignBudgetName string `json:"campaignBudgetName"`
	CampaignID         string `json:"campaignId"`
	CampaignBudgetID   string `json:"campaignBudgetId"`
	// AdGroupName/AdGroupID/AdID are set by the GA-3 ad group + ad cascade
	// (createAdGroupAndAd in adgroup_ad.go). AdGroupID/AdID are empty on a
	// pre-ad-group failure; AdID alone is empty if the ad group was created but
	// the ad step failed/is unconfirmed.
	AdGroupName string `json:"adGroupName,omitempty"`
	AdGroupID   string `json:"adGroupId,omitempty"`
	AdID        string `json:"adId,omitempty"`
	// KeywordCriteriaIDs/AudienceCriteriaIDs are set by the GA-4 targeting step
	// (createAdGroupTargeting in targeting.go) when the corresponding input list
	// was non-empty. Both are empty if targeting was never attempted or failed
	// before any criterion resource name could be parsed.
	KeywordCriteriaIDs  []string `json:"keywordCriteriaIds,omitempty"`
	AudienceCriteriaIDs []string `json:"audienceCriteriaIds,omitempty"`
	GoogleAdsURL        string   `json:"googleAdsUrl"`
	Steps               []string `json:"steps"`
}

// mutateOperation is one {create: <resource>} entry in a :mutate request.
type mutateOperation struct {
	// omitempty so an UPDATE operation doesn't emit "create":null — a :mutate operation
	// must carry exactly ONE of create/update/remove, and a null create alongside an update
	// is an invalid (and confusing) payload.
	Create any `json:"create,omitempty"`
	// Update + UpdateMask carry an UPDATE operation (e.g. a status flip). Both are omitted
	// on a create so the payload stays exactly as the create path sends it today.
	Update     any    `json:"update,omitempty"`
	UpdateMask string `json:"updateMask,omitempty"`
}

// mutateRequest is the POST body for a *:mutate endpoint. partialFailure is left
// false (default): each call carries a single operation, so the request either
// wholly succeeds or wholly fails — there is no partial state to report.
type mutateRequest struct {
	Operations []mutateOperation `json:"operations"`
}

// mutateResponse is the (subset of the) :mutate response we consume. results is
// index-aligned with the request operations; each carries the created resource's
// resourceName (RESOURCE_NAME_ONLY is the default responseContentType).
type mutateResponse struct {
	Results []struct {
		ResourceName string `json:"resourceName"`
	} `json:"results"`
}

// campaignBudgetCreate is the create payload for campaignBudgets:mutate.
type campaignBudgetCreate struct {
	Name           string `json:"name"`
	AmountMicros   int64  `json:"amountMicros"`
	DeliveryMethod string `json:"deliveryMethod"`
	// ExplicitlyShared=false makes this a non-shared budget bound to one campaign
	// (Google defaults budgets to shared). A pointer so the false value is always
	// emitted rather than omitted.
	ExplicitlyShared *bool `json:"explicitlyShared"`
}

// campaignCreate is the create payload for campaigns:mutate. Exactly one bidding
// strategy is required; manualCpc{} is the dependency-free choice for a PAUSED
// shell (maximizeConversions requires conversion tracking configured on the
// account, which a generic broker can't assume).
//
// containsEuPoliticalAdvertising is REQUIRED on every v23 create: omitting it fails
// with FieldError.REQUIRED, and since 2026-04-01 an account with any undeclared
// campaign has ALL mutate calls rejected with
// MutateError.EU_POLITICAL_ADVERTISING_DECLARATION_REQUIRED. These are non-political
// ad campaigns, so we declare DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING.
//
// networkSettings must be set for a SEARCH create: a Campaign that targets NO network
// (which is what an omitted networkSettings resolves to — proto3 bools default false)
// is rejected with CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK, AFTER the
// budget mutate has committed (an avoidable orphan). Google documents no protective
// default, and every official create sample sets it. We target Google Search only
// (the conservative choice for a PAUSED broker shell); targetSearchNetwork stays false
// because true would require targetGoogleSearch AND opt this into Search Partners,
// which a generic broker shouldn't assume.
type campaignCreate struct {
	Name                           string          `json:"name"`
	Status                         string          `json:"status"`
	AdvertisingChannelType         string          `json:"advertisingChannelType"`
	CampaignBudget                 string          `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string          `json:"containsEuPoliticalAdvertising"`
	NetworkSettings                networkSettings `json:"networkSettings"`
	ManualCPC                      json.RawMessage `json:"manualCpc"`
}

// targetingSetting / targetRestriction declare, per targeting dimension, whether
// a criterion of that dimension RESTRICTS delivery or only observes it (bids/
// reports without narrowing reach). Google requires targetingSetting be set at
// the SAME level the criterion is attached to — an AdGroup's targetingSetting
// cannot even be set while the parent Campaign has one, and a campaign-level
// setting has no effect on ad-group-level criteria (see Google's
// UpdateAudienceTargetRestriction sample, which reads/writes ad_group, not
// campaign, targeting_setting). GA-4's audience criteria are created as
// AdGroupCriterions (createAdGroupAndAd), so this is set on the AD GROUP
// create, not the campaign create — see adGroupCreate in adgroup_ad.go. With
// no targetingSetting at all, Google's default for a Search ad group's
// AUDIENCE dimension is TARGETING (restrictive) — an audience segment added
// for bid/reporting purposes would otherwise silently narrow delivery to that
// segment alone, a serious, easy-to-miss behavior change. bidOnly=true keeps
// AUDIENCE observation-only so keyword targeting (GA-4's other half) remains
// the thing that actually controls reach.
type targetingSetting struct {
	TargetRestrictions []targetRestriction `json:"targetRestrictions"`
}

type targetRestriction struct {
	TargetingDimension string `json:"targetingDimension"`
	BidOnly            bool   `json:"bidOnly"`
}

// networkSettings selects which networks a campaign's ads serve on. For a SEARCH
// campaign, targetGoogleSearch MUST be true (see campaignCreate). The remaining flags
// are sent explicitly as false rather than omitted so the payload is unambiguous.
type networkSettings struct {
	TargetGoogleSearch   bool `json:"targetGoogleSearch"`
	TargetSearchNetwork  bool `json:"targetSearchNetwork"`
	TargetContentNetwork bool `json:"targetContentNetwork"`
}

// googleAdsErrorEnvelope is the error body shape: the machine-readable error codes
// live at error.details[<GoogleAdsFailure>].errors[].errorCode, which is a
// single-key object (category -> enum). message/requestId are intentionally NOT
// captured — only the codes are retained, and only for internal classification.
type googleAdsErrorEnvelope struct {
	Error struct {
		Details []struct {
			Type   string `json:"@type"`
			Errors []struct {
				ErrorCode map[string]json.RawMessage `json:"errorCode"`
			} `json:"errors"`
		} `json:"details"`
	} `json:"error"`
}

// parseErrorCodes extracts Google's enum error codes from a non-2xx body, e.g.
// "DUPLICATE_NAME" or "REQUIRED". Each errorCode is a single-key object whose VALUE
// is the enum constant; we retain the values (the enum constants) for matching.
// Over-long values and codes beyond the cap are dropped: they are used only for
// enum classification, never surfaced, so bounding them keeps a hostile body from
// being retained even internally. Returns nil on a malformed/absent body.
func parseErrorCodes(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var env googleAdsErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	var codes []string
	for _, d := range env.Error.Details {
		if !strings.HasSuffix(d.Type, "GoogleAdsFailure") {
			continue
		}
		for _, e := range d.Errors {
			for _, raw := range e.ErrorCode {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil || v == "" {
					continue
				}
				if len(v) > maxErrorCodeLen {
					continue
				}
				codes = append(codes, v)
				if len(codes) >= maxRetainedErrorCodes {
					return codes
				}
			}
		}
	}
	return codes
}

const (
	maxRetainedErrorCodes = 16
	maxErrorCodeLen       = 128
)

// hasErrorCode reports whether the apiError carried the given Google Ads enum
// error code. It reads the ErrorCodes parsed from the FULL body in doRequest — NOT
// the truncated Body — so classification works for error payloads longer than
// maxErrorBodyChars.
func (e *apiError) hasErrorCode(code string) bool {
	for _, c := range e.ErrorCodes {
		if strings.EqualFold(c, code) {
			return true
		}
	}
	return false
}

// isDefiniteClientError reports whether ae is a definite 4xx client-error rejection
// that is NOT ambiguous — i.e. a 4xx EXCEPT 429. A 429 is excluded because
// createOutcomeAmbiguous classifies a mutating 429 as possibly-committed regardless of
// whether it was retried (the create path never retries one; the toggle path's bounded
// retries can still EXHAUST after a request was received), so a duplicate-name code on a 429
// must NOT be read as a known prior create — the throttled request itself may be
// the one that created it. Keeping this exclusion here (the duplicate predicates run
// BEFORE createOutcomeAmbiguous on the create path) preserves the ambiguity contract.
func isDefiniteClientError(ae *apiError) bool {
	return ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.StatusCode != http.StatusTooManyRequests
}

// isDuplicateBudgetNameErr reports whether err is Google's CampaignBudgetError
// DUPLICATE_NAME rejection on a definite 4xx (excluding 429). A 3xx/5xx/429 carrying
// the code stays ambiguous via createOutcomeAmbiguous, so a create that may have
// committed is not mislabeled a known duplicate.
func isDuplicateBudgetNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateBudgetName)
}

// isDuplicateCampaignNameErr reports whether err is Google's CampaignError
// DUPLICATE_CAMPAIGN_NAME rejection on a definite 4xx (excluding 429) — the
// campaign-name analogue of isDuplicateBudgetNameErr (the two families use different
// codes).
func isDuplicateCampaignNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateCampaignName)
}

// createOutcomeAmbiguous reports whether a failed MUTATING request MAY have been
// committed upstream (so a caller must reconcile/verify before retrying, to avoid a
// duplicate — :mutate has no idempotency key). A 5xx apiError or any transportError
// is ambiguous regardless of method; a 3xx is ambiguous only on a mutating method
// (a GET redirect is not a create). A 429 on a mutating call is ALSO ambiguous,
// whether or not doRequest retried it: the CREATE path passes idempotent=false and
// never retries (a throttled create may already have committed), while the status
// TOGGLE passes true and its bounded retries can still EXHAUST — in both cases a
// request reached Google, so the caller must reconcile rather than blind-retry. A definite 4xx (Google
// rejected it) and a pre-send error are NOT ambiguous. Mirrors the sibling clients.
func createOutcomeAmbiguous(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode >= 500 || ae.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return ae.StatusCode >= 300 && ae.StatusCode < 400 && isMutatingMethod(ae.Method)
}

// isMutatingMethod reports whether an HTTP method can create/modify server state,
// so a 3xx on it may hide a committed mutation. Mirrors the sibling clients.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// resourceID returns the trailing id segment of a Google Ads resourceName, e.g.
// "customers/123/campaigns/456" -> "456". Empty if the name is empty or malformed.
func resourceID(resourceName string) string {
	if resourceName == "" {
		return ""
	}
	i := strings.LastIndex(resourceName, "/")
	if i < 0 || i == len(resourceName)-1 {
		return ""
	}
	return resourceName[i+1:]
}

// CreateCampaign creates a PAUSED Google Ads search campaign as a four-resource
// cascade: a non-shared campaign budget, a campaign referencing that budget, an ad
// group under the campaign, and a responsive search ad in that ad group. Everything
// is created PAUSED so nothing serves until a human enables it.
//
// Each stage may leave a PARTIAL result: the returned *CampaignResult is populated
// as far as the cascade got, and past the campaign stage a failure is returned
// ALONGSIDE a non-nil result rather than as (nil, err), so the caller can record
// what exists upstream. See the ad group/ad stage below for that contract.
//
// Because :mutate has no idempotency key, every failure is classified by whether
// the request may have committed upstream (createOutcomeAmbiguous). An ambiguous
// budget/campaign failure — a mutating 3xx/5xx or a transport error, or a 2xx with
// no resourceName — is reported UNCONFIRMED (verify before retrying) rather than a
// clean failure, and once the budget exists its id is returned in a partial result
// so the orphan is reconcilable. A definite 4xx means only THAT mutate was rejected,
// NOT that nothing was created: a 4xx on the SECOND (campaign) mutate still leaves
// the budget from the FIRST mutate committed, and the returned partial result
// carries that budget id so the caller can reconcile the orphan. (Only a 4xx on the
// first/budget mutate means nothing was created.) A DUPLICATE_NAME 4xx on a retry
// with a stable NameSuffix is surfaced as UNCONFIRMED-already-exists (the resource
// likely exists from a prior attempt; reconcile by name rather than treating it as
// created here).
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Require BOTH attribution fields (mirrors the meta/twitter/reddit clients, which
	// require Project and EventName independently). Project is the canonical
	// attribution key the data pipeline parses out of the campaign name, so a campaign
	// with no Project segment is mis-attributed even if EventName is present.
	//
	// Validate the SANITIZED values, not the raw input: composeName only includes a
	// segment when its sanitizeNamePart is non-empty, so a delimiter-only value like
	// "|||" passes a raw TrimSpace check yet sanitizes to nothing — which would drop
	// the Project segment while still creating a paid budget/campaign. Checking the
	// sanitized value here keeps validation and composition consistent.
	if sanitizeNamePart(in.Project) == "" {
		return nil, fmt.Errorf("google-ads campaign requires a non-empty Project")
	}
	if sanitizeNamePart(in.EventName) == "" {
		return nil, fmt.Errorf("google-ads campaign requires a non-empty EventName")
	}
	// Validate the budget and compute amountMicros ONCE. Reject NaN/Inf explicitly
	// (NaN passes every ordered comparison, so `> 0`/`<= max` alone would let it
	// through and create a 0 budget), and reject anything that rounds to <= 0 micros
	// (a sub-micro budget like 0.0000001 is > 0 but converts to 0 amountMicros).
	if math.IsNaN(in.Budget) || math.IsInf(in.Budget, 0) {
		return nil, fmt.Errorf("google-ads campaign budget must be a finite number, got %v", in.Budget)
	}
	if in.Budget > maxBudget {
		return nil, fmt.Errorf("google-ads campaign budget %.2f exceeds the maximum %.0f", in.Budget, maxBudget)
	}
	// Round (not truncate): float64(2.01)*1e6 is 2009999.99…, which int64() would
	// truncate to 2009999 — silently dropping a micro on ordinary budgets.
	amountMicros := int64(math.Round(in.Budget * microsPerUnit))
	if amountMicros <= 0 {
		return nil, fmt.Errorf("google-ads campaign budget must be > 0 (rounds to %d micros), got %.6f", amountMicros, in.Budget)
	}

	budgetName := composeName("Budget", in)
	campaignName := composeName("Search Campaign", in)
	// Budget name is limited in UTF-8 BYTES (len is the byte count); campaign name in
	// CHARACTERS (utf8.RuneCountInString). See maxBudgetNameBytes/maxCampaignNameRunes.
	if err := validateEntityName("budget", budgetName, len(budgetName), maxBudgetNameBytes, "UTF-8 bytes"); err != nil {
		return nil, err
	}
	if err := validateEntityName("campaign", campaignName, utf8.RuneCountInString(campaignName), maxCampaignNameRunes, "characters"); err != nil {
		return nil, err
	}
	// Validate the ad-group/ad inputs (destination URL, ad copy, ad-group name,
	// keywords/audience segments) BEFORE the first (budget) mutate: a failure
	// here is purely local input validation, and surfacing it only after the
	// budget+campaign already committed (inside createAdGroupAndAd, which runs
	// last) would orphan a real paid campaign for what amounts to a bad
	// RegistrationURL, over-length name, or invalid targeting value.
	finalURL, headlines, descriptions, adGroupName, keywords, audienceSegments, err := precomputeAdGroupAdInputs(in)
	if err != nil {
		return nil, err
	}

	var steps []string
	googleAdsURL := "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID

	// campaignNamePartial carries BOTH deterministic names (no ids yet) so an
	// ambiguous/duplicate budget or campaign create is reconcilable by name rather
	// than discarded. CampaignBudgetName is the reliable reconcile key ONLY for a
	// PRE-ATTACHMENT (budget-stage) failure — before the campaign attaches, the budget
	// carries `budgetName`. Once the campaign attaches, a non-shared
	// (explicitlyShared=false) budget's name SYNCHRONIZES to the campaign name, so at a
	// campaign-stage ambiguous failure the budget's current name is unknown (it may be
	// campaignName). That is why the budget-stage partial (`budgetPartial`) also carries
	// CampaignBudgetID: past attachment, reconcile the budget by ID, not name. Mirrors
	// the meta/twitter name-only partials.
	campaignNamePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:           "google-ads",
			AccountLabel:       c.account.Label,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			GoogleAdsURL:       googleAdsURL,
			Steps:              steps,
		}
	}

	// If the caller's context is ALREADY cancelled/expired before the first mutate,
	// nothing has been sent — return a clean (nil, err) rather than firing the request.
	// doRequest would otherwise fail inside httpClient.Do and classify it as an
	// ambiguous transportError → UNCONFIRMED, wrongly implying the budget MIGHT exist.
	// (This is only observable when the OAuth token is already cached: with no cached
	// token the token fetch surfaces the ctx error pre-send anyway, but the cached-token
	// path reaches httpClient.Do directly, so guard it explicitly here.)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign creation aborted before any request (context already done): %w", ctxErr)
	}

	// Step 1: create the campaign budget.
	shared := false
	budgetReq := mutateRequest{Operations: []mutateOperation{{Create: campaignBudgetCreate{
		Name:             budgetName,
		AmountMicros:     amountMicros,
		DeliveryMethod:   "STANDARD",
		ExplicitlyShared: &shared,
	}}}}
	budgetPath := c.customerPath("campaignBudgets:mutate")
	budgetResp, err := c.doRequest(ctx, http.MethodPost, budgetPath, budgetReq, false)
	if err != nil {
		switch {
		case isDuplicateBudgetNameErr(err):
			// A retry with a stable NameSuffix hit a name that already exists: the
			// budget was (almost certainly) created by a prior attempt. Not created
			// here, but NOT a clean failure either — reconcile by name.
			return campaignNamePartial(), fmt.Errorf("google-ads campaign budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return campaignNamePartial(), fmt.Errorf("google-ads campaign budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads campaign budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		// A 2xx with no/malformed resourceName is a malformed success: the budget MAY
		// have been created. UNCONFIRMED, not a clean failure.
		return campaignNamePartial(), fmt.Errorf("google-ads campaign budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency, STANDARD delivery, non-shared)", budgetID, in.Budget))

	// budgetPartial carries the created budget id (plus the campaign name) so an
	// ambiguous/failed CAMPAIGN create leaves the budget reconcilable, not orphaned
	// anonymously.
	budgetPartial := func() *CampaignResult {
		r := campaignNamePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	// The budget is now committed. If the caller's context has already been
	// cancelled/timed out, do NOT fire the campaign :mutate — surface the created
	// budget as a reconcilable partial + UNCONFIRMED, so a retry reconciles the
	// orphan budget by name rather than blind-proceeding on a dead context.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: create the campaign referencing the budget.
	campaignCreateVal := campaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelSearch,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		NetworkSettings:                networkSettings{TargetGoogleSearch: true},
		ManualCPC:                      json.RawMessage(`{}`),
	}
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: campaignCreateVal}}}
	campaignPath := c.customerPath("campaigns:mutate")
	campaignResp, err := c.doRequest(ctx, http.MethodPost, campaignPath, campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — a campaign may exist; verify in Google Ads before retrying): %w", budgetID, err)
	}
	// Validate that the campaign resource is in the exact current-account campaign shape
	// before forwarding it to createAdGroupAndAd. A malformed 2xx such as
	// customers/1234567890/adGroups/222 would be treated as a confirmed campaign and
	// only fail after the real campaign has been created, persisting an untrustworthy ID.
	if err := c.validateCampaignResource(campaignResource); err != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, SEARCH, manual CPC)", campaignID))

	res := budgetPartial()
	res.CampaignID = campaignID
	res.Steps = steps

	// The campaign+budget are now committed. GA-3: extend the shell with a PAUSED
	// ad group + responsive search ad. Any failure here (ambiguous, duplicate, or
	// definite) is returned ALONGSIDE the now-non-nil res — the campaign/budget
	// exist regardless, so this can never become a (nil, err) return past this
	// point. The caller (GoogleAdsDispatcher.Dispatch) treats a non-nil result +
	// error as "retain the claim, record the partial" — the same contract already
	// used for an ambiguous/duplicate budget or campaign.
	if err := c.createAdGroupAndAd(ctx, campaignResource, campaignID, finalURL, headlines, descriptions, adGroupName, keywords, audienceSegments, res); err != nil {
		return res, err
	}
	return res, nil
}

// firstResourceName decodes a :mutate response and returns
// (results[0].resourceName, its trailing id). It errors if the body is malformed,
// carries no result/resourceName, OR the resourceName is present but MALFORMED
// (e.g. "customers/123/campaigns/" or "noslash") such that no id can be extracted —
// accepting that would let creation continue with an empty, unreconcilable id or
// report success with a blank id. The caller treats the error as UNCONFIRMED.
func firstResourceName(body []byte) (resourceName, id string, err error) {
	var mr mutateResponse
	if uErr := json.Unmarshal(body, &mr); uErr != nil {
		return "", "", fmt.Errorf("decode mutate response: %w", uErr)
	}
	if len(mr.Results) == 0 || mr.Results[0].ResourceName == "" {
		return "", "", fmt.Errorf("mutate response carried no resource name")
	}
	rn := mr.Results[0].ResourceName
	rid := resourceID(rn)
	if rid == "" {
		return "", "", fmt.Errorf("mutate response resource name %q is malformed (no id segment)", rn)
	}
	return rn, rid, nil
}

// validateCampaignResource validates that a resource name is exactly the
// current-account campaign resource shape: customers/{currentCustomerID}/campaigns/{numericID}.
// A malformed 2xx such as customers/1234567890/adGroups/222 could otherwise be
// accepted as a confirmed campaign and only fail later when forwarded to adGroups:mutate,
// after the real campaign has been created. This guard ensures that only a trustworthy
// campaign resource is persisted.
func (c *Client) validateCampaignResource(resourceName string) error {
	return c.validateResourceKind("campaigns", resourceName, true)
}

// validateResourceKind validates that resourceName is exactly the current-account
// resource shape customers/{currentCustomerID}/{kind}/{id}, checking segment count,
// resource kind, and that the resource belongs to THIS account. A malformed or
// wrong-account 2xx (e.g. a different customer's adGroups resource, or a campaigns
// resource returned where an adGroups resource was expected) could otherwise be
// accepted as confirmed and persisted, or forwarded into a later mutate/resourceName
// only to fail confusingly downstream.
//
// requireNumericID validates the trailing id segment is a plain numeric id — set
// false for composite trailing segments (e.g. adGroupAds' "{adGroupId}~{adId}"),
// whose shape is validated separately by adGroupAdID, the only composite splitter
// in this package.
func (c *Client) validateResourceKind(kind, resourceName string, requireNumericID bool) error {
	pathParts := strings.Split(resourceName, "/")
	// Require exactly 4 segments: customers, {id}, {kind}, {id}
	if len(pathParts) != 4 {
		return fmt.Errorf("%s resource name %q has %d segments, want exactly 4", kind, resourceName, len(pathParts))
	}
	if pathParts[0] != "customers" || pathParts[2] != kind {
		return fmt.Errorf("%s resource name %q has wrong resource kind (want customers/.../%s/...)", kind, resourceName, kind)
	}
	if pathParts[1] != c.account.CustomerID {
		return fmt.Errorf("%s resource name %q is from a different account (want customers/%s/...)", kind, resourceName, c.account.CustomerID)
	}
	if requireNumericID && !numericID(pathParts[3]) {
		return fmt.Errorf("%s resource name %q has a non-numeric id", kind, resourceName)
	}
	return nil
}

// composeName builds a deterministic budget/campaign name from the input. The
// NameSuffix (when supplied) makes it unique+stable per logical campaign so a retry
// collides on DUPLICATE_NAME rather than silently double-creating.
func composeName(kind string, in CampaignInput) string {
	parts := []string{"LFX", kind}
	if p := sanitizeNamePart(in.Project); p != "" {
		parts = append(parts, p)
	}
	if e := sanitizeNamePart(in.EventName); e != "" {
		parts = append(parts, e)
	}
	if s := sanitizeNamePart(in.NameSuffix); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// sanitizeNamePart trims a caller-supplied name segment and strips the "|"
// delimiter: a raw "|" in Project/EventName/NameSuffix would inject extra fields
// into the pipe-delimited composed name and break project attribution / name-based
// reconciliation (which split on "|"). Mirrors the meta/twitter/reddit builders'
// delimiter sanitization. It also replaces ANY control character (incl. NUL, which
// Google Ads v23 explicitly forbids in a name; strings.Fields only folds the
// whitespace control chars like CR/LF, not NUL) with a space, so an embedded control
// char can't reach a paid :mutate as a guaranteed-invalid name. All runs of the
// resulting whitespace are collapsed to a single space.
func sanitizeNamePart(s string) string {
	s = strings.ReplaceAll(s, "|", " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// validateEntityName rejects an empty or over-length composed name before any
// create call. Google enforces DIFFERENT length units per resource (Campaign.name in
// characters, CampaignBudget.name in UTF-8 bytes), so the caller passes the measured
// length and the unit label; measuring in the wrong unit would let a multibyte name
// slip past the budget's byte ceiling (or reject a valid campaign name early).
func validateEntityName(kind, name string, measuredLen, maxLen int, unit string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("google-ads %s name is empty", kind)
	}
	if measuredLen > maxLen {
		return fmt.Errorf("google-ads %s name exceeds %d %s (%d): shorten EventName/Project/NameSuffix", kind, maxLen, unit, measuredLen)
	}
	return nil
}

// Campaign run states in the Google Ads vocabulary. Note ENABLED (not "ACTIVE") — the
// create path uses PAUSED from this same enum.
const (
	// StatusEnabled lets a campaign serve.
	StatusEnabled = "ENABLED"
	// StatusPaused stops a campaign serving.
	StatusPaused = "PAUSED"
)

// campaignStatusUpdate is the update payload for campaigns:mutate. resourceName identifies
// the campaign; only the fields named in the operation's updateMask are applied.
type campaignStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// IsOutcomeUnconfirmed reports whether err leaves the mutation's outcome AMBIGUOUS — Google
// may have applied it despite the error, so the caller must VERIFY before retrying rather
// than assume "not applied". Exported so the dispatcher can classify across the package
// boundary (mirrors the reddit/twitter clients' helper of the same name).
func IsOutcomeUnconfirmed(err error) bool {
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		return true
	}
	return createOutcomeAmbiguous(err)
}

// UpdateCampaignStatus toggles a campaign between ENABLED and PAUSED via campaigns:mutate
// with an updateMask of "status".
//
// This method flips ONLY the campaign — it does not cascade to the ad group/ad. The cascade
// is implemented in GoogleAdsDispatcher.ToggleStatus (dispatch/googleads.go), and the two
// directions cascade in OPPOSITE order so neither leaves a parent enabled ahead of its
// children: PAUSE goes campaign-first (stopping delivery immediately) then the ad group/ad;
// ACTIVATE goes children-first, campaign last, so the campaign only reports ENABLED once its
// ad group and ad already do.
//
// Kept as two client methods (rather than one combined call) because the ad group/ad may
// legitimately not exist (a duplicate-name orphan from GA-3b's create path — see
// createAdGroupAndAd) — the dispatcher's activate guard (ErrCampaignNotProvisioned) checks
// for that BEFORE calling either method. Additionally, keeping them separate lets a caller
// pause a campaign whose children failed to create, without that call depending on child ids
// it may not have. ACTIVATE is no longer rejected unconditionally: with GA-4's targeting
// provisioning in place, the dispatcher's guard now rejects only a campaign that is actually
// missing its ad group/ad ids or its keyword criteria (ErrCampaignNotProvisioned), and
// activates every campaign that has them.
//
// The mutate IS sent as idempotent (doRequest's last arg), unlike the create path. That flag
// gates only bounded 429 retries, and the create path's reason for declining them (no
// idempotency key, so a throttled retry could DOUBLE-CREATE) does not apply to a status flip:
// re-applying the same ENABLED/PAUSED converges on identical state. See the call site below.
func (c *Client) UpdateCampaignStatus(ctx context.Context, campaignID, status string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	if status != StatusEnabled && status != StatusPaused {
		return fmt.Errorf("google-ads: unsupported campaign status %q (want %s or %s)", status, StatusEnabled, StatusPaused)
	}
	id := strings.TrimSpace(campaignID)
	if id == "" {
		return fmt.Errorf("google-ads: cannot update status: campaign id is empty")
	}
	// The id is interpolated into a resourceName, so keep it strictly numeric — Google
	// campaign ids are digits, and anything else could alter the resource path.
	if !customerIDRE.MatchString(id) {
		return fmt.Errorf("google-ads: campaign id %q is not numeric", campaignID)
	}

	// NOTE on an already-done context: no explicit guard is needed here. doRequest ->
	// accessTokenValue checks ctx.Err() FIRST, before the cached-token fast path, so a
	// cancelled caller never reaches httpClient.Do and the error surfaces as a clean
	// context error rather than an ambiguous transportError. (CreateCampaign's own guard
	// predates that check and is now belt-and-braces.) Pinned by
	// TestGoogleAds_ToggleStatus_AlreadyCanceledContextSendsNothing, which primes the token
	// cache so it exercises exactly the cached path this note describes.

	req := mutateRequest{Operations: []mutateOperation{{
		Update: campaignStatusUpdate{
			ResourceName: "customers/" + c.account.CustomerID + "/campaigns/" + id,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	// idempotent=TRUE, unlike the create path. That flag gates ONLY bounded 429 retries, and
	// the create path's reason for declining them (no idempotency key, so a 429 whose first
	// attempt may have committed would DOUBLE-CREATE) does not apply here: re-applying the
	// same ENABLED/PAUSED value converges on the identical state. Passing false would turn
	// ordinary Google throttling into an avoidable UNCONFIRMED failure. A cancellation while
	// waiting to retry is WRAPPED by doRequest as a transportError and stays UNCONFIRMED,
	// which is correct: by then a mutation HAS been sent. That path is reachable without any
	// user cancellation, since the orchestrator wraps every toggle in a toggleCallTimeout —
	// pinned by TestUpdateCampaignStatus_CancelDuringBackoffIsUnconfirmed.
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), req, true); err != nil {
		return fmt.Errorf("google-ads campaign %s status update to %s failed: %w", id, status, err)
	}
	return nil
}
