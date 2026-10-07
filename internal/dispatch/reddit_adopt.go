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

// LookupCampaign implements service.CampaignAdopter for Reddit: it confirms that
// platformCampaignID names a live campaign in the PROJECT'S OWN connected ad account. See
// adopt.go for the order every adopter follows and why.
//
// The read is GET /ad_accounts/{account}/campaigns/{id} (reddit.Client.GetCampaign), scoped by
// its path to the connection's account. The campaign's ad_account_id, when Reddit reports one,
// is compared as well — the same check-don't-assume rule the Reddit budget writer applies — and a
// different account is refused with domain.ErrCampaignAccountMismatch. An unreported account is
// left to the path scoping, which is what makes it the primary guard.
func (d *RedditDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := reddit.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	client, err := d.resolveRedditClient(ctx, projectID, platform, d.creds.resolveOwnedForAdoption)
	if err != nil {
		return nil, err
	}
	ref, err := client.GetCampaign(ctx, platformCampaignID)
	if err != nil {
		switch {
		case errors.Is(err, reddit.ErrInvalidAccountID):
			// The connection's stored account id, not the caller's input: decided before
			// anything was sent.
			return nil, adoptionAccountIDUnusable(platform, projectID)
		case errors.Is(err, reddit.ErrInvalidCampaignID):
			return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
		}
		return nil, fmt.Errorf("look up reddit campaign: %w", err)
	}
	if ref == nil {
		return nil, nil
	}
	own := strings.TrimSpace(client.AccountID())
	if ref.AdAccountID != "" && ref.AdAccountID != own {
		return nil, adoptionAccountMismatch(platform, ref.ID, ref.AdAccountID, own)
	}
	// Provenance only — no ad group or ad id — so ToggleStatus refuses ACTIVATE (redditChildIDs
	// is empty) and PAUSE addresses the campaign alone. AccountID is what redditCreationAccountID
	// reads back.
	return adoptedRef(ref.ID, ref.Name, &reddit.CampaignResult{
		Platform:     string(model.ProviderRedditAds),
		CampaignName: ref.Name,
		CampaignID:   ref.ID,
		AccountID:    own,
		Steps:        []string{adoptionStep(ref.ID)},
	})
}
