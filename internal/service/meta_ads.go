// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetaAdReader is an OPTIONAL dispatcher capability (decision D7 first step, Meta only): read the
// ads under ONE ad set of a campaign with their status, creative and delivery counters.
// Type-asserted, so a dispatcher without it yields ErrMetaAdSetsUnsupported → 400. A pure read: no
// upstream mutation, nothing written to the row.
//
// Contract: the ad set must be proven to be the campaign's under the connection's ad account
// (domain.ErrMetaAdSetNotInCampaign otherwise) before its ads are read.
type MetaAdReader interface {
	ReadMetaAds(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID string, window model.MetricsWindow) (*model.MetaAds, error)
}

// ReadMetaAds reads one ad set's ads through its dispatcher's MetaAdReader. The capability is
// checked FIRST, so a non-Meta row is 400 whatever its provisioning state; a row with no platform
// campaign id is then refused without contacting anything.
func (o *Orchestrator) ReadMetaAds(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, adSetID string, window model.MetricsWindow) (*model.MetaAds, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	reader, ok := d.(MetaAdReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrMetaAdSetsUnsupported, platform)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, ErrCampaignNotProvisioned
	}
	callCtx, cancel := context.WithTimeout(ctx, metricsCallTimeout)
	defer cancel()
	start := time.Now()
	out, rerr := reader.ReadMetaAds(callCtx, projectID, platform, campaign, adSetID, window)
	o.recordUpstream(ctx, platform, opReadMetaAds, start, rerr)
	if rerr != nil {
		return nil, rerr
	}
	if out == nil {
		return nil, fmt.Errorf("%s ad reader returned a nil result with no error", platform)
	}
	if out.Ads == nil {
		out.Ads = []model.MetaAd{}
	}
	return out, nil
}

// ListMetaAds reads the ads under one ad set of a Meta campaign live (list-meta-ads). A pure read;
// nothing is persisted, so there is no If-Match and no ETag. It mirrors ListMetaAdSets: the
// (project, brief, campaign) triple is the ownership guard and the only 404 for the campaign.
func (s *BriefService) ListMetaAds(ctx context.Context, p *briefs.ListMetaAdsPayload) (*briefs.MetaAds, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	window := model.MetricsWindowLast30Days
	if p.Window != nil {
		window = model.MetricsWindow(*p.Window)
		if !model.IsValidMetricsWindow(window) {
			return nil, &briefs.BadRequestError{Code: "400", Message: "window must be one of: today, yesterday, last_7_days, last_14_days, last_30_days, this_month, last_month"}
		}
	}
	// Repeated for a direct (non-generated) caller; Goa enforces it over HTTP. Checked before ANY
	// connection work, so a permanent input fault is always 400.
	if !metaAdSetIDOK(p.AdSetID) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "ad_set_id must be a Meta ad set id: 1-32 digits without a leading zero"}
	}
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	rb, rerr := orch.ReadMetaAds(ctx, p.ProjectID, existing.Platform, existing, p.AdSetID, window)
	if rerr != nil {
		return nil, s.classifyMetaAdSetError(ctx, "read_ads", p.ProjectID, p.BriefID, p.CampaignID, existing.Platform, rerr)
	}
	out := &briefs.MetaAds{
		CampaignID:         p.CampaignID,
		PlatformCampaignID: rb.PlatformCampaignID,
		AdSetID:            rb.AdSetID,
		Window:             string(window),
		Currency:           rb.Currency,
		ReadAt:             rb.ReadAt.UTC().Format(time.RFC3339),
		Truncated:          rb.Truncated,
		Ads:                make([]*briefs.MetaAd, 0, len(rb.Ads)),
	}
	for _, a := range rb.Ads {
		out.Ads = append(out.Ads, &briefs.MetaAd{
			ID:              a.ID,
			Listed:          a.Listed,
			Name:            a.Name,
			Status:          a.Status,
			EffectiveStatus: a.EffectiveStatus,
			CreativeID:      a.CreativeID,
			CreativeName:    a.CreativeName,
			Impressions:     a.Impressions,
			Clicks:          a.Clicks,
			CostMicros:      a.CostMicros,
			Ctr:             a.Ctr,
		})
	}
	out.AdCount = len(out.Ads)
	return out, nil
}
