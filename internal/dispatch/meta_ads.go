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

var _ service.MetaAdReader = (*MetaDispatcher)(nil)

// ReadMetaAds implements service.MetaAdReader: the ads under ONE ad set of the campaign, with
// their status, creative and delivery over window (decision D7, first step).
//
// STRICTLY READ-ONLY: GETs only, nothing written upstream or to the row. It mirrors ReadMetaAdSets
// and the ad-set toggle's ownership proof:
//
//  1. window and ad_set_id are validated (400) and a non-canonical stored campaign id is
//     ErrStoredPlatformIDInvalid (409) — all before any credential work.
//  2. Provenance (metaAdSetScope): unknown provenance or an account mismatch is 409 with zero
//     requests.
//  3. The ad set is READ (GET /{id}?fields=id,campaign_id,account_id,status) and must report this
//     row's platform campaign under the connection's account (ErrMetaAdSetNotInCampaign, 409),
//     so an empty answer can never be given for another campaign's ad set. A read failure —
//     Graph 100/33 included — is unverifiable (503), never absence.
//  4. meta.Client.ListAdSetAds, which proves every listed ad's adset_id, campaign_id and
//     account_id and every Insights row's ad set and campaign. An ad under another account is
//     ErrCampaignUpstreamIdentityMismatch (409).
func (d *MetaDispatcher) ReadMetaAds(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID string, window model.MetricsWindow) (*model.MetaAds, error) {
	const op = "read meta ads"
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	if err := meta.ValidateAdSetID(adSetID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetaAdSetInvalid, err))
	}
	if campaign == nil || meta.ValidateCampaignID(campaign.PlatformCampaignID) != nil {
		return nil, fmt.Errorf("%s: the campaign's stored platform id is not a canonical Meta campaign id: %w", op, domain.ErrStoredPlatformIDInvalid)
	}
	client, accountID, err := d.metaAdSetScope(ctx, op, projectID, platform, campaign)
	if err != nil {
		return nil, err
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
	ads, err := client.ListAdSetAds(ctx, adSetID, campaign.PlatformCampaignID, accountID, metaWindow)
	if err != nil {
		if errors.Is(err, meta.ErrAdAccountMismatch) {
			return nil, fmt.Errorf("%s: ad set %s: Meta reports an ad under a different ad account than the one the campaign was created under: %w",
				op, adSetID, errors.Join(err, domain.ErrCampaignUpstreamIdentityMismatch))
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	out := &model.MetaAds{
		CampaignID:         campaign.ID,
		PlatformCampaignID: ads.CampaignID,
		AdSetID:            ads.AdSetID,
		Window:             window,
		Currency:           ads.Currency,
		ReadAt:             settingsNow(d.settingsNow),
		Truncated:          ads.Truncated,
		Ads:                make([]model.MetaAd, 0, len(ads.Ads)),
	}
	for _, a := range ads.Ads {
		out.Ads = append(out.Ads, model.MetaAd{
			ID: a.ID, Listed: a.Listed, Name: a.Name, Status: a.Status, EffectiveStatus: a.EffectiveStatus,
			CreativeID: a.CreativeID, CreativeName: a.CreativeName,
			Impressions: a.Impressions, Clicks: a.Clicks, CostMicros: a.CostMicros, Ctr: a.Ctr,
		})
	}
	return out, nil
}
