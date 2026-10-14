// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// redditAudienceDispatcher implements ONLY RedditAudienceReader (plus Dispatch). calls, gotWindow
// and gotScope let a test assert whether, and with what, Reddit was contacted.
type redditAudienceDispatcher struct {
	disabled  error
	err       error
	result    *model.RedditAudienceInsights
	nilOut    bool
	calls     int
	gotWindow model.MetricsWindow
	gotScope  []model.ProjectCampaignScope
}

func (d *redditAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *redditAudienceDispatcher) AudienceEnabled() error { return d.disabled }

func (d *redditAudienceDispatcher) ReadRedditAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.RedditAudienceInsights, error) {
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
	return &model.RedditAudienceInsights{Window: w, Buckets: []model.RedditAudienceBucket{
		{Dimension: model.RedditAudienceDimensionGender, Value: "female", Impressions: 11, Clicks: 3, CostMicros: 4_000_000, Ctr: 3.0 / 11},
		{Dimension: model.RedditAudienceDimensionGender, Value: "", Impressions: 5, Clicks: 1, CostMicros: 1_000_000, Ctr: 0.2},
		{Dimension: model.RedditAudienceDimensionCommunity, Value: "kubernetes", Impressions: 7, Clicks: 1, CostMicros: 2_000_000, Ctr: 1.0 / 7},
	}}, nil
}

func redditAudienceService(t *testing.T, d PlatformDispatcher, scopeIDs ...string) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{scopeIDs: scopeIDs}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d}))
	return svc
}

func TestGetRedditAdsAudience_HappyPath(t *testing.T) {
	d := &redditAudienceDispatcher{}
	w := "this_month"
	res, err := redditAudienceService(t, d, "t3_c").GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf", Window: &w})
	if err != nil {
		t.Fatalf("GetRedditAdsAudience: %v", err)
	}
	if res.Window != w || res.BucketCount != 3 || len(res.Buckets) != 3 {
		t.Fatalf("res = %+v", res)
	}
	g, unattributed, c := res.Buckets[0], res.Buckets[1], res.Buckets[2]
	if g.Dimension != "gender" || g.Value == nil || *g.Value != "female" || g.Impressions != 11 || g.Clicks != 3 || g.CostMicros != 4_000_000 || g.Ctr != 3.0/11 {
		t.Errorf("gender bucket = %+v", g)
	}
	// Reddit's explicit null is published with NO value, never as an empty string.
	if unattributed.Value != nil || unattributed.Impressions != 5 {
		t.Errorf("unattributed bucket = %+v, want value absent", unattributed)
	}
	if c.Dimension != "community" || c.Value == nil || *c.Value != "kubernetes" {
		t.Errorf("community bucket = %+v", c)
	}
	if len(d.gotScope) != 1 || d.gotScope[0].PlatformCampaignID != "t3_c" || d.gotWindow != model.MetricsWindowThisMonth {
		t.Errorf("scope/window = %+v/%q, want the project's own campaign over this_month", d.gotScope, d.gotWindow)
	}
}

func TestGetRedditAdsAudience_DefaultWindowIsLast30Days(t *testing.T) {
	d := &redditAudienceDispatcher{}
	res, err := redditAudienceService(t, d, "t3_c").GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetRedditAdsAudience: %v", err)
	}
	if d.gotWindow != model.MetricsWindowLast30Days || res.Window != "last_30_days" {
		t.Errorf("window = %q / %q, want last_30_days", d.gotWindow, res.Window)
	}
}

// A project with no Reddit campaigns of its own: 200 with an empty array, Reddit never contacted.
func TestGetRedditAdsAudience_EmptyScopeIs200EmptyWithoutUpstreamCall(t *testing.T) {
	d := &redditAudienceDispatcher{}
	res, err := redditAudienceService(t, d).GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetRedditAdsAudience: %v", err)
	}
	if d.calls != 0 {
		t.Errorf("Reddit was contacted %d time(s) with an empty scope", d.calls)
	}
	if res.Buckets == nil || len(res.Buckets) != 0 || res.BucketCount != 0 {
		t.Errorf("res = %+v, want an empty []", res)
	}
}

// The system scope is 404; yesterday and last_14_days (which the Reddit report client cannot map)
// are 400 with one fixed message; neither reaches Reddit.
func TestGetRedditAdsAudience_RejectsSystemScopeAndUnmappedWindows(t *testing.T) {
	d := &redditAudienceDispatcher{}
	svc := redditAudienceService(t, d, "t3_c")
	if _, err := svc.GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Error("system scope: expected 404")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("system scope: error = %T (%v), want *conn.NotFoundError", err, err)
	}
	for _, w := range []string{"yesterday", "last_14_days", "next_tuesday"} {
		_, err := svc.GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf", Window: &w})
		br, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Errorf("%s: error = %T (%v), want *conn.BadRequestError", w, err, err)
			continue
		}
		if br.Message != "window must be one of: last_30_days, last_7_days, last_month, this_month, today" {
			t.Errorf("%s: message = %q", w, br.Message)
		}
	}
	if d.calls != 0 {
		t.Errorf("Reddit was contacted %d time(s) for refused requests", d.calls)
	}
}

func TestGetRedditAdsAudience_ErrorMapping(t *testing.T) {
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
		{"no own connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", errors.Join(domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), &conn.BadRequestError{}},
		{"upstream failure (5xx/401/403/429/malformed)", errors.New("reddit API POST /ad_accounts/t2_acct/reports -> 403 CANARY"), &conn.ConnServiceUnavailableError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := redditAudienceService(t, &redditAudienceDispatcher{err: tc.err}, "t3_c")
			_, err := svc.GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
			assertInsightsErr(t, err, tc.want, tc.err)
			// No upstream text and no account id reaches the client body.
			if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "t2_acct") {
				t.Errorf("client error echoes upstream text: %v", err)
			}
		})
	}
}

// A campaign that records no creating account is a 409 with the re-dispatch remedy — not the
// reconnect remedy an actual mismatch gets, though the two sentinels arrive joined.
func TestGetRedditAdsAudience_UnknownProvenanceIs409ReDispatch(t *testing.T) {
	err := fmt.Errorf("2 of 3 campaigns CANARY: %w", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	_, got := redditAudienceService(t, &redditAudienceDispatcher{err: err}, "t3_c").
		GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
	ce, ok := got.(*conn.ConflictError)
	if !ok || !strings.Contains(ce.Message, "do not record which ad account") || strings.Contains(ce.Message, "CANARY") {
		t.Fatalf("error = %T (%v), want the fixed provenance 409", got, got)
	}
}

// A scope with BOTH faults is a 409 naming both remedies — re-dispatching the unrecorded campaigns
// and reconciling the foreign ones — not the single-fault text of either arm.
func TestGetRedditAdsAudience_MixedProvenanceNamesBothRemedies(t *testing.T) {
	err := fmt.Errorf("1 foreign and 1 unrecorded CANARY: %w",
		errors.Join(domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	_, got := redditAudienceService(t, &redditAudienceDispatcher{err: err}, "t3_c").
		GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
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

// The 409s carry the sentinels' fixed text, nothing from the wrapped detail.
func TestGetRedditAdsAudience_ScopeConflictsUseFixedText(t *testing.T) {
	for _, sentinel := range []error{domain.ErrAudienceScopeTooLarge, domain.ErrAudienceScopeInvalid} {
		svc := redditAudienceService(t, &redditAudienceDispatcher{err: fmt.Errorf("%w: 9 campaigns CANARY", sentinel)}, "t3_c")
		_, err := svc.GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
		ce, ok := err.(*conn.ConflictError)
		if !ok || ce.Message != sentinel.Error() {
			t.Errorf("error = %T (%v), want the fixed 409 %q", err, err, sentinel.Error())
		}
	}
}

// The flag gate is checked BEFORE the scope lookup: a project with no Reddit campaigns gets the
// same 400 as any other while the read is off, not a 200 with empty buckets.
func TestGetRedditAdsAudience_DisabledIs400EvenWithEmptyScope(t *testing.T) {
	for name, scope := range map[string][]string{"empty scope": nil, "with campaigns": {"t3_c"}} {
		t.Run(name, func(t *testing.T) {
			d := &redditAudienceDispatcher{disabled: domain.ErrKeywordInsightsUnsupported}
			_, err := redditAudienceService(t, d, scope...).GetRedditAdsAudience(context.Background(), &conn.GetRedditAdsAudiencePayload{ProjectID: "cncf"})
			if _, ok := err.(*conn.BadRequestError); !ok {
				t.Fatalf("error = %T (%v), want *conn.BadRequestError", err, err)
			}
			if d.calls != 0 {
				t.Errorf("the read ran %d time(s) while disabled", d.calls)
			}
		})
	}
}

// Only Reddit implements RedditAudienceReader: every other platform is 400 not supported, and the
// other audience reads do not reach the Reddit dispatcher.
func TestOrchestratorReadRedditAudience_UnsupportedPlatforms(t *testing.T) {
	ctx := context.Background()
	for _, p := range []model.Provider{model.ProviderMicrosoftAds, model.ProviderTwitterAds, model.ProviderMetaAds, model.ProviderGoogleAds, model.ProviderLinkedInAds} {
		orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"c555"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{p: upstreamCapableDispatcher{}})
		if _, err := orch.ReadRedditAudienceInsights(ctx, "p1", p, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: error = %v, want ErrKeywordInsightsUnsupported", p, err)
		}
	}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{})
	if _, err := orch.ReadRedditAudienceInsights(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("unregistered: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
	d := &redditAudienceDispatcher{}
	orch = NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"t3_c"}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})
	if _, err := orch.ReadMetaAudienceInsights(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("meta read on reddit: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
	if _, err := orch.ReadTwitterAudienceInsights(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast7Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("x read on reddit: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
	if d.calls != 0 {
		t.Errorf("a sibling read reached the Reddit dispatcher %d time(s)", d.calls)
	}
}

func TestOrchestratorReadRedditAudience_ScopeLookupFailureDoesNotFallBack(t *testing.T) {
	d := &redditAudienceDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})
	if _, err := orch.ReadRedditAudienceInsights(context.Background(), "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a scope lookup failure must fail the read, not proceed unscoped")
	}
	if d.calls != 0 {
		t.Errorf("Reddit was contacted %d time(s) after the scope could not be established", d.calls)
	}
}

func TestOrchestratorReadRedditAudience_NilResultAndNilSlice(t *testing.T) {
	ctx := context.Background()
	orch := func(d PlatformDispatcher) *Orchestrator {
		return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"t3_c"}}, newFakeJobRepo(),
			map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})
	}
	if _, err := orch(&redditAudienceDispatcher{nilOut: true}).ReadRedditAudienceInsights(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a nil result with no error must be rejected")
	}
	ai, err := orch(&redditAudienceDispatcher{result: &model.RedditAudienceInsights{Window: model.MetricsWindowLast30Days}}).
		ReadRedditAudienceInsights(ctx, "p1", model.ProviderRedditAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadRedditAudienceInsights: %v", err)
	}
	if ai.Buckets == nil {
		t.Error("Buckets is nil; it must be an empty slice so the wire shape is [] not null")
	}
}

// TestPublishedRedditAudienceExamplesArePossible pins that every example Goa publishes for the
// Reddit audience read (all four generated documents) is a response the endpoint can return: known
// dimension, value inside the read's charset, ctr = clicks/impressions, dimensions in order, and
// bucket_count == len(buckets) with a window Reddit serves.
func TestPublishedRedditAudienceExamplesArePossible(t *testing.T) {
	rank := map[string]int{"gender": 0, "country": 1, "community": 2}
	valueRE := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,63}$`)
	checkBucket := func(path string, b map[string]any) {
		dim, _ := b["dimension"].(string)
		if _, ok := rank[dim]; !ok {
			t.Errorf("%s: dimension %q", path, dim)
		}
		if v, ok := b["value"]; ok {
			if s, _ := v.(string); !valueRE.MatchString(s) {
				t.Errorf("%s: value %v outside the charset", path, v)
			}
		}
		imp, _ := b["impressions"].(float64)
		clk, _ := b["clicks"].(float64)
		ctr, _ := b["ctr"].(float64)
		want := 0.0
		if imp > 0 {
			want = clk / imp
		}
		if math.Abs(ctr-want) > 1e-9 {
			t.Errorf("%s: ctr %v, want %v", path, ctr, want)
		}
	}
	for _, rel := range twitterAudienceSpecs {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			sawEnvelope, sawBucket := false, false
			var walk func(path string, v any, inScope bool)
			walk = func(path string, v any, inScope bool) {
				switch n := v.(type) {
				case map[string]any:
					for k, child := range n {
						childScope := inScope || strings.Contains(k, "RedditAdsAudience") || strings.Contains(k, "reddit-ads/audience")
						if k == "example" && childScope {
							walkTwitterAudienceValues(child, func(x any) {
								m, ok := x.(map[string]any)
								if !ok {
									return
								}
								if _, ok := m["dimension"]; ok {
									sawBucket = true
									checkBucket(path, m)
								}
								if bs, ok := m["buckets"].([]any); ok {
									sawEnvelope = true
									if c, _ := m["bucket_count"].(float64); int(c) != len(bs) {
										t.Errorf("%s: bucket_count %v beside %d buckets", path, m["bucket_count"], len(bs))
									}
									if w, _ := m["window"].(string); !redditAudienceWindows[model.MetricsWindow(w)] {
										t.Errorf("%s: window %q", path, w)
									}
									last := -1
									for _, b := range bs {
										bm, _ := b.(map[string]any)
										r := rank[fmt.Sprint(bm["dimension"])]
										if r < last {
											t.Errorf("%s: dimensions out of order", path)
										}
										last = r
									}
								}
							})
							continue
						}
						walk(path+"/"+k, child, childScope)
					}
				case []any:
					for i, child := range n {
						walk(fmt.Sprintf("%s[%d]", path, i), child, inScope)
					}
				}
			}
			walk("", doc, false)
			if !sawEnvelope || !sawBucket {
				t.Errorf("found envelope=%v bucket=%v examples; the walk no longer reaches the Reddit audience schemas", sawEnvelope, sawBucket)
			}
		})
	}
}
