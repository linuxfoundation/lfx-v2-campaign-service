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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// WriteBid implements service.BidWriter for LinkedIn: it sets the campaign's unitCost — its
// max cost-per-click bid — after establishing that the campaign bids MANUALLY per click.
//
// WHERE THE BID LIVES. On the CAMPAIGN (unitCost), not on a child entity, so the row's own
// PlatformCampaignID addresses it and there is no recorded child id to require. See
// internal/platform/linkedin/bid_update.go for the cited reasoning.
//
// WHEN IT IS A MAX CPC. costType CPC with optimizationTargetType NONE or ENHANCED_CONVERSION —
// the two LinkedIn documents as "manual bidding mode". Automated (MAX_*), target-cost
// (TARGET_COST_PER_*) and cost-cap (CAP_COST_AND_*) bidding, a CPM or CPV campaign, and any
// UNREPORTED value are refused (ErrBidUnwritable, 409). This path never sends
// optimizationTargetType or costType, so it can never switch a strategy.
//
// WHAT THE CREATE PATH SENDS: costType CPC with no optimizationTargetType, which LinkedIn's
// schema defaults to NONE (manual) — so, unlike Reddit and X (which create on automatic bidding
// and are therefore always refused here), a LinkedIn campaign this service created is expected
// to PASS this guard if LinkedIn reports the default. That expectation is unverified against a
// live account, and is not relied on: the guard decides on what LinkedIn REPORTS, and an
// automated or unreported strategy is refused exactly like the siblings' auto-bid campaigns.
//
// ADOPTED ROWS ARE REFUSED (see linkedin_adopt.go): adoption verified identity, not bidding.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, and the one mutation is sent ONCE.
func (d *LinkedInDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE — WriteBudget's guard, for its reason.
	created := linkedInCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write linkedin campaign bid: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("write linkedin campaign bid: the request names bid type %q, but this endpoint sets a cost-per-click bid only: %w",
			bid.Type, domain.ErrBidUnwritable)
	}
	if linkedInAdopted(campaign) {
		return fmt.Errorf("write linkedin campaign bid: campaign %s was ADOPTED, and adoption verifies the campaign's identity, not the bidding setup a bid would be written into; change the bid in LinkedIn Campaign Manager: %w: %w",
			campaign.PlatformCampaignID, domain.ErrBidAdoptedCampaign, domain.ErrBidUnwritable)
	}
	// VERBATIM, never trimmed: a row holding " 777" is not campaign 777, and trimming it would read
	// and mutate a campaign the row does not name. A non-canonical stored id fails closed here.
	campaignID := campaign.PlatformCampaignID
	if err := linkedin.ValidateCampaignID(campaignID); err != nil {
		return fmt.Errorf("write linkedin campaign bid: the campaign row's platform campaign id cannot address a linkedin request: %w: %w", err, domain.ErrBidUnwritable)
	}

	// AMOUNT — a pure function of the request, refused before any call (400).
	wire, err := linkedin.ValidateBidAmount(bid.Amount)
	if err != nil {
		return linkedinBidAmountRejected(err)
	}

	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(res.accountID)
	if !linkedin.ValidAccountID(accountID) {
		return linkedinAccountIDUnusable(res, projectID, "write linkedin campaign bid")
	}
	if err := verifyLinkedInAccountMatch("write linkedin campaign bid", campaign, accountID); err != nil {
		return err
	}
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)

	current, err := client.GetCampaignSettings(ctx, campaignID)
	if err != nil {
		// A PURE READ: its failure is DEFINITE — no write was built.
		rerr := fmt.Errorf("write linkedin campaign bid: read current bid: %w", err)
		if linkedinConnectionDefect(err) {
			return res.systemScoped(linkedinExpiry(rerr))
		}
		return rerr
	}
	if current == nil {
		return fmt.Errorf("%w: linkedin campaign %s", domain.ErrPlatformCampaignAbsent, campaignID)
	}

	// GUARD 1 — THE CAMPAIGN MUST BE THIS ACCOUNT'S, AS REPORTED. `account` is a required
	// field, so an unreported one is an unestablished fact and is refused, never assumed.
	if current.AccountID == "" {
		return fmt.Errorf("write linkedin campaign bid: campaign %s did not report its ad account, so it cannot be established that it is the connection's: %w",
			campaignID, domain.ErrBidUnwritable)
	}
	if current.AccountID != accountID {
		return fmt.Errorf("write linkedin campaign bid: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaignID, current.AccountID, accountID, domain.ErrCampaignAccountMismatch)
	}
	// GUARD 2 — NOT TERMINAL. A canceled or deleted campaign can never serve again. `status` is a
	// required field, so an unreported one leaves that unestablished and is refused like every
	// other unreported fact here.
	if current.Status == nil {
		return fmt.Errorf("write linkedin campaign bid: campaign %s did not report its status, so it cannot be established that it can still serve: %w",
			campaignID, domain.ErrBidUnwritable)
	}
	if current.Terminal() {
		return fmt.Errorf("write linkedin campaign bid: campaign %s is %s upstream and can never serve again: %w",
			campaignID, *current.Status, domain.ErrBidUnwritable)
	}
	// GUARD 3 — MANUAL CPC ONLY.
	if !current.ManualCPC() {
		return fmt.Errorf("write linkedin campaign bid: campaign %s reports costType %s with optimizationTargetType %s, which is not a manual max CPC (MAX_* is LinkedIn's automated bidding, TARGET_COST_PER_* target-cost and CAP_COST_AND_* cost-cap bidding), so a manual bid would be ignored or misread; this endpoint never changes a bid strategy — move the campaign to manual CPC bidding in LinkedIn Campaign Manager first: %w",
			campaignID, reportedOr(current.CostType, "unreported"), reportedOr(current.OptimizationTargetType, "unreported"), domain.ErrBidUnwritable)
	}
	// GUARD 4 — THE CURRENT BID MUST BE LEGIBLE. unitCost is a required field; an absent or
	// unreadable amount is an unreported fact.
	if current.UnitCostUnparseable || current.UnitCostMicros == nil {
		return fmt.Errorf("write linkedin campaign bid: campaign %s did not report a legible unitCost: %w",
			campaignID, domain.ErrBidUnwritable)
	}
	// GUARD 5 — CURRENCY. This client writes USD, as every LinkedIn write here does; writing it
	// over a bid in another (or an unreported) currency would redenominate it.
	if !strings.EqualFold(current.UnitCostCurrency, "USD") {
		return fmt.Errorf("write linkedin campaign bid: campaign %s bids in %s, but this service writes LinkedIn bids in USD only: %w",
			campaignID, reportedOr(nonEmpty(current.UnitCostCurrency), "a currency LinkedIn did not report"), domain.ErrBidUnwritable)
	}

	// Every guard has passed; the first and only mutating call, sent once and classified.
	if err := client.UpdateCampaignUnitCost(ctx, campaignID, wire); err != nil {
		werr := fmt.Errorf("write linkedin campaign bid for campaign %s: %w", campaignID, err)
		// AMBIGUITY FIRST, as in WriteBudget: an expiry that may have applied stays unconfirmed.
		if linkedin.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBidWriteError{err: werr}
		}
		if errors.Is(err, linkedin.ErrBidAmountInvalid) {
			return linkedinBidAmountRejected(err)
		}
		if linkedinConnectionDefect(err) {
			return res.systemScoped(linkedinExpiry(werr))
		}
		return werr
	}
	return nil
}

// linkedinBidAmountRejected maps a LinkedIn bid-amount refusal to the service's 400.
func linkedinBidAmountRejected(err error) error {
	reason, ok := linkedin.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by LinkedIn"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write linkedin campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}

// reportedOr renders an optional upstream value for an error message.
func reportedOr(v *string, fallback string) string {
	if v == nil || *v == "" {
		return fallback
	}
	return *v
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
