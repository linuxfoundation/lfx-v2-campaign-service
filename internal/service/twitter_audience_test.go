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

// twitterAudienceDispatcher implements ONLY TwitterAudienceReader (plus Dispatch). calls,
// gotWindow and gotScope let a test assert whether, and with what, X was contacted.
type twitterAudienceDispatcher struct {
	err       error
	result    *model.TwitterAudienceInsights
	nilOut    bool
	calls     int
	gotWindow model.MetricsWindow
	gotScope  []model.ProjectCampaignScope
}

func (d *twitterAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *twitterAudienceDispatcher) ReadTwitterAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.TwitterAudienceInsights, error) {
	d.calls++
	d.gotWindow = w
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
	return &model.TwitterAudienceInsights{Window: w, Currency: "GBP", Buckets: []model.TwitterAudienceBucket{
		{Dimension: model.TwitterAudienceDimensionAge, Value: "25-34", Impressions: 11, Clicks: 3, CostMicros: 4_000_000, Ctr: 3.0 / 11},
		{Dimension: model.TwitterAudienceDimensionPlatform, Value: "iOS", Impressions: 7, Clicks: 1, CostMicros: 2_000_000, Ctr: 1.0 / 7},
	}}, nil
}

func twitterAudienceService(t *testing.T, d PlatformDispatcher, scopeIDs ...string) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{scopeIDs: scopeIDs}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: d}))
	return svc
}

func TestGetTwitterAdsAudience_HappyPath(t *testing.T) {
	d := &twitterAudienceDispatcher{}
	svc := twitterAudienceService(t, d, "c555")
	w := "today"
	res, err := svc.GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf", Window: &w})
	if err != nil {
		t.Fatalf("GetTwitterAdsAudience: %v", err)
	}
	if res.Window != w || res.BucketCount != 2 || len(res.Buckets) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if res.AccountCurrency == nil || *res.AccountCurrency != "GBP" {
		t.Errorf("account_currency = %v", res.AccountCurrency)
	}
	age, pl := res.Buckets[0], res.Buckets[1]
	if age.Dimension != "age" || age.Value != "25-34" || age.Impressions != 11 || age.Clicks != 3 || age.CostMicros != 4_000_000 || age.Ctr != 3.0/11 {
		t.Errorf("age bucket = %+v", age)
	}
	if pl.Dimension != "platform" || pl.Value != "iOS" || pl.Impressions != 7 {
		t.Errorf("platform bucket = %+v", pl)
	}
	if len(d.gotScope) != 1 || d.gotScope[0].PlatformCampaignID != "c555" || d.gotWindow != model.MetricsWindowToday {
		t.Errorf("scope/window = %+v/%q, want the project's own campaign over today", d.gotScope, d.gotWindow)
	}
}

// Omitted, the window is X's widest — last_7_days — not the last_30_days other reads default to.
func TestGetTwitterAdsAudience_DefaultWindowIsLast7Days(t *testing.T) {
	d := &twitterAudienceDispatcher{}
	res, err := twitterAudienceService(t, d, "c555").GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetTwitterAdsAudience: %v", err)
	}
	if d.gotWindow != model.MetricsWindowLast7Days || res.Window != "last_7_days" {
		t.Errorf("window = %q / %q, want last_7_days", d.gotWindow, res.Window)
	}
}

// A project with no X campaigns of its own: 200 with an empty array, X never contacted.
func TestGetTwitterAdsAudience_EmptyScopeIs200EmptyWithoutUpstreamCall(t *testing.T) {
	d := &twitterAudienceDispatcher{}
	res, err := twitterAudienceService(t, d).GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetTwitterAdsAudience: %v", err)
	}
	if d.calls != 0 {
		t.Errorf("X was contacted %d time(s) with an empty scope; the read would be account-wide", d.calls)
	}
	if res.Buckets == nil || len(res.Buckets) != 0 || res.BucketCount != 0 || res.AccountCurrency != nil {
		t.Errorf("res = %+v, want an empty [] with no currency", res)
	}
}

// The system scope is 404; every window longer than X's 7 days is a 400 with one fixed message;
// neither reaches X.
func TestGetTwitterAdsAudience_RejectsSystemScopeAndLongWindows(t *testing.T) {
	d := &twitterAudienceDispatcher{}
	svc := twitterAudienceService(t, d, "c555")
	if _, err := svc.GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Error("system scope: expected 404")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("system scope: error = %T (%v), want *conn.NotFoundError", err, err)
	}
	for _, w := range []string{"last_14_days", "last_30_days", "this_month", "last_month", "next_tuesday"} {
		_, err := svc.GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf", Window: &w})
		br, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Errorf("%s: error = %T (%v), want *conn.BadRequestError", w, err, err)
			continue
		}
		if br.Message != twitterAudienceWindowMessage {
			t.Errorf("%s: message = %q, want the fixed X window message", w, br.Message)
		}
	}
	if d.calls != 0 {
		t.Errorf("X was contacted %d time(s) for refused requests", d.calls)
	}
}

func TestGetTwitterAdsAudience_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want any
	}{
		{"unsupported or flag off", domain.ErrKeywordInsightsUnsupported, &conn.BadRequestError{}},
		{"window unsupported", domain.ErrMetricsWindowUnsupported, &conn.BadRequestError{}},
		{"a campaign in another account", domain.ErrCampaignAccountMismatch, &conn.ConflictError{}},
		{"scope too large", domain.ErrAudienceScopeTooLarge, &conn.ConflictError{}},
		{"scope invalid", domain.ErrAudienceScopeInvalid, &conn.ConflictError{}},
		{"account timezone off the hour", domain.ErrAccountTimezoneUnsupported, &conn.ConflictError{}},
		{"no connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", errors.Join(domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), &conn.BadRequestError{}},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, &conn.InternalServerError{}},
		{"upstream failure (5xx/401/403/429/malformed/unfinished)", errors.New("x ads api GET stats/jobs failed (403) CANARY"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := twitterAudienceService(t, &twitterAudienceDispatcher{err: tc.err}, "c555")
			_, err := svc.GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf"})
			assertInsightsErr(t, err, tc.want, tc.err)
			// No upstream text reaches the client body.
			if strings.Contains(err.Error(), "CANARY") {
				t.Errorf("client error echoes upstream text: %v", err)
			}
		})
	}
}

// The 409 for an off-the-hour account zone carries the sentinel's fixed text, nothing upstream.
func TestGetTwitterAdsAudience_TimezoneConflictText(t *testing.T) {
	svc := twitterAudienceService(t, &twitterAudienceDispatcher{err: errors.Join(domain.ErrAccountTimezoneUnsupported, errors.New("Asia/Kolkata"))}, "c555")
	_, err := svc.GetTwitterAdsAudience(context.Background(), &conn.GetTwitterAdsAudiencePayload{ProjectID: "cncf"})
	ce, ok := err.(*conn.ConflictError)
	if !ok || ce.Message != domain.ErrAccountTimezoneUnsupported.Error() {
		t.Errorf("error = %T (%v), want the fixed timezone 409", err, err)
	}
}

// Only X implements TwitterAudienceReader: every other platform is 400 not supported.
func TestOrchestratorReadTwitterAudience_UnsupportedPlatforms(t *testing.T) {
	ctx := context.Background()
	for _, p := range []model.Provider{model.ProviderMicrosoftAds, model.ProviderRedditAds, model.ProviderMetaAds, model.ProviderGoogleAds, model.ProviderLinkedInAds} {
		orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"c555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{p: upstreamCapableDispatcher{}})
		if _, err := orch.ReadTwitterAudienceInsights(ctx, "p1", p, model.MetricsWindowLast7Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: error = %v, want ErrKeywordInsightsUnsupported", p, err)
		}
	}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{})
	if _, err := orch.ReadTwitterAudienceInsights(ctx, "p1", model.ProviderTwitterAds, model.MetricsWindowLast7Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("unregistered: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
	// And the Meta read does not reach the X dispatcher.
	orch = NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"c555"}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: &twitterAudienceDispatcher{}})
	if _, err := orch.ReadMetaAudienceInsights(ctx, "p1", model.ProviderTwitterAds, model.MetricsWindowLast7Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("meta read on x: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
}

func TestOrchestratorReadTwitterAudience_ScopeLookupFailureDoesNotFallBack(t *testing.T) {
	d := &twitterAudienceDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: d})
	if _, err := orch.ReadTwitterAudienceInsights(context.Background(), "p1", model.ProviderTwitterAds, model.MetricsWindowLast7Days); err == nil {
		t.Error("a scope lookup failure must fail the read, not proceed unscoped")
	}
	if d.calls != 0 {
		t.Errorf("X was contacted %d time(s) after the scope could not be established", d.calls)
	}
}

func TestOrchestratorReadTwitterAudience_NilResultAndNilSlice(t *testing.T) {
	ctx := context.Background()
	orch := func(d PlatformDispatcher) *Orchestrator {
		return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"c555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: d})
	}
	if _, err := orch(&twitterAudienceDispatcher{nilOut: true}).ReadTwitterAudienceInsights(ctx, "p1", model.ProviderTwitterAds, model.MetricsWindowLast7Days); err == nil {
		t.Error("a nil result with no error must be rejected")
	}
	ai, err := orch(&twitterAudienceDispatcher{result: &model.TwitterAudienceInsights{Window: model.MetricsWindowLast7Days}}).
		ReadTwitterAudienceInsights(ctx, "p1", model.ProviderTwitterAds, model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadTwitterAudienceInsights: %v", err)
	}
	if ai.Buckets == nil {
		t.Error("Buckets is nil; it must be an empty slice so the wire shape is [] not null")
	}
}
