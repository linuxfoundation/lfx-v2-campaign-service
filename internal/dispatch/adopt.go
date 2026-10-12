// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ---------------------------------------------------------------------------
// Campaign adoption (service.CampaignAdopter) — the pieces every adopter shares.
//
// Google Ads was the first adopter (GoogleAdsDispatcher.LookupCampaign) and DEFINES the
// contract; Microsoft, Meta, Reddit and X implement the same capability (LFXV2-2665) in
// microsoft_adopt.go, meta_adopt.go, reddit_adopt.go and twitter_adopt.go. Every adopter follows
// the same order, and the order is the safety argument:
//
//  1. Validate the platform campaign id as an IDENTITY before any connection work. A malformed
//     id is a permanent input fault (400) whatever state the connection is in, so it must mask
//     every contingent fault and cost no decrypt.
//  2. Resolve the PROJECT's own connection only (resolveOwnedForAdoption) — never the LF system
//     fallback — and validate it with the same rules as the platform's toggle path.
//  3. Make ONE read of the campaign by id under that connection.
//  4. Prove provenance: the campaign belongs to the connection's own ad account. A campaign
//     reported under another account is domain.ErrCampaignAccountMismatch (409).
//  5. Record provenance ONLY: the account id the campaign was verified under, never an ad group,
//     ad, ad set or line item. Every platform's toggle refuses ACTIVATE without those child ids
//     (ErrCampaignNotProvisioned), so an adopted campaign can be paused here and is un-paused in
//     the platform's own UI — exactly as on Google.
//
// The read's outcomes map onto the contract the service layer answers from
// (BriefService.AdoptCampaign): (nil, nil) is "the platform answered, no such campaign" (404);
// every error not carrying a sentinel is "could not be verified" (503); the platform's own text
// is never shown to the client.
// ---------------------------------------------------------------------------

// resolveOwnedForAdoption is credsSource.resolveOwned for the adoption path, with absence
// translated into the permanent, actionable refusal the adopt handler answers 409 for.
//
// resolveOwned never consults the LF system scope, so a domain.ErrNotFound here means the
// PROJECT has no connection — the one thing adoption requires. Untranslated it would land in the
// adopt switch's default arm, a 503 "could not be verified" about a platform that was never
// contacted. Every OTHER failure (a repository error, an unusable connection) passes through
// unchanged, because those are different remedies. It mirrors the translation
// GoogleAdsDispatcher.resolveOwnedGoogleAdsClient applies, and has the credsResolver shape so a
// dispatcher that takes a resolver (Meta, Reddit) can be handed it directly.
func (s *credsSource) resolveOwnedForAdoption(ctx context.Context, projectID string, provider model.Provider) (*resolved, error) {
	res, err := s.resolveOwned(ctx, projectID, provider)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: project %s has no %s connection, and adoption cannot fall back to the LF system account: %w",
				domain.ErrAdoptionRequiresOwnConnection, projectID, provider, err)
		}
		return nil, err
	}
	return res, nil
}

// adoptionStep is the Steps line every adopted row's result blob carries, so the row says how it
// came to exist and what it does NOT hold.
func adoptionStep(campaignID string) string {
	return "Campaign adopted: " + campaignID + " (already exists on account; no child entities recorded, so activation is done in the platform's own UI)"
}

// adoptedRef builds the CampaignAdopter answer for a verified campaign: the id and name the
// PLATFORM reported, the provenance blob (result, marshalled), and the platform's single slot.
//
// VariantDefault is not a guess here, unlike on Google: every platform but Google has exactly
// one slot per brief (model.AdoptableVariants), because its objective configures one campaign
// rather than multiplying it.
func adoptedRef(campaignID, name string, result any) (*model.PlatformCampaignRef, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		// Refused rather than bound with an empty Result: an adopted row with no recorded
		// account reads as "unknown, proceed" to every account-mismatch guard, which is the
		// exact provenance gap recording the account exists to close.
		return nil, fmt.Errorf("record the adopted campaign's account scope: %w", err)
	}
	return &model.PlatformCampaignRef{ID: campaignID, Name: name, Result: raw, Variant: model.VariantDefault}, nil
}

// adoptionAccountMismatch is the refusal for a campaign the platform reports under an ad account
// other than the connection's own. Not an absence: the campaign exists, it is simply not this
// project's to bind, and a 404 would invite the operator to create a duplicate of it.
func adoptionAccountMismatch(platform model.Provider, campaignID, reported, connection string) error {
	return fmt.Errorf("adopt %s campaign %s: the platform reports it under ad account %s, not the connection's account %s: %w",
		platform, campaignID, reported, connection, domain.ErrCampaignAccountMismatch)
}

// adoptionAccountIDUnusable is the refusal for a connection whose stored account id cannot be
// addressed. A permanent defect on the row, decided before anything is sent — the 409
// "repair the connection" arm, not a 503 that promises a retry could help.
func adoptionAccountIDUnusable(platform model.Provider, projectID string) error {
	return fmt.Errorf("%w: %w: %s connection for project %s stores an account id that cannot name an ad account",
		domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, platform, projectID)
}
