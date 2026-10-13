// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// LookupCampaign implements service.CampaignAdopter for LinkedIn: it confirms that
// platformCampaignID names a live campaign in the PROJECT'S OWN connected ad account. See
// adopt.go for the order every adopter follows and why.
//
// The read is GET adAccounts/{acct}/adCampaigns/{id} (linkedin.Client.GetCampaignSettings),
// scoped by its path to the connection's account; the `account` URN it echoes is compared too,
// and another account is refused with domain.ErrCampaignAccountMismatch. A 404 is absence only
// once the ad account itself is confirmed readable; CANCELED, PENDING_DELETION and REMOVED are
// absences (terminal — nothing can serve), as DELETED/ARCHIVED are on Reddit.
//
// THE ADOPTED ROW'S RULES (LinkedIn has no child ids to be missing, so the row carries an
// explicit `adopted` flag — linkedin.AdoptedCampaignResult — for every lever to consult):
//
//   - ACTIVATE is refused (ErrCampaignNotProvisioned, 409) — un-pausing is done in Campaign
//     Manager, exactly as on every other platform. PAUSE is allowed.
//   - the BUDGET lever is allowed, because a LinkedIn budget is the campaign's own field and
//     cannot be shared with another campaign (LinkedInDispatcher.WriteBudget). A campaign under
//     a campaign group's DYNAMIC budget optimization carries no budget of its own, and that
//     writer already refuses a campaign reporting no budget (409) — so the shared group budget
//     is never written through it.
func (d *LinkedInDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := linkedin.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.resolveOwnedForAdoption)
	if err != nil {
		return nil, err
	}
	accountID := strings.TrimSpace(res.accountID)
	if !linkedin.ValidAccountID(accountID) {
		return nil, adoptionAccountIDUnusable(platform, projectID)
	}
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)

	settings, err := client.GetCampaignSettings(ctx, platformCampaignID)
	if err != nil {
		if linkedinConnectionDefect(err) {
			return nil, linkedinExpiry(fmt.Errorf("look up linkedin campaign: %w", err))
		}
		return nil, fmt.Errorf("look up linkedin campaign: %w", err)
	}
	if settings == nil || settings.Terminal() {
		return nil, nil
	}
	if settings.AccountID != "" && settings.AccountID != accountID {
		return nil, adoptionAccountMismatch(platform, settings.CampaignID, settings.AccountID, accountID)
	}
	if settings.Name == nil || strings.TrimSpace(*settings.Name) == "" {
		return nil, fmt.Errorf("look up linkedin campaign %s: the campaign was returned without a usable name", platformCampaignID)
	}
	if settings.Status == nil {
		return nil, fmt.Errorf("look up linkedin campaign %s: the campaign was returned without a status, so whether it is live cannot be established", platformCampaignID)
	}
	return adoptedRef(settings.CampaignID, *settings.Name,
		linkedin.AdoptedCampaignResult(accountID, settings.CampaignID, *settings.Name, adoptionStep(settings.CampaignID)))
}

// linkedInAdopted reports whether the row was bound by adoption (linkedin.CampaignResult.Adopted).
// An unreadable blob reads as not adopted: every path that consults this has its own provenance
// guard, which an unreadable blob already fails.
func linkedInAdopted(campaign *model.Campaign) bool {
	if campaign == nil || len(campaign.Result) == 0 {
		return false
	}
	var blob struct {
		Adopted bool `json:"adopted"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return false
	}
	return blob.Adopted
}

// errLinkedInAdoptedActivate is the toggle's ACTIVATE refusal for an adopted row.
func errLinkedInAdoptedActivate(campaign *model.Campaign) error {
	return fmt.Errorf("%w: linkedin campaign %s cannot be activated here because it was ADOPTED, which records no serving resources and leaves un-pausing to LinkedIn Campaign Manager",
		domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
}
