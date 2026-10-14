// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// linkedInAudienceDispatcher implements ONLY LinkedInAudienceReader (plus Dispatch).
type linkedInAudienceDispatcher struct {
	err       error
	result    *model.LinkedInAudienceInsights
	nilOut    bool
	calls     int
	gotScope  []model.ProjectCampaignScope
	gotWindow model.MetricsWindow
}

func (d *linkedInAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *linkedInAudienceDispatcher) ReadLinkedInAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.LinkedInAudienceInsights, error) {
	d.calls++
	d.gotScope = scope
	d.gotWindow = w
	if d.err != nil {
		return nil, d.err
	}
	if d.nilOut {
		return nil, nil
	}
	if d.result != nil {
		return d.result, nil
	}
	// Distinct non-zero values per field, so a mapper that drops or swaps one is visible.
	return &model.LinkedInAudienceInsights{Window: w, Buckets: []model.LinkedInAudienceBucket{
		{Dimension: model.LinkedInAudienceDimensionJobFunction, Value: "urn:li:function:8", Impressions: 11, Clicks: 3, CostMicros: 4_000_000, Ctr: 3.0 / 11},
		{Dimension: model.LinkedInAudienceDimensionCountry, Value: "urn:li:geo:103644278", Impressions: 7, Clicks: 1, CostMicros: 2_000_000, Ctr: 1.0 / 7},
	}}, nil
}

func linkedInAudienceService(t *testing.T, d PlatformDispatcher, scopeIDs ...string) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{scopeIDs: scopeIDs}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d}))
	return svc
}

func TestGetLinkedinAdsAudience_HappyPathMapsEveryField(t *testing.T) {
	d := &linkedInAudienceDispatcher{}
	svc := linkedInAudienceService(t, d, "555")
	w := "last_7_days"
	res, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf", Window: &w})
	if err != nil {
		t.Fatalf("GetLinkedinAdsAudience: %v", err)
	}
	if res.Window != w || res.BucketCount != 2 || len(res.Buckets) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if !res.Approximate {
		t.Error("approximate must be true: LinkedIn documents demographic metrics as approximate")
	}
	jf, co := res.Buckets[0], res.Buckets[1]
	if jf.Dimension != "job_function" || jf.Value != "urn:li:function:8" || jf.Impressions != 11 || jf.Clicks != 3 || jf.CostMicros != 4_000_000 || jf.Ctr != 3.0/11 {
		t.Errorf("job_function bucket = %+v", jf)
	}
	if co.Dimension != "country" || co.Value != "urn:li:geo:103644278" || co.Impressions != 7 || co.Clicks != 1 || co.CostMicros != 2_000_000 || co.Ctr != 1.0/7 {
		t.Errorf("country bucket = %+v", co)
	}
	if len(d.gotScope) != 1 || d.gotScope[0].PlatformCampaignID != "555" {
		t.Errorf("scope = %+v, want the project's own campaign", d.gotScope)
	}
	if d.gotWindow != model.MetricsWindowLast7Days {
		t.Errorf("window forwarded = %q", d.gotWindow)
	}
}

// A project with no LinkedIn campaigns of its own: 200 with an empty array, LinkedIn never
// contacted, and the default window.
func TestGetLinkedinAdsAudience_EmptyScopeIs200EmptyWithoutUpstreamCall(t *testing.T) {
	d := &linkedInAudienceDispatcher{}
	svc := linkedInAudienceService(t, d)
	res, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetLinkedinAdsAudience: %v", err)
	}
	if d.calls != 0 {
		t.Errorf("LinkedIn was contacted %d time(s) with an empty scope", d.calls)
	}
	if res.Buckets == nil || len(res.Buckets) != 0 || res.BucketCount != 0 || !res.Approximate {
		t.Errorf("res = %+v, want an empty [] still flagged approximate", res)
	}
	if res.Window != string(model.MetricsWindowLast30Days) {
		t.Errorf("default window = %q", res.Window)
	}
}

// yesterday and last_14_days are 400 on the LinkedIn metrics read (no explicit dateRange mapping),
// so they are 400 here too, before LinkedIn is contacted; the message names the real choices.
func TestGetLinkedinAdsAudience_RejectsSystemScopeAndUnservableWindows(t *testing.T) {
	d := &linkedInAudienceDispatcher{}
	svc := linkedInAudienceService(t, d, "555")
	if _, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Error("system scope: expected 404")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("system scope: error = %T (%v), want *conn.NotFoundError", err, err)
	}
	for _, bad := range []string{"yesterday", "last_14_days", "next_tuesday"} {
		w := bad
		_, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf", Window: &w})
		br, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Errorf("%s: error = %T (%v), want *conn.BadRequestError", bad, err, err)
			continue
		}
		if br.Message != "window must be one of: last_30_days, last_7_days, last_month, this_month, today" {
			t.Errorf("%s: message = %q", bad, br.Message)
		}
	}
	for _, ok := range []string{"today", "last_7_days", "last_30_days", "this_month", "last_month"} {
		w := ok
		if _, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf", Window: &w}); err != nil {
			t.Errorf("%s must be served: %v", ok, err)
		}
	}
	if d.calls != 5 {
		t.Errorf("LinkedIn was contacted %d time(s); want exactly the 5 served windows", d.calls)
	}
}

func TestGetLinkedinAdsAudience_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want any
	}{
		{"unsupported", domain.ErrKeywordInsightsUnsupported, &conn.BadRequestError{}},
		{"window unsupported", domain.ErrMetricsWindowUnsupported, &conn.BadRequestError{}},
		{"a campaign in another account", domain.ErrCampaignAccountMismatch, &conn.ConflictError{}},
		{"scope too large", domain.ErrAudienceScopeTooLarge, &conn.ConflictError{}},
		{"scope invalid", domain.ErrAudienceScopeInvalid, &conn.ConflictError{}},
		{"no own connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", errors.Join(domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), &conn.BadRequestError{}},
		{"credential expired", errors.Join(domain.ErrConnectionNotUsable, domain.ErrCredentialsExpired), &conn.BadRequestError{}},
		{"upstream failure (5xx/429/malformed/foreign row)", errors.New("LinkedIn API GET adAnalytics -> 429 urn:li:sponsoredCampaign:999"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := linkedInAudienceService(t, &linkedInAudienceDispatcher{err: tc.err}, "555")
			_, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf"})
			assertInsightsErr(t, err, tc.want, tc.err)
		})
	}
}

// The 503 carries fixed text: nothing from the upstream error (paths, URNs, status codes) leaks.
func TestGetLinkedinAdsAudience_503CarriesNoUpstreamText(t *testing.T) {
	svc := linkedInAudienceService(t, &linkedInAudienceDispatcher{err: errors.New("LinkedIn API GET adAnalytics -> 503 urn:li:sponsoredCampaign:999 secret-body")}, "555")
	_, err := svc.GetLinkedinAdsAudience(context.Background(), &conn.GetLinkedinAdsAudiencePayload{ProjectID: "cncf"})
	su, ok := err.(*conn.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("error = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
	}
	for _, leak := range []string{"adAnalytics", "urn:li", "secret-body", "999"} {
		if strings.Contains(su.Message, leak) {
			t.Errorf("503 message leaks upstream text %q: %q", leak, su.Message)
		}
	}
}

// Every platform but LinkedIn lacks LinkedInAudienceReader and answers 400 "not supported".
func TestOrchestratorReadLinkedInAudience_UnsupportedPlatforms(t *testing.T) {
	ctx := context.Background()
	for _, p := range []model.Provider{model.ProviderMicrosoftAds, model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderGoogleAds, model.ProviderMetaAds} {
		orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{p: upstreamCapableDispatcher{}})
		if _, err := orch.ReadLinkedInAudienceInsights(ctx, "p1", p, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: error = %v, want ErrKeywordInsightsUnsupported", p, err)
		}
	}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{})
	if _, err := orch.ReadLinkedInAudienceInsights(ctx, "p1", model.ProviderLinkedInAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("unregistered: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
}

func TestOrchestratorReadLinkedInAudience_ScopeLookupFailureDoesNotFallBack(t *testing.T) {
	d := &linkedInAudienceDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d})
	if _, err := orch.ReadLinkedInAudienceInsights(context.Background(), "p1", model.ProviderLinkedInAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a scope lookup failure must fail the read, not proceed unscoped")
	}
	if d.calls != 0 {
		t.Errorf("LinkedIn was contacted %d time(s) after the scope could not be established", d.calls)
	}
}

func TestOrchestratorReadLinkedInAudience_NilResultAndNilSlice(t *testing.T) {
	ctx := context.Background()
	orch := func(d PlatformDispatcher) *Orchestrator {
		return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d})
	}
	if _, err := orch(&linkedInAudienceDispatcher{nilOut: true}).ReadLinkedInAudienceInsights(ctx, "p1", model.ProviderLinkedInAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a nil result with no error must be rejected")
	}
	ai, err := orch(&linkedInAudienceDispatcher{result: &model.LinkedInAudienceInsights{Window: model.MetricsWindowLast30Days}}).
		ReadLinkedInAudienceInsights(ctx, "p1", model.ProviderLinkedInAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadLinkedInAudienceInsights: %v", err)
	}
	if ai.Buckets == nil {
		t.Error("Buckets is nil; it must be an empty slice so the wire shape is [] not null")
	}
}
