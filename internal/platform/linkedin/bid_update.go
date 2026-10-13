// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Campaign max bid: write (LFXV2-2665)
//
// WHERE A LINKEDIN BID LIVES. On the CAMPAIGN itself: `unitCost` is "amount to bid (for manual
// bidding), amount which is the target cost (for target cost bidding) ... or cost cap (for cost
// cap bidding)", charged per the campaign's `costType` (CPC, CPM, CPV). Source — LinkedIn
// Marketing API, "Create and Manage Campaigns" (li-lms-2026-09), Campaign Schema and
// "Optimization of Campaigns",
// https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/account-structure/create-and-manage-campaigns
// consulted 2026-10-13.
//
// WHEN unitCost IS A MAX CPC. That page lists exactly two optimizationTargetType values that
// "keep a campaign in manual bidding mode" — NONE and ENHANCED_CONVERSION; every MAX_* value
// "activates auto-bidding", TARGET_COST_PER_* target-cost bidding, and CAP_COST_AND_* cost-cap
// bidding (always costType CPM). So a manual max CPC is costType CPC AND optimizationTargetType
// NONE or ENHANCED_CONVERSION — the LinkedIn counterpart of Microsoft's ManualCpc/EnhancedCpc.
// Anything else, or an unreported value, is refused by the dispatcher; this path never sends
// optimizationTargetType or costType, so it cannot switch a strategy.
//
// WHAT THE CREATE PATH SENDS. createSponsoredCampaign sends costType CPC with objectiveType
// WEBSITE_CONVERSION and NO optimizationTargetType and NO unitCost. Per the schema the target
// type then defaults to NONE — manual — and unitCost to 0 ("For manual bidding ... if unitCost is
// 0, the campaign does not deliver"). Whether LinkedIn instead applies an objective-based
// automated default to such a campaign is NOT verified against a live account; the dispatcher
// therefore reads the strategy LinkedIn REPORTS and never assumes it.
//
// THE WRITE is a Rest.li PARTIAL_UPDATE of `unitCost` alone ("you can update your campaign bid
// amounts" — same page, "Update a Campaign"), sent ONCE (see withSendOnce).
//
// REFUSAL. A definite 400 is an AMOUNT refusal only when its body is LinkedIn's structured error
// envelope ("Updated Marketing API Error Responses",
// https://learn.microsoft.com/en-us/linkedin/marketing/error-responses, consulted 2026-10-13),
// accepted by identityjson, decoded without error, whose `status` agrees with the HTTP status,
// and whose every input error names fieldPath /Campaign/unitCost/amount with an amount code.
// LinkedIn's text is never surfaced. Every other definite 4xx is a plain definite failure.
// ---------------------------------------------------------------------------

// The bidding setup under which unitCost is a manual max CPC.
const (
	CostTypeCPC                          = "CPC"
	OptimizationTargetNone               = "NONE"
	OptimizationTargetEnhancedConversion = "ENHANCED_CONVERSION"
)

// ManualCPC reports whether the campaign's unitCost is a manual maximum cost per click.
func (s *CampaignSettings) ManualCPC() bool {
	if s == nil || s.CostType == nil || s.OptimizationTargetType == nil || *s.CostType != CostTypeCPC {
		return false
	}
	switch *s.OptimizationTargetType {
	case OptimizationTargetNone, OptimizationTargetEnhancedConversion:
		return true
	}
	return false
}

// linkedinMaxBid is the contract's ceiling (service.maxCampaignBid). LinkedIn's own range is
// audience-dependent and LinkedIn's to enforce — its refusal is classified below.
const linkedinMaxBid = 1_000_000.0

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by ValidateBidAmount, or by
// LinkedIn's structured refusal of unitCost.amount. Read the sentence with BidAmountReason.
var ErrBidAmountInvalid = errors.New("linkedin: bid amount is not acceptable")

// bidAmountError carries ONLY a client-safe sentence of this service's own.
type bidAmountError struct{ msg string }

func (e *bidAmountError) Error() string { return e.msg }
func (e *bidAmountError) Unwrap() error { return ErrBidAmountInvalid }

// BidAmountReason reports whether err is a bid-amount refusal from this package and, if so,
// returns its sentence — the only text from this path that may be shown to a caller.
func BidAmountReason(err error) (string, bool) {
	var amountErr *bidAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// ValidateBidAmount returns the exact wire string unitCost.amount is sent as. A LinkedIn amount
// is a decimal string in the account currency, not micro-units; this client writes USD (as every
// LinkedIn write here does) at whole-cent precision, the precision its budgets use. An amount
// that is not a whole number of cents is REFUSED rather than rounded: rounding would set one bid
// upstream and persist another on the row.
func ValidateBidAmount(amount float64) (string, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return "", &bidAmountError{msg: "the bid must be a finite number greater than zero"}
	case amount > linkedinMaxBid:
		return "", &bidAmountError{msg: fmt.Sprintf("the bid %s exceeds the largest bid this service sets on LinkedIn (%s)",
			strconv.FormatFloat(amount, 'f', -1, 64), strconv.FormatFloat(linkedinMaxBid, 'f', -1, 64))}
	}
	cents := math.Round(amount * 100)
	if cents < 1 {
		return "", &bidAmountError{msg: "the bid rounds to zero; the smallest bid this service sets on LinkedIn is 0.01"}
	}
	// WHOLE CENTS, EXACTLY: the canonical two-decimal wire form, parsed back, must be the very
	// float requested. No tolerance — 2.340000001 is refused, not sent as "2.34" while the row
	// persists 2.340000001.
	wire := strconv.FormatFloat(cents/100, 'f', 2, 64)
	if back, err := strconv.ParseFloat(wire, 64); err != nil || back != amount {
		return "", &bidAmountError{msg: fmt.Sprintf("the bid %s is not a whole number of cents; LinkedIn bids are set in the account currency to two decimal places",
			strconv.FormatFloat(amount, 'f', -1, 64))}
	}
	return wire, nil
}

// sendOnceKey marks a request context whose mutation must not be retried in-call.
type sendOnceKey struct{}

// withSendOnce marks ctx so doRequest does not retry a 429 on it. The bid write uses it: a
// throttle may be reported AFTER LinkedIn applied the write, and a definite refusal returned by a
// retry answers the last attempt, not the first — so without a retry, every throttle comes back
// as the 429 itself, which createOutcomeAmbiguous classifies UNCONFIRMED. The Meta and X bid
// writers take the same route.
func withSendOnce(ctx context.Context) context.Context {
	return context.WithValue(ctx, sendOnceKey{}, true)
}

func sendOnce(ctx context.Context) bool {
	v, _ := ctx.Value(sendOnceKey{}).(bool)
	return v
}

// UpdateCampaignUnitCost sets the campaign's unitCost.amount via PARTIAL_UPDATE. It writes the
// AMOUNT only (with currencyCode USD); the caller is responsible for having established
// ManualCPC and a USD campaign — this function writes what it is told.
//
// wire must be the string ValidateBidAmount produced; it is re-validated here, because this
// method is exported and "every caller validated first" would otherwise be a convention.
//
// Errors are returned for the caller to classify with IsOutcomeUnconfirmed: a transport failure,
// 3xx, 429, 5xx or mutating 401 may have applied the bid. A PARTIAL_UPDATE returns no body, so
// the 2xx IS the confirmation (see UpdateCampaignBudget).
func (c *Client) UpdateCampaignUnitCost(ctx context.Context, campaignID, wire string) error {
	if err := c.validateCredentialShape(); err != nil {
		return err
	}
	if err := ValidateCampaignID(campaignID); err != nil {
		return err
	}
	parsed, pErr := strconv.ParseFloat(wire, 64)
	if pErr != nil {
		return fmt.Errorf("linkedin: bid amount %q is not a wire-formatted amount; it must come from ValidateBidAmount", wire)
	}
	if canonical, vErr := ValidateBidAmount(parsed); vErr != nil || canonical != wire {
		return fmt.Errorf("linkedin: bid amount %q is not a wire-formatted amount; it must come from ValidateBidAmount", wire)
	}
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return fmt.Errorf("linkedin: %w", err)
	}
	if !ValidAccountID(accountID) {
		return fmt.Errorf("linkedin: ad account id %q is not a positive 64-bit integer", accountID)
	}
	path := fmt.Sprintf("adAccounts/%s/adCampaigns/%s", accountID, campaignID)
	body := map[string]any{"patch": map[string]any{"$set": map[string]any{
		"unitCost": map[string]any{"amount": wire, "currencyCode": "USD"},
	}}}
	headers := map[string]string{"X-Restli-Method": "PARTIAL_UPDATE"}
	if _, err := c.doRequest(withSendOnce(ctx), http.MethodPost, path, body, nil, headers, true); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest && isMutatingMethod(ae.Method) && bidAmountRefused(ae, wire) {
			return &bidAmountError{msg: "LinkedIn refused this max CPC bid for the campaign; it is outside the range LinkedIn accepts for the campaign's audience in the account's currency"}
		}
		return fmt.Errorf("linkedin: update campaign %s unitCost to %s: %w", campaignID, wire, err)
	}
	return nil
}

// errorEnvelope is LinkedIn's structured Marketing API error body. Only what the amount
// classification needs is decoded.
type errorEnvelope struct {
	Status       *int    `json:"status"`
	Code         *string `json:"code"`
	ErrorDetails *struct {
		InputErrors            []inputErrorWire `json:"inputErrors"`
		ConditionalInputErrors []inputErrorWire `json:"conditionalInputErrors"`
	} `json:"errorDetails"`
}

type inputErrorWire struct {
	Code  string `json:"code"`
	Input struct {
		InputPath struct {
			FieldPath string `json:"fieldPath"`
		} `json:"inputPath"`
		Value *struct {
			DecimalValue *string `json:"decimalValue"`
		} `json:"value"`
	} `json:"input"`
}

// unitCostFieldPath is the fieldPath LinkedIn's envelope uses for the bid amount (the
// documented form is "/Campaign/<field>/<subfield>", e.g. /Campaign/totalBudget/amount).
const unitCostFieldPath = "/Campaign/unitCost/amount"

// amountErrorCodes are the documented input-error codes that are statements about a VALUE's
// size or acceptability, as opposed to permissions, state or configuration.
var amountErrorCodes = map[string]bool{
	"FIELD_VALUE_TOO_LOW":        true,
	"FIELD_VALUE_TOO_HIGH":       true,
	"CONDITIONAL_VALUE_TOO_LOW":  true,
	"CONDITIONAL_VALUE_TOO_HIGH": true,
	"INVALID_VALUE_FOR_FIELD":    true,
}

// bidAmountRefused reports whether a definite 400 is LinkedIn refusing the bid AMOUNT we sent.
// Every condition is required:
//
//   - the FULL body passes identityjson (a duplicated key would let the decoder pick one of two
//     codes) and decodes without error (a type mismatch leaves a half-read envelope);
//   - the envelope's own `status` is the HTTP status it arrived with — the identity check that
//     this body describes this response;
//   - there is at least one input error, and EVERY one names /Campaign/unitCost/amount with an
//     amount code — an envelope that also blames another field is not an amount refusal;
//   - EVERY input error echoes `input.value.decimalValue`, and it is the value sent.
//
// The message text is never consulted.
func bidAmountRefused(ae *apiError, wire string) bool {
	if len(ae.raw) == 0 || identityjson.Check(ae.raw) != nil {
		return false
	}
	var env errorEnvelope
	if err := json.Unmarshal(ae.raw, &env); err != nil {
		return false
	}
	if env.Status == nil || *env.Status != ae.StatusCode || env.ErrorDetails == nil {
		return false
	}
	all := append(append([]inputErrorWire(nil), env.ErrorDetails.InputErrors...), env.ErrorDetails.ConditionalInputErrors...)
	if len(all) == 0 {
		return false
	}
	sent, err := strconv.ParseFloat(wire, 64)
	if err != nil {
		return false
	}
	for _, ie := range all {
		if ie.Input.InputPath.FieldPath != unitCostFieldPath || !amountErrorCodes[ie.Code] {
			return false
		}
		// The echo is REQUIRED, not merely checked when present: an input error that does not
		// say which value it refused cannot prove it was ours, so the envelope is structurally
		// incomplete and stays an ordinary definite failure, never an amount rejection.
		v := ie.Input.Value
		if v == nil || v.DecimalValue == nil {
			return false
		}
		echoed, perr := strconv.ParseFloat(*v.DecimalValue, 64)
		if perr != nil || echoed != sent {
			return false
		}
	}
	return true
}
