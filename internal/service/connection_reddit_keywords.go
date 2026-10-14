// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// redditAdsKeywordInsights is the Reddit keyword read's descriptor for the shared connection-state
// arms (classifyInsightsErrorFor): the audience read's remedy text, its own operation name.
var redditAdsKeywordInsights = accountDiscovery{
	provider:        model.ProviderRedditAds,
	displayName:     "reddit ads",
	notUsableRemedy: redditAdsAudienceInsights.notUsableRemedy,
	operation:       "keyword insights",
}

// GetRedditAdsKeywords reads Reddit keyword performance across the project's OWN campaigns — the
// Reddit sibling of GetMicrosoftAdsKeywords, served live like GetRedditAdsAudience and with its
// guards in its order: the reserved system scope is refused (404), the window is validated against
// the same Reddit subset (resolveRedditAudienceWindow), and every failure is classified by
// classifyInsightsErrorFor, whose default arm answers an upstream failure with a fixed 503.
func (s *ConnectionService) GetRedditAdsKeywords(ctx context.Context, p *conn.GetRedditAdsKeywordsPayload) (*conn.RedditAdsKeywords, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveRedditAudienceWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("keyword insights")
	if err != nil {
		return nil, err
	}
	kp, kerr := orch.ReadRedditKeywordPerformance(ctx, p.ProjectID, model.ProviderRedditAds, window)
	if kerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, redditAdsKeywordInsights, kerr)
	}
	rows := make([]*conn.RedditAdsKeyword, 0, len(kp.Rows))
	for _, r := range kp.Rows {
		rows = append(rows, &conn.RedditAdsKeyword{
			CampaignID: r.CampaignID,
			// "" is Reddit's explicit null — no keyword — published with no `text`.
			Text:        optionalString(r.Text),
			Impressions: r.Impressions,
			Clicks:      r.Clicks,
			CostMicros:  r.CostMicros,
			Ctr:         r.Ctr,
		})
	}
	return &conn.RedditAdsKeywords{
		Window:    string(kp.Window),
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: kp.Truncated,
	}, nil
}
