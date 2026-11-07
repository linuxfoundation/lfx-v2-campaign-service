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

type mockRedditAudienceDispatcher struct {
	res       *model.RedditCustomAudiences
	err       error
	notCalled bool
	calls     int
}

func (m *mockRedditAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockRedditAudienceDispatcher) ListRedditCustomAudiences(context.Context, string, model.Provider) (*model.RedditCustomAudiences, bool, error) {
	m.calls++
	return m.res, !m.notCalled, m.err
}

func redditLookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d}})
	return svc
}

func i64(v int64) *int64 { return &v }

func TestListRedditAdsCustomAudiences_SortsAndMapsEveryField(t *testing.T) {
	d := &mockRedditAudienceDispatcher{res: &model.RedditCustomAudiences{AccountID: "t2_acct", Truncated: true, Audiences: []model.RedditCustomAudience{
		{ID: "3", Name: "registrants", Type: "CUSTOMER_LIST", Status: "PENDING"},
		{ID: "2", Name: "KubeCon visitors", Type: "PIXEL_RETARGETING", Status: "VALID", SizeLower: i64(1000), SizeUpper: i64(5000)},
		{ID: "1", Name: "Registrants", Type: "CUSTOMER_LIST", Status: "VALID"},
	}}}
	res, err := redditLookupService(d).ListRedditAdsCustomAudiences(context.Background(), &conn.ListRedditAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListRedditAdsCustomAudiences: %v", err)
	}
	if res.AccountID != "t2_acct" || !res.Truncated || res.AudienceCount != 3 {
		t.Fatalf("res = %+v", res)
	}
	var ids []string
	for _, a := range res.Audiences {
		ids = append(ids, a.ID)
	}
	if fmt.Sprint(ids) != "[2 1 3]" {
		t.Errorf("order = %v, want by name ignoring case, then id", ids)
	}
	b, _ := json.Marshal(res.Audiences[0])
	const want = `{"ID":"2","Name":"KubeCon visitors","Type":"PIXEL_RETARGETING","Status":"VALID","SizeLower":1000,"SizeUpper":5000}`
	if string(b) != want {
		t.Errorf("row = %s\nwant  %s", b, want)
	}
	if a := res.Audiences[1]; a.SizeLower != nil || a.SizeUpper != nil {
		t.Errorf("absent sizes must stay absent, got %+v", a)
	}
}

func TestListRedditAdsCustomAudiences_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockRedditAudienceDispatcher{res: &model.RedditCustomAudiences{AccountID: "t2_acct", Audiences: []model.RedditCustomAudience{}}}
	res, err := redditLookupService(d).ListRedditAdsCustomAudiences(context.Background(), &conn.ListRedditAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil || res.Audiences == nil || res.AudienceCount != 0 {
		t.Fatalf("got (%+v, %v), want an empty, non-nil list", res, err)
	}
}

// The deployment gate answers the lookup-unsupported sentinel, which is a 400; an untrustworthy
// answer is 503, and a reserved scope never reaches the dispatcher.
func TestListRedditAdsCustomAudiences_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		err     error
		want    string
		calls   int
	}{
		{"flag off", "cncf", fmt.Errorf("disabled: %w", domain.ErrTargetingLookupUnsupported), "400", 1},
		{"malformed upstream", "cncf", errors.New("reddit: custom audience list could not be confirmed"), "503", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockRedditAudienceDispatcher{err: tc.err, notCalled: true}
			_, err := redditLookupService(d).ListRedditAdsCustomAudiences(context.Background(), &conn.ListRedditAdsCustomAudiencesPayload{ProjectID: tc.project})
			if errCode(err) != tc.want || d.calls != tc.calls {
				t.Fatalf("err = %T %v (calls %d), want %s after %d call(s)", err, err, d.calls, tc.want, tc.calls)
			}
		})
	}
}

func TestListRedditAdsCustomAudiences_NoCapabilityIs400(t *testing.T) {
	_, err := lookupService(&mockLookupDispatcher{}).ListRedditAdsCustomAudiences(context.Background(), &conn.ListRedditAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if errCode(err) != "400" {
		t.Fatalf("err = %T %v, want 400 for a platform with no custom-audience read", err, err)
	}
}

func TestReadRedditCustomAudiences_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.RedditCustomAudiences{"nil result": nil, "nil audiences": {AccountID: "t2_acct"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: &mockRedditAudienceDispatcher{res: res}}}
		if _, err := o.ReadRedditCustomAudiences(context.Background(), "cncf", model.ProviderRedditAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only a read that reached Reddit is recorded.
func TestReadRedditCustomAudiences_RecordsOnlyCallsThatReachedReddit(t *testing.T) {
	empty := &model.RedditCustomAudiences{AccountID: "t2_acct", Audiences: []model.RedditCustomAudience{}}
	for _, tc := range []struct {
		name string
		d    *mockRedditAudienceDispatcher
		want int
	}{
		{"flag off", &mockRedditAudienceDispatcher{err: domain.ErrTargetingLookupUnsupported, notCalled: true}, 0},
		{"success", &mockRedditAudienceDispatcher{res: empty}, 1},
		{"upstream failure", &mockRedditAudienceDispatcher{err: errors.New("reddit 500")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadRedditCustomAudiences(context.Background(), "cncf", model.ProviderRedditAds)
			if got := len(rec.upstream); got != tc.want {
				t.Errorf("recorded %d upstream call(s), want %d", got, tc.want)
			}
		})
	}
}
