// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

type mockRedditPixelDispatcher struct {
	res       *model.RedditPixels
	err       error
	notCalled bool
	calls     int
}

func (m *mockRedditPixelDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockRedditPixelDispatcher) ListRedditPixels(context.Context, string, model.Provider) (*model.RedditPixels, bool, error) {
	m.calls++
	return m.res, !m.notCalled, m.err
}

func TestListRedditAdsPixels_SortsAndMapsEveryField(t *testing.T) {
	d := &mockRedditPixelDispatcher{res: &model.RedditPixels{AccountID: "t2_acct", Truncated: true, Pixels: []model.RedditPixel{
		{ID: "t2_c", Name: "beta"},
		{ID: "t2_b", Name: "Gamma", BusinessID: "biz1"},
		{ID: "t2_a", Name: "alpha"},
		{ID: "t2_0", Name: "Beta"},
	}}}
	res, err := redditLookupService(d).ListRedditAdsPixels(context.Background(), &conn.ListRedditAdsPixelsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListRedditAdsPixels: %v", err)
	}
	if res.AccountID != "t2_acct" || !res.Truncated || res.PixelCount != 4 {
		t.Fatalf("res = %+v", res)
	}
	var ids []string
	for _, p := range res.Pixels {
		ids = append(ids, p.ID)
	}
	if fmt.Sprint(ids) != "[t2_a t2_0 t2_c t2_b]" {
		t.Errorf("order = %v, want by name ignoring case, then id", ids)
	}
	if p := res.Pixels[3]; p.BusinessID == nil || *p.BusinessID != "biz1" || p.Name != "Gamma" {
		t.Errorf("last = %+v", p)
	}
	if p := res.Pixels[0]; p.BusinessID != nil {
		t.Errorf("an absent business_id must stay absent, got %+v", p)
	}
}

func TestListRedditAdsPixels_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockRedditPixelDispatcher{res: &model.RedditPixels{AccountID: "t2_acct", Pixels: []model.RedditPixel{}}}
	res, err := redditLookupService(d).ListRedditAdsPixels(context.Background(), &conn.ListRedditAdsPixelsPayload{ProjectID: "cncf"})
	if err != nil || res.Pixels == nil || res.PixelCount != 0 {
		t.Fatalf("got (%+v, %v), want an empty, non-nil list", res, err)
	}
}

// The deployment gate answers the lookup-unsupported sentinel, which is a 400; an untrustworthy
// answer is 503; a platform without the capability is 400.
func TestListRedditAdsPixels_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"flag off", fmt.Errorf("disabled: %w", domain.ErrTargetingLookupUnsupported), "400"},
		{"malformed upstream", errors.New("reddit: pixel list could not be confirmed"), "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockRedditPixelDispatcher{err: tc.err, notCalled: true}
			_, err := redditLookupService(d).ListRedditAdsPixels(context.Background(), &conn.ListRedditAdsPixelsPayload{ProjectID: "cncf"})
			if errCode(err) != tc.want || d.calls != 1 {
				t.Fatalf("err = %T %v (calls %d), want %s", err, err, d.calls, tc.want)
			}
		})
	}
	_, err := lookupService(&mockLookupDispatcher{}).ListRedditAdsPixels(context.Background(), &conn.ListRedditAdsPixelsPayload{ProjectID: "cncf"})
	if errCode(err) != "400" {
		t.Fatalf("err = %T %v, want 400 for a platform with no pixel read", err, err)
	}
}

func TestReadRedditPixels_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.RedditPixels{"nil result": nil, "nil pixels": {AccountID: "t2_acct"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: &mockRedditPixelDispatcher{res: res}}}
		if _, err := o.ReadRedditPixels(context.Background(), "cncf", model.ProviderRedditAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only a read that reached Reddit is recorded.
func TestReadRedditPixels_RecordsOnlyCallsThatReachedReddit(t *testing.T) {
	empty := &model.RedditPixels{AccountID: "t2_acct", Pixels: []model.RedditPixel{}}
	for _, tc := range []struct {
		name string
		d    *mockRedditPixelDispatcher
		want int
	}{
		{"flag off", &mockRedditPixelDispatcher{err: domain.ErrTargetingLookupUnsupported, notCalled: true}, 0},
		{"success", &mockRedditPixelDispatcher{res: empty}, 1},
		{"upstream failure", &mockRedditPixelDispatcher{err: errors.New("reddit 500")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadRedditPixels(context.Background(), "cncf", model.ProviderRedditAds)
			if got := len(rec.upstream); got != tc.want {
				t.Errorf("recorded %d upstream call(s), want %d", got, tc.want)
			}
		})
	}
}
