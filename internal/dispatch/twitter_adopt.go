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

// LookupCampaign implements service.CampaignAdopter for X (Twitter) Ads: it confirms that
// platformCampaignID names a live campaign in the PROJECT'S OWN connected ad account. See
// adopt.go for the order every adopter follows and why.
//
// The read is GET accounts/:account_id/campaigns/:campaign_id (twitter.Client.GetCampaign),
// scoped by its path to the connection's account — the scoping every X call here relies on,
// because X's campaign object is not documented to carry the account id (see
// twitter.Client.GetCampaignBudget). When a response does carry one it is compared, and a
// different account is refused with domain.ErrCampaignAccountMismatch.
func (d *TwitterDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := twitter.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	res, err := d.creds.resolveOwnedForAdoption(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	// The same cache entry the toggle and create paths use for this connection, so adoption
	// shares their write pacer rather than pacing independently against the account's budget.
	client := d.cachedTwitterClient(projectID, platform, res, creds, accountID,
		strings.TrimSpace(res.providerConfig["funding_instrument_id"]))
	ref, err := client.GetCampaign(ctx, platformCampaignID)
	if err != nil {
		switch {
		case errors.Is(err, twitter.ErrInvalidAccountID):
			return nil, adoptionAccountIDUnusable(platform, projectID)
		case errors.Is(err, twitter.ErrInvalidCampaignID):
			return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
		}
		return nil, fmt.Errorf("look up x ads campaign: %w", err)
	}
	if ref == nil {
		return nil, nil
	}
	if ref.AccountID != "" && ref.AccountID != accountID {
		return nil, adoptionAccountMismatch(platform, ref.ID, ref.AccountID, accountID)
	}
	// Provenance only — no line item id — so ToggleStatus refuses ACTIVATE (twitterChildIDs is
	// empty) and PAUSE addresses the campaign alone. AccountID is what twitterCreationAccountID
	// reads back.
	return adoptedRef(ref.ID, ref.Name, &twitter.CampaignResult{
		Platform:     string(model.ProviderTwitterAds),
		CampaignName: ref.Name,
		CampaignID:   ref.ID,
		AccountID:    accountID,
		Steps:        []string{adoptionStep(ref.ID)},
	})
}
