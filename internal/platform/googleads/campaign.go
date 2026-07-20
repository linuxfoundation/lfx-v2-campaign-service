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
)

// ---------------------------------------------------------------------------
// Campaign creation (GA-2): campaignBudget:mutate -> campaigns:mutate
// ---------------------------------------------------------------------------

const (
	// microsPerUnit converts a currency amount to Google Ads "micros"
	// (amount * 1,000,000). Budgets are expressed in micros of the account's
	// currency.
	microsPerUnit = 1_000_000

	// maxBudgetUSD caps the budget well below the int64 micros-overflow threshold
	// (math.MaxInt64 / 1e6 ≈ 9.2e12) so the *microsPerUnit conversion can never wrap
	// to a negative value. Mirrors the reddit/twitter clients' budget cap. This is a
	// sanity bound on caller input, NOT the account's minimum — Google enforces a
	// currency-dependent minimum server-side and rejects a too-low budget with a
	// campaignBudgetError, which surfaces as a definite failure.
	maxBudgetUSD = 1_000_000_000.0

	// maxEntityNameLen bounds the composed budget / campaign name. Names must be
	// unique per account (Google rejects a duplicate non-shared budget/campaign name
	// with DUPLICATE_NAME), and an unbounded caller-supplied EventName could produce
	// an oversized payload, so the composed name is length-capped before the call.
	maxEntityNameLen = 255

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

// CampaignInput is the platform-agnostic request to create a Google Ads campaign.
// Only the fields needed for a PAUSED search-campaign shell are consumed today;
// targeting/keywords/audience are added in GA-3+.
type CampaignInput struct {
	// EventName is the human-readable campaign subject, folded into the budget and
	// campaign names. Caller-supplied and otherwise unbounded, so it is trimmed and
	// the composed names are length-capped before any create call.
	EventName string
	// Project is folded into the composed name alongside EventName.
	Project string
	// BudgetUSD is the campaign budget in the account's currency (labeled USD by
	// convention; Google interprets amountMicros in the account currency). Converted
	// to micros. Must be > 0 and <= maxBudgetUSD.
	BudgetUSD float64
	// NameSuffix, when non-empty, is appended to the composed budget/campaign names
	// to make them unique+deterministic per logical campaign. A caller that wants
	// at-most-once retry semantics passes a stable value (e.g. the brief id): a
	// retry then reuses the same name and Google rejects it with DUPLICATE_NAME,
	// which the client reports as UNCONFIRMED-already-exists rather than re-creating.
	NameSuffix string
}

// CampaignResult reports what CreateCampaign created. The Google Ads hierarchy is
// campaignBudget -> campaign, so both IDs are surfaced for reconcile/cleanup.
type CampaignResult struct {
	Platform         string   `json:"platform"`
	AccountLabel     string   `json:"accountLabel,omitempty"`
	CampaignName     string   `json:"campaignName"`
	CampaignID       string   `json:"campaignId"`
	CampaignBudgetID string   `json:"campaignBudgetId"`
	GoogleAdsURL     string   `json:"googleAdsUrl"`
	Steps            []string `json:"steps"`
}

// mutateOperation is one {create: <resource>} entry in a :mutate request.
type mutateOperation struct {
	Create any `json:"create"`
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
type campaignCreate struct {
	Name                           string          `json:"name"`
	Status                         string          `json:"status"`
	AdvertisingChannelType         string          `json:"advertisingChannelType"`
	CampaignBudget                 string          `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string          `json:"containsEuPoliticalAdvertising"`
	ManualCPC                      json.RawMessage `json:"manualCpc"`
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

// isDuplicateBudgetNameErr reports whether err is Google's CampaignBudgetError
// DUPLICATE_NAME rejection on a definite 4xx. Gated to a 4xx (Google returns it as
// a validation error): a 3xx/5xx carrying the code stays ambiguous via
// createOutcomeAmbiguous, so a create that may have committed is not mislabeled a
// known duplicate.
func isDuplicateBudgetNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.hasErrorCode(errCodeDuplicateBudgetName)
}

// isDuplicateCampaignNameErr reports whether err is Google's CampaignError
// DUPLICATE_CAMPAIGN_NAME rejection on a definite 4xx — the campaign-name analogue
// of isDuplicateBudgetNameErr (the two families use different codes).
func isDuplicateCampaignNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.hasErrorCode(errCodeDuplicateCampaignName)
}

// createOutcomeAmbiguous reports whether a failed MUTATING request MAY have been
// committed upstream (so a caller must reconcile/verify before retrying, to avoid a
// duplicate — :mutate has no idempotency key). A 5xx apiError or any transportError
// is ambiguous regardless of method; a 3xx is ambiguous only on a mutating method
// (a GET redirect is not a create). A 429 on a mutating call is ALSO ambiguous:
// doRequest deliberately does NOT retry a non-idempotent 429 (idempotent=false)
// precisely because the throttled request may already have committed upstream, so
// the caller must reconcile rather than blind-retry. A definite 4xx (Google
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

// CreateCampaign creates a PAUSED Google Ads search campaign: it first creates a
// non-shared campaign budget, then a campaign referencing that budget. Everything
// is created PAUSED so nothing serves until a human enables it.
//
// Because :mutate has no idempotency key, every failure is classified by whether
// the request may have committed upstream (createOutcomeAmbiguous). An ambiguous
// budget/campaign failure — a mutating 3xx/5xx or a transport error, or a 2xx with
// no resourceName — is reported UNCONFIRMED (verify before retrying) rather than a
// clean failure, and once the budget exists its id is returned in a partial result
// so the orphan is reconcilable. A definite 4xx (Google rejected it) is a clean
// failure: nothing was created. A DUPLICATE_NAME 4xx on a retry with a stable
// NameSuffix is surfaced as UNCONFIRMED-already-exists (the resource likely exists
// from a prior attempt; reconcile by name rather than treating it as created here).
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Require at least one attribution field so a paid campaign is never created
	// under a nameless "LFX | <kind>" — a mis-attributed spend is hard to trace.
	if strings.TrimSpace(in.Project) == "" && strings.TrimSpace(in.EventName) == "" {
		return nil, fmt.Errorf("google-ads campaign requires a non-empty Project or EventName")
	}
	// Validate the budget and compute amountMicros ONCE. Reject NaN/Inf explicitly
	// (NaN passes every ordered comparison, so `> 0`/`<= max` alone would let it
	// through and create a $0 budget), and reject anything that rounds to <= 0 micros
	// (a sub-micro budget like 0.0000001 is > 0 but converts to 0 amountMicros).
	if math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) {
		return nil, fmt.Errorf("google-ads campaign budget must be a finite number, got %v", in.BudgetUSD)
	}
	if in.BudgetUSD > maxBudgetUSD {
		return nil, fmt.Errorf("google-ads campaign budget %.2f exceeds the maximum %.0f", in.BudgetUSD, maxBudgetUSD)
	}
	amountMicros := int64(in.BudgetUSD * microsPerUnit)
	if amountMicros <= 0 {
		return nil, fmt.Errorf("google-ads campaign budget must be > 0 (rounds to %d micros), got %.6f", amountMicros, in.BudgetUSD)
	}

	budgetName := composeName("Budget", in)
	campaignName := composeName("Search Campaign", in)
	if err := validateEntityName("budget", budgetName); err != nil {
		return nil, err
	}
	if err := validateEntityName("campaign", campaignName); err != nil {
		return nil, err
	}

	var steps []string
	googleAdsURL := "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID

	// campaignNamePartial carries the deterministic campaign NAME (no ids yet) so an
	// ambiguous/duplicate budget or campaign create is reconcilable by name rather
	// than discarded. Mirrors the meta/twitter name-only partials.
	campaignNamePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "google-ads",
			AccountLabel: c.account.Label,
			CampaignName: campaignName,
			GoogleAdsURL: googleAdsURL,
			Steps:        steps,
		}
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
	budgetResource, err := firstResourceName(budgetResp)
	if err != nil {
		// A 2xx with no resourceName is a malformed success: the budget MAY have been
		// created. UNCONFIRMED, not a clean failure.
		return campaignNamePartial(), fmt.Errorf("google-ads campaign budget creation UNCONFIRMED (2xx with no resource name; %q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	budgetID := resourceID(budgetResource)
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s ($%.2f/day, STANDARD delivery, non-shared)", budgetID, in.BudgetUSD))

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
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: campaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelSearch,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		ManualCPC:                      json.RawMessage(`{}`),
	}}}}
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
	campaignResource, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; 2xx with no resource name — a campaign may exist; verify in Google Ads before retrying): %w", budgetID, err)
	}
	campaignID := resourceID(campaignResource)
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, SEARCH, manual CPC)", campaignID))

	res := budgetPartial()
	res.CampaignID = campaignID
	res.Steps = steps
	return res, nil
}

// firstResourceName decodes a :mutate response and returns results[0].resourceName,
// erroring if the body is malformed or carries no result/resourceName (a 2xx that
// the caller must treat as UNCONFIRMED rather than a confirmed create).
func firstResourceName(body []byte) (string, error) {
	var mr mutateResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return "", fmt.Errorf("decode mutate response: %w", err)
	}
	if len(mr.Results) == 0 || mr.Results[0].ResourceName == "" {
		return "", fmt.Errorf("mutate response carried no resource name")
	}
	return mr.Results[0].ResourceName, nil
}

// composeName builds a deterministic budget/campaign name from the input. The
// NameSuffix (when supplied) makes it unique+stable per logical campaign so a retry
// collides on DUPLICATE_NAME rather than silently double-creating.
func composeName(kind string, in CampaignInput) string {
	parts := []string{"LFX", kind}
	if p := strings.TrimSpace(in.Project); p != "" {
		parts = append(parts, p)
	}
	if e := strings.TrimSpace(in.EventName); e != "" {
		parts = append(parts, e)
	}
	if s := strings.TrimSpace(in.NameSuffix); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// validateEntityName rejects an empty or over-length composed name before any
// create call (Google enforces a name length limit and rejects duplicates).
func validateEntityName(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("google-ads %s name is empty", kind)
	}
	if len(name) > maxEntityNameLen {
		return fmt.Errorf("google-ads %s name exceeds %d chars (%d): shorten EventName/Project/NameSuffix", kind, maxEntityNameLen, len(name))
	}
	return nil
}
