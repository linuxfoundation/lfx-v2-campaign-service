// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

const msSettingsQueryPath = "/Campaigns/QueryByIds"

// msSettingsDispatcher wires a MicrosoftDispatcher against a fake API that answers the QueryByIds
// read with (status, body), with a pinned readback clock.
func msSettingsDispatcher(t *testing.T, status int, body string) (*MicrosoftDispatcher, *settingsAPI, func() int32) {
	t.Helper()
	api := newSettingsAPI(t, map[string]settingsRoute{msSettingsQueryPath: {status: status, body: body}})
	tokURL, tokens := newSettingsTokenServer(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokURL), microsoft.WithBaseURL(api.srv.URL))
	d.settingsNow = pinnedSettingsClock
	return d, api, tokens
}

// msSettingsRow is a created row: provenance matching activeMicrosoftConn (account 1234567), a
// daily budget and the composed name. Microsoft's create path records no flight dates.
func msSettingsRow() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		CampaignName: "KubeCon — Search",
		BudgetAmount: floatPtr(75.25), BudgetType: budgetTypePtr(model.BudgetDaily),
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987"}`),
	}
}

func msSettingsBody(fields string) string {
	return `{"Campaigns":[{"Id":321` + fields + `}],"PartialErrors":[]}`
}

const msSettingsAgreeing = `,"Name":"KubeCon — Search","Status":"Paused","BudgetType":"DailyBudgetStandard","DailyBudget":75.25,"BudgetId":null,"BiddingScheme":{"Type":"EnhancedCpcBiddingScheme"}`

func TestMicrosoft_ReadSettings_MatchWhenBothAgree(t *testing.T) {
	d, api, _ := msSettingsDispatcher(t, http.StatusOK, msSettingsBody(msSettingsAgreeing))
	row := msSettingsRow()
	before := cloneCampaign(row)

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"75.25", "75.25", model.SettingsMatch},
		settingsFieldBudgetType:      {"daily", "daily", model.SettingsMatch},
		settingsFieldName:            {"KubeCon — Search", "KubeCon — Search", model.SettingsMatch},
		settingsFieldStatus:          {"", "Paused", model.SettingsUnknown},
		settingsFieldBudgetShared:    {"", "false", model.SettingsUnknown},
		settingsFieldBiddingStrategy: {"", "EnhancedCpc", model.SettingsUnknown},
	})
	if rb.CampaignID != "camp-1" || rb.PlatformCampaignID != "321" || rb.Platform != model.ProviderMicrosoftAds || !rb.ReadAt.Equal(settingsClock) {
		t.Errorf("readback header = %+v", rb)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one read, got %+v", reqs)
	}
	assertSettingsReadOnly(t, reqs, msSettingsQueryPath)
	assertRowUntouched(t, before, row)
}

func TestMicrosoft_ReadSettings_Divergences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		want   map[string]fieldWant
	}{
		{
			name:   "budget and name changed upstream",
			fields: `,"Name":"Renamed","Status":"Active","BudgetType":"DailyBudgetStandard","DailyBudget":80`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"75.25", "80.00", model.SettingsDiverged},
				settingsFieldName:         {"KubeCon — Search", "Renamed", model.SettingsDiverged},
			},
		},
		{
			// The formatter must not round a sub-cent upstream amount into the recorded one.
			name:   "sub-cent upstream budget is not rounded into a match",
			fields: `,"BudgetType":"DailyBudgetStandard","DailyBudget":75.254`,
			want:   map[string]fieldWant{settingsFieldBudgetAmount: {"75.25", "75.254", model.SettingsDiverged}},
		},
		{
			// A lifetime type makes DailyBudget a different quantity: the type diverges, the
			// amount is not compared at all.
			name:   "lifetime pacing diverges and its amount is not compared",
			fields: `,"BudgetType":"LifetimeBudgetStandard","DailyBudget":75.25`,
			want: map[string]fieldWant{
				settingsFieldBudgetType:   {"daily", "lifetime", model.SettingsDiverged},
				settingsFieldBudgetAmount: {"75.25", "", model.SettingsUnknown},
			},
		},
		{
			name:   "an unrecognised budget type is unknown, not guessed",
			fields: `,"BudgetType":" DailyBudgetStandard","DailyBudget":75.25`,
			want: map[string]fieldWant{
				settingsFieldBudgetType:   {"daily", "", model.SettingsUnknown},
				settingsFieldBudgetAmount: {"75.25", "", model.SettingsUnknown},
			},
		},
		{
			name:   "accelerated is still a daily budget",
			fields: `,"BudgetType":"DailyBudgetAccelerated","DailyBudget":75.25`,
			want: map[string]fieldWant{
				settingsFieldBudgetType:   {"daily", "daily", model.SettingsMatch},
				settingsFieldBudgetAmount: {"75.25", "75.25", model.SettingsMatch},
			},
		},
		{
			name:   "a shared budget is reported upstream-only",
			fields: `,"BudgetType":"DailyBudgetStandard","DailyBudget":75.25,"BudgetId":99`,
			want:   map[string]fieldWant{settingsFieldBudgetShared: {"", "true", model.SettingsUnknown}},
		},
		{
			name:   "an unreadable budget id is absent, not false",
			fields: `,"BudgetId":-3`,
			want:   map[string]fieldWant{settingsFieldBudgetShared: {"", "", model.SettingsUnknown}},
		},
		{
			// Nothing reported upstream is ABSENT — never zero-filled into a match or divergence.
			name:   "absent upstream fields are unknown",
			fields: ``,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount:    {"75.25", "", model.SettingsUnknown},
				settingsFieldBudgetType:      {"daily", "", model.SettingsUnknown},
				settingsFieldName:            {"KubeCon — Search", "", model.SettingsUnknown},
				settingsFieldStatus:          {"", "", model.SettingsUnknown},
				settingsFieldBiddingStrategy: {"", "", model.SettingsUnknown},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := msSettingsDispatcher(t, http.StatusOK, msSettingsBody(tc.fields))
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, msSettingsRow())
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			for name, w := range tc.want {
				f := settingsField(t, rb, name)
				if derefOr(f.Recorded) != w.recorded || derefOr(f.Upstream) != w.upstream || f.Comparison != w.verdict {
					t.Errorf("%s = (%q, %q, %s), want (%q, %q, %s)", name, derefOr(f.Recorded), derefOr(f.Upstream), f.Comparison, w.recorded, w.upstream, w.verdict)
				}
			}
		})
	}
}

// An ADOPTED row records provenance and the name only — no budget, no child ids. It reads at the
// campaign level, with the unrecorded sides unknown rather than errors.
func TestMicrosoft_ReadSettings_AdoptedRowReadsAtTheCampaignLevel(t *testing.T) {
	ref, err := adoptedRef("321", "KubeCon — Search", &microsoft.CampaignResult{
		Platform: string(model.ProviderMicrosoftAds), AccountID: "1234567", CampaignID: "321", CampaignName: "KubeCon — Search",
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Campaign{ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321", CampaignName: ref.Name, Result: ref.Result}
	d, _, _ := msSettingsDispatcher(t, http.StatusOK, msSettingsBody(msSettingsAgreeing))
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"", "75.25", model.SettingsUnknown},
		settingsFieldBudgetType:      {"", "daily", model.SettingsUnknown},
		settingsFieldName:            {"KubeCon — Search", "KubeCon — Search", model.SettingsMatch},
		settingsFieldStatus:          {"", "Paused", model.SettingsUnknown},
		settingsFieldBudgetShared:    {"", "false", model.SettingsUnknown},
		settingsFieldBiddingStrategy: {"", "EnhancedCpc", model.SettingsUnknown},
	})
}

func TestMicrosoft_ReadSettings_UnknownProvenanceRefusedBeforeAnyRequest(t *testing.T) {
	var consulted atomic.Bool
	api := newSettingsAPI(t, map[string]settingsRoute{msSettingsQueryPath: {status: 200, body: msSettingsBody(msSettingsAgreeing)}})
	tokURL, tokens := newSettingsTokenServer(t)
	d := NewMicrosoftDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{},
		microsoft.WithTokenURL(tokURL), microsoft.WithBaseURL(api.srv.URL))
	row := msSettingsRow()
	row.Result = json.RawMessage(`{"campaignId":"321"}`)

	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, row)
	assertProvenanceUnknown(t, err)
	if consulted.Load() || len(api.requests()) != 0 || tokens() != 0 {
		t.Fatalf("unknown provenance still resolved a connection (%v) or sent %d request(s) / %d token call(s)", consulted.Load(), len(api.requests()), tokens())
	}
}

func TestMicrosoft_ReadSettings_AccountMismatchRefusedBeforeTheRead(t *testing.T) {
	d, api, _ := msSettingsDispatcher(t, http.StatusOK, msSettingsBody(msSettingsAgreeing))
	row := msSettingsRow()
	row.Result = json.RawMessage(`{"accountId":"7654321","campaignId":"321"}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, row)
	assertMismatch(t, err)
	if n := len(api.requests()); n != 0 {
		t.Fatalf("a mismatched account still sent %d request(s)", n)
	}
}

func TestMicrosoft_ReadSettings_NoSuchCampaignIs404(t *testing.T) {
	d, _, _ := msSettingsDispatcher(t, http.StatusOK,
		`{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, msSettingsRow())
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
	}
}

func TestMicrosoft_ReadSettings_UnreachableOrUntrustworthyIs503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", http.StatusInternalServerError, `{}`},
		{"401", http.StatusUnauthorized, `{}`},
		{"403", http.StatusForbidden, `{}`},
		{"malformed body", http.StatusOK, `{"Campaigns":[`},
		{"another campaign echoed", http.StatusOK, `{"Campaigns":[{"Id":322}]}`},
		{"duplicated keys", http.StatusOK, `{"Campaigns":[{"Id":321,"DailyBudget":1,"DailyBudget":2}]}`},
		{"a non-numeric amount", http.StatusOK, `{"Campaigns":[{"Id":321,"DailyBudget":true}]}`},
		{"an unexplained PartialError", http.StatusOK, `{"Campaigns":[null],"PartialErrors":[{"Code":1,"ErrorCode":"InternalError","Index":0}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api, _ := msSettingsDispatcher(t, tc.status, tc.body)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMicrosoftAds, msSettingsRow())
			if rb != nil {
				t.Fatalf("a failed read still returned a readback: %+v", rb)
			}
			assertSettings503(t, err)
			assertSettingsReadOnly(t, api.requests(), msSettingsQueryPath)
		})
	}
}
