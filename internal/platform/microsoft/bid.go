// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Ad-group max CPC bid: bid-strategy read + bid write (LFXV2-2665)
//
// Microsoft's v13 model, as documented, decides every guard the dispatcher makes:
//
//   - The bid lives on the AD GROUP: AdGroup.CpcBid is "the default bid to use when the user's
//     query and the ad group's keywords match", and "Specifying a broad, exact, or phrase match
//     bid at the keyword level overrides the ad group's Cpc bid". The create path sets exactly
//     this field on the one ad group it creates and gives its keywords no bids, so they
//     inherit it. Update: "If no value is set for the update, this setting is not changed."
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/adgroup
//   - The STRATEGY lives on the CAMPAIGN: "As of April 2021, you cannot set any bid strategies
//     for ad groups or keywords ... Ad groups and keywords will inherit their campaign's bid
//     strategy." https://learn.microsoft.com/en-us/advertising/campaign-management-service/campaign
//   - Only two strategies READ the ad group bid. EnhancedCpc: "you set your ad group and keyword
//     bids, and Microsoft Advertising will automatically adjust your bids in real time"
//     (https://learn.microsoft.com/en-us/advertising/campaign-management-service/enhancedcpcbiddingscheme),
//     and it is the documented default for a Search campaign. ManualCpc: "you set your ad group
//     and keyword bids, and Microsoft Advertising uses these bids every time"
//     (https://learn.microsoft.com/en-us/advertising/campaign-management-service/manualcpcbiddingscheme).
//     For the automated types "your bid and ad rotation settings are ignored"
//     (https://learn.microsoft.com/en-us/advertising/campaign-management-service/biddingscheme).
//   - The Campaign's BiddingScheme "will be nil or empty by default if the campaign uses the
//     MaxConversionValueBiddingScheme or TargetImpressionShareBiddingScheme" — so an ABSENT
//     scheme is not "manual"; it is refused.
//   - A campaign with BidStrategyId > 0 "is using a portfolio bid strategy" shared with other
//     campaigns (same Campaign reference); it is refused rather than reasoned about. BidStrategyId
//     is NOT returned by default: it is a CampaignAdditionalField ("Request that the BidStrategyId
//     element be included within each returned Campaign object"), so this read sends
//     ReturnAdditionalFields=BidStrategyId
//     (https://learn.microsoft.com/en-us/advertising/campaign-management-service/campaignadditionalfield,
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/getcampaignsbyids).
//     Once requested, an absent/null value is Microsoft's documented "not using a portfolio bid
//     strategy" ("If the field is empty, then the campaign is not using a portfolio bid
//     strategy"); a present value that is neither 0 nor a positive id fails closed.
//
// The write is UpdateAdGroups (PUT AdGroups) naming ONLY Id and CpcBid — the same endpoint and
// 200-with-PartialErrors contract the status toggle's ad-group PUT uses (putUpdate).
// https://learn.microsoft.com/en-us/advertising/campaign-management-service/updateadgroups
// ---------------------------------------------------------------------------

// The bid-strategy types whose ad group CpcBid is used. Microsoft's REST JSON example names the
// discriminator "EnhancedCpc" while the element description says the value "is
// EnhancedCpcBiddingScheme when you retrieve" one, so both spellings are recognized
// (normalizeBidSchemeType) rather than one of them being refused as unknown.
const (
	BidSchemeEnhancedCpc = "EnhancedCpc"
	BidSchemeManualCpc   = "ManualCpc"
)

// Microsoft error codes the ad-group bid write classifies, each paired with its symbolic name in
// the v13 operation error-code reference
// (https://learn.microsoft.com/en-us/advertising/guides/operation-error-codes, checked
// 2026-10-05). Amount refusals: 602 BidAmountsLessThanFloorPrice, 633 InvalidBidAmount, 1017
// CampaignServiceInvalidSearchBids, 1515 CampaignServiceBidAmountsLessThanFloorPrice, 1516
// CampaignServiceBidAmountsGreaterThanCeilingPrice, 1538 CampaignServiceInvalidBidAmount. Not an
// amount: 1229 CampaignServiceCannotSetSearchBidOnAdGroup ("Cpc bids are not currently supported
// for the ad group"), and 605 AdGroupIdInvalid / 1201 CampaignServiceInvalidAdGroupId (the
// recorded ad group no longer addresses anything).
var (
	bidFloorCodes   = []string{"BidAmountsLessThanFloorPrice", "602", "CampaignServiceBidAmountsLessThanFloorPrice", "1515"}
	bidCeilingCodes = []string{"CampaignServiceBidAmountsGreaterThanCeilingPrice", "1516"}
	bidInvalidCodes = []string{"InvalidBidAmount", "633", "CampaignServiceInvalidSearchBids", "1017", "CampaignServiceInvalidBidAmount", "1538"}
	bidNotSettable  = []string{"CampaignServiceCannotSetSearchBidOnAdGroup", "1229"}
	adGroupInvalid  = []string{"AdGroupIdInvalid", "605", "CampaignServiceInvalidAdGroupId", "1201"}
)

// campaignFieldBidStrategyID is the CampaignAdditionalField that makes GetCampaignsByIds return
// BidStrategyId. Without it the element is never populated and the portfolio guard would be a
// guard in name only.
const campaignFieldBidStrategyID = "BidStrategyId"

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by this client's own bounds or
// by Microsoft's definite refusal. Read the client-safe sentence with BidAmountReason.
var ErrBidAmountInvalid = errors.New("microsoft-ads: bid amount is not accepted")

// ErrBidNotSettable marks a bid write Microsoft DEFINITELY refused for a reason other than the
// amount: the ad group does not take a CPC bid, or the recorded ad group id addresses nothing.
// Nothing was applied.
var ErrBidNotSettable = errors.New("microsoft-ads: the ad group's CPC bid cannot be set")

// bidAmountError carries ONLY a client-safe sentence, mirroring budgetAmountError.
type bidAmountError struct{ msg string }

func (e *bidAmountError) Error() string { return e.msg }
func (e *bidAmountError) Unwrap() error { return ErrBidAmountInvalid }

// BidAmountReason reports whether err is a bid-amount refusal from this package and, if so,
// returns its sentence — which names the amount and Microsoft's documented reason and nothing
// about the account.
func BidAmountReason(err error) (string, bool) {
	var amountErr *bidAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// ValidateMaxCPCBid applies the bounds the create path applies to an ad-group CpcBid
// (validateCpcBid: finite, at least minCpcBid, at most maxCpcBid), so a bid this service would
// refuse to create with cannot be reached by editing. Unlike validateCpcBid, zero is not "unset"
// here — an edit always names a bid. The amount is NOT rounded, for ValidateDailyBudget's reason:
// Microsoft's own validation of the account currency is the authority on the settable unit.
func ValidateMaxCPCBid(amount float64) error {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0):
		return &bidAmountError{msg: "the Microsoft Advertising max CPC bid must be a finite number"}
	case amount < minCpcBid:
		return &bidAmountError{msg: "the Microsoft Advertising max CPC bid " + formatBudgetAmount(amount) + " is below the minimum " + formatBudgetAmount(minCpcBid) + " this service sets"}
	case amount > maxCpcBid:
		return &bidAmountError{msg: "the Microsoft Advertising max CPC bid " + formatBudgetAmount(amount) + " exceeds the maximum " + formatBudgetAmount(maxCpcBid) + " this service sets"}
	}
	return nil
}

// CampaignBidStrategy is the bid-strategy subset of one campaign as GetCampaignBidStrategy read
// it.
type CampaignBidStrategy struct {
	// CampaignID is the id Microsoft ECHOED, already checked against the requested one.
	CampaignID string
	// SchemeType is the reported BiddingScheme.Type with any "BiddingScheme" suffix removed
	// ("EnhancedCpc", "MaxClicks", ...); "" when Microsoft reported no scheme.
	SchemeType string
	// SchemeUnreadable reports a BiddingScheme that was present but not an object with a string
	// Type, so the strategy cannot be established. Never read as "manual".
	SchemeUnreadable bool
	// PortfolioBidStrategyID is the shared portfolio strategy the campaign uses, "" when none
	// (BidStrategyId null, absent or 0).
	PortfolioBidStrategyID string
	// PortfolioUnreadable reports a BidStrategyId that was present but neither 0 nor a positive
	// id, so whether a portfolio governs the campaign cannot be established.
	PortfolioUnreadable bool
}

// UsesAdGroupBid reports whether the campaign's strategy is one that reads the ad group CpcBid:
// its OWN (non-portfolio) EnhancedCpc or ManualCpc scheme. Everything else — an automated type,
// an unreported or unreadable scheme, any portfolio — is false.
func (s *CampaignBidStrategy) UsesAdGroupBid() bool {
	if s.SchemeUnreadable || s.PortfolioUnreadable || s.PortfolioBidStrategyID != "" {
		return false
	}
	return s.SchemeType == BidSchemeEnhancedCpc || s.SchemeType == BidSchemeManualCpc
}

// normalizeBidSchemeType strips the "BiddingScheme" suffix Microsoft documents on retrieval, so
// "EnhancedCpcBiddingScheme" and "EnhancedCpc" compare equal.
func normalizeBidSchemeType(t string) string {
	return strings.TrimSuffix(strings.TrimSpace(t), "BiddingScheme")
}

// GetCampaignBidStrategy reads the campaign's bid strategy with GetCampaignsByIds, through the
// same read and the same answer validation as GetCampaignBudget. It is a READ: every failure is
// DEFINITE. (nil, nil) means Microsoft affirmatively reported no such campaign.
func (c *Client) GetCampaignBidStrategy(ctx context.Context, campaignID string) (*CampaignBidStrategy, error) {
	camp, id, err := c.queryCampaignByID(ctx, campaignID, "bid strategy", campaignFieldBidStrategyID)
	if err != nil || camp == nil {
		return nil, err
	}
	out := &CampaignBidStrategy{CampaignID: id}
	if raw := strings.TrimSpace(string(camp.BiddingScheme)); raw != "" && raw != "null" {
		var scheme struct {
			Type *string `json:"Type"`
		}
		if uerr := json.Unmarshal(camp.BiddingScheme, &scheme); uerr != nil || scheme.Type == nil {
			out.SchemeUnreadable = true
		} else {
			out.SchemeType = normalizeBidSchemeType(*scheme.Type)
		}
	}
	if raw := strings.TrimSpace(string(camp.BidStrategyId)); raw != "" && raw != "null" {
		var n json.Number
		if uerr := json.Unmarshal(camp.BidStrategyId, &n); uerr != nil {
			out.PortfolioUnreadable = true
		} else {
			switch v := strings.TrimSpace(n.String()); {
			case v == "0":
				// Microsoft's documented "own bid strategy" value.
			case numberID(&n) != "":
				out.PortfolioBidStrategyID = numberID(&n)
			default:
				out.PortfolioUnreadable = true
			}
		}
	}
	return out, nil
}

// updateAdGroupBidRequest is the PUT AdGroups (UpdateAdGroups) body for a bid-only update.
// CampaignId is the documented top-level owner of the ad groups. ReturnInheritedBidStrategyTypes
// is "Reserved for future use" but listed without an optional note, so it is sent false, as the
// create path does. UpdateAudienceAdsBidAdjustment is sent false explicitly so the audience-ads
// adjustment is documented-ignored rather than resting on a default.
type updateAdGroupBidRequest struct {
	CampaignId                      json.Number      `json:"CampaignId"`
	AdGroups                        []msAdGroupBidUp `json:"AdGroups"`
	UpdateAudienceAdsBidAdjustment  bool             `json:"UpdateAudienceAdsBidAdjustment"`
	ReturnInheritedBidStrategyTypes bool             `json:"ReturnInheritedBidStrategyTypes"`
}

// msAdGroupBidUp names ONLY Id and CpcBid. No BiddingScheme is ever sent: Microsoft ignores an
// ad-group scheme anyway, and this path never asks for a strategy change.
type msAdGroupBidUp struct {
	Id     json.Number `json:"Id"`
	CpcBid msBid       `json:"CpcBid"`
}

// UpdateAdGroupCpcBid sets an existing ad group's default CpcBid.
//
// The PUT is IDEMPOTENT (setting the same bid twice converges), so a 429 is retried, and the
// outcome classification is putUpdate's: a 5xx, transport failure, redirect, exhausted 429 or a
// success body that does not answer is UNCONFIRMED; a definite 4xx or PartialError is a definite
// refusal ONLY when no earlier attempt was retried (retriedUnconfirmedError). Definite refusals
// are given identities the caller answers differently:
//
//   - an amount refusal (floor, ceiling, invalid) → a bidAmountError (ErrBidAmountInvalid);
//   - CannotSetSearchBidOnAdGroup, or an ad group id that addresses nothing → ErrBidNotSettable.
func (c *Client) UpdateAdGroupCpcBid(ctx context.Context, campaignID, adGroupID string, amount float64) error {
	cID, agID := strings.TrimSpace(campaignID), strings.TrimSpace(adGroupID)
	if !idRE.MatchString(cID) {
		return fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", campaignID)
	}
	if !idRE.MatchString(agID) {
		return fmt.Errorf("microsoft-ads: ad group id %q is not a numeric id", adGroupID)
	}
	if err := ValidateMaxCPCBid(amount); err != nil {
		return err
	}
	err := c.putUpdate(ctx, "AdGroups", updateAdGroupBidRequest{
		CampaignId: json.Number(cID),
		AdGroups:   []msAdGroupBidUp{{Id: json.Number(agID), CpcBid: msBid{Amount: amount}}},
	}, "ad group bid")
	if err == nil {
		return nil
	}
	if IsOutcomeUnconfirmed(err) {
		return err
	}
	has := func(codes []string) bool {
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
	case has(bidNotSettable):
		return fmt.Errorf("%w: Microsoft reports this ad group does not take a CPC bid: %w", ErrBidNotSettable, err)
	case has(adGroupInvalid):
		return fmt.Errorf("%w: Microsoft reports the ad group id is not valid in this campaign: %w", ErrBidNotSettable, err)
	case has(bidFloorCodes):
		return &bidAmountError{msg: fmt.Sprintf("Microsoft Advertising refused a max CPC bid of %s as below the minimum bid for this ad account's currency", formatBudgetAmount(amount))}
	case has(bidCeilingCodes):
		return &bidAmountError{msg: fmt.Sprintf("Microsoft Advertising refused a max CPC bid of %s as above the ceiling it allows — a bid may not exceed the campaign's daily budget", formatBudgetAmount(amount))}
	case has(bidInvalidCodes):
		return &bidAmountError{msg: fmt.Sprintf("Microsoft Advertising refused a max CPC bid of %s as not valid for this ad account's currency", formatBudgetAmount(amount))}
	}
	return err
}
