// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LookupCampaign implements service.CampaignAdopter for LinkedIn: it confirms that
// platformCampaignID names a live campaign in the PROJECT'S OWN connected ad account. See
// adopt.go for the order every adopter follows and why.
//
// ONE EXCEPTION (#581): when ctx carries model.AdoptionAccount, the read goes through the LF
// system row scoped to that account — its default account or one of its grants
// (systemAccountResolverForAdoption) — instead of the project's own connection. That is the
// fallback the CampaignAdopter contract forbids for an ordinary caller, admitted here only because
// the service has already required global_marketing_ops and LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED,
// and because the binding index still refuses a campaign any other brief already records. The
// recorded account is the named one, so resolveExisting, the scoped monitor and the levers reach
// the campaign through the same grant afterwards.
//
// The read is GET adAccounts/{acct}/adCampaigns/{id} (linkedin.Client.GetCampaignSettings),
// scoped by its path to the connection's account; the `account` URN it echoes must be present and
// the connection's — an unreported or another account is refused with
// domain.ErrCampaignAccountMismatch. A 404 is absence only
// once the ad account itself is confirmed readable; CANCELED, PENDING_DELETION and REMOVED are
// absences (terminal — nothing can serve), as DELETED/ARCHIVED are on Reddit.
//
// THE ADOPTED ROW'S RULES (LinkedIn has no child ids to be missing, so the row carries an
// explicit `adopted` flag — linkedin.AdoptedCampaignResult):
//
//   - ACTIVATE is refused (ErrCampaignNotProvisioned, 409) — un-pausing is done in Campaign
//     Manager, exactly as on every other platform. PAUSE is allowed.
//   - the BID lever is refused (ErrBidUnwritable, 409): adoption verifies identity and
//     provenance, not the bidding setup this service would be writing into.
//   - the BUDGET lever is allowed, because a LinkedIn budget is the campaign's own field and
//     cannot be shared with another campaign (LinkedInDispatcher.WriteBudget). A campaign under
//     a campaign group's DYNAMIC budget optimization carries no budget of its own, and that
//     writer already refuses a campaign reporting no budget (409) — so the shared group budget
//     is never written through it.
func (d *LinkedInDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	if err := linkedin.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}
	resolver := d.creds.resolveOwnedForAdoption
	if want := model.AdoptionAccount(ctx); want != "" {
		// #581: the caller named an account the LF system row addresses. The service has already
		// required global_marketing_ops and the flag; both refusals here are defense in depth.
		r, rerr := d.creds.systemAccountResolverForAdoption(want)
		if rerr != nil {
			return nil, rerr
		}
		resolver = r
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, resolver)
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
	if settings == nil {
		return nil, nil
	}
	// IDENTITY BEFORE STATE: the echoed account is validated before any status is interpreted,
	// so a foreign (or unreported) campaign is the 409 mismatch even when it reports a terminal
	// status, never an "absent" read off another account's record.
	//
	// `account` is a REQUIRED campaign field, so an unreported one is an unestablished fact and is
	// refused like a foreign one (409) — the bid lever's rule. The path scopes the read, but
	// adoption binds the campaign for good, so the answer's own account must confirm it.
	if settings.AccountID == "" {
		return nil, fmt.Errorf("adopt %s campaign %s: the platform did not report the campaign's ad account, so it cannot be established that it is the connection's account %s: %w",
			platform, settings.CampaignID, accountID, domain.ErrCampaignAccountMismatch)
	}
	if settings.AccountID != accountID {
		return nil, adoptionAccountMismatch(platform, settings.CampaignID, settings.AccountID, accountID)
	}
	if settings.Terminal() {
		return nil, nil
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
// It reads the one marker model.CampaignAdopted reads, so the activate and bid refusals here
// and the public `adopted` flag cannot disagree. An unreadable blob reads as not adopted: every
// path that consults this has its own provenance guard, which an unreadable blob already fails.
func linkedInAdopted(campaign *model.Campaign) bool {
	return model.CampaignAdopted(campaign)
}

// errLinkedInAdoptedActivate is the toggle's ACTIVATE refusal for an adopted row.
func errLinkedInAdoptedActivate(campaign *model.Campaign) error {
	return fmt.Errorf("%w: linkedin campaign %s cannot be activated here because it was ADOPTED, which records no serving resources and leaves un-pausing to LinkedIn Campaign Manager",
		domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
}

// systemAccountResolverForAdoption returns the resolver an adoption naming accountID uses (#581):
// the LF system row, read directly (resolveForcedSystem: whatever connection the project has, and
// whether or not it disconnected one, because the caller asked for the system row's account by
// name), scoped to accountID when that is the row's default account or one of its grants
// (existingOnSystem, the same test the levers apply, so whatever can be adopted can be paused).
// Every other account is ErrAccountNotManagedByConnection with nothing sent. So is any account at
// all while LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED is off.
func (s *credsSource) systemAccountResolverForAdoption(accountID string) (credsResolver, error) {
	if !linkedinAccountSelectionEnabled() {
		return nil, fmt.Errorf("%w: adopting from linkedin account %s through the LF system account is not enabled on this deployment (%s is not \"true\")",
			domain.ErrAccountNotManagedByConnection, domain.CallerValue(accountID), constants.EnvLinkedInAdsAccountSelectionEnabled)
	}
	if !linkedin.ValidAccountID(accountID) {
		return nil, fmt.Errorf("%w: %s is not a linkedin ad account id", domain.ErrAccountNotManagedByConnection, domain.CallerValue(accountID))
	}
	return func(ctx context.Context, _ string, provider model.Provider) (*resolved, error) {
		sys, err := s.resolveForcedSystem(ctx, provider)
		if err != nil {
			return nil, err
		}
		if onSys, ok := existingOnSystem(sys, accountID); ok {
			return onSys, nil
		}
		return nil, fmt.Errorf("%w: linkedin account %s is neither the LF system account's default account nor one of its grants",
			domain.ErrAccountNotManagedByConnection, domain.CallerValue(accountID))
	}, nil
}

// ListAdoptableCampaigns implements service.AdoptableCampaignLister for LinkedIn (#581): the ACTIVE
// and PAUSED campaigns in accountID, read through the LF system row scoped to that account, as
// adopt-campaign with account_id would read them. One paged campaign-list walk, no analytics.
func (d *LinkedInDispatcher) ListAdoptableCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AdoptableCampaign, error) {
	resolver, err := d.creds.systemAccountResolverForAdoption(accountID)
	if err != nil {
		return nil, err
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, resolver)
	if err != nil {
		return nil, err
	}
	scoped := strings.TrimSpace(res.accountID)
	if !linkedin.ValidAccountID(scoped) {
		return nil, linkedinAccountIDUnusable(res, projectID, "linkedin adoptable campaign list")
	}
	rows, err := d.cachedLinkedInClient(projectID, platform, res, creds, scoped).ListAccountCampaignRefs(ctx, scoped)
	if err != nil {
		return nil, linkedInLookupError("list linkedin adoptable campaigns", res, err)
	}
	out := make([]model.AdoptableCampaign, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.AdoptableCampaign{ID: r.CampaignID, Name: r.Name, Status: r.Status})
	}
	return out, nil
}
