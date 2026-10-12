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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// WriteBid implements service.BidWriter for Meta: it sets the bid_amount of the ONE ad set this
// service created for the campaign, after establishing that bid_amount there is a maximum cost
// per link click.
//
// WHERE THE BID LIVES. Meta bids on the AD SET, the same object WriteBudget writes, named by the
// row's result blob (metaAdSetID). A row without one is refused rather than resolved by listing
// ad sets.
//
// WHEN IT IS A MAX CPC. Only under bid_strategy LOWEST_COST_WITH_BID_CAP with billing_event AND
// optimization_goal both LINK_CLICKS — see internal/platform/meta/bid_update.go for the cited
// reasoning. A bid cap billed on impressions is a per-1,000 figure (a CPM), COST_CAP is a target
// average, and LOWEST_COST_WITHOUT_CAP is automatic bidding: all are refused (ErrBidUnwritable,
// 409), never re-bid, and the strategy is never sent.
//
// WHAT THE CREATE PATH SENDS MAKES THIS LEG NARROW: CreateCampaign creates the ad set with
// billing_event IMPRESSIONS and LOWEST_COST_WITHOUT_CAP, so every Meta campaign this service
// creates is refused here until an operator moves its ad set to a link-click bid cap in Ads
// Manager.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED.
func (d *MetaDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE — WriteBudget's guard, for its reason.
	created := metaCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write meta campaign bid: campaign %s does not record which ad account it was created under, so its ad set cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("write meta campaign bid: the request names bid type %q, but this endpoint sets a cost-per-click bid only: %w",
			bid.Type, domain.ErrBidUnwritable)
	}
	adSetID := strings.TrimSpace(metaAdSetID(campaign))
	if adSetID == "" {
		return fmt.Errorf("write meta campaign bid: campaign %s records no ad set created by this service — either it was never provisioned, or the campaign was ADOPTED, which records none — so there is no ad set whose bid this endpoint may set; change the bid in Meta Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}

	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	// The account selection is load-bearing for the reason WriteBudget states: its currency
	// decides the minor-unit scale bid_amount is encoded in.
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return fmt.Errorf("write meta campaign bid: %w", err)
	}
	// SHAPE, BEFORE ANY REQUEST. requireMetaAccountID checks only presence, and
	// verifyMetaAccountMatch reads a malformed current id as "unknown" and proceeds — so a stored
	// id that is not act_<digits> would both skip the mismatch guard and be spliced into the
	// currency preflight's Graph path. It is a defect of the CONNECTION, attributed to the system
	// row when that is where it came from.
	if verr := meta.ValidateAccountID(accountID); verr != nil {
		return res.systemScoped(fmt.Errorf("%w: %w: write meta campaign bid: the connection's ad account id cannot address a meta request: %w",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, verr))
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	if err := verifyMetaAccountMatch("write meta campaign bid", campaign, accountID); err != nil {
		return err
	}

	current, err := client.GetAdSetBid(ctx, adSetID)
	if err != nil {
		// A PURE READ, so DEFINITE — except an ad set id the client refuses before any request,
		// which is a property of the row.
		if errors.Is(err, meta.ErrInvalidAdSetID) {
			return fmt.Errorf("write meta campaign bid: the campaign row's recorded ad set id cannot address a meta request: %w: %w", err, domain.ErrBidUnwritable)
		}
		return fmt.Errorf("write meta campaign bid: read ad set bid: %w", err)
	}

	// GUARD 1 — THE AD SET MUST BELONG TO THIS CAMPAIGN. An unreported owner is refused too:
	// "could not establish" and "it does" are opposite facts, and only the second justifies a
	// write that moves money.
	if current.CampaignID != strings.TrimSpace(campaign.PlatformCampaignID) {
		reported := current.CampaignID
		if reported == "" {
			reported = "a campaign meta did not report"
		}
		return fmt.Errorf("write meta campaign bid: ad set %s recorded for campaign %s belongs to %s upstream: %w",
			adSetID, campaign.PlatformCampaignID, reported, domain.ErrBidUnwritable)
	}
	// GUARD 2 — A LINK-CLICK BID CAP ONLY.
	if current.BidStrategy != meta.BidStrategyBidCap {
		reported := current.BidStrategy
		if reported == "" {
			reported = "no bid_strategy"
		}
		return fmt.Errorf("write meta campaign bid: ad set %s reports %s, so bid_amount is not a manual bid cap (LOWEST_COST_WITHOUT_CAP is automatic bidding; COST_CAP is a target average; LOWEST_COST_WITH_MIN_ROAS is a return floor); this endpoint never changes a bid strategy — move the ad set to a bid cap in Meta Ads Manager first: %w",
			adSetID, reported, domain.ErrBidUnwritable)
	}
	if !current.CPCBidCap() {
		return fmt.Errorf("write meta campaign bid: ad set %s bills on %q and optimizes for %q, so its bid cap is not a cost per link click (an impression-billed cap is per 1,000 impressions); this endpoint sets a max CPC bid and never changes billing or optimization: %w",
			adSetID, current.BillingEvent, current.OptimizationGoal, domain.ErrBidUnwritable)
	}
	// GUARD 3 — THE CURRENT BID MUST BE LEGIBLE.
	if current.BidAmountUnparseable {
		return fmt.Errorf("write meta campaign bid: ad set %s reported a bid_amount this service could not read: %w",
			adSetID, domain.ErrBidUnwritable)
	}

	// Encoded against the account's own currency — still a read, failing definitely.
	minor, err := client.ResolveBidMinorUnits(ctx, bid.Amount)
	if err != nil {
		if errors.Is(err, meta.ErrInvalidAccountID) {
			return res.systemScoped(fmt.Errorf("%w: %w: write meta campaign bid: the connection's ad account id cannot address a meta request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, err))
		}
		if errors.Is(err, meta.ErrAccountCurrencyUnresolvable) {
			return fmt.Errorf("write meta campaign bid: %w: %w", err, domain.ErrBidUnwritable)
		}
		if errors.Is(err, meta.ErrBidAmountInvalid) {
			return metaBidAmountRejected(err)
		}
		return fmt.Errorf("write meta campaign bid: %w", err)
	}

	// Every guard has passed; the first and only mutating call, classified.
	if err := client.UpdateAdSetBid(ctx, adSetID, minor); err != nil {
		werr := fmt.Errorf("write meta campaign bid for campaign %s (ad set %s): %w", campaign.PlatformCampaignID, adSetID, err)
		if meta.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBidWriteError{err: werr}
		}
		if errors.Is(err, meta.ErrBidAmountInvalid) {
			return metaBidAmountRejected(err)
		}
		return werr
	}
	return nil
}

// metaBidAmountRejected maps a Meta bid-amount refusal to the service's 400.
func metaBidAmountRejected(err error) error {
	reason, ok := meta.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by Meta"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write meta campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}
