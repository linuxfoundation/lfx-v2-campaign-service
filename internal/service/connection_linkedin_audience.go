// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// LinkedInAudienceReader is an OPTIONAL dispatcher capability: LinkedIn's professional-demographic
// breakdowns (job function, seniority, industry, company size, country) for the campaigns a
// project OWNS, live from LinkedIn's Ad Analytics API. Type-asserted like MetaAudienceReader, so a
// dispatcher without it — every platform but LinkedIn — yields ErrKeywordInsightsUnsupported →
// 400. Separate from the other audience readers because the result shape differs (one URN value
// per pivot, every counter approximate). scope carries KeywordInsightsReader's contract: never
// empty from the orchestrator, and the adapter refuses the whole read on any provenance mismatch
// rather than dropping entries.
//
// No AudienceEnabled gate (unlike TwitterAudienceReader): LinkedIn has no metrics feature flag —
// its campaign metrics read and account monitor are ungated — so there is nothing to check before
// the scope lookup.
type LinkedInAudienceReader interface {
	ReadLinkedInAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.LinkedInAudienceInsights, error)
}

// ReadLinkedInAudienceInsights reads LinkedIn audience breakdowns across the project's OWN
// campaigns. Same shape as ReadMetaAudienceInsights — capability check, then readScopedAudience
// (empty scope answers empty WITHOUT contacting LinkedIn) — only the capability and the result type
// differ.
func (o *Orchestrator) ReadLinkedInAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow) (*model.LinkedInAudienceInsights, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	reader, ok := d.(LinkedInAudienceReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	return readScopedAudience(ctx, o, projectID, platform,
		func(callCtx context.Context, scope []model.ProjectCampaignScope) (*model.LinkedInAudienceInsights, error) {
			return reader.ReadLinkedInAudienceInsights(callCtx, projectID, platform, window, scope)
		},
		func() *model.LinkedInAudienceInsights {
			return &model.LinkedInAudienceInsights{Window: window, Buckets: []model.LinkedInAudienceBucket{}}
		},
		func(ai *model.LinkedInAudienceInsights) {
			if ai.Buckets == nil {
				ai.Buckets = []model.LinkedInAudienceBucket{}
			}
		})
}

// linkedInAdsAudienceInsights is the LinkedIn audience read's descriptor for the shared
// connection-state arms (classifyInsightsErrorFor): linkedInAdsMonitorDiscovery's remedy, plus the
// account selection the read needs (an account-less connection is 400, not 503).
var linkedInAdsAudienceInsights = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with access_token set, and that an account_id is selected",
	operation: "audience insights",
}

// linkedInAudienceWindows are the windows the LinkedIn audience read serves: exactly the ones the
// LinkedIn campaign metrics read maps to an explicit Ad Analytics dateRange
// (linkedin.ValidateMetricsWindow). yesterday and last_14_days are refused there, so they are
// refused here with a 400 that names the real choices. The design enum declares the same subset.
var linkedInAudienceWindows = map[model.MetricsWindow]bool{
	model.MetricsWindowToday:      true,
	model.MetricsWindowLast7Days:  true,
	model.MetricsWindowLast30Days: true,
	model.MetricsWindowThisMonth:  true,
	model.MetricsWindowLastMonth:  true,
}

// linkedInAudienceWindowMessage is the 400 text for an unservable window, derived from the map.
var linkedInAudienceWindowMessage = func() string {
	names := make([]string, 0, len(linkedInAudienceWindows))
	for w := range linkedInAudienceWindows {
		names = append(names, string(w))
	}
	sort.Strings(names)
	return "window must be one of: " + strings.Join(names, ", ")
}()

// resolveLinkedInAudienceWindow defaults an absent window to last_30_days and refuses any window
// LinkedIn's Ad Analytics mapping does not serve.
func resolveLinkedInAudienceWindow(window *string) (model.MetricsWindow, error) {
	if window == nil {
		return model.MetricsWindowLast30Days, nil
	}
	w := model.MetricsWindow(*window)
	if !linkedInAudienceWindows[w] {
		return "", &conn.BadRequestError{Code: "400", Message: linkedInAudienceWindowMessage}
	}
	return w, nil
}

// GetLinkedinAdsAudience reads LinkedIn professional-demographic breakdowns across the project's
// OWN campaigns — the LinkedIn sibling of GetMetaAdsAudience, with the same guards in the same
// order: the reserved system scope is refused (404), the window is checked against LinkedIn's
// subset, and every failure is classified by classifyInsightsErrorFor, so anything unverifiable is
// a 503 with fixed text and no upstream content.
//
// `approximate` is always true: LinkedIn documents every professional-demographic metric as an
// approximation, so the flag is a property of the source, not of a particular answer.
func (s *ConnectionService) GetLinkedinAdsAudience(ctx context.Context, p *conn.GetLinkedinAdsAudiencePayload) (*conn.LinkedinAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveLinkedInAudienceWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsAudienceInsights.label())
	if err != nil {
		return nil, err
	}
	ai, aerr := orch.ReadLinkedInAudienceInsights(ctx, p.ProjectID, model.ProviderLinkedInAds, window)
	if aerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, linkedInAdsAudienceInsights, aerr)
	}
	buckets := make([]*conn.LinkedinAdsAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, &conn.LinkedinAdsAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	return &conn.LinkedinAdsAudience{
		Window:      string(ai.Window),
		Buckets:     buckets,
		BucketCount: len(buckets),
		Approximate: true,
	}, nil
}
