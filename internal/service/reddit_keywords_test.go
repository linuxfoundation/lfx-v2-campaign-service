// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// redditKeywordDispatcher implements ONLY RedditKeywordReader (plus Dispatch).
type redditKeywordDispatcher struct {
	disabled  error
	err       error
	result    *model.RedditKeywordPerformance
	nilOut    bool
	calls     int
	gotWindow model.MetricsWindow
	gotScope  []model.ProjectCampaignScope
}

func (d *redditKeywordDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *redditKeywordDispatcher) KeywordsEnabled() error { return d.disabled }

func (d *redditKeywordDispatcher) ReadRedditKeywordPerformance(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.RedditKeywordPerformance, error) {
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
	return &model.RedditKeywordPerformance{Window: w, Truncated: true, Rows: []model.RedditKeywordRow{
		{CampaignID: "t3_c", Text: "service mesh", Impressions: 11, Clicks: 3, CostMicros: 4_000_000, Ctr: 3.0 / 11},
		{CampaignID: "t3_c", Text: "", Impressions: 5, Clicks: 1, CostMicros: 1_000_000, Ctr: 0.2},
	}}, nil
}

func redditKeywordService(t *testing.T, d PlatformDispatcher, scopeIDs ...string) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{scopeIDs: scopeIDs}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d}))
	return svc
}

func TestGetRedditAdsKeywords_HappyPath(t *testing.T) {
	d := &redditKeywordDispatcher{}
	w := "last_month"
	res, err := redditKeywordService(t, d, "t3_c").GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf", Window: &w})
	if err != nil {
		t.Fatalf("GetRedditAdsKeywords: %v", err)
	}
	if res.Window != w || res.RowCount != 2 || len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("res = %+v", res)
	}
	r0, r1 := res.Rows[0], res.Rows[1]
	if r0.CampaignID != "t3_c" || r0.Text == nil || *r0.Text != "service mesh" || r0.Impressions != 11 || r0.Clicks != 3 || r0.CostMicros != 4_000_000 || r0.Ctr != 3.0/11 {
		t.Errorf("row 0 = %+v", r0)
	}
	if r1.Text != nil {
		t.Errorf("row 1 text = %q, want absent for Reddit's null keyword", *r1.Text)
	}
	if len(d.gotScope) != 1 || d.gotScope[0].PlatformCampaignID != "t3_c" || d.gotWindow != model.MetricsWindowLastMonth {
		t.Errorf("scope/window = %+v/%q", d.gotScope, d.gotWindow)
	}
}

func TestGetRedditAdsKeywords_DefaultWindowAndEmptyScope(t *testing.T) {
	d := &redditKeywordDispatcher{}
	res, err := redditKeywordService(t, d).GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetRedditAdsKeywords: %v", err)
	}
	if d.calls != 0 {
		t.Errorf("Reddit was contacted %d time(s) with an empty scope", d.calls)
	}
	if res.Window != "last_30_days" || res.Rows == nil || len(res.Rows) != 0 || res.RowCount != 0 || res.Truncated {
		t.Errorf("res = %+v, want an empty last_30_days result", res)
	}
}

func TestGetRedditAdsKeywords_RejectsSystemScopeAndUnmappedWindows(t *testing.T) {
	d := &redditKeywordDispatcher{}
	svc := redditKeywordService(t, d, "t3_c")
	if _, err := svc.GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Error("system scope: expected 404")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("system scope: error = %T", err)
	}
	for _, w := range []string{"yesterday", "last_14_days"} {
		_, err := svc.GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf", Window: &w})
		if br, ok := err.(*conn.BadRequestError); !ok || br.Message != redditAudienceWindowMessage {
			t.Errorf("%s: error = %T (%v), want the fixed Reddit window 400", w, err, err)
		}
	}
	if d.calls != 0 {
		t.Errorf("Reddit was contacted %d time(s) for refused requests", d.calls)
	}
}

func TestGetRedditAdsKeywords_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want any
	}{
		{"unsupported or flag off", domain.ErrKeywordInsightsUnsupported, &conn.BadRequestError{}},
		{"window unsupported", domain.ErrMetricsWindowUnsupported, &conn.BadRequestError{}},
		{"a campaign in another account", domain.ErrCampaignAccountMismatch, &conn.ConflictError{}},
		{"scope too large", domain.ErrKeywordReportScopeTooLarge, &conn.ConflictError{}},
		{"scope invalid", domain.ErrKeywordReportScopeInvalid, &conn.ConflictError{}},
		{"no own connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", errors.Join(domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), &conn.BadRequestError{}},
		{"upstream failure", errors.New("reddit API POST /ad_accounts/t2_acct/reports -> 500 CANARY"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := redditKeywordService(t, &redditKeywordDispatcher{err: tc.err}, "t3_c").
				GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf"})
			assertInsightsErr(t, err, tc.want, tc.err)
			if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "t2_acct") {
				t.Errorf("client error echoes upstream text: %v", err)
			}
		})
	}
	for _, sentinel := range []error{domain.ErrKeywordReportScopeTooLarge, domain.ErrKeywordReportScopeInvalid} {
		_, err := redditKeywordService(t, &redditKeywordDispatcher{err: fmt.Errorf("%w: CANARY", sentinel)}, "t3_c").
			GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf"})
		if ce, ok := err.(*conn.ConflictError); !ok || ce.Message != sentinel.Error() {
			t.Errorf("error = %T (%v), want the fixed 409 %q", err, err, sentinel.Error())
		}
	}
}

// The Mixed provenance arm applies to the keyword read as to the audience read: a scope with both
// faults is a 409 naming both remedies, not either single-fault text, and never the wrapped detail.
func TestGetRedditAdsKeywords_MixedProvenanceNamesBothRemedies(t *testing.T) {
	err := fmt.Errorf("1 foreign and 1 unrecorded CANARY: %w",
		errors.Join(domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	_, got := redditKeywordService(t, &redditKeywordDispatcher{err: err}, "t3_c").
		GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf"})
	ce, ok := got.(*conn.ConflictError)
	if !ok {
		t.Fatalf("error = %T (%v), want a 409", got, got)
	}
	for _, want := range []string{"different ad account", "do not record which ad account", "re-dispatch the unrecorded campaigns", "reconcile"} {
		if !strings.Contains(ce.Message, want) {
			t.Errorf("message %q does not contain %q", ce.Message, want)
		}
	}
	if strings.Contains(ce.Message, "CANARY") {
		t.Errorf("message echoes the wrapped detail: %q", ce.Message)
	}
}

func TestGetRedditAdsKeywords_DisabledIs400EvenWithEmptyScope(t *testing.T) {
	for name, scope := range map[string][]string{"empty scope": nil, "with campaigns": {"t3_c"}} {
		t.Run(name, func(t *testing.T) {
			d := &redditKeywordDispatcher{disabled: domain.ErrKeywordInsightsUnsupported}
			_, err := redditKeywordService(t, d, scope...).GetRedditAdsKeywords(context.Background(), &conn.GetRedditAdsKeywordsPayload{ProjectID: "cncf"})
			if _, ok := err.(*conn.BadRequestError); !ok {
				t.Fatalf("error = %T (%v), want *conn.BadRequestError", err, err)
			}
			if d.calls != 0 {
				t.Errorf("the read ran %d time(s) while disabled", d.calls)
			}
		})
	}
}

func TestOrchestratorReadRedditKeywords_UnsupportedScopeFailureAndNil(t *testing.T) {
	ctx := context.Background()
	for _, p := range []model.Provider{model.ProviderMicrosoftAds, model.ProviderTwitterAds, model.ProviderMetaAds, model.ProviderGoogleAds, model.ProviderLinkedInAds} {
		orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"c555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{p: upstreamCapableDispatcher{}})
		if _, err := orch.ReadRedditKeywordPerformance(ctx, "p1", p, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: error = %v, want ErrKeywordInsightsUnsupported", p, err)
		}
	}
	// The audience reader alone does not serve keywords.
	orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"t3_c"}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: &redditAudienceDispatcher{}})
	if _, err := orch.ReadRedditKeywordPerformance(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("audience-only dispatcher: error = %v", err)
	}
	d := &redditKeywordDispatcher{}
	orch = NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})
	if _, err := orch.ReadRedditKeywordPerformance(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); err == nil || d.calls != 0 {
		t.Errorf("scope failure: err = %v, calls = %d; want a failure and no read", err, d.calls)
	}
	mk := func(d PlatformDispatcher) *Orchestrator {
		return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"t3_c"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})
	}
	if _, err := mk(&redditKeywordDispatcher{nilOut: true}).ReadRedditKeywordPerformance(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a nil result with no error must be rejected")
	}
	kp, err := mk(&redditKeywordDispatcher{result: &model.RedditKeywordPerformance{Window: model.MetricsWindowLast30Days}}).
		ReadRedditKeywordPerformance(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days)
	if err != nil || kp.Rows == nil {
		t.Errorf("kp = %+v, err = %v; want a non-nil empty slice", kp, err)
	}
}
