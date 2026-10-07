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

// metaAudienceDispatcher implements ONLY MetaAudienceReader (plus Dispatch). calls and gotScope
// let a test assert whether, and with what, the platform was contacted; result/err override
// the canned answer.
type metaAudienceDispatcher struct {
	err      error
	result   *model.MetaAudienceInsights
	nilOut   bool
	calls    int
	gotScope []model.ProjectCampaignScope
}

func (d *metaAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *metaAudienceDispatcher) ReadMetaAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.MetaAudienceInsights, error) {
	d.calls++
	d.gotScope = scope
	if d.err != nil {
		return nil, d.err
	}
	if d.nilOut {
		return nil, nil
	}
	if d.result != nil {
		return d.result, nil
	}
	// Distinct non-zero values per field, so a mapper that drops one is visible.
	return &model.MetaAudienceInsights{Window: w, Currency: "EUR", Buckets: []model.MetaAudienceBucket{
		{Dimension: model.MetaAudienceDimensionAgeGender, Age: "25-34", Gender: "female", Impressions: 11, Clicks: 3, CostMicros: 4_000_000, Ctr: 3.0 / 11},
		{Dimension: model.MetaAudienceDimensionPlacement, PublisherPlatform: "instagram", PlatformPosition: "feed", Impressions: 7, Clicks: 1, CostMicros: 2_000_000, Ctr: 1.0 / 7},
	}}, nil
}

func metaAudienceService(t *testing.T, d PlatformDispatcher, scopeIDs ...string) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{scopeIDs: scopeIDs}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: d}))
	return svc
}

func TestGetMetaAdsAudience_HappyPath(t *testing.T) {
	d := &metaAudienceDispatcher{}
	svc := metaAudienceService(t, d, "555")
	w := "last_7_days"
	res, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: "cncf", Window: &w})
	if err != nil {
		t.Fatalf("GetMetaAdsAudience: %v", err)
	}
	if res.Window != w || res.BucketCount != 2 || len(res.Buckets) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if res.AccountCurrency == nil || *res.AccountCurrency != "EUR" {
		t.Errorf("account_currency = %v", res.AccountCurrency)
	}
	ag, pl := res.Buckets[0], res.Buckets[1]
	if ag.Dimension != "age_gender" || ag.Age == nil || *ag.Age != "25-34" || ag.Gender == nil || *ag.Gender != "female" ||
		ag.Impressions != 11 || ag.Clicks != 3 || ag.CostMicros != 4_000_000 || ag.Ctr != 3.0/11 {
		t.Errorf("age_gender bucket = %+v", ag)
	}
	// The other dimension's value fields are ABSENT, not published empty.
	if ag.PublisherPlatform != nil || ag.PlatformPosition != nil || pl.Age != nil || pl.Gender != nil {
		t.Errorf("cross-dimension fields must be absent: %+v / %+v", ag, pl)
	}
	if pl.Dimension != "placement" || *pl.PublisherPlatform != "instagram" || *pl.PlatformPosition != "feed" {
		t.Errorf("placement bucket = %+v", pl)
	}
	if len(d.gotScope) != 1 || d.gotScope[0].PlatformCampaignID != "555" {
		t.Errorf("scope = %+v, want the project's own campaign", d.gotScope)
	}
}

// A project with no Meta campaigns of its own: 200 with an empty array, Meta never contacted.
func TestGetMetaAdsAudience_EmptyScopeIs200EmptyWithoutUpstreamCall(t *testing.T) {
	d := &metaAudienceDispatcher{}
	svc := metaAudienceService(t, d)
	res, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetMetaAdsAudience: %v", err)
	}
	if d.calls != 0 {
		t.Errorf("Meta was contacted %d time(s) with an empty scope; the read would be account-wide", d.calls)
	}
	if res.Buckets == nil || len(res.Buckets) != 0 || res.BucketCount != 0 || res.AccountCurrency != nil {
		t.Errorf("res = %+v, want an empty [] with no currency", res)
	}
	if res.Window != string(model.MetricsWindowLast30Days) {
		t.Errorf("default window = %q", res.Window)
	}
}

func TestGetMetaAdsAudience_RejectsSystemScopeAndBadWindow(t *testing.T) {
	d := &metaAudienceDispatcher{}
	svc := metaAudienceService(t, d, "555")
	if _, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Error("system scope: expected 404")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("system scope: error = %T (%v), want *conn.NotFoundError", err, err)
	}
	bad := "next_tuesday"
	if _, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: "cncf", Window: &bad}); err == nil {
		t.Error("bad window: expected 400")
	} else if _, ok := err.(*conn.BadRequestError); !ok {
		t.Errorf("bad window: error = %T (%v), want *conn.BadRequestError", err, err)
	}
	if d.calls != 0 {
		t.Errorf("Meta was contacted %d time(s) for refused requests", d.calls)
	}
}

func TestGetMetaAdsAudience_ErrorMapping(t *testing.T) {
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
		{"no connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", errors.Join(domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), &conn.BadRequestError{}},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, &conn.InternalServerError{}},
		{"upstream failure (5xx/401/403/429/malformed)", errors.New("meta API GET /act_777/insights failed (403)"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := metaAudienceService(t, &metaAudienceDispatcher{err: tc.err}, "555")
			_, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: "cncf"})
			assertInsightsErr(t, err, tc.want, tc.err)
		})
	}
}

// Microsoft, Reddit and X (and Google, LinkedIn) do not implement MetaAudienceReader, so the
// read answers the same 400 "not supported" the Google audience read gives them.
func TestOrchestratorReadMetaAudience_UnsupportedPlatforms(t *testing.T) {
	ctx := context.Background()
	for _, p := range []model.Provider{model.ProviderMicrosoftAds, model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderGoogleAds} {
		orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{p: upstreamCapableDispatcher{}})
		if _, err := orch.ReadMetaAudienceInsights(ctx, "p1", p, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: error = %v, want ErrKeywordInsightsUnsupported", p, err)
		}
	}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{})
	if _, err := orch.ReadMetaAudienceInsights(ctx, "p1", model.ProviderMetaAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("unregistered: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
}

func TestOrchestratorReadMetaAudience_ScopeLookupFailureDoesNotFallBack(t *testing.T) {
	d := &metaAudienceDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: d})
	if _, err := orch.ReadMetaAudienceInsights(context.Background(), "p1", model.ProviderMetaAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a scope lookup failure must fail the read, not proceed unscoped")
	}
	if d.calls != 0 {
		t.Errorf("Meta was contacted %d time(s) after the scope could not be established", d.calls)
	}
}

func TestOrchestratorReadMetaAudience_NilResultAndNilSlice(t *testing.T) {
	ctx := context.Background()
	orch := func(d PlatformDispatcher) *Orchestrator {
		return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{model.ProviderMetaAds: d})
	}
	if _, err := orch(&metaAudienceDispatcher{nilOut: true}).ReadMetaAudienceInsights(ctx, "p1", model.ProviderMetaAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a nil result with no error must be rejected")
	}
	ai, err := orch(&metaAudienceDispatcher{result: &model.MetaAudienceInsights{Window: model.MetricsWindowLast30Days}}).
		ReadMetaAudienceInsights(ctx, "p1", model.ProviderMetaAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetaAudienceInsights: %v", err)
	}
	if ai.Buckets == nil {
		t.Error("Buckets is nil; it must be an empty slice so the wire shape is [] not null")
	}
}

// The account-mismatch 409 is shared by every insights read, including this one, which has no
// keywords — so its fixed text must not name keywords.
func TestGetMetaAdsAudience_MismatchMessageIsRouteNeutral(t *testing.T) {
	svc := metaAudienceService(t, &metaAudienceDispatcher{err: domain.ErrCampaignAccountMismatch}, "555")
	_, err := svc.GetMetaAdsAudience(context.Background(), &conn.GetMetaAdsAudiencePayload{ProjectID: "cncf"})
	ce, ok := err.(*conn.ConflictError)
	if !ok {
		t.Fatalf("error = %T (%v), want *conn.ConflictError", err, err)
	}
	if strings.Contains(ce.Message, "keyword") {
		t.Errorf("the audience read's 409 names keywords: %q", ce.Message)
	}
}
