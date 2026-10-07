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

var (
	_ service.MetaAdSetReader        = (*MetaDispatcher)(nil)
	_ service.MetaAdSetStatusToggler = (*MetaDispatcher)(nil)
)

// metaAdSetScope runs the provenance rules the settings readback runs, BEFORE any platform call:
// a row that does not record its creating account is refused (unknown provenance, 409) before a
// credential is even resolved; the connection must name an ad account; and the recorded account
// must be the connection's (409). It returns the resolved client and the account id.
func (d *MetaDispatcher) metaAdSetScope(ctx context.Context, op, projectID string, platform model.Provider, campaign *model.Campaign) (*meta.Client, string, error) {
	created := metaCreationAccountID(campaign)
	if created == "" {
		return nil, "", fmt.Errorf("%s: campaign %s does not record which ad account it was created under, so its ad sets cannot be resolved against any account: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return nil, "", err
	}
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", op, err)
	}
	if verr := meta.ValidateAccountID(accountID); verr != nil {
		return nil, "", res.systemScoped(fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr))
	}
	if err := verifyMetaAccountMatch(op, campaign, accountID); err != nil {
		return nil, "", err
	}
	return d.cachedMetaClient(projectID, platform, res, creds), accountID, nil
}

// ReadMetaAdSets implements service.MetaAdSetReader: the campaign's ad sets with their status,
// own budget and delivery over window (meta.Client.ListCampaignAdSets).
//
// STRICTLY READ-ONLY: three kinds of GET, nothing written upstream or to the row.
//
// Provenance is the settings readback's (metaAdSetScope). The /adsets edge is not account-scoped,
// so every listed ad set's account_id is ALSO compared with the connection's; one reported under
// another account is ErrCampaignUpstreamIdentityMismatch (409) — the connection already IS the
// recorded account, so the remedy is re-dispatch, not reconnect.
//
// ABSENCE IS NEVER REPORTED: Graph 100/33 on the campaign is unverifiable (503), as on the
// settings readback and the adoption read.
//
// Budgets are rendered from minor units with the account currency's own offset, by the same
// function the settings readback uses; an unmapped currency leaves the amount absent.
func (d *MetaDispatcher) ReadMetaAdSets(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.MetaAdSets, error) {
	const op = "read meta ad sets"
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	if campaign == nil || meta.ValidateCampaignID(campaign.PlatformCampaignID) != nil {
		// A stored id that is not a canonical Meta id cannot be addressed; refused before any
		// credential work, with its OWN sentinel — Meta was never asked, so nothing may be said
		// about what Meta reports. (An empty one never reaches here — the orchestrator refuses it.)
		return nil, fmt.Errorf("%s: the campaign's stored platform id is not a canonical Meta campaign id: %w", op, domain.ErrStoredPlatformIDInvalid)
	}
	client, accountID, err := d.metaAdSetScope(ctx, op, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	sets, err := client.ListCampaignAdSets(ctx, campaign.PlatformCampaignID, accountID, metaWindow)
	if err != nil {
		if errors.Is(err, meta.ErrAdSetAccountMismatch) {
			return nil, fmt.Errorf("%s: campaign %s: Meta reports an ad set under a different ad account than the one it was created under: %w",
				op, campaign.PlatformCampaignID, errors.Join(err, domain.ErrCampaignUpstreamIdentityMismatch))
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	recorded := metaAdSetID(campaign)
	out := &model.MetaAdSets{
		CampaignID:         campaign.ID,
		PlatformCampaignID: sets.CampaignID,
		Window:             window,
		Currency:           sets.Currency,
		ReadAt:             settingsNow(d.settingsNow),
		AdSets:             make([]model.MetaAdSet, 0, len(sets.AdSets)),
	}
	for _, as := range sets.AdSets {
		m := model.MetaAdSet{
			ID: as.ID, Listed: as.Listed, Name: as.Name, Status: as.Status,
			EffectiveStatus: as.EffectiveStatus, BidStrategy: as.BidStrategy,
			Impressions: as.Impressions, Clicks: as.Clicks, CostMicros: as.CostMicros, Ctr: as.Ctr,
			Recorded: recorded != "" && as.ID == recorded,
		}
		var minor *int64
		switch {
		case as.DailyMinor != nil:
			bt := model.BudgetDaily
			m.BudgetType, minor = &bt, as.DailyMinor
		case as.LifetimeMinor != nil:
			bt := model.BudgetLifetime
			m.BudgetType, minor = &bt, as.LifetimeMinor
		}
		if minor != nil && sets.CurrencyKnown {
			m.BudgetAmount = strPtr(formatMinorUnits(*minor, sets.CurrencyOffset))
		}
		out.AdSets = append(out.AdSets, m)
	}
	return out, nil
}

// ToggleMetaAdSetStatus implements service.MetaAdSetStatusToggler: pause or resume ONE ad set.
//
// ORDER, every step before the write:
//
//  1. ad_set_id and status are validated (400) — no connection work.
//  2. Provenance (metaAdSetScope): unknown provenance or an account mismatch is 409 with zero
//     requests.
//  3. ACTIVE on a row that records no ad set — an ADOPTED campaign, or one never fully
//     provisioned — is refused (ErrCampaignNotProvisioned, 409) with zero requests, exactly as the
//     campaign toggle refuses it: this service has not verified that anything under the campaign
//     can serve. ACTIVE on any ad set other than the recorded one is refused too
//     (ErrMetaAdSetNotRecorded, 409, zero requests). PAUSED is allowed on any of its ad sets.
//  4. The ad set is READ (GET /{id}?fields=id,campaign_id,account_id,status) and must report this
//     row's platform campaign under the connection's account (ErrMetaAdSetNotInCampaign, 409). A
//     DELETED or ARCHIVED ad set is ErrMetaAdSetUnwritable (409); any other status that is not
//     ACTIVE/PAUSED is unverifiable (503). A read failure — Graph 100/33 included — is
//     unverifiable (503); nothing has been written.
//  5. Already at the requested status: ALREADY_IN_STATE, and NOTHING is sent.
//  6. ONE POST /{id} {status}, never repeated (meta.Client.UpdateAdSetStatusOnce), classified by
//     meta.ClassifyAdSetWrite: APPLIED → nil error; UNCONFIRMED → unconfirmedToggleError; NOT_SENT
//     and REJECTED → an ordinary error (nothing changed).
func (d *MetaDispatcher) ToggleMetaAdSetStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID, status string) (*model.MetaAdSetStatusResult, error) {
	const op = "toggle meta ad set status"
	if err := meta.ValidateAdSetID(adSetID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetaAdSetInvalid, err))
	}
	var metaStatus string
	switch status {
	case model.MetaAdSetStatusActive:
		metaStatus = meta.StatusActive
	case model.MetaAdSetStatusPaused:
		metaStatus = meta.StatusPaused
	default:
		return nil, fmt.Errorf("%s: unsupported ad set status %q", op, status)
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
	// ACTIVATE only the ad set this service created: one added by hand in Ads Manager has
	// targeting this service never verified. Decided before any request. PAUSE is unrestricted.
	if metaStatus == meta.StatusActive && adSetID != metaAdSetID(campaign) {
		return nil, fmt.Errorf("%s: ad set %s is not the ad set this service created for campaign %s: %w",
			op, adSetID, campaign.PlatformCampaignID, domain.ErrMetaAdSetNotRecorded)
	}

	state, err := client.GetAdSetState(ctx, adSetID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if state.CampaignID != campaign.PlatformCampaignID {
		return nil, fmt.Errorf("%s: ad set %s is reported under campaign %s, not %s: %w",
			op, adSetID, state.CampaignID, campaign.PlatformCampaignID, domain.ErrMetaAdSetNotInCampaign)
	}
	if normalizeMetaAccountID(state.AccountID) != normalizeMetaAccountID(accountID) {
		return nil, fmt.Errorf("%s: ad set %s is reported under ad account %s, not the connection's %s: %w",
			op, adSetID, state.AccountID, accountID, domain.ErrMetaAdSetNotInCampaign)
	}
	switch state.Status {
	case meta.StatusActive, meta.StatusPaused:
	case meta.StatusDeleted, meta.StatusArchived:
		return nil, fmt.Errorf("%s: ad set %s is %s: %w", op, adSetID, state.Status, domain.ErrMetaAdSetUnwritable)
	default:
		return nil, fmt.Errorf("%s: ad set %s reports a status this service does not recognise (%d bytes); nothing was changed", op, adSetID, len(state.Status))
	}
	if state.Status == metaStatus {
		return &model.MetaAdSetStatusResult{AdSetID: adSetID, Outcome: model.MetaAdSetAlreadyInState, PreviousStatus: state.Status}, nil
	}

	werr := client.UpdateAdSetStatusOnce(ctx, adSetID, metaStatus)
	switch meta.ClassifyAdSetWrite(werr) {
	case meta.AdSetWriteApplied:
		return &model.MetaAdSetStatusResult{AdSetID: adSetID, Outcome: model.MetaAdSetApplied, PreviousStatus: state.Status}, nil
	case meta.AdSetWriteUnconfirmed:
		return nil, &unconfirmedToggleError{err: werr}
	case meta.AdSetWriteNotSent:
		return nil, fmt.Errorf("%s: the write was never sent, so nothing was changed: %w", op, werr)
	default:
		return nil, fmt.Errorf("%s: Meta refused the write, so nothing was changed: %w", op, werr)
	}
}
