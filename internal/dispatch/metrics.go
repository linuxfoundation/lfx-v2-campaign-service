// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// FetchMetrics implements service.MetricsFetcher for Google Ads.
//
// It reuses resolveGoogleAdsClient — the same connection resolution and validation
// the status-toggle path uses — so a create, a toggle and a metrics read accept
// EXACTLY the same connections and cannot drift.
//
// Errors are deliberately NOT wrapped with notCreated here. preCreateError exists to
// tell the orchestrator whether a dispatch CLAIM may be released; a read holds no
// claim, so tagging it would be meaningless at best and misleading at worst.
func (d *GoogleAdsDispatcher) FetchMetrics(ctx context.Context, campaign *model.Campaign, from, to time.Time) ([]model.CampaignMetric, error) {
	if campaign == nil {
		return nil, fmt.Errorf("google-ads metrics: campaign is nil")
	}
	// No upstream id means the campaign was never created on the platform, so there
	// is nothing to report on. This is a state error, not a platform failure: calling
	// Google with an empty id would produce a confusing API error instead.
	if campaign.PlatformCampaignID == "" {
		return nil, fmt.Errorf("%w: google ads campaign %s has no upstream campaign id",
			domain.ErrCampaignNotProvisioned, campaign.ID)
	}

	client, err := d.resolveGoogleAdsClient(ctx, campaign.ProjectID, campaign.Platform)
	if err != nil {
		return nil, err
	}

	rows, err := client.FetchCampaignMetrics(ctx, campaign.PlatformCampaignID, from, to)
	if err != nil {
		return nil, err
	}

	out := make([]model.CampaignMetric, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.CampaignMetric{
			CampaignID:  campaign.ID,
			MetricDate:  r.Date,
			Impressions: r.Impressions,
			Clicks:      r.Clicks,
			Spend:       r.Spend,
			Conversions: r.Conversions,
			Currency:    r.Currency,
			// Every row carries the basis under which ITS conversions were counted.
			// This is what lets a rollup refuse to add incomparable numbers instead of
			// producing a plausible-looking wrong total.
			AttributionBasis: model.AttributionGoogleAdsClickTime,
			Platform:         campaign.Platform,
			Raw:              r.Raw,
		})
	}
	return out, nil
}

// ─── Deferred platforms ───
//
// Each dispatcher below implements MetricsFetcher by returning ErrMetricsUnsupported
// rather than a fabricated result.
//
// This is a deliberate choice, not an oversight. A metrics fetcher must decode a
// platform's reporting response, and NONE of these platforms' reporting contracts can
// be verified from this repository: there are no committed response fixtures anywhere
// under internal/platform, and none of these clients has any reporting code to model
// one on. Writing decode structs from memory would produce a fetcher that does not
// fail loudly when the guess is wrong — it decodes absent fields to zero and reports a
// campaign that spent real money as having spent nothing, which is indistinguishable
// from a campaign that simply did not run.
//
// An explicit "unsupported" is strictly better than a plausible zero: the sweeper
// skips it quietly and the API says so plainly. Each is implemented when its response
// shape can be verified against the real API.

// FetchMetrics implements service.MetricsFetcher for Meta.
//
// Deferred: the client has a GET-capable doRequest, but the /insights edge response
// shape is not represented anywhere in this repo (no fixture, no existing call), so it
// cannot be decoded without guessing. Meta also defaults to a 7-day-click/1-day-view
// attribution window, which must be recorded as its own AttributionBasis so its
// conversions are never summed with Google's click-time counting.
func (d *MetaDispatcher) FetchMetrics(_ context.Context, _ *model.Campaign, _, _ time.Time) ([]model.CampaignMetric, error) {
	return nil, fmt.Errorf("%w: meta insights reporting is not wired yet", domain.ErrMetricsUnsupported)
}

// FetchMetrics implements service.MetricsFetcher for LinkedIn.
//
// Deferred: internal/platform/linkedin contains no analytics code and no fixture for
// the adAnalytics finder, so neither its response shape nor its required Rest.li
// parameter encoding at the pinned LinkedIn-Version can be verified here.
func (d *LinkedInDispatcher) FetchMetrics(_ context.Context, _ *model.Campaign, _, _ time.Time) ([]model.CampaignMetric, error) {
	return nil, fmt.Errorf("%w: linkedin ad analytics reporting is not wired yet", domain.ErrMetricsUnsupported)
}

// FetchMetrics implements service.MetricsFetcher for Microsoft Ads.
//
// Deferred, and the largest of these by some margin: Microsoft reporting is a separate
// SOAP Reporting service on a DIFFERENT host from the Campaign Management API this
// client speaks, and it is asynchronous — submit a report request, poll for
// completion, then download and parse a ZIPped CSV. That is a new transport, not a new
// query over the existing one.
func (d *MicrosoftDispatcher) FetchMetrics(_ context.Context, _ *model.Campaign, _, _ time.Time) ([]model.CampaignMetric, error) {
	return nil, fmt.Errorf("%w: microsoft ads reporting uses a separate async SOAP reporting service and is not wired yet", domain.ErrMetricsUnsupported)
}

// FetchMetrics implements service.MetricsFetcher for Reddit.
//
// Deferred: the reddit client has no read/report helper at all, and no fixture
// describing its reporting response.
func (d *RedditDispatcher) FetchMetrics(_ context.Context, _ *model.Campaign, _, _ time.Time) ([]model.CampaignMetric, error) {
	return nil, fmt.Errorf("%w: reddit ads reporting is not wired yet", domain.ErrMetricsUnsupported)
}

// FetchMetrics implements service.MetricsFetcher for X/Twitter.
//
// Deferred: no reporting code or fixture, and its stats endpoints are OAuth1-signed
// with their own segmentation and entity-id semantics that cannot be verified here.
func (d *TwitterDispatcher) FetchMetrics(_ context.Context, _ *model.Campaign, _, _ time.Time) ([]model.CampaignMetric, error) {
	return nil, fmt.Errorf("%w: x/twitter ads reporting is not wired yet", domain.ErrMetricsUnsupported)
}

// metricsFetcherAssertions pins, at COMPILE time, which dispatchers satisfy the
// metrics-fetch contract. Without this a dispatcher could silently stop implementing
// it (a signature drift) and the orchestrator's type assertion would just start
// returning "unsupported" at runtime, which looks like a deliberate deferral rather
// than a bug.
//
// The interface is restated here rather than imported from internal/service because
// service imports this package; declaring it locally keeps the dependency one-way.
type metricsFetcher interface {
	FetchMetrics(ctx context.Context, campaign *model.Campaign, from, to time.Time) ([]model.CampaignMetric, error)
}

var (
	_ metricsFetcher = (*GoogleAdsDispatcher)(nil)
	_ metricsFetcher = (*MetaDispatcher)(nil)
	_ metricsFetcher = (*LinkedInDispatcher)(nil)
	_ metricsFetcher = (*MicrosoftDispatcher)(nil)
	_ metricsFetcher = (*RedditDispatcher)(nil)
	_ metricsFetcher = (*TwitterDispatcher)(nil)
)
