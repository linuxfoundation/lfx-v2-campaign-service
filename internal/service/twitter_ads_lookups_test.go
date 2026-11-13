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

type mockTwitterAudienceDispatcher struct {
	res       *model.TwitterCustomAudiences
	err       error
	notCalled bool
	calls     int
}

func (m *mockTwitterAudienceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockTwitterAudienceDispatcher) ListTwitterCustomAudiences(context.Context, string, model.Provider) (*model.TwitterCustomAudiences, bool, error) {
	m.calls++
	return m.res, !m.notCalled, m.err
}

func twitterLookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: d}})
	return svc
}

func TestListTwitterAdsCustomAudiences_SortsAndMapsEveryField(t *testing.T) {
	d := &mockTwitterAudienceDispatcher{res: &model.TwitterCustomAudiences{AccountID: "18ce54d4x5t", Truncated: true, Audiences: []model.TwitterCustomAudience{
		{ID: "3c", Name: "registrants", AudienceType: "LIST", ReasonsNotTargetable: []string{"TOO_SMALL"}},
		{ID: "2b", Name: "KubeCon visitors", AudienceType: "WEB", Targetable: true, AudienceSize: i64(1470)},
		{ID: "1a", Name: "Registrants", AudienceType: "LIST", Targetable: true},
	}}}
	res, err := twitterLookupService(d).ListTwitterAdsCustomAudiences(context.Background(), &conn.ListTwitterAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListTwitterAdsCustomAudiences: %v", err)
	}
	if res.AccountID != "18ce54d4x5t" || !res.Truncated || res.AudienceCount != 3 {
		t.Fatalf("res = %+v", res)
	}
	var ids []string
	for _, a := range res.Audiences {
		ids = append(ids, a.ID)
	}
	if fmt.Sprint(ids) != "[2b 1a 3c]" {
		t.Errorf("order = %v, want by name ignoring case, then id", ids)
	}
	b, _ := json.Marshal(res.Audiences[0])
	const want = `{"ID":"2b","Name":"KubeCon visitors","AudienceType":"WEB","Targetable":true,"AudienceSize":1470,"ReasonsNotTargetable":[]}`
	if string(b) != want {
		t.Errorf("row = %s\nwant  %s", b, want)
	}
	if a := res.Audiences[2]; a.AudienceSize != nil || a.Targetable || fmt.Sprint(a.ReasonsNotTargetable) != "[TOO_SMALL]" {
		t.Errorf("row 3 = %+v", a)
	}
}

func TestListTwitterAdsCustomAudiences_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockTwitterAudienceDispatcher{res: &model.TwitterCustomAudiences{AccountID: "acc", Audiences: []model.TwitterCustomAudience{}}}
	res, err := twitterLookupService(d).ListTwitterAdsCustomAudiences(context.Background(), &conn.ListTwitterAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if err != nil || res.Audiences == nil || res.AudienceCount != 0 {
		t.Fatalf("got (%+v, %v), want an empty, non-nil list", res, err)
	}
}

// The deployment gate answers the lookup-unsupported sentinel, which is a 400; an untrustworthy
// answer is 503.
func TestListTwitterAdsCustomAudiences_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"flag off", fmt.Errorf("disabled: %w", domain.ErrTargetingLookupUnsupported), "400"},
		{"malformed upstream", errors.New("x: custom audience list could not be confirmed"), "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockTwitterAudienceDispatcher{err: tc.err, notCalled: true}
			_, err := twitterLookupService(d).ListTwitterAdsCustomAudiences(context.Background(), &conn.ListTwitterAdsCustomAudiencesPayload{ProjectID: "cncf"})
			if errCode(err) != tc.want || d.calls != 1 {
				t.Fatalf("err = %T %v (calls %d), want %s after 1 call", err, err, d.calls, tc.want)
			}
		})
	}
}

func TestListTwitterAdsCustomAudiences_NoCapabilityIs400(t *testing.T) {
	_, err := lookupService(&mockLookupDispatcher{}).ListTwitterAdsCustomAudiences(context.Background(), &conn.ListTwitterAdsCustomAudiencesPayload{ProjectID: "cncf"})
	if errCode(err) != "400" {
		t.Fatalf("err = %T %v, want 400 for a platform with no custom-audience read", err, err)
	}
}

func TestReadTwitterCustomAudiences_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.TwitterCustomAudiences{"nil result": nil, "nil audiences": {AccountID: "acc"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: &mockTwitterAudienceDispatcher{res: res}}}
		if _, err := o.ReadTwitterCustomAudiences(context.Background(), "cncf", model.ProviderTwitterAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only a read that reached X is recorded.
func TestReadTwitterCustomAudiences_RecordsOnlyCallsThatReachedX(t *testing.T) {
	empty := &model.TwitterCustomAudiences{AccountID: "acc", Audiences: []model.TwitterCustomAudience{}}
	for _, tc := range []struct {
		name string
		d    *mockTwitterAudienceDispatcher
		want int
	}{
		{"flag off", &mockTwitterAudienceDispatcher{err: domain.ErrTargetingLookupUnsupported, notCalled: true}, 0},
		{"success", &mockTwitterAudienceDispatcher{res: empty}, 1},
		{"upstream failure", &mockTwitterAudienceDispatcher{err: errors.New("x 500")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderTwitterAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadTwitterCustomAudiences(context.Background(), "cncf", model.ProviderTwitterAds)
			if got := len(rec.upstream); got != tc.want {
				t.Errorf("recorded %d upstream call(s), want %d", got, tc.want)
			}
		})
	}
}
