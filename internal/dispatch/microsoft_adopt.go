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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// LookupCampaign implements service.CampaignAdopter for Microsoft Advertising: it confirms that
// platformCampaignID names a live Search campaign in the PROJECT'S OWN connected ad account, so
// an existing campaign can be bound to a brief without creating anything. See adopt.go for the
// order every adopter follows and why.
//
// The read is GetCampaignsByIds (microsoft.Client.GetCampaign), which is ACCOUNT-SCOPED: the
// connection's account rides the request body and the CustomerAccountId header, and Microsoft
// answers CampaignServiceInvalidCampaignId for a campaign in any other account. The returned
// Campaign carries no account id to cross-check, so — exactly as on Google, whose GAQL query is
// issued against one customer — the scoping of the request is the provenance proof, and the
// account recorded on the row is the one the request was scoped to.
func (d *MicrosoftDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := microsoft.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	res, err := d.creds.resolveOwnedForAdoption(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	// Both ids reach the wire as headers; one that names no account or customer is a defect on
	// the row, refused here rather than surfacing from the client's own guard as an
	// unverifiable 503.
	if microsoft.ValidateAccountID(accountID) != nil ||
		microsoft.ValidateCustomerID(strings.TrimSpace(res.providerConfig["customer_id"])) != nil {
		return nil, adoptionAccountIDUnusable(platform, projectID)
	}
	client := d.cachedMicrosoftClient(projectID, platform, res, creds, accountID)
	ref, err := client.GetCampaign(ctx, platformCampaignID)
	if err != nil {
		// Unreachable as written — ValidateCampaignID above is GetCampaign's own first check —
		// and kept for the reason Google keeps its twin: if the client ever grows a validation
		// the pre-check does not mirror, the answer stays a 400, not a retryable 503.
		if errors.Is(err, microsoft.ErrNotACampaignID) {
			return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
		}
		return nil, fmt.Errorf("look up microsoft campaign: %w", err)
	}
	if ref == nil {
		return nil, nil
	}
	// Provenance only — no ad group, ad or keyword ids — so ToggleStatus refuses ACTIVATE
	// (microsoftChildIDs is empty) and PAUSE addresses the campaign alone. AccountID is what
	// microsoftCreationAccountID reads back, keeping every later read, toggle and lever on this
	// row held to the account it was verified under.
	return adoptedRef(ref.ID, ref.Name, &microsoft.CampaignResult{
		Platform:     string(model.ProviderMicrosoftAds),
		AccountLabel: res.label,
		AccountID:    accountID,
		CampaignID:   ref.ID,
		CampaignName: ref.Name,
		Steps:        []string{adoptionStep(ref.ID)},
	})
}
