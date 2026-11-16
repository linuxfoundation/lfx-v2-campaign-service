// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// WriteBid implements service.BidWriter for Microsoft Advertising: it sets the default CpcBid of
// the ONE ad group this service created for the campaign, after establishing that the
// campaign's bid strategy actually uses that bid.
//
// WHERE THE BID LIVES is decided by the create path, not assumed here: CreateCampaign sets
// CpcBid on the ad group it creates (findOrCreateAdGroup) and creates the keywords with no bids
// of their own, so they inherit it. The ad group id is the one the create path recorded in the
// row's result blob (microsoftChildIDs); a row without one — an adopted campaign, or a create
// that never reached the ad group — is refused rather than resolved by listing ad groups,
// because choosing which of several to re-bid is not this endpoint's decision.
//
// READ-THEN-WRITE, and the read establishes the one fact that makes the write honest: the
// campaign's OWN bid strategy is EnhancedCpc or ManualCpc, the two Microsoft documents as using
// the ad group bid (see internal/platform/microsoft/bid.go for the citations). Any automated
// strategy, any portfolio strategy, and an unreported or unreadable scheme is refused with
// ErrBidUnwritable: the bid would be ignored, and this path never changes a strategy.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED. The PUT's own DEFINITE refusals (an amount
// Microsoft rejects, an ad group that takes no CPC bid) carry the same "platform unchanged"
// guarantee; an UNCONFIRMED outcome — including any failure after a retried 429 — never does.
func (d *MicrosoftDispatcher) WriteBid(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, bid model.BidChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE — WriteBudget's guard, for its reason: a
	// Microsoft id is unique only within its account, and the cost of being wrong is changing a
	// stranger's bid.
	created := microsoftCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write microsoft campaign bid: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// UNIT — an ad group CpcBid is a cost-per-click bid by definition.
	if bid.Type != model.BidTypeCPC {
		return fmt.Errorf("write microsoft campaign bid: campaign %s's ad group bids cost-per-click, but the request names %q: %w",
			campaign.PlatformCampaignID, bid.Type, domain.ErrBidUnwritable)
	}

	// IDS — facts about the row, refused before a credential is decrypted.
	campaignID := strings.TrimSpace(campaign.PlatformCampaignID)
	if !microsoftCampaignIDRE.MatchString(campaignID) {
		return fmt.Errorf("write microsoft campaign bid: the recorded platform campaign id %q is not a Microsoft campaign id, so there is nothing to address: %w",
			campaign.PlatformCampaignID, domain.ErrBidUnwritable)
	}
	adGroupID, _ := microsoftChildIDs(campaign)
	adGroupID = strings.TrimSpace(adGroupID)
	if adGroupID == "" {
		return fmt.Errorf("write microsoft campaign bid: campaign %s records no ad group created by this service — either it was never provisioned, or the campaign was ADOPTED, which records none — so there is no ad group whose bid this endpoint may set; change the bid in Microsoft Advertising: %w",
			campaignID, domain.ErrBidUnwritable)
	}
	if !microsoftCampaignIDRE.MatchString(adGroupID) {
		return fmt.Errorf("write microsoft campaign bid: the recorded ad group id %q is not a Microsoft ad group id: %w",
			adGroupID, domain.ErrBidUnwritable)
	}

	// AMOUNT — the create path's bounds, a pure function of the request, so refused before any
	// call as a permanent request fault (400).
	if verr := microsoft.ValidateMaxCPCBid(bid.Amount); verr != nil {
		return microsoftBidAmountRejected(verr)
	}

	client, err := d.resolveMicrosoftClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if err := verifyMicrosoftAccountMatch("write microsoft campaign bid", campaign, client); err != nil {
		return err
	}

	strategy, err := client.GetCampaignBidStrategy(ctx, campaignID)
	if err != nil {
		// A PURE READ: its failure is DEFINITE, so it is returned unclassified.
		return fmt.Errorf("write microsoft campaign bid: read bid strategy: %w", err)
	}
	if strategy == nil {
		return fmt.Errorf("%w: microsoft campaign %s", domain.ErrPlatformCampaignAbsent, campaignID)
	}

	// THE GUARD — the strategy must be one that reads the ad group bid.
	switch {
	case strategy.PortfolioUnreadable:
		return fmt.Errorf("write microsoft campaign bid: campaign %s reported a BidStrategyId this service could not read, so whether a shared portfolio strategy governs its bids cannot be established: %w",
			campaignID, domain.ErrBidUnwritable)
	case strategy.PortfolioBidStrategyID != "":
		return fmt.Errorf("write microsoft campaign bid: campaign %s uses a shared portfolio bid strategy, which manages its bids automatically, so a manual ad group bid would be ignored; this endpoint never changes a bid strategy: %w",
			campaignID, domain.ErrBidUnwritable)
	case strategy.SchemeUnreadable:
		return fmt.Errorf("write microsoft campaign bid: campaign %s reported a BiddingScheme this service could not read: %w",
			campaignID, domain.ErrBidUnwritable)
	case strategy.SchemeType == "":
		return fmt.Errorf("write microsoft campaign bid: campaign %s did not report its bid strategy (Microsoft omits it for MaxConversionValue and TargetImpressionShare), so it cannot be established that a manual bid would be used: %w",
			campaignID, domain.ErrBidUnwritable)
	case !strategy.UsesAdGroupBid():
		return fmt.Errorf("write microsoft campaign bid: campaign %s uses the automated %q bid strategy, under which Microsoft ignores ad group bids; this endpoint never changes a bid strategy — change it in Microsoft Advertising first: %w",
			campaignID, strategy.SchemeType, domain.ErrBidUnwritable)
	}

	// Every guard has passed; this is the first and only mutating call, and its error MUST be
	// classified — an unwrapped ambiguous failure would be answered "the campaign was not
	// modified", a false claim about a money-moving write.
	//
	// A multi-ad-group create (#563 gap 12) bids every ad group it made at the same CpcBid, so the
	// campaign's bid is changed on ALL of them, first group first. A failure after an earlier
	// group changed is UNCONFIRMED as a whole: a definite refusal leaves the campaign partly
	// re-bid, and an ambiguous one (timeout, 5xx, unreadable success) leaves the later group's own
	// outcome unknown, so the campaign MAY be partly re-bid. The message says which.
	// Every CONFIRMED further group is re-bid, including one whose ad or keyword step failed: it
	// exists upstream at the old bid, and leaving it there would save a campaign bid it does not
	// carry.
	// A further group whose create was UNCONFIRMED and recorded no id may exist upstream at the
	// old bid. It is reconciled first, by name (reconcileMicrosoftUnaddressedAdGroups): found, its
	// id is recorded and it is re-bid below; definitely absent, it stops blocking. One that still
	// cannot be established refuses before any change rather than report a campaign bid that
	// group may not carry. The reconciled result is persisted with the new bid on success.
	unresolved, unsettled := reconcileMicrosoftUnaddressedAdGroups(ctx, client, campaign, campaignID, adGroupID, d.microsoftCreateSettled(campaign))
	if len(unresolved) > 0 {
		return fmt.Errorf("write microsoft campaign bid: campaign %s has ad group %s whose create was unconfirmed and whose lookup by name could not establish it (the read failed or was unreadable, or the name resolved to an unusable or already-recorded id), so it cannot be re-bid; retry the bid change — it clears once the lookup finds that ad group or confirms it absent — or resolve the ad group in Microsoft Advertising: %w",
			campaignID, strings.Join(unresolved, ", "), domain.ErrBidUnwritable)
	}
	if len(unsettled) > 0 {
		if retryAt, ok := microsoftCreateSettlesAt(campaign); ok {
			// TEMPORARY: the caller is told so, which entry blocks, and when to retry — in a
			// sentence this service writes (entry labels are the row's own recorded positions
			// and suffixes, never Microsoft's text). An entry that settles to "found" before
			// then is recorded and re-bid at once.
			return &settlingBidWriteError{
				// Retry time first: the service clips the sentence to its summary length.
				reason: fmt.Sprintf("retry after %s, or once the ad group appears in Microsoft Advertising — ad group %s has an unconfirmed create that may still commit, so its absence is not yet trusted",
					retryAt.UTC().Format(time.RFC3339), strings.Join(unsettled, ", ")),
				err: fmt.Errorf("write microsoft campaign bid: campaign %s has ad group %s whose create was unconfirmed and which is not listed yet, but a timed-out create can still commit, so its absence is not trusted until %s after the campaign row was last written; retry the bid change after that, or once the ad group appears (it is then recorded and re-bid at once): %w",
					campaignID, strings.Join(unsettled, ", "), microsoftUnconfirmedSettleWindow, domain.ErrBidUnwritable),
			}
		}
		// No recorded update time: the window never closes, so this is not a refusal that
		// clears by waiting, and it keeps the generic 409.
		return fmt.Errorf("write microsoft campaign bid: campaign %s has ad group %s whose create was unconfirmed and which is not listed yet, but a timed-out create can still commit, so its absence is not trusted until %s after the campaign row was last written; retry the bid change after that, or once the ad group appears (it is then recorded and re-bid at once): %w",
			campaignID, strings.Join(unsettled, ", "), microsoftUnconfirmedSettleWindow, domain.ErrBidUnwritable)
	}
	extraIDs := microsoftConfirmedExtraAdGroupIDs(campaign.Result, adGroupID)
	for _, id := range extraIDs {
		if !microsoftCampaignIDRE.MatchString(id) {
			return fmt.Errorf("write microsoft campaign bid: the recorded ad group id %q is not a Microsoft ad group id: %w", id, domain.ErrBidUnwritable)
		}
	}
	if err := client.UpdateAdGroupCpcBid(ctx, campaignID, adGroupID, bid.Amount); err != nil {
		werr := fmt.Errorf("write microsoft campaign bid for campaign %s: %w", campaignID, err)
		switch {
		case microsoft.IsOutcomeUnconfirmed(err):
			return &unconfirmedBidWriteError{err: werr}
		case errors.Is(err, microsoft.ErrBidNotSettable):
			return fmt.Errorf("%w: %w", werr, domain.ErrBidUnwritable)
		case errors.Is(err, microsoft.ErrBidAmountInvalid):
			return microsoftBidAmountRejected(err)
		}
		return werr
	}
	for _, extraID := range extraIDs {
		if err := client.UpdateAdGroupCpcBid(ctx, campaignID, extraID, bid.Amount); err != nil {
			if microsoft.IsOutcomeUnconfirmed(err) {
				return &unconfirmedBidWriteError{err: fmt.Errorf("write microsoft campaign bid for campaign %s: ad group %s was re-bid but the outcome for ad group %s is unconfirmed (it may or may not have been re-bid), so the campaign may be partly re-bid: %w", campaignID, adGroupID, extraID, err)}
			}
			return &unconfirmedBidWriteError{err: fmt.Errorf("write microsoft campaign bid for campaign %s: ad group %s was re-bid but ad group %s was not, so the campaign is partly re-bid: %w", campaignID, adGroupID, extraID, err)}
		}
	}
	return nil
}

// microsoftBidAmountRejected maps a Microsoft bid-amount refusal to the service's 400, carrying
// only the client's own sentence as the reason.
func microsoftBidAmountRejected(err error) error {
	reason, ok := microsoft.BidAmountReason(err)
	if !ok {
		reason = "the requested max CPC bid is not accepted by Microsoft Advertising"
	}
	return &rejectedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write microsoft campaign bid: %w: %w", err, domain.ErrBidAmountRejected),
	}
}

// microsoftUnconfirmedSettleWindow is how long after an UNCONFIRMED ad group create its absence
// from the campaign's ad group list is trusted. A create that timed out (or answered 5xx) may
// still commit after the response was lost, so a lookup soon after can miss an ad group that is
// about to exist; an hour is far beyond any request Microsoft is still processing.
const microsoftUnconfirmedSettleWindow = time.Hour

// microsoftCreateSettled reports whether the campaign's unconfirmed creates have had
// microsoftUnconfirmedSettleWindow to settle, measured from the row's UpdatedAt: the row's last
// write, which is never earlier than the dispatch result that recorded the unconfirmed group
// (CreatedAt is the claim, BEFORE the create ran, so it could clear too early). A later write
// only moves UpdatedAt forward, extending the window — the safe direction. An unknown (zero)
// UpdatedAt is never settled.
// microsoftCreateSettlesAt reports when microsoftCreateSettled turns true for the row, or false
// when it never will (an unknown UpdatedAt).
func microsoftCreateSettlesAt(campaign *model.Campaign) (time.Time, bool) {
	if campaign.UpdatedAt.IsZero() {
		return time.Time{}, false
	}
	return campaign.UpdatedAt.Add(microsoftUnconfirmedSettleWindow), true
}

func (d *MicrosoftDispatcher) microsoftCreateSettled(campaign *model.Campaign) bool {
	if campaign.UpdatedAt.IsZero() {
		return false
	}
	now := time.Now
	if d.bidNow != nil {
		now = d.bidNow
	}
	return now().Sub(campaign.UpdatedAt) >= microsoftUnconfirmedSettleWindow
}
