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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.MetaAdStatusToggler = (*MetaDispatcher)(nil)

// ToggleMetaAdStatus implements service.MetaAdStatusToggler: pause or resume ONE ad of one ad set
// of the campaign (#550, gap 14). It mirrors ToggleMetaAdSetStatus step for step.
//
// ORDER, every step before the write:
//
//  1. ad_set_id, ad_id and status are validated (400) — no connection work.
//  2. Provenance (metaAdSetScope): unknown provenance or an account mismatch is 409 with zero
//     requests.
//  3. ACTIVE on a row that records no ad set (ADOPTED, or never fully provisioned) is
//     ErrCampaignNotProvisioned (409), and ACTIVE under any ad set but the recorded one is
//     ErrMetaAdNotRecorded (409), both with zero requests: the campaign cascade activates only the
//     recorded ad set's ads, and this lever activates no more. PAUSED is allowed on any of the
//     campaign's ads.
//  4. The ad is READ (GET /{ad}?fields=id,adset_id,campaign_id,account_id,status) and must report
//     ad_set_id, this row's platform campaign and the connection's account (ErrMetaAdNotInAdSet,
//     409). DELETED or ARCHIVED is ErrMetaAdUnwritable (409); any other status that is not
//     ACTIVE/PAUSED is unverifiable (503). A read failure is unverifiable (503); nothing written.
//  5. Already at the requested status: ALREADY_IN_STATE, and NOTHING is sent.
//  6. beforeWrite, when non-nil, runs ONCE, here — after every guard above and only when a write
//     is about to be sent; if it fails nothing is sent.
//  7. ONE POST /{ad} {status}, never repeated (meta.Client.UpdateAdStatusOnce), classified by
//     meta.ClassifyAdSetWrite exactly as the ad-set write is.
func (d *MetaDispatcher) ToggleMetaAdStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, adID, status string, beforeWrite service.MetaAdSetBeforeWrite) (*model.MetaAdStatusResult, error) {
	const op = "toggle meta ad status"
	if err := meta.ValidateAdSetID(adSetID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetaAdSetInvalid, err))
	}
	if err := meta.ValidateAdID(adID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetaAdInvalid, err))
	}
	var metaStatus string
	switch status {
	case model.MetaAdSetStatusActive:
		metaStatus = meta.StatusActive
	case model.MetaAdSetStatusPaused:
		metaStatus = meta.StatusPaused
	default:
		return nil, fmt.Errorf("%s: unsupported ad status %q", op, status)
	}
	if campaign == nil || meta.ValidateCampaignID(campaign.PlatformCampaignID) != nil {
		return nil, fmt.Errorf("%s: the campaign's stored platform id is not a canonical Meta campaign id: %w", op, domain.ErrStoredPlatformIDInvalid)
	}
	client, accountID, err := d.metaAdSetScope(ctx, op, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	if metaStatus == meta.StatusActive && metaAdSetID(campaign) == "" {
		return nil, fmt.Errorf("%w: %s: meta campaign %s records no ad set of its own — it was ADOPTED, which does not verify targeting, or never fully provisioned — so this service will not activate anything under it; activate in Meta Ads Manager",
			domain.ErrCampaignNotProvisioned, op, campaign.PlatformCampaignID)
	}
	if metaStatus == meta.StatusActive && adSetID != metaAdSetID(campaign) {
		return nil, fmt.Errorf("%s: ad set %s is not the ad set this service created for campaign %s, so its ads are not activated here: %w",
			op, adSetID, campaign.PlatformCampaignID, domain.ErrMetaAdNotRecorded)
	}

	state, err := client.GetAdState(ctx, adID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if state.AdSetID != adSetID || state.CampaignID != campaign.PlatformCampaignID {
		return nil, fmt.Errorf("%s: ad %s is reported under ad set %s of campaign %s, not ad set %s of %s: %w",
			op, adID, state.AdSetID, state.CampaignID, adSetID, campaign.PlatformCampaignID, domain.ErrMetaAdNotInAdSet)
	}
	if normalizeMetaAccountID(state.AccountID) != normalizeMetaAccountID(accountID) {
		return nil, fmt.Errorf("%s: ad %s is reported under ad account %s, not the connection's %s: %w",
			op, adID, state.AccountID, accountID, domain.ErrMetaAdNotInAdSet)
	}
	switch state.Status {
	case meta.StatusActive, meta.StatusPaused:
	case meta.StatusDeleted, meta.StatusArchived:
		return nil, fmt.Errorf("%s: ad %s is %s: %w", op, adID, state.Status, domain.ErrMetaAdUnwritable)
	default:
		return nil, fmt.Errorf("%s: ad %s reports a status this service does not recognise (%d bytes); nothing was changed", op, adID, len(state.Status))
	}
	if state.Status == metaStatus {
		return &model.MetaAdStatusResult{AdID: adID, PreviousStatus: state.Status, Outcome: model.MetaAdSetAlreadyInState}, nil
	}

	if beforeWrite != nil {
		if herr := beforeWrite(ctx); herr != nil {
			return nil, fmt.Errorf("%s: recording the change before the write failed, so nothing was sent: %w", op, herr)
		}
	}
	werr := client.UpdateAdStatusOnce(ctx, adID, metaStatus)
	switch meta.ClassifyAdSetWrite(werr) {
	case meta.AdSetWriteApplied:
		return &model.MetaAdStatusResult{AdID: adID, PreviousStatus: state.Status, Outcome: model.MetaAdSetApplied}, nil
	case meta.AdSetWriteUnconfirmed:
		return nil, &unconfirmedToggleError{err: werr}
	case meta.AdSetWriteNotSent:
		return nil, fmt.Errorf("%s: the write was never sent, so nothing was changed: %w", op, werr)
	default:
		return nil, fmt.Errorf("%s: Meta refused the write, so nothing was changed: %w", op, werr)
	}
}

// verifyMetaHeldAdsPaused reads every ad the row holds as deliberately paused (#550, gap 14)
// before a campaign ACTIVATE that leaves them paused, exactly as verifyMetaHeldAdSetPaused reads
// the held ad set: the record is the row's, not Meta's, so an ad Meta reports ACTIVE (its pause
// never applied, or it was re-activated elsewhere) would serve the moment the campaign is
// activated. Each ad's CONFIGURED status decides:
//
//   - PAUSED, DELETED or ARCHIVED → it may be held;
//   - ACTIVE → domain.ErrMetaHeldAdActive (409), nothing written;
//   - a failed read, another ad set / campaign / account, or an unrecognised status → an ordinary
//     error (503, the campaign not modified).
//
// An ad the record names but Meta no longer lists under the ad set is read like any other: if it
// moved, the identity check refuses rather than guesses.
func verifyMetaHeldAdsPaused(ctx context.Context, client *meta.Client, campaign *model.Campaign, adSetID, accountID string, adIDs []string) error {
	const op = "verify the held meta ads before activating their campaign"
	if len(adIDs) == 0 {
		return nil
	}
	if normalizeMetaAccountID(accountID) == "" {
		return fmt.Errorf("%s: neither the connection nor the campaign records an ad account to check the held ads against; nothing was changed", op)
	}
	for _, adID := range adIDs {
		state, err := client.GetAdState(ctx, adID)
		if err != nil {
			return fmt.Errorf("%s: %v (nothing was changed)", op, err)
		}
		if state.AdSetID != adSetID || state.CampaignID != campaign.PlatformCampaignID || normalizeMetaAccountID(state.AccountID) != normalizeMetaAccountID(accountID) {
			return fmt.Errorf("%s: ad %s is reported under ad set %s / campaign %s / account %s, not %s / %s / %s; nothing was changed",
				op, adID, state.AdSetID, state.CampaignID, state.AccountID, adSetID, campaign.PlatformCampaignID, accountID)
		}
		switch state.Status {
		case meta.StatusPaused, meta.StatusDeleted, meta.StatusArchived:
		case meta.StatusActive:
			return fmt.Errorf("%s: ad %s is ACTIVE on Meta although this service holds it as deliberately paused: %w", op, adID, domain.ErrMetaHeldAdActive)
		default:
			return fmt.Errorf("%s: ad %s reports a status this service does not recognise (%d bytes); nothing was changed", op, adID, len(state.Status))
		}
	}
	return nil
}
