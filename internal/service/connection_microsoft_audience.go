// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// microsoftAdsAudienceInsights is the Microsoft audience read's descriptor for the shared
// connection-state arms (classifyInsightsErrorFor): microsoftAdsKeywordInsights' remedy, with
// the operation named for this read.
var microsoftAdsAudienceInsights = accountDiscovery{
	provider:        model.ProviderMicrosoftAds,
	displayName:     microsoftAdsKeywordInsights.displayName,
	notUsableRemedy: microsoftAdsKeywordInsights.notUsableRemedy,
	operation:       "audience insights",
}

// GetMicrosoftAdsAudience reads Microsoft Advertising age/gender audience buckets across the
// project's OWN campaigns, served from the last finished saved report
// (Orchestrator.ReadReportedAudience). Same guards, in the same order, as GetMicrosoftAdsKeywords:
// the reserved system scope is refused, the window is checked against the Microsoft subset
// (resolveMicrosoftKeywordWindow — the same five windows and the same 400 text), and every failure
// is classified by classifyInsightsErrorFor, so a gated-off read answers the keyword read's exact
// 400 and anything unverifiable a 503 with no upstream text.
func (s *ConnectionService) GetMicrosoftAdsAudience(ctx context.Context, p *conn.GetMicrosoftAdsAudiencePayload) (*conn.MicrosoftAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveMicrosoftKeywordWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(microsoftAdsAudienceInsights.label())
	if err != nil {
		return nil, err
	}
	read, rerr := orch.ReadReportedAudience(ctx, p.ProjectID, model.ProviderMicrosoftAds, window)
	if rerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, microsoftAdsAudienceInsights, rerr)
	}
	buckets := make([]*conn.MicrosoftAdsAudienceBucket, 0, len(read.Buckets))
	for _, b := range read.Buckets {
		buckets = append(buckets, &conn.MicrosoftAdsAudienceBucket{
			AgeGroup:    b.AgeGroup,
			Gender:      b.Gender,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	out := &conn.MicrosoftAdsAudience{
		Window:         string(read.Window),
		Buckets:        buckets,
		BucketCount:    len(buckets),
		MetricsPending: read.MetricsPending,
		DataIncomplete: read.DataIncomplete,
	}
	if read.MetricsAsOf != nil {
		asOf := read.MetricsAsOf.UTC().Format(time.RFC3339)
		out.MetricsAsOf = &asOf
	}
	return out, nil
}
