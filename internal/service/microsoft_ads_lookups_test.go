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

type mockMicrosoftListDispatcher struct {
	res       *model.MicrosoftRemarketingLists
	err       error
	notCalled bool
	calls     int
}

func (m *mockMicrosoftListDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockMicrosoftListDispatcher) ListMicrosoftRemarketingLists(context.Context, string, model.Provider) (*model.MicrosoftRemarketingLists, bool, error) {
	m.calls++
	return m.res, !m.notCalled, m.err
}

func microsoftLookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: d}})
	return svc
}

func TestListMicrosoftAdsRemarketingLists_SortsAndMapsEveryField(t *testing.T) {
	d := &mockMicrosoftListDispatcher{res: &model.MicrosoftRemarketingLists{AccountID: "1234567", Lists: []model.MicrosoftRemarketingList{
		{ID: "30", Name: "registrants"},
		{ID: "20", Name: "KubeCon visitors", Description: "30d", Scope: "Account", MembershipDuration: intp(30), SearchSize: i64(12000)},
		{ID: "10", Name: "Registrants", Scope: "Customer"},
	}}}
	res, err := microsoftLookupService(d).ListMicrosoftAdsRemarketingLists(context.Background(), &conn.ListMicrosoftAdsRemarketingListsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListMicrosoftAdsRemarketingLists: %v", err)
	}
	if res.AccountID != "1234567" || res.ListCount != 3 {
		t.Fatalf("res = %+v", res)
	}
	var ids []string
	for _, l := range res.Lists {
		ids = append(ids, l.ID)
	}
	if fmt.Sprint(ids) != "[20 10 30]" {
		t.Errorf("order = %v, want by name ignoring case, then id", ids)
	}
	b, _ := json.Marshal(res.Lists[0])
	const want = `{"ID":"20","Name":"KubeCon visitors","Description":"30d","Scope":"Account","MembershipDuration":30,"SearchSize":12000}`
	if string(b) != want {
		t.Errorf("row = %s\nwant  %s", b, want)
	}
	if l := res.Lists[2]; l.MembershipDuration != nil || l.SearchSize != nil {
		t.Errorf("row 3 = %+v", l)
	}
}

func TestListMicrosoftAdsRemarketingLists_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockMicrosoftListDispatcher{res: &model.MicrosoftRemarketingLists{AccountID: "acc", Lists: []model.MicrosoftRemarketingList{}}}
	res, err := microsoftLookupService(d).ListMicrosoftAdsRemarketingLists(context.Background(), &conn.ListMicrosoftAdsRemarketingListsPayload{ProjectID: "cncf"})
	if err != nil || res.Lists == nil || res.ListCount != 0 {
		t.Fatalf("got (%+v, %v), want an empty, non-nil list", res, err)
	}
}

// The deployment gate answers the lookup-unsupported sentinel, which is a 400; an untrustworthy
// answer is 503.
func TestListMicrosoftAdsRemarketingLists_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"flag off", fmt.Errorf("disabled: %w", domain.ErrTargetingLookupUnsupported), "400"},
		{"malformed upstream", errors.New("microsoft-ads remarketing list read could not be confirmed"), "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockMicrosoftListDispatcher{err: tc.err, notCalled: true}
			_, err := microsoftLookupService(d).ListMicrosoftAdsRemarketingLists(context.Background(), &conn.ListMicrosoftAdsRemarketingListsPayload{ProjectID: "cncf"})
			if errCode(err) != tc.want || d.calls != 1 {
				t.Fatalf("err = %T %v (calls %d), want %s after 1 call", err, err, d.calls, tc.want)
			}
		})
	}
}

func TestListMicrosoftAdsRemarketingLists_NoCapabilityIs400(t *testing.T) {
	_, err := microsoftLookupService(&mockLookupDispatcher{}).ListMicrosoftAdsRemarketingLists(context.Background(), &conn.ListMicrosoftAdsRemarketingListsPayload{ProjectID: "cncf"})
	if errCode(err) != "400" {
		t.Fatalf("err = %T %v, want 400 for a platform with no remarketing-list read", err, err)
	}
}

func TestReadMicrosoftRemarketingLists_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.MicrosoftRemarketingLists{"nil result": nil, "nil lists": {AccountID: "acc"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: &mockMicrosoftListDispatcher{res: res}}}
		if _, err := o.ReadMicrosoftRemarketingLists(context.Background(), "cncf", model.ProviderMicrosoftAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only a read that reached Microsoft is recorded.
func TestReadMicrosoftRemarketingLists_RecordsOnlyCallsThatReachedMicrosoft(t *testing.T) {
	empty := &model.MicrosoftRemarketingLists{AccountID: "acc", Lists: []model.MicrosoftRemarketingList{}}
	for _, tc := range []struct {
		name string
		d    *mockMicrosoftListDispatcher
		want int
	}{
		{"flag off", &mockMicrosoftListDispatcher{err: domain.ErrTargetingLookupUnsupported, notCalled: true}, 0},
		{"success", &mockMicrosoftListDispatcher{res: empty}, 1},
		{"upstream failure", &mockMicrosoftListDispatcher{err: errors.New("microsoft 500")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadMicrosoftRemarketingLists(context.Background(), "cncf", model.ProviderMicrosoftAds)
			if got := len(rec.upstream); got != tc.want {
				t.Errorf("recorded %d upstream call(s), want %d", got, tc.want)
			}
		})
	}
}

type mockMicrosoftGoalDispatcher struct {
	res       *model.MicrosoftConversionGoals
	err       error
	notCalled bool
	calls     int
}

func (m *mockMicrosoftGoalDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockMicrosoftGoalDispatcher) ListMicrosoftConversionGoals(context.Context, string, model.Provider) (*model.MicrosoftConversionGoals, bool, error) {
	m.calls++
	return m.res, !m.notCalled, m.err
}

func TestListMicrosoftAdsConversionGoals_SortsAndMapsEveryField(t *testing.T) {
	d := &mockMicrosoftGoalDispatcher{res: &model.MicrosoftConversionGoals{AccountID: "1234567", Goals: []model.MicrosoftConversionGoal{
		{ID: "30", Name: "registration", Status: "Paused"},
		{ID: "20", Name: "KubeCon purchase", Status: "Active", Type: "Event", Scope: "Customer"},
		{ID: "10", Name: "Registration", Status: "Active"},
	}}}
	res, err := microsoftLookupService(d).ListMicrosoftAdsConversionGoals(context.Background(), &conn.ListMicrosoftAdsConversionGoalsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListMicrosoftAdsConversionGoals: %v", err)
	}
	if res.AccountID != "1234567" || res.GoalCount != 3 {
		t.Fatalf("res = %+v", res)
	}
	var ids []string
	for _, g := range res.Goals {
		ids = append(ids, g.ID)
	}
	if fmt.Sprint(ids) != "[20 10 30]" {
		t.Errorf("order = %v, want by name ignoring case, then id", ids)
	}
	b, _ := json.Marshal(res.Goals[0])
	const want = `{"ID":"20","Name":"KubeCon purchase","Status":"Active","Type":"Event","Scope":"Customer"}`
	if string(b) != want {
		t.Errorf("row = %s\nwant  %s", b, want)
	}
}

func TestListMicrosoftAdsConversionGoals_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    PlatformDispatcher
		want string
	}{
		{"flag off", &mockMicrosoftGoalDispatcher{err: fmt.Errorf("disabled: %w", domain.ErrTargetingLookupUnsupported), notCalled: true}, "400"},
		{"malformed upstream", &mockMicrosoftGoalDispatcher{err: errors.New("microsoft-ads conversion goal read could not be confirmed")}, "503"},
		{"no capability", &mockLookupDispatcher{}, "400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := microsoftLookupService(tc.d).ListMicrosoftAdsConversionGoals(context.Background(), &conn.ListMicrosoftAdsConversionGoalsPayload{ProjectID: "cncf"})
			if errCode(err) != tc.want {
				t.Fatalf("err = %T %v, want %s", err, err, tc.want)
			}
		})
	}
	for name, res := range map[string]*model.MicrosoftConversionGoals{"nil result": nil, "nil goals": {AccountID: "acc"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: &mockMicrosoftGoalDispatcher{res: res}}}
		if _, err := o.ReadMicrosoftConversionGoals(context.Background(), "cncf", model.ProviderMicrosoftAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}
