// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sort"
	"strings"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// redditAdsAudienceInsights is the Reddit audience read's descriptor for the shared
// connection-state arms (classifyInsightsErrorFor); remedy text from redditAdsAccountDiscovery,
// plus the account selection the reports operation needs.
var redditAdsAudienceInsights = accountDiscovery{
	provider:    model.ProviderRedditAds,
	displayName: "reddit ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with client_id, client_secret and refresh_token set, and that an account_id is selected",
	operation: "audience insights",
}

// redditAudienceWindows mirrors design/brief.go's redditAudienceWindowEnum: the windows the Reddit
// report client maps to a date range (reddit.ValidateMetricsWindow). Enforced here as well as by
// the generated decoder, for resolveInsightsWindow's reason (a non-HTTP caller bypasses the
// decoder).
var redditAudienceWindows = map[model.MetricsWindow]bool{
	model.MetricsWindowToday:      true,
	model.MetricsWindowLast7Days:  true,
	model.MetricsWindowLast30Days: true,
	model.MetricsWindowThisMonth:  true,
	model.MetricsWindowLastMonth:  true,
}

// redditAudienceWindowMessage is the fixed 400 text for an unservable window, derived from the map
// so it can never advertise a window the map does not hold.
var redditAudienceWindowMessage = func() string {
	names := make([]string, 0, len(redditAudienceWindows))
	for w := range redditAudienceWindows {
		names = append(names, string(w))
	}
	sort.Strings(names)
	return "window must be one of: " + strings.Join(names, ", ")
}()

// resolveRedditAudienceWindow maps the optional window onto the Reddit subset, defaulting to
// last_30_days. Not resolveInsightsWindow: that helper's 400 lists all seven windows, two of which
// this read refuses.
func resolveRedditAudienceWindow(window *string) (model.MetricsWindow, error) {
	if window == nil {
		return model.MetricsWindowLast30Days, nil
	}
	w := model.MetricsWindow(*window)
	if !redditAudienceWindows[w] {
		return "", &conn.BadRequestError{Code: "400", Message: redditAudienceWindowMessage}
	}
	return w, nil
}

// GetRedditAdsAudience reads Reddit audience breakdowns (gender, country, community) across the
// project's OWN campaigns — the Reddit sibling of GetTwitterAdsAudience, with the same guards in the
// same order: the reserved system scope is refused (404), the window is validated locally against
// the Reddit subset, and every failure is classified by classifyInsightsErrorFor, whose default arm
// answers an upstream failure with a fixed 503 that carries no upstream text.
func (s *ConnectionService) GetRedditAdsAudience(ctx context.Context, p *conn.GetRedditAdsAudiencePayload) (*conn.RedditAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveRedditAudienceWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("audience insights")
	if err != nil {
		return nil, err
	}
	ai, aerr := orch.ReadRedditAudienceInsights(ctx, p.ProjectID, model.ProviderRedditAds, window)
	if aerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, redditAdsAudienceInsights, aerr)
	}
	buckets := make([]*conn.RedditAdsAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, &conn.RedditAdsAudienceBucket{
			Dimension: b.Dimension,
			// "" is Reddit's explicit null — unattributed traffic — published with no `value`.
			Value:       optionalString(b.Value),
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	return &conn.RedditAdsAudience{
		Window:      string(ai.Window),
		Buckets:     buckets,
		BucketCount: len(buckets),
	}, nil
}
