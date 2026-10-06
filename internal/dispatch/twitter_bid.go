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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// WriteBid implements service.BidWriter for X (Twitter) Ads: it sets the bid_amount_local_micro
// of the ONE line item this service created for the campaign, after establishing that the line
// item bids a manual maximum charged per link click.
//
// WHERE THE BID LIVES. X bids on the LINE ITEM, named by the row's result blob
// (twitterChildIDs). A row without one is refused rather than resolved by listing line items.
//
// WHEN IT IS A MAX CPC. Only bid_strategy MAX with pay_by LINK_CLICK — see
// internal/platform/twitter/bid_update.go for the cited reasoning. AUTO (automatic bidding),
// TARGET (a target average), an impression-charged line item and an unreported value are all
// refused (ErrBidUnwritable, 409); the strategy and charge unit are never sent.
//
// WHAT THE CREATE PATH SENDS MAKES THIS LEG NARROW: CreateCampaign creates the line item with
// bid_strategy AUTO, so every X campaign this service creates is refused here until an operator
// moves its line item to a manual max bid charged per link click in X Ads Manager.
//
// PROVENANCE IS STRICTER THAN THE TOGGLE'S. verifyTwitterAccountMatch waves through a row that
// records no creating account (the pre-existing-row case); a bid write cannot, because it
// moves money, so that row is refused first.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED.
func (d *TwitterDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	if twitterCreationAccountID(campaign) == "" {
		return fmt.Errorf("write x campaign bid: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("write x campaign bid: the request names bid type %q, but this endpoint sets a cost-per-click bid only: %w",
			bid.Type, domain.ErrBidUnwritable)
	}
	lineItemID := strings.TrimSpace(twitterChildIDs(campaign))
	if lineItemID == "" {
		return fmt.Errorf("write x campaign bid: campaign %s records no line item created by this service, so there is no line item whose bid this endpoint may set; change the bid in X Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}

	// AMOUNT — a pure function of the request, refused before any call (400).
	micros, err := twitter.BidMicros(bid.Amount)
	if err != nil {
		return twitterBidAmountRejected(err)
	}

	client, err := d.resolveTwitterClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if err := verifyTwitterAccountMatch("write x campaign bid", campaign, client); err != nil {
		return err
	}

	current, err := client.GetLineItemBid(ctx, lineItemID)
	if err != nil {
		if errors.Is(err, twitter.ErrInvalidLineItemID) {
			return fmt.Errorf("write x campaign bid: the campaign row's recorded line item id cannot address an x request: %w: %w", err, domain.ErrBidUnwritable)
		}
		// A PURE READ: its failure is DEFINITE.
		return fmt.Errorf("write x campaign bid: read line item bid: %w", err)
	}
	if current == nil || current.Deleted {
		// The line item is gone. The CAMPAIGN may still exist, so this is not
		// ErrPlatformCampaignAbsent's 404: the bid cannot be addressed.
		return fmt.Errorf("write x campaign bid: X holds no live line item %s for campaign %s — it may have been deleted upstream: %w",
			lineItemID, campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}

	// GUARD 1 — THE LINE ITEM MUST BELONG TO THIS CAMPAIGN; an unreported owner is refused.
	if current.CampaignID != strings.TrimSpace(campaign.PlatformCampaignID) {
		reported := current.CampaignID
		if reported == "" {
			reported = "a campaign x did not report"
		}
		return fmt.Errorf("write x campaign bid: line item %s recorded for campaign %s belongs to %s upstream: %w",
			lineItemID, campaign.PlatformCampaignID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 2 — A MANUAL MAX BID ONLY.
	if current.BidStrategy != twitter.BidStrategyMax {
		reported := current.BidStrategy
		if reported == "" {
			reported = "no bid_strategy"
		}
		return fmt.Errorf("write x campaign bid: line item %s reports %s, so a manual bid would be ignored (AUTO is X's automatic bidding; TARGET bids to an average); this endpoint never changes a bid strategy — move the line item to a maximum bid in X Ads Manager first: %w",
			lineItemID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 3 — THE CHARGE UNIT MUST BE A LINK CLICK.
	if !current.ManualCPC() {
		reported := current.PayBy
		if reported == "" {
			reported = "no pay_by"
		}
		return fmt.Errorf("write x campaign bid: line item %s is charged by %s, not per link click; this endpoint sets a max CPC bid and never changes the charge unit: %w",
			lineItemID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 4 — THE CURRENT BID MUST BE LEGIBLE.
	if current.BidAmountUnparseable {
		return fmt.Errorf("write x campaign bid: line item %s reported a bid_amount_local_micro this service could not read: %w",
			lineItemID, domain.ErrBidUnwritable)
	}

	// Every guard has passed; the first and only mutating call, classified.
	if err := client.UpdateLineItemBid(ctx, lineItemID, micros); err != nil {
		werr := fmt.Errorf("write x campaign bid for line item %s: %w", lineItemID, err)
		if twitter.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBidWriteError{err: werr}
		}
		if errors.Is(err, twitter.ErrBidAmountInvalid) {
			return twitterBidAmountRejected(err)
		}
		return werr
	}
	return nil
}

// twitterBidAmountRejected maps an X bid-amount refusal to the service's 400.
func twitterBidAmountRejected(err error) error {
	reason, ok := twitter.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by X"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write x campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}
