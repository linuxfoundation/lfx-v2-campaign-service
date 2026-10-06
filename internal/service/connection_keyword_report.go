// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sort"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// microsoftAdsKeywordInsights is the Microsoft keyword read's descriptor for the shared
// connection-state arms (classifyInsightsErrorFor); remedy text copied verbatim from
// microsoftAdsMonitorDiscovery.
var microsoftAdsKeywordInsights = accountDiscovery{
	provider:    model.ProviderMicrosoftAds,
	displayName: "microsoft ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with client_id, client_secret, developer_token and refresh_token set",
	operation: "keyword insights",
}

// microsoftKeywordWindows mirrors design/connection.go's microsoftKeywordsWindowEnum: the
// windows the Microsoft client maps to a date range. Enforced here as well as by the generated
// decoder, for resolveInsightsWindow's reason (a non-HTTP caller bypasses the decoder).
var microsoftKeywordWindows = map[model.MetricsWindow]bool{
	model.MetricsWindowToday:      true,
	model.MetricsWindowLast7Days:  true,
	model.MetricsWindowLast30Days: true,
	model.MetricsWindowThisMonth:  true,
	model.MetricsWindowLastMonth:  true,
}

// resolveMicrosoftKeywordWindow maps the optional window onto the Microsoft subset, defaulting to
// last_30_days. It does NOT go through resolveInsightsWindow: that helper's 400 lists all seven
// windows, two of which this read refuses, so a caller would be told `yesterday` is valid and
// then refused for it. The message here is built from microsoftKeywordWindows itself, sorted, so
// it can never advertise a window the map does not hold.
func resolveMicrosoftKeywordWindow(window *string) (model.MetricsWindow, error) {
	if window == nil {
		return model.MetricsWindowLast30Days, nil
	}
	w := model.MetricsWindow(*window)
	if !microsoftKeywordWindows[w] {
		return "", &conn.BadRequestError{Code: "400", Message: microsoftKeywordWindowMessage}
	}
	return w, nil
}

// microsoftKeywordWindowMessage is the 400 text for an unservable window, derived from the map.
var microsoftKeywordWindowMessage = func() string {
	names := make([]string, 0, len(microsoftKeywordWindows))
	for w := range microsoftKeywordWindows {
		names = append(names, string(w))
	}
	sort.Strings(names)
	return "window must be one of: " + strings.Join(names, ", ")
}()

// GetMicrosoftAdsKeywords reads Microsoft Advertising keyword performance across the project's
// OWN campaigns, served from the last finished saved report (Orchestrator.ReadReportedKeywordPerformance).
//
// Same guards and classification as GetGoogleAdsKeywords; the differences are that the rows come
// from a saved report — so the response says how old they are and whether newer ones are
// building — and that the read uses the project's own connection only.
func (s *ConnectionService) GetMicrosoftAdsKeywords(ctx context.Context, p *conn.GetMicrosoftAdsKeywordsPayload) (*conn.MicrosoftAdsKeywords, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveMicrosoftKeywordWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(microsoftAdsKeywordInsights.label())
	if err != nil {
		return nil, err
	}
	read, rerr := orch.ReadReportedKeywordPerformance(ctx, p.ProjectID, model.ProviderMicrosoftAds, window)
	if rerr != nil {
		return nil, s.classifyInsightsErrorFor(ctx, p.ProjectID, microsoftAdsKeywordInsights, rerr)
	}
	rows := make([]*conn.GoogleAdsKeyword, 0, len(read.Rows))
	for _, r := range read.Rows {
		rows = append(rows, &conn.GoogleAdsKeyword{
			CriterionID:  r.CriterionID,
			AdGroupID:    r.AdGroupID,
			CampaignID:   r.CampaignID,
			AdGroupName:  r.AdGroupName,
			CampaignName: r.CampaignName,
			Text:         r.Text,
			MatchType:    r.MatchType,
			Status:       r.Status,
			QualityScore: r.QualityScore,
			Impressions:  r.Impressions,
			Clicks:       r.Clicks,
			CostMicros:   r.CostMicros,
			Ctr:          r.Ctr,
			Conversions:  r.Conversions,
		})
	}
	out := &conn.MicrosoftAdsKeywords{
		Window:              string(read.Window),
		Rows:                rows,
		RowCount:            len(rows),
		Truncated:           read.Truncated,
		MetricsPending:      read.MetricsPending,
		ConversionsComplete: read.ConversionsComplete,
		DataIncomplete:      read.DataIncomplete,
	}
	if read.MetricsAsOf != nil {
		asOf := read.MetricsAsOf.UTC().Format(time.RFC3339)
		out.MetricsAsOf = &asOf
	}
	return out, nil
}
