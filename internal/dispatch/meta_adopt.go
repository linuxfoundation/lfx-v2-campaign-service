// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// LookupCampaign implements service.CampaignAdopter for Meta: it confirms that platformCampaignID
// names a live campaign in the PROJECT'S OWN connected ad account. See adopt.go for the order
// every adopter follows and why.
//
// The read is GET /{campaign_id} (meta.Client.GetCampaign), and unlike Google's and Microsoft's
// it is NOT account-scoped: a Graph node is addressed by id alone, and one access token commonly
// reaches several ad accounts. Provenance is therefore proved from the ANSWER — the campaign's
// account_id must equal the connection's own account, compared through normalizeMetaAccountID
// (Meta reports bare digits, the connection stores act_<digits>). A campaign reported under any
// other account is refused with domain.ErrCampaignAccountMismatch, and one reported under no
// readable account never reaches this comparison (the client refuses it as unverifiable).
//
// The account is required here even though Meta's toggle and metrics paths deliberately do not
// require one (resolveMetaCredentials): those address a campaign this project already has a row
// for, while adoption has nothing to compare the campaign's account against without it.
func (d *MetaDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := meta.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolveOwnedForAdoption)
	if err != nil {
		return nil, err
	}
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return nil, err
	}
	if meta.ValidateAccountID(accountID) != nil {
		return nil, adoptionAccountIDUnusable(platform, projectID)
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	ref, err := client.GetCampaign(ctx, platformCampaignID)
	if err != nil {
		if errors.Is(err, meta.ErrInvalidCampaignID) {
			return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
		}
		return nil, fmt.Errorf("look up meta campaign: %w", err)
	}
	if ref == nil {
		return nil, nil
	}
	if reported, own := normalizeMetaAccountID(ref.AccountID), normalizeMetaAccountID(accountID); reported == "" || reported != own {
		return nil, adoptionAccountMismatch(platform, ref.ID, ref.AccountID, accountID)
	}
	// Provenance only — no ad set id — so ToggleStatus refuses ACTIVATE (metaAdSetID is empty)
	// and PAUSE addresses the campaign alone. AccountID is what metaCreationAccountID reads back.
	return adoptedRef(ref.ID, ref.Name, &meta.CampaignResult{
		Platform:     string(model.ProviderMetaAds),
		CampaignName: ref.Name,
		CampaignID:   ref.ID,
		AccountID:    accountID,
		Steps:        []string{adoptionStep(ref.ID)},
	})
}
