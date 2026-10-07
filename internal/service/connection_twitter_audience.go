// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// twitterAdsAudienceInsights is the X audience read's descriptor for the shared connection-state
// arms (classifyInsightsErrorFor); remedy text from twitterAdsMonitorDiscovery, plus the account
// selection the stats endpoints need.
var twitterAdsAudienceInsights = accountDiscovery{
	provider:    model.ProviderTwitterAds,
	displayName: "x/twitter ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with consumer_key, consumer_secret, access_token and access_token_secret set, and that " +
		"an account_id is selected",
	operation: "audience insights",
}

// twitterAudienceWindows mirrors design/brief.go's twitterAudienceWindowEnum: the windows the X
// metrics read serves (dispatch.twitterMetricsWindow). Enforced here as well as by the generated
// decoder, for resolveInsightsWindow's reason (a non-HTTP caller bypasses the decoder).
var twitterAudienceWindows = map[model.MetricsWindow]bool{
	model.MetricsWindowToday:     true,
	model.MetricsWindowYesterday: true,
	model.MetricsWindowLast7Days: true,
}

// twitterAudienceWindowMessage is the fixed 400 text for a window the X reads cannot serve.
const twitterAudienceWindowMessage = "window must be one of: last_7_days, today, yesterday (X Ads reads cover at most 7 days)"

// resolveTwitterAudienceWindow maps the optional window onto the X subset, defaulting to
// last_7_days — the X metrics read's default, since last_30_days (every other audience read's
// default) is a window X's reads refuse.
func resolveTwitterAudienceWindow(window *string) (model.MetricsWindow, error) {
	if window == nil {
		return model.MetricsWindowLast7Days, nil
	}
	w := model.MetricsWindow(*window)
	if !twitterAudienceWindows[w] {
		return "", &conn.BadRequestError{Code: "400", Message: twitterAudienceWindowMessage}
	}
	return w, nil
}

// GetTwitterAdsAudience reads X audience segmentations (age, gender, platform) across the
// project's OWN campaigns — the X sibling of GetMetaAdsAudience, with the same guards in the same
// order: the reserved system scope is refused (404), the window is validated locally against the
// X subset, and every failure is classified by classifyInsightsErrorFor.
func (s *ConnectionService) GetTwitterAdsAudience(ctx context.Context, p *conn.GetTwitterAdsAudiencePayload) (*conn.TwitterAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveTwitterAudienceWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("audience insights")
	if err != nil {
		return nil, err
	}
	ai, aerr := orch.ReadTwitterAudienceInsights(ctx, p.ProjectID, model.ProviderTwitterAds, window)
	if aerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, twitterAdsAudienceInsights, aerr)
	}
	buckets := make([]*conn.TwitterAdsAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, &conn.TwitterAdsAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	return &conn.TwitterAdsAudience{
		Window:          string(ai.Window),
		Buckets:         buckets,
		BucketCount:     len(buckets),
		AccountCurrency: optionalString(ai.Currency),
	}, nil
}
