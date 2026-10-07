// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

const (
	redditSettingsPath = "/api/v3/ad_accounts/t2_acct/campaigns/t3_c"
	redditSettingsBody = `{"data":{"id":"t3_c","ad_account_id":"t2_acct","name":"KubeCon — Traffic","configured_status":"PAUSED","goal_type":"LIFETIME_SPEND","goal_value":2500000000,"is_campaign_budget_optimization":true,"start_time":"2026-08-01T00:00:00+00:00","end_time":"2026-08-31T00:00:00+00:00","bid_strategy":"BIDLESS"}}`
)

func redditSettingsDispatcher(t *testing.T, status int, body string) (*RedditDispatcher, *settingsAPI) {
	t.Helper()
	api := newSettingsAPI(t, map[string]settingsRoute{redditSettingsPath: {status: status, body: body}})
	tokURL, _ := newSettingsTokenServer(t)
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.srv.URL+"/api/v3"), reddit.WithTokenURL(tokURL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) }))
	d.settingsNow = pinnedSettingsClock
	return d, api
}

// redditSettingsRow is a created row: provenance matching activeRedditConn (t2_acct), the
// LIFETIME budget the create path always records, and a flight.
func redditSettingsRow() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderRedditAds, PlatformCampaignID: "t3_c",
		CampaignName: "KubeCon — Traffic",
		BudgetAmount: floatPtr(2500), BudgetType: budgetTypePtr(model.BudgetLifetime),
		StartDate: dayPtr("2026-08-01"), EndDate: dayPtr("2026-08-31"),
		CreatedAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		Result:    json.RawMessage(`{"accountId":"t2_acct","campaignId":"t3_c","adGroupId":"t3_ag","adId":"t3_ad"}`),
	}
}

func redditBody(fields string) string {
	return `{"data":{"id":"t3_c","ad_account_id":"t2_acct"` + fields + `}}`
}

func TestReddit_ReadSettings_MatchWhenBothAgree(t *testing.T) {
	d, api := redditSettingsDispatcher(t, 200, redditSettingsBody)
	row := redditSettingsRow()
	before := cloneCampaign(row)

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:               {"2500.00", "2500.00", model.SettingsMatch},
		settingsFieldBudgetType:                 {"lifetime", "lifetime", model.SettingsMatch},
		settingsFieldName:                       {"KubeCon — Traffic", "KubeCon — Traffic", model.SettingsMatch},
		settingsFieldStatus:                     {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:                  {"2026-08-01", "2026-08-01", model.SettingsMatch},
		settingsFieldEndDate:                    {"2026-08-31", "2026-08-31", model.SettingsMatch},
		settingsFieldBiddingStrategy:            {"", "BIDLESS", model.SettingsUnknown},
		settingsFieldCampaignBudgetOptimization: {"", "true", model.SettingsUnknown},
	})
	if rb.PlatformCampaignID != "t3_c" || !rb.ReadAt.Equal(settingsClock) {
		t.Errorf("readback header = %+v", rb)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one campaign read (no ad group), got %+v", reqs)
	}
	assertSettingsReadOnly(t, reqs, "")
	assertRowUntouched(t, before, row)
}

func TestReddit_ReadSettings_Cases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		createdAt time.Time
		want      map[string]fieldWant
	}{
		{
			name: "budget, pacing and name diverge",
			body: redditBody(`,"name":"Renamed","goal_type":"DAILY_SPEND","goal_value":100000000,"is_campaign_budget_optimization":true`),
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"2500.00", "100.00", model.SettingsDiverged},
				settingsFieldBudgetType:   {"lifetime", "daily", model.SettingsDiverged},
				settingsFieldName:         {"KubeCon — Traffic", "Renamed", model.SettingsDiverged},
			},
		},
		{
			name: "a sub-cent upstream budget is not rounded into a match",
			body: redditBody(`,"goal_type":"LIFETIME_SPEND","goal_value":2500004000,"is_campaign_budget_optimization":true`),
			want: map[string]fieldWant{settingsFieldBudgetAmount: {"2500.00", "2500.004", model.SettingsDiverged}},
		},
		{
			// With CBO off the spend is governed per ad group: goal_value is not the budget the
			// row recorded, so neither budget field is compared.
			name: "CBO off leaves the budget unknown",
			body: redditBody(`,"goal_type":"LIFETIME_SPEND","goal_value":2500000000,"is_campaign_budget_optimization":false`),
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"2500.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:   {"lifetime", "", model.SettingsUnknown},
				// Reported upstream-only, so the operator can see WHY the budget is unknown.
				settingsFieldCampaignBudgetOptimization: {"", "false", model.SettingsUnknown},
			},
		},
		{
			name: "an unreported CBO flag is not assumed on",
			body: redditBody(`,"goal_type":"LIFETIME_SPEND","goal_value":2500000000`),
			want: map[string]fieldWant{
				settingsFieldBudgetAmount:               {"2500.00", "", model.SettingsUnknown},
				settingsFieldCampaignBudgetOptimization: {"", "", model.SettingsUnknown},
			},
		},
		{
			name: "an unmapped goal type is unknown",
			body: redditBody(`,"goal_type":"IMPRESSIONS","goal_value":2500000000,"is_campaign_budget_optimization":true`),
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"2500.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:   {"lifetime", "", model.SettingsUnknown},
			},
		},
		{
			name:      "a start nudged across midnight at dispatch is unknown, not diverged",
			body:      redditBody(`,"start_time":"2026-08-02T00:10:00+00:00"`),
			createdAt: time.Date(2026, 8, 1, 23, 58, 0, 0, time.UTC),
			want:      map[string]fieldWant{settingsFieldStartDate: {"2026-08-01", "2026-08-02", model.SettingsUnknown}},
		},
		{
			name: "an earlier start is a real divergence",
			body: redditBody(`,"start_time":"2026-07-30T00:00:00+00:00","end_time":"2026-09-01T00:00:00Z"`),
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "2026-07-30", model.SettingsDiverged},
				settingsFieldEndDate:   {"2026-08-31", "2026-09-01", model.SettingsDiverged},
			},
		},
		{
			name: "a date-only flight is absent, not a fabricated match",
			body: redditBody(`,"start_time":"2026-08-01","end_time":"2026-08-31"`),
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "", model.SettingsUnknown},
				settingsFieldEndDate:   {"2026-08-31", "", model.SettingsUnknown},
			},
		},
		{
			name: "a deleted campaign is reported, not hidden",
			body: redditBody(`,"configured_status":"DELETED"`),
			want: map[string]fieldWant{settingsFieldStatus: {"", "DELETED", model.SettingsUnknown}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := redditSettingsDispatcher(t, 200, tc.body)
			row := redditSettingsRow()
			if !tc.createdAt.IsZero() {
				row.CreatedAt = tc.createdAt
			}
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, row)
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

// An ADOPTED row records provenance only: it reads the campaign, with the recorded sides absent.
func TestReddit_ReadSettings_AdoptedRowReadsAtTheCampaignLevel(t *testing.T) {
	ref, err := adoptedRef("t3_c", "KubeCon — Traffic", &reddit.CampaignResult{CampaignID: "t3_c", CampaignName: "KubeCon — Traffic", AccountID: "t2_acct"})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Campaign{ID: "camp-1", Platform: model.ProviderRedditAds, PlatformCampaignID: "t3_c", CampaignName: ref.Name, Result: ref.Result}
	d, _ := redditSettingsDispatcher(t, 200, redditSettingsBody)
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:               {"", "2500.00", model.SettingsUnknown},
		settingsFieldBudgetType:                 {"", "lifetime", model.SettingsUnknown},
		settingsFieldName:                       {"KubeCon — Traffic", "KubeCon — Traffic", model.SettingsMatch},
		settingsFieldStatus:                     {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:                  {"", "2026-08-01", model.SettingsUnknown},
		settingsFieldEndDate:                    {"", "2026-08-31", model.SettingsUnknown},
		settingsFieldBiddingStrategy:            {"", "BIDLESS", model.SettingsUnknown},
		settingsFieldCampaignBudgetOptimization: {"", "true", model.SettingsUnknown},
	})
}

func TestReddit_ReadSettings_UnknownProvenanceRefusedBeforeAnyRequest(t *testing.T) {
	var consulted atomic.Bool
	api := newSettingsAPI(t, map[string]settingsRoute{redditSettingsPath: {status: 200, body: redditSettingsBody}})
	tokURL, tokens := newSettingsTokenServer(t)
	d := NewRedditDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{},
		reddit.WithBaseURL(api.srv.URL+"/api/v3"), reddit.WithTokenURL(tokURL))
	row := redditSettingsRow()
	row.Result = json.RawMessage(`{"campaignId":"t3_c"}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, row)
	assertProvenanceUnknown(t, err)
	if consulted.Load() || len(api.requests()) != 0 || tokens() != 0 {
		t.Fatalf("unknown provenance still resolved a connection (%v) or sent %d request(s) / %d token call(s)", consulted.Load(), len(api.requests()), tokens())
	}
}

func TestReddit_ReadSettings_AccountMismatches(t *testing.T) {
	t.Run("recorded account differs from the connection", func(t *testing.T) {
		d, api := redditSettingsDispatcher(t, 200, redditSettingsBody)
		row := redditSettingsRow()
		row.Result = json.RawMessage(`{"accountId":"t2_other","campaignId":"t3_c"}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, row)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("a mismatched account still sent %d request(s)", n)
		}
	})
	t.Run("Reddit reports the campaign under another account", func(t *testing.T) {
		d, _ := redditSettingsDispatcher(t, 200, `{"data":{"id":"t3_c","ad_account_id":"t2_other","name":"n"}}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, redditSettingsRow())
		assertUpstreamIdentityMismatch(t, err)
	})
}

func TestReddit_ReadSettings_NoSuchCampaignIs404(t *testing.T) {
	d, _ := redditSettingsDispatcher(t, 404, `{}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, redditSettingsRow())
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
	}
}

func TestReddit_ReadSettings_UnreachableOrUntrustworthyIs503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", 500, `{}`},
		{"401", 401, `{}`},
		{"403", 403, `{}`},
		{"malformed body", 200, `{"data":{"id":`},
		{"another campaign echoed", 200, `{"data":{"id":"t3_other"}}`},
		{"duplicated keys", 200, `{"data":{"id":"t3_c","goal_value":1,"goal_value":2}}`},
		{"a fractional goal_value", 200, `{"data":{"id":"t3_c","goal_value":1.5}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := redditSettingsDispatcher(t, tc.status, tc.body)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderRedditAds, redditSettingsRow())
			if rb != nil {
				t.Fatalf("a failed read still returned a readback: %+v", rb)
			}
			assertSettings503(t, err)
			assertSettingsReadOnly(t, api.requests(), "")
		})
	}
}
