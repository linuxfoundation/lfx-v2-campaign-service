// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// googleAdsManualCPC is the only Google bidding strategy that reads an ad group's CPC bid.
// Everything else — Target CPA, Target ROAS, Maximize Clicks/Conversions/Conversion Value,
// Target Impression Share, Manual CPM/CPV — bids automatically, and Google KEEPS the ad group's
// cpc_bid_micros while never bidding it (internal/platform/googleads/bidding.go says the same
// thing on the create path, where a CPC bid under an automated strategy is refused rather than
// written and ignored).
const googleAdsManualCPC = "MANUAL_CPC"

// WriteBid implements service.BidWriter for Google Ads: it sets the DEFAULT CPC BID of the ONE
// ad group this service created for the campaign, after establishing that the campaign's bidding
// strategy actually uses that bid.
//
// WHERE THE BID LIVES is decided by the create path, not assumed here: CreateCampaign sets
// cpc_bid_micros on the ad group it creates and creates the keywords with no bids of their own,
// so they inherit it. The ad group is the one the create path recorded in the row's result blob
// (googleAdsCampaignAdGroupIDs).
//
// GOOGLE IS THE FIRST PLATFORM WHERE THAT SET CAN HOLD MORE THAN ONE AD GROUP, and more than one
// is REFUSED rather than resolved. Sibling ad groups carry INDEPENDENT cpc_bid_micros values —
// a campaign can legitimately bid 0.80 on one and 2.40 on another — so there is no single bid
// to set and no basis for picking one: writing to the first would silently leave the others, and
// writing the same amount to all would flatten a deliberate spread the caller never asked about.
// The request names a campaign, not an ad group, so the ambiguity belongs to the caller. A row
// recording NONE is refused for the same reason every sibling refuses it: either the campaign was
// never provisioned, or it was ADOPTED, which records none — and choosing an ad group by listing
// the account's is a decision this endpoint does not make.
//
// READ-THEN-WRITE, and the read establishes the one fact that makes the write honest: the
// campaign's bidding strategy is MANUAL_CPC. Any automated strategy, and an unreported one, is
// refused with ErrBidUnwritable — the bid would be stored and never used, so reporting the write
// as a bid change would be false. This path NEVER changes a bidding strategy. The same read also
// covers the PORTFOLIO case without a second query: a portfolio (shared) bid strategy cannot be
// MANUAL_CPC — Google does not offer it as a portfolio type — so a campaign governed by one
// never reports MANUAL_CPC here and is refused by the same guard.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED. Each refusal below returns before
// adGroups:mutate is built, so a caller seeing one of them knows the platform is untouched.
func (d *GoogleAdsDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	const op = "write google ads campaign bid"

	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE — WriteBudget's guard, for its reason.
	// Google Ads is ONE customer shared across every foundation, and ad group ids are bare
	// numerics unique only within a customer, so a row that cannot prove which customer it was
	// created under cannot prove the ad group it names is its own. The cost of being wrong here
	// is changing a stranger's bid.
	created := googleAdsCreationCustomerID(campaign)
	if created == "" {
		return fmt.Errorf("%s: campaign %s does not record which customer it was created under, so its id cannot be resolved against any account: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// UNIT — an ad group's cpc_bid_micros is a cost-per-click bid by definition.
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("%s: campaign %s's ad group bids cost-per-click, but the request names %q: %w",
			op, campaign.PlatformCampaignID, bid.Type, domain.ErrBidUnwritable)
	}

	// IDS — facts about the row, refused before a credential is decrypted.
	campaignID := strings.TrimSpace(campaign.PlatformCampaignID)
	if err := googleads.ValidateCampaignID(campaignID); err != nil {
		return fmt.Errorf("%s: the recorded platform campaign id %q is not a Google Ads campaign id, so there is nothing to address: %w",
			op, campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}
	adGroupIDs := sortedKeys(googleAdsCampaignAdGroupIDs(campaign))
	switch {
	case len(adGroupIDs) == 0:
		return fmt.Errorf("%s: campaign %s records no ad group created by this service — either it was never provisioned, or the campaign was ADOPTED, which records none — so there is no ad group whose bid this endpoint may set; change the bid in Google Ads: %w",
			op, campaignID, domain.ErrBidUnwritable)
	case len(adGroupIDs) > 1:
		return fmt.Errorf("%s: campaign %s records %d ad groups (%s), each of which carries its own max CPC bid, so this request does not name one bid to set; set the bid on the ad group you mean in Google Ads: %w",
			op, campaignID, len(adGroupIDs), strings.Join(adGroupIDs, ", "), domain.ErrBidUnwritable)
	}
	adGroupID := adGroupIDs[0]

	// AMOUNT — the create path's bounds, a pure function of the request, so refused before any
	// call as a permanent request fault (400).
	//
	// UNLIKE WriteBudget, which maps no amount rejection, this path NEEDS the mapping: minCPCBid
	// (0.01) and maxCPCBid (100_000.0) are the ADAPTER's own bounds in the account's currency,
	// and the service layer knows neither — its own checks (maxCampaignBid of 1e6, and refusing
	// anything rounding below half a micro) leave a wide band of amounts it accepts and this
	// client refuses. Without the mapping each of those would fall through to a 503 and invite a
	// retry that can never succeed.
	micros, err := googleads.ValidateMaxCPCBid(bid.Amount)
	if err != nil {
		return googleAdsBidAmountRejected(err)
	}

	// `campaign`, not nil: this operates on an ALREADY-CREATED campaign, so the account to
	// authenticate as is the one the campaign RECORDS being created under — the same resolution
	// ToggleStatus, ReadMetrics and WriteBudget use.
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if created != client.CustomerID() {
		return fmt.Errorf("%s: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			op, campaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	settings, err := client.GetCampaignSettings(ctx, campaignID)
	if err != nil {
		// A PURE READ: its failure is DEFINITE, so it is returned unclassified.
		return fmt.Errorf("%s: read bidding strategy: %w", op, err)
	}
	if settings == nil {
		// The platform answered and holds no such campaign. There is nothing to write to.
		return fmt.Errorf("%w: google-ads campaign %s", domain.ErrPlatformCampaignAbsent, campaignID)
	}

	// THE GUARD — the strategy must be the one that reads the ad group bid. A nil or empty
	// BiddingStrategyType is refused rather than assumed manual: "we could not establish that
	// this campaign bids manually" and "this campaign bids manually" are opposite facts, and only
	// one of them justifies reporting a stored-and-ignored value as a bid change.
	switch {
	case settings.BiddingStrategyType == nil || *settings.BiddingStrategyType == "":
		return fmt.Errorf("%s: campaign %s did not report its bidding strategy, so it cannot be established that a manual bid would be used: %w",
			op, campaignID, domain.ErrBidUnwritable)
	case *settings.BiddingStrategyType != googleAdsManualCPC:
		return fmt.Errorf("%s: campaign %s uses the %s bidding strategy, under which Google keeps an ad group's max CPC bid but never bids it; this endpoint never changes a bidding strategy — change it in Google Ads first: %w",
			op, campaignID, *settings.BiddingStrategyType, domain.ErrBidUnwritable)
	}

	// Every guard has passed; this is the first and only mutating call, and its error MUST be
	// classified — an unwrapped ambiguous failure would be answered "the campaign was not
	// modified", a false claim about a money-moving write.
	//
	// There is no ErrBidAmountInvalid arm here, unlike the Microsoft path: Google states no bid
	// floor of its own that only its mutate can report, so every amount refusal this client makes
	// happens in ValidateMaxCPCBid above, before anything is sent. If Google ever rejects an
	// amount at the mutate — a per-currency minimum, say — the arm belongs here, mapped through
	// googleAdsBidAmountRejected exactly as the pre-call check is.
	if err := client.UpdateAdGroupCPCBid(ctx, adGroupID, micros); err != nil {
		werr := fmt.Errorf("%s for campaign %s: %w", op, campaignID, err)
		if googleads.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBidWriteError{err: werr}
		}
		return werr
	}
	return nil
}

// googleAdsBidAmountRejected maps a Google bid-amount refusal to the service's 400, carrying only
// the client's own sentence as the reason. Mirrors microsoftBidAmountRejected.
func googleAdsBidAmountRejected(err error) error {
	reason, ok := googleads.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by Google Ads"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write google ads campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}

// sortedKeys returns a set's members in a stable order. Map iteration order is randomised, so
// without this both the single-ad-group pick and the multi-ad-group refusal message would vary
// between identical requests — the pick silently (one run bids group A, the next group B) and the
// message visibly.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
