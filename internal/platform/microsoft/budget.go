// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Campaign budget read + write (LFXV2-2665)
//
// Microsoft's budget model, as the v13 Campaign object documents it, decides every guard the
// dispatcher makes:
//
//   - The budget is a pair of FIELDS ON THE CAMPAIGN — DailyBudget (a plain decimal in the AD
//     ACCOUNT's currency, NOT micros) and BudgetType — UNLESS the campaign is attached to a SHARED
//     Budget entity, which BudgetId names. "If the value is not null and greater than zero, then
//     the campaign is using a shared budget." On a shared budget both fields become read-only
//     echoes of the shared Budget, and writing them is refused upstream with
//     CampaignServiceCannotUpdateSharedBudget (1159).
//   - BudgetType is DailyBudgetStandard for a Search campaign. DailyBudgetAccelerated and
//     LifetimeBudgetStandard exist in the enum but are documented as available ONLY to Audience
//     campaigns (https://learn.microsoft.com/en-us/advertising/campaign-management-service/budgetlimittype);
//     this service creates and reads Search campaigns only, so neither is a shape this path can
//     write, and a Search campaign reporting either is refused by the dispatcher.
//   - An EXPERIMENT campaign's budget is inherited from its base campaign: "With experiment
//     campaigns you cannot set the Budget, BudgetType, or Status."
//
// The read is GetCampaignsByIds (POST Campaigns/QueryByIds); the write is UpdateCampaigns (PUT
// Campaigns) — the same endpoint, body envelope and 200-with-PartialErrors contract the status
// toggle already uses (putUpdate).
// ---------------------------------------------------------------------------

// The v13 BudgetLimitType values.
const (
	// BudgetTypeDailyStandard spreads the daily budget evenly through the day. The create path
	// sets this one.
	BudgetTypeDailyStandard = "DailyBudgetStandard"
	// BudgetTypeDailyAccelerated spends the daily budget as fast as traffic allows. Microsoft
	// documents it as available only to Audience campaigns that use unshared campaign-level
	// budgets, so it is never written here; it is named so a read reporting it on a Search
	// campaign can be recognized and refused rather than treated as unknown.
	BudgetTypeDailyAccelerated = "DailyBudgetAccelerated"
	// BudgetTypeLifetimeStandard is a whole-flight budget, documented as available only to
	// Audience campaigns with an unshared campaign-level budget.
	BudgetTypeLifetimeStandard = "LifetimeBudgetStandard"
)

// Microsoft error codes the budget write classifies. Both spellings are matched because
// Microsoft reports a numeric Code and a symbolic ErrorCode, and either may be the one present.
// Each numeric Code is the one Microsoft's v13 operation error-code reference pairs with the
// symbolic name beside it (https://learn.microsoft.com/en-us/advertising/guides/operation-error-codes,
// checked 2026-10-05): 1100 CampaignServiceInvalidCampaignId, 1106
// CampaignServiceInvalidDailyBudget, 1123 CampaignServiceCampaignBudgetAmountIsLessThanSpendAmount,
// 1159 CampaignServiceCannotUpdateSharedBudget. A wrong number here misclassifies a refusal
// (1100 reads as "deleted upstream", 1159 as the shared-budget 409), so change one only against
// that reference.
const (
	errCodeInvalidCampaignID        = "CampaignServiceInvalidCampaignId"
	errCodeInvalidCampaignIDNum     = "1100"
	errCodeInvalidDailyBudget       = "CampaignServiceInvalidDailyBudget"
	errCodeInvalidDailyBudgetNum    = "1106"
	errCodeBudgetLessThanSpend      = "CampaignServiceCampaignBudgetAmountIsLessThanSpendAmount"
	errCodeBudgetLessThanSpendNum   = "1123"
	errCodeCannotUpdateSharedBudget = "CampaignServiceCannotUpdateSharedBudget"
	errCodeCannotUpdateSharedNum    = "1159"
)

// ErrSharedBudget marks a budget write Microsoft DEFINITELY REFUSED because the campaign is
// attached to a shared Budget (CampaignServiceCannotUpdateSharedBudget). The dispatcher refuses a
// shared budget itself, from the read, before any write; this is the server-side backstop for a
// budget attached between that read and the write. The PUT was sent, but Microsoft confirmed
// nothing was changed when it is returned; an unconfirmed outcome is never this error.
var ErrSharedBudget = errors.New("microsoft-ads: the campaign uses a shared budget, which cannot be changed through the campaign")

// ErrBudgetAmountInvalid marks a budget write refused because of the AMOUNT — by this client's
// own validation, or by Microsoft's (CampaignServiceInvalidDailyBudget, or an amount below what
// the campaign has already spent). Read the client-safe sentence with BudgetAmountReason.
var ErrBudgetAmountInvalid = errors.New("microsoft-ads: budget amount is not accepted")

// budgetAmountError carries ONLY a client-safe sentence and reaches ErrBudgetAmountInvalid
// through Unwrap, mirroring the linkedin and meta clients' type of the same name.
type budgetAmountError struct{ msg string }

func (e *budgetAmountError) Error() string { return e.msg }
func (e *budgetAmountError) Unwrap() error { return ErrBudgetAmountInvalid }

// BudgetAmountReason reports whether err is an amount refusal from this package and, if so,
// returns its sentence. The sentence names the amount or Microsoft's documented reason and
// nothing about the account, which is what makes it safe to hand back to a caller.
func BudgetAmountReason(err error) (string, bool) {
	var amountErr *budgetAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// ValidateDailyBudget applies the bounds CreateCampaign applies to a daily budget — finite,
// greater than zero, at most maxBudget — so an amount this service would refuse to create with
// cannot be reached by editing. It does NOT round: Microsoft takes DailyBudget as a decimal in
// the account currency, this client does not know that currency's minor unit, and rounding to
// an assumed one (two decimals) would silently change a JPY amount. Microsoft's own validation
// is the authority on the smallest settable amount, and its refusal is classified as an amount
// error by UpdateCampaignDailyBudget.
func ValidateDailyBudget(amount float64) error {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0):
		return &budgetAmountError{msg: "the Microsoft Advertising daily budget must be a finite number"}
	case amount <= 0:
		return &budgetAmountError{msg: "the Microsoft Advertising daily budget must be greater than zero"}
	case amount > maxBudget:
		return &budgetAmountError{msg: "the Microsoft Advertising daily budget " + formatBudgetAmount(amount) + " exceeds the maximum " + formatBudgetAmount(maxBudget)}
	}
	return nil
}

// CampaignBudget is the budget-relevant subset of one campaign as GetCampaignBudget read it.
type CampaignBudget struct {
	// CampaignID is the id Microsoft ECHOED, already checked against the requested one.
	CampaignID string
	// SharedBudgetID is the shared Budget the campaign is attached to, or "" when it is not on
	// a shared budget (BudgetId null, absent or 0 — Microsoft's documented "not shared" forms).
	SharedBudgetID string
	// BudgetIDUnreadable reports a BudgetId that was present but neither a positive id nor 0,
	// so whether the budget is shared CANNOT be established. Never read as "not shared".
	BudgetIDUnreadable bool
	// BudgetType is the reported BudgetLimitType, "" when Microsoft did not report one.
	BudgetType string
	// ExperimentID is non-empty for an experiment campaign, whose budget is inherited from its
	// base campaign and cannot be set.
	ExperimentID string
}

// IsShared reports whether the campaign is attached to a shared budget.
func (b *CampaignBudget) IsShared() bool { return b.SharedBudgetID != "" }

// queryCampaignsByIDsRequest is the POST Campaigns/QueryByIds (GetCampaignsByIds) body.
// CampaignType is sent explicitly even though Search is the documented default, so the read's
// scope does not depend on a default that could change.
type queryCampaignsByIDsRequest struct {
	AccountId    json.Number   `json:"AccountId"`
	CampaignIds  []json.Number `json:"CampaignIds"`
	CampaignType string        `json:"CampaignType"`
}

// msCampaignBudgetRead is the subset of a returned Campaign the budget guards need. Every id is
// a *json.Number: Microsoft types them `long`, a nil pointer is how an absent/null field stays
// distinguishable from a present one, and json.Number keeps digits a float64 would round.
type msCampaignBudgetRead struct {
	Id           *json.Number `json:"Id"`
	BudgetId     *json.Number `json:"BudgetId"`
	BudgetType   *string      `json:"BudgetType"`
	ExperimentId *json.Number `json:"ExperimentId"`
}

// queryCampaignsByIDsResponse is the (subset of the) 200 body. Campaigns is a pointer so an
// OMITTED or null field — a body that never answered — is distinguishable from an answered one.
// Each element is a pointer because Microsoft null-pads the slot of a campaign it did not return.
type queryCampaignsByIDsResponse struct {
	Campaigns     *[]*msCampaignBudgetRead `json:"Campaigns"`
	PartialErrors boundedErrorItems        `json:"PartialErrors"`
}

// GetCampaignBudget reads the campaign's budget shape with GetCampaignsByIds. It is a READ: it
// is retried on 429, and every failure is DEFINITE — no write was built.
//
// Returns (nil, nil) when Microsoft affirmatively reports there is no such campaign in the
// account (CampaignServiceInvalidCampaignId), so the caller can report the campaign as gone
// rather than the platform as broken. Every other unusable answer is an error: an omitted
// Campaigns field, a null slot Microsoft did not explain, a slot for a DIFFERENT campaign id, or
// a PartialError of any other kind. None of those describes this campaign, and a budget guard
// reasoning about a campaign it did not actually read is a guard in name only.
func (c *Client) GetCampaignBudget(ctx context.Context, campaignID string) (*CampaignBudget, error) {
	id := strings.TrimSpace(campaignID)
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", campaignID)
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Campaigns/QueryByIds", queryCampaignsByIDsRequest{
		AccountId:    json.Number(c.account.AccountID),
		CampaignIds:  []json.Number{json.Number(id)},
		CampaignType: campaignTypeSearch,
	}, true)
	if err != nil {
		// Microsoft may answer an unknown id with a fault rather than a 200 PartialError; the
		// documented code means the same thing either way.
		var ae *apiError
		if errors.As(err, &ae) && isDefiniteClientError(ae) &&
			(ae.hasErrorCode(errCodeInvalidCampaignID) || ae.hasErrorCode(errCodeInvalidCampaignIDNum)) {
			return nil, nil
		}
		return nil, err
	}
	var resp queryCampaignsByIDsResponse
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		return nil, fmt.Errorf("decode Campaigns/QueryByIds response: %w", uerr)
	}
	if resp.PartialErrors.AnyErrors {
		if !resp.PartialErrors.Truncated && partialErrorsHaveAny(resp.PartialErrors.Items) &&
			(partialErrorsHaveCode(resp.PartialErrors.Items, errCodeInvalidCampaignID) ||
				partialErrorsHaveCode(resp.PartialErrors.Items, errCodeInvalidCampaignIDNum)) {
			return nil, nil
		}
		return nil, fmt.Errorf("microsoft-ads campaign read for %s reported errors: %s", id, partialErrorCodes(resp.PartialErrors.Items))
	}
	if resp.Campaigns == nil {
		return nil, fmt.Errorf("Campaigns/QueryByIds response omitted the Campaigns field, so campaign %s's budget cannot be read", id)
	}
	if len(*resp.Campaigns) != 1 || (*resp.Campaigns)[0] == nil {
		return nil, fmt.Errorf("Campaigns/QueryByIds response for one campaign id returned %d entries with no usable campaign and no error, so campaign %s's budget cannot be read", len(*resp.Campaigns), id)
	}
	camp := (*resp.Campaigns)[0]
	// The answer must describe the campaign that was ASKED about. Every guard the caller makes
	// rests on this; a slot for another campaign would have them approve a write to this one on
	// the strength of someone else's budget.
	if got := numberID(camp.Id); got != id {
		return nil, fmt.Errorf("Campaigns/QueryByIds asked for campaign %s but the response describes %q, so its budget cannot be trusted", id, got)
	}
	out := &CampaignBudget{CampaignID: id}
	if camp.BudgetId != nil {
		raw := strings.TrimSpace(camp.BudgetId.String())
		switch {
		case raw == "0":
			// Microsoft's documented "own budget" value.
		case numberID(camp.BudgetId) != "":
			out.SharedBudgetID = numberID(camp.BudgetId)
		default:
			out.BudgetIDUnreadable = true
		}
	}
	if camp.BudgetType != nil {
		out.BudgetType = strings.TrimSpace(*camp.BudgetType)
	}
	if camp.ExperimentId != nil {
		if raw := strings.TrimSpace(camp.ExperimentId.String()); raw != "" && raw != "0" {
			out.ExperimentID = raw
		}
	}
	return out, nil
}

// updateCampaignBudgetRequest is the PUT Campaigns (UpdateCampaigns) body for a budget-only
// update. Like the status update it names only Id plus the fields being set — "If no value is
// set for the update, this setting is not changed" — so it cannot clobber anything else.
type updateCampaignBudgetRequest struct {
	AccountId json.Number              `json:"AccountId"`
	Campaigns []msCampaignBudgetUpdate `json:"Campaigns"`
}

// msCampaignBudgetUpdate sends BudgetType ALONGSIDE DailyBudget, set to the value the campaign
// already has. Omitting it would also leave it unchanged per the docs; sending it makes the
// "amount only, never the pacing" rule visible on the wire rather than resting on a default,
// and a test pins it.
type msCampaignBudgetUpdate struct {
	Id          json.Number `json:"Id"`
	BudgetType  string      `json:"BudgetType"`
	DailyBudget float64     `json:"DailyBudget"`
}

// UpdateCampaignDailyBudget sets an existing campaign's DailyBudget, keeping budgetType — which
// must be DailyBudgetStandard, the only daily type a Search campaign can have (DailyBudgetAccelerated
// is Audience-only), and should be the one the campaign already reports.
//
// The amount is sent as the JSON double encoding/json produces for it — the shortest decimal that
// round-trips — with NO rounding, for the reason ValidateDailyBudget gives.
//
// The PUT is IDEMPOTENT (setting the same amount twice converges), so a 429 is retried. The
// outcome classification is putUpdate's: a 5xx, transport failure, redirect, exhausted 429 or a
// success body that does not answer is UNCONFIRMED (IsOutcomeUnconfirmed); a definite 4xx or a
// PartialError is a definite refusal ONLY when no earlier attempt was retried — after a retried
// 429 every failure is unconfirmed, because the refusal answers the last attempt, not the first.
// Two refusals are given their own identity because the caller answers them differently from a
// generic failure:
//
//   - CampaignServiceCannotUpdateSharedBudget → ErrSharedBudget.
//   - CampaignServiceInvalidDailyBudget, or a budget below the amount already spent →
//     an ErrBudgetAmountInvalid carrying a client-safe sentence (BudgetAmountReason).
func (c *Client) UpdateCampaignDailyBudget(ctx context.Context, campaignID string, amount float64, budgetType string) error {
	id := strings.TrimSpace(campaignID)
	if !idRE.MatchString(id) {
		return fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", campaignID)
	}
	if budgetType != BudgetTypeDailyStandard {
		return fmt.Errorf("microsoft-ads: budget type %q is not writable on a Search campaign (want %s)", budgetType, BudgetTypeDailyStandard)
	}
	if err := ValidateDailyBudget(amount); err != nil {
		return err
	}
	err := c.putUpdate(ctx, "Campaigns", updateCampaignBudgetRequest{
		AccountId: json.Number(c.account.AccountID),
		Campaigns: []msCampaignBudgetUpdate{{Id: json.Number(id), BudgetType: budgetType, DailyBudget: amount}},
	}, "campaign budget")
	if err == nil {
		return nil
	}
	if IsOutcomeUnconfirmed(err) {
		return err
	}
	has := func(codes ...string) bool {
		var pe *partialUpdateError
		if errors.As(err, &pe) && pe.hasCode(codes...) {
			return true
		}
		var ae *apiError
		if errors.As(err, &ae) && isDefiniteClientError(ae) {
			for _, code := range codes {
				if ae.hasErrorCode(code) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(errCodeCannotUpdateSharedBudget, errCodeCannotUpdateSharedNum):
		return fmt.Errorf("%w: %w", ErrSharedBudget, err)
	case has(errCodeBudgetLessThanSpend, errCodeBudgetLessThanSpendNum):
		return &budgetAmountError{msg: fmt.Sprintf("Microsoft Advertising refused a daily budget of %s because it is less than the amount the campaign has already spent", formatBudgetAmount(amount))}
	case has(errCodeInvalidDailyBudget, errCodeInvalidDailyBudgetNum):
		return &budgetAmountError{msg: fmt.Sprintf("Microsoft Advertising refused a daily budget of %s as not valid for this ad account — it is below the minimum, or not a settable amount, in the account's currency", formatBudgetAmount(amount))}
	}
	return err
}

// formatBudgetAmount renders an amount for a client-facing sentence in plain decimal notation.
// %g would print 1500000 as "1.5e+06", and seven-figure daily budgets are ordinary in
// currencies such as JPY, KRW, IDR and VND; 'f' with precision -1 is the shortest plain
// decimal that round-trips, so no digit is invented or dropped either.
func formatBudgetAmount(amount float64) string {
	return strconv.FormatFloat(amount, 'f', -1, 64)
}
