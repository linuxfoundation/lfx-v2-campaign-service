// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// metaAdsAudienceInsights is the Meta audience read's descriptor for the shared connection-state
// arms (classifyInsightsErrorFor); remedy text copied verbatim from metaAdsMonitorDiscovery, and
// it adds the account selection Insights needs (an account-less connection is 400, not 503).
var metaAdsAudienceInsights = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with access_token set, and that an act_<digits> account_id is selected",
	operation: "audience insights",
}

// GetMetaAdsAudience reads Meta audience breakdowns (age+gender, placement) across the project's
// OWN campaigns — the Meta sibling of GetGoogleAdsAudience, with the same guards in the same
// order: the reserved system scope is refused (404), the window is validated locally (Meta's
// date_preset map serves all seven), and every failure is classified by classifyInsightsErrorFor.
//
// Meta's age and gender arrive as one combined breakdown, so the flat array carries two
// dimensions; each independently covers the same traffic, so counters total within a dimension,
// never across them.
func (s *ConnectionService) GetMetaAdsAudience(ctx context.Context, p *conn.GetMetaAdsAudiencePayload) (*conn.MetaAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveInsightsWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("audience insights")
	if err != nil {
		return nil, err
	}
	ai, aerr := orch.ReadMetaAudienceInsights(ctx, p.ProjectID, model.ProviderMetaAds, window)
	if aerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, metaAdsAudienceInsights, aerr)
	}
	buckets := make([]*conn.MetaAdsAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, &conn.MetaAdsAudienceBucket{
			Dimension:         b.Dimension,
			Age:               optionalString(b.Age),
			Gender:            optionalString(b.Gender),
			PublisherPlatform: optionalString(b.PublisherPlatform),
			PlatformPosition:  optionalString(b.PlatformPosition),
			Impressions:       b.Impressions,
			Clicks:            b.Clicks,
			CostMicros:        b.CostMicros,
			Ctr:               b.Ctr,
		})
	}
	return &conn.MetaAdsAudience{
		Window:          string(ai.Window),
		Buckets:         buckets,
		BucketCount:     len(buckets),
		AccountCurrency: optionalString(ai.Currency),
	}, nil
}
