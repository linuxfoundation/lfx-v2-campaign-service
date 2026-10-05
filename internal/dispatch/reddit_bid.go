// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// WriteBid implements service.BidWriter for Reddit: it sets the bid_value of the ONE ad group
// this service created for the campaign, after establishing that the ad group bids MANUALLY in
// CPC.
//
// WHERE THE BID LIVES. Reddit bids on the AD GROUP (bid_strategy, bid_type, bid_value), and the
// create path creates exactly one, recorded in the row's result blob (redditChildIDs). A row
// without one is refused rather than resolved by listing ad groups.
//
// WHAT THE CREATE PATH SENDS MAKES THIS LEG NARROW, and that is stated rather than hidden:
// CreateCampaign sends bid_strategy=BIDLESS — Reddit's automatic bidding — so every Reddit
// campaign this service creates is refused here (ErrBidUnwritable, 409) until an operator moves
// its ad group to MANUAL_BIDDING in Reddit Ads Manager. Writing a bid_value under BIDLESS would
// be ignored, or read as a request to switch strategy; this path never sends bid_strategy.
//
// READ-THEN-WRITE, and the read establishes four facts: the ad group belongs to this campaign;
// its strategy is MANUAL_BIDDING; its bid_type is CPC (the unit the request names); and its
// current bid_value is legible. Each unreported fact is refused, never assumed.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED.
func (d *RedditDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE — WriteBudget's guard, for its reason.
	created := redditCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write reddit campaign bid: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("write reddit campaign bid: the request names bid type %q, but this endpoint sets a cost-per-click bid only: %w",
			bid.Type, domain.ErrBidUnwritable)
	}
	adGroupID, _ := redditChildIDs(campaign)
	adGroupID = strings.TrimSpace(adGroupID)
	if adGroupID == "" {
		return fmt.Errorf("write reddit campaign bid: campaign %s records no ad group created by this service, so there is no ad group whose bid this endpoint may set; change the bid in Reddit Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}

	// AMOUNT — a pure function of the request, refused before any call (400).
	micros, err := reddit.BidMicros(bid.Amount)
	if err != nil {
		return redditBidAmountRejected(err)
	}

	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	if err := verifyRedditAccountMatch("write reddit campaign bid", campaign, client); err != nil {
		return err
	}

	current, err := client.GetAdGroupBid(ctx, adGroupID)
	if err != nil {
		// A PURE READ, so DEFINITE — except the two ids the client refuses before any request,
		// classified by their owner exactly as WriteBudget classifies its own two.
		if errors.Is(err, reddit.ErrInvalidAccountID) {
			return res.systemScoped(fmt.Errorf("%w: %w: write reddit campaign bid: the connection's ad account id cannot address a reddit request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, err))
		}
		if errors.Is(err, reddit.ErrInvalidAdGroupID) {
			return fmt.Errorf("write reddit campaign bid: the campaign row's recorded ad group id cannot address a reddit request: %w: %w", err, domain.ErrBidUnwritable)
		}
		return fmt.Errorf("write reddit campaign bid: read ad group bid: %w", err)
	}
	if current == nil {
		// The ad group the row recorded is gone. The CAMPAIGN may still exist, so this is not
		// ErrPlatformCampaignAbsent's 404: the bid cannot be addressed.
		return fmt.Errorf("write reddit campaign bid: Reddit holds no ad group %s for campaign %s — it may have been deleted upstream: %w",
			adGroupID, campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}

	// GUARD 1 — THE AD GROUP MUST BELONG TO THIS CAMPAIGN, when Reddit says which it belongs to.
	if current.CampaignID != "" && current.CampaignID != strings.TrimSpace(campaign.PlatformCampaignID) {
		return fmt.Errorf("write reddit campaign bid: ad group %s is reported under campaign %s, not %s: %w",
			adGroupID, current.CampaignID, campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}
	// GUARD 2 — MANUAL BIDDING ONLY.
	if current.BidStrategy != reddit.BidStrategyManual {
		reported := current.BidStrategy
		if reported == "" {
			reported = "no bid_strategy"
		}
		return fmt.Errorf("write reddit campaign bid: ad group %s reports %s, so a manual bid would be ignored (BIDLESS, MAXIMIZE_VOLUME and TARGET_CPX are managed by Reddit); this endpoint never changes a bid strategy — move the ad group to manual bidding in Reddit Ads Manager first: %w",
			adGroupID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 3 — THE UNIT MUST BE CPC.
	if current.BidType != reddit.BidTypeCPC {
		reported := current.BidType
		if reported == "" {
			reported = "no bid_type"
		}
		return fmt.Errorf("write reddit campaign bid: ad group %s bids %s, not cost-per-click; this endpoint sets a max CPC bid and never changes the bid type: %w",
			adGroupID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 4 — THE CURRENT BID MUST BE LEGIBLE.
	if current.BidValueUnparseable {
		return fmt.Errorf("write reddit campaign bid: ad group %s reported a bid_value this service could not read: %w",
			adGroupID, domain.ErrBidUnwritable)
	}

	// Every guard has passed; the first and only mutating call, classified.
	if err := client.UpdateAdGroupBid(ctx, adGroupID, micros); err != nil {
		werr := fmt.Errorf("write reddit campaign bid for ad group %s: %w", adGroupID, err)
		if reddit.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBidWriteError{err: werr}
		}
		if errors.Is(err, reddit.ErrBidAmountInvalid) {
			return redditBidAmountRejected(err)
		}
		return werr
	}
	return nil
}

// redditBidAmountRejected maps a Reddit bid-amount refusal to the service's 400.
func redditBidAmountRejected(err error) error {
	reason, ok := reddit.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by Reddit"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write reddit campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}
