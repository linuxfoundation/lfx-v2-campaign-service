// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

const (
	xSettingsCampaignPath = "/12/accounts/acc1/campaigns/cmp1"
	xSettingsLineItemPath = "/12/accounts/acc1/line_items/li1"
	// xSettingsCampaignBody is a campaign that REPORTS budget_optimization CAMPAIGN. It is NOT
	// evidence that a campaign this service creates reads back that way: the create path sends no
	// budget_optimization, and which value X then reports is unverified (X's v11 announcement
	// says CAMPAIGN is the default; its current reference lists LINE_ITEM as the only value and
	// default). TestTwitter_ReadSettings_CreatedCampaignUnderEachReportedOptimization pins the
	// readback for every answer X could give for a created campaign.
	xSettingsCampaignBody = `{"data":{"id":"cmp1","name":"KubeCon — Awareness","entity_status":"PAUSED","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null,"deleted":false}}`
	xSettingsLineItemBody = `{"data":{"id":"li1","campaign_id":"cmp1","start_time":"2026-08-01T00:00:00Z","end_time":"2026-08-31T00:00:00Z","bid_strategy":"AUTO","deleted":false}}`
)

func xSettingsRoutes() map[string]settingsRoute {
	return map[string]settingsRoute{
		xSettingsCampaignPath: {status: 200, body: xSettingsCampaignBody},
		xSettingsLineItemPath: {status: 200, body: xSettingsLineItemBody},
	}
}

func xSettingsDispatcher(t *testing.T, routes map[string]settingsRoute) (*TwitterDispatcher, *settingsAPI) {
	t.Helper()
	api := newSettingsAPI(t, routes)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.srv.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0))
	d.settingsNow = pinnedSettingsClock
	return d, api
}

// xSettingsRow is a created row: provenance matching activeTwitterConn (acc1), the line item the
// create path made, the daily budget and a flight.
func xSettingsRow() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderTwitterAds, PlatformCampaignID: "cmp1",
		CampaignName: "KubeCon — Awareness",
		BudgetAmount: floatPtr(50), BudgetType: budgetTypePtr(model.BudgetDaily),
		StartDate: dayPtr("2026-08-01"), EndDate: dayPtr("2026-08-31"),
		Result: json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc1"}`),
	}
}

func TestTwitter_ReadSettings_MatchWhenBothAgree(t *testing.T) {
	d, api := xSettingsDispatcher(t, xSettingsRoutes())
	row := xSettingsRow()
	before := cloneCampaign(row)

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:       {"50.00", "50.00", model.SettingsMatch},
		settingsFieldBudgetType:         {"daily", "daily", model.SettingsMatch},
		settingsFieldName:               {"KubeCon — Awareness", "KubeCon — Awareness", model.SettingsMatch},
		settingsFieldStatus:             {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:          {"2026-08-01", "2026-08-01", model.SettingsMatch},
		settingsFieldEndDate:            {"2026-08-31", "2026-08-31", model.SettingsMatch},
		settingsFieldBiddingStrategy:    {"", "AUTO", model.SettingsUnknown},
		settingsFieldBudgetOptimization: {"", "CAMPAIGN", model.SettingsUnknown},
	})
	if rb.PlatformCampaignID != "cmp1" || !rb.ReadAt.Equal(settingsClock) {
		t.Errorf("readback header = %+v", rb)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("want the campaign and line item reads, got %+v", reqs)
	}
	assertSettingsReadOnly(t, reqs, "")
	assertRowUntouched(t, before, row)
}

// A created row's budget is compared ONLY when X reports budget_optimization CAMPAIGN. The create
// path sends no budget_optimization and X's own documents disagree about the default, so the
// readback never assumes one: an unreported value and LINE_ITEM both leave the budget `unknown`
// (fail safe), whatever amount the campaign carries.
func TestTwitter_ReadSettings_CreatedCampaignUnderEachReportedOptimization(t *testing.T) {
	for _, tc := range []struct {
		name         string
		optimization string // the JSON fragment X reports, "" for none
		budget       fieldWant
		budgetType   fieldWant
		reported     string
	}{
		{"reported CAMPAIGN is compared", `"budget_optimization":"CAMPAIGN",`,
			fieldWant{"50.00", "50.00", model.SettingsMatch}, fieldWant{"daily", "daily", model.SettingsMatch}, "CAMPAIGN"},
		{"unreported is not assumed and stays unknown", ``,
			fieldWant{"50.00", "", model.SettingsUnknown}, fieldWant{"daily", "", model.SettingsUnknown}, ""},
		{"reported LINE_ITEM stays unknown", `"budget_optimization":"LINE_ITEM",`,
			fieldWant{"50.00", "", model.SettingsUnknown}, fieldWant{"daily", "", model.SettingsUnknown}, "LINE_ITEM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := xSettingsRoutes()
			// Exactly the campaign the create path makes: the daily budget on the campaign, no
			// total cap, PAUSED — differing only in what X reports for budget_optimization.
			routes[xSettingsCampaignPath] = settingsRoute{status: 200, body: `{"data":{"id":"cmp1","name":"KubeCon — Awareness","entity_status":"PAUSED",` +
				tc.optimization + `"daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null,"deleted":false}}`}
			d, _ := xSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			assertSettingsFields(t, rb, map[string]fieldWant{
				settingsFieldBudgetAmount:       tc.budget,
				settingsFieldBudgetType:         tc.budgetType,
				settingsFieldName:               {"KubeCon — Awareness", "KubeCon — Awareness", model.SettingsMatch},
				settingsFieldStatus:             {"", "PAUSED", model.SettingsUnknown},
				settingsFieldStartDate:          {"2026-08-01", "2026-08-01", model.SettingsMatch},
				settingsFieldEndDate:            {"2026-08-31", "2026-08-31", model.SettingsMatch},
				settingsFieldBiddingStrategy:    {"", "AUTO", model.SettingsUnknown},
				settingsFieldBudgetOptimization: {"", tc.reported, model.SettingsUnknown},
			})
		})
	}
}

func TestTwitter_ReadSettings_Cases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		campaign string
		lineItem *settingsRoute
		want     map[string]fieldWant
	}{
		{
			name:     "budget and name diverge",
			campaign: `{"data":{"id":"cmp1","name":"Renamed","entity_status":"ACTIVE","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":75500000}}`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "75.50", model.SettingsDiverged},
				settingsFieldName:         {"KubeCon — Awareness", "Renamed", model.SettingsDiverged},
				settingsFieldStatus:       {"", "ACTIVE", model.SettingsUnknown},
			},
		},
		{
			name:     "a sub-cent upstream budget is not rounded into a match",
			campaign: `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50004000}}`,
			want:     map[string]fieldWant{settingsFieldBudgetAmount: {"50.00", "50.004", model.SettingsDiverged}},
		},
		{
			name:     "a total-only budget is lifetime",
			campaign: `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","total_budget_amount_local_micro":500000000}}`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "500.00", model.SettingsDiverged},
				settingsFieldBudgetType:   {"daily", "lifetime", model.SettingsDiverged},
			},
		},
		{
			// Under LINE_ITEM optimization each line item governs its own spend: a campaign-level
			// total cap is not the recorded daily amount, so neither budget field is compared.
			name:     "line-item budget optimization leaves the budget unknown",
			campaign: `{"data":{"id":"cmp1","budget_optimization":"LINE_ITEM","total_budget_amount_local_micro":500000000}}`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount:       {"50.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:         {"daily", "", model.SettingsUnknown},
				settingsFieldBudgetOptimization: {"", "LINE_ITEM", model.SettingsUnknown},
			},
		},
		{
			name:     "an unreported budget optimization is not assumed CAMPAIGN",
			campaign: `{"data":{"id":"cmp1","daily_budget_amount_local_micro":50000000}}`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount:       {"50.00", "", model.SettingsUnknown},
				settingsFieldBudgetOptimization: {"", "", model.SettingsUnknown},
			},
		},
		{
			name:     "no budget reported is unknown, never zero",
			campaign: `{"data":{"id":"cmp1","daily_budget_amount_local_micro":null}}`,
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:   {"daily", "", model.SettingsUnknown},
				settingsFieldName:         {"KubeCon — Awareness", "", model.SettingsUnknown},
			},
		},
		{
			name:     "a moved flight diverges",
			campaign: xSettingsCampaignBody,
			lineItem: &settingsRoute{status: 200, body: `{"data":{"id":"li1","campaign_id":"cmp1","start_time":"2026-08-03T00:00:00Z","end_time":"2026-09-30T00:00:00Z"}}`},
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "2026-08-03", model.SettingsDiverged},
				settingsFieldEndDate:   {"2026-08-31", "2026-09-30", model.SettingsDiverged},
			},
		},
		{
			name:     "a date-only flight is absent, not a fabricated match",
			campaign: xSettingsCampaignBody,
			lineItem: &settingsRoute{status: 200, body: `{"data":{"id":"li1","campaign_id":"cmp1","start_time":"2026-08-01","end_time":"2026-08-31"}}`},
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "", model.SettingsUnknown},
				settingsFieldEndDate:   {"2026-08-31", "", model.SettingsUnknown},
			},
		},
		{
			name:     "a missing line item leaves its fields unknown",
			campaign: xSettingsCampaignBody,
			lineItem: &settingsRoute{status: 404, body: `{}`},
			want: map[string]fieldWant{
				settingsFieldStartDate:       {"2026-08-01", "", model.SettingsUnknown},
				settingsFieldBiddingStrategy: {"", "", model.SettingsUnknown},
				settingsFieldBudgetAmount:    {"50.00", "50.00", model.SettingsMatch},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := xSettingsRoutes()
			routes[xSettingsCampaignPath] = settingsRoute{status: 200, body: tc.campaign}
			if tc.lineItem != nil {
				routes[xSettingsLineItemPath] = *tc.lineItem
			}
			d, _ := xSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
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

// An ADOPTED row records provenance only — no line item — so it reads the campaign alone, with
// the line-item fields absent rather than an error.
func TestTwitter_ReadSettings_AdoptedRowReadsAtTheCampaignLevel(t *testing.T) {
	ref, err := adoptedRef("cmp1", "KubeCon — Awareness", &twitter.CampaignResult{CampaignID: "cmp1", CampaignName: "KubeCon — Awareness", AccountID: "acc1"})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Campaign{ID: "camp-1", Platform: model.ProviderTwitterAds, PlatformCampaignID: "cmp1", CampaignName: ref.Name, Result: ref.Result}
	d, api := xSettingsDispatcher(t, xSettingsRoutes())
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:       {"", "50.00", model.SettingsUnknown},
		settingsFieldBudgetType:         {"", "daily", model.SettingsUnknown},
		settingsFieldName:               {"KubeCon — Awareness", "KubeCon — Awareness", model.SettingsMatch},
		settingsFieldStatus:             {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:          {"", "", model.SettingsUnknown},
		settingsFieldEndDate:            {"", "", model.SettingsUnknown},
		settingsFieldBiddingStrategy:    {"", "", model.SettingsUnknown},
		settingsFieldBudgetOptimization: {"", "CAMPAIGN", model.SettingsUnknown},
	})
	if reqs := api.requests(); len(reqs) != 1 || reqs[0].Path != xSettingsCampaignPath {
		t.Fatalf("an adopted row must read the campaign alone, got %+v", reqs)
	}
}

func TestTwitter_ReadSettings_UnknownProvenanceRefusedBeforeAnyRequest(t *testing.T) {
	var consulted atomic.Bool
	api := newSettingsAPI(t, xSettingsRoutes())
	d := NewTwitterDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{},
		twitter.WithBaseURL(api.srv.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0))
	row := xSettingsRow()
	row.Result = json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1"}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, row)
	assertProvenanceUnknown(t, err)
	if consulted.Load() || len(api.requests()) != 0 {
		t.Fatalf("unknown provenance still resolved a connection (%v) or sent %d request(s)", consulted.Load(), len(api.requests()))
	}
}

func TestTwitter_ReadSettings_AccountMismatches(t *testing.T) {
	t.Run("recorded account differs from the connection", func(t *testing.T) {
		d, api := xSettingsDispatcher(t, xSettingsRoutes())
		row := xSettingsRow()
		row.Result = json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc9"}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, row)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("a mismatched account still sent %d request(s)", n)
		}
	})
	t.Run("X reports the campaign under another account", func(t *testing.T) {
		routes := xSettingsRoutes()
		routes[xSettingsCampaignPath] = settingsRoute{status: 200, body: `{"data":{"id":"cmp1","account_id":"acc9"}}`}
		d, _ := xSettingsDispatcher(t, routes)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
		assertUpstreamIdentityMismatch(t, err)
	})
	t.Run("the recorded line item belongs to another campaign", func(t *testing.T) {
		routes := xSettingsRoutes()
		routes[xSettingsLineItemPath] = settingsRoute{status: 200, body: `{"data":{"id":"li1","campaign_id":"cmp9"}}`}
		d, _ := xSettingsDispatcher(t, routes)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
		assertUpstreamIdentityMismatch(t, err)
	})
}

func TestTwitter_ReadSettings_NoSuchCampaignIs404(t *testing.T) {
	for _, rep := range []settingsRoute{
		{status: 404, body: `{"errors":[{"code":"NOT_FOUND"}]}`},
		{status: 200, body: `{"data":{"id":"cmp1","deleted":true}}`},
	} {
		routes := xSettingsRoutes()
		routes[xSettingsCampaignPath] = rep
		d, _ := xSettingsDispatcher(t, routes)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
		if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
			t.Fatalf("%+v: err = %v, want ErrPlatformCampaignAbsent", rep, err)
		}
	}
}

func TestTwitter_ReadSettings_UnreachableOrUntrustworthyIs503(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		rep  settingsRoute
	}{
		{"campaign 5xx", xSettingsCampaignPath, settingsRoute{status: 503, body: `{}`}},
		{"campaign 401", xSettingsCampaignPath, settingsRoute{status: 401, body: `{}`}},
		{"campaign 403", xSettingsCampaignPath, settingsRoute{status: 403, body: `{}`}},
		{"campaign 429 the client will not wait out", xSettingsCampaignPath, settingsRoute{status: 429, body: `{}`, retryAfter: "86400"}},
		{"malformed campaign body", xSettingsCampaignPath, settingsRoute{status: 200, body: `{"data":{`}},
		{"another campaign echoed", xSettingsCampaignPath, settingsRoute{status: 200, body: `{"data":{"id":"cmp2"}}`}},
		{"duplicated keys", xSettingsCampaignPath, settingsRoute{status: 200, body: `{"data":{"id":"cmp1","daily_budget_amount_local_micro":1,"daily_budget_amount_local_micro":2}}`}},
		{"a fractional amount", xSettingsCampaignPath, settingsRoute{status: 200, body: `{"data":{"id":"cmp1","daily_budget_amount_local_micro":1.5}}`}},
		{"line item 5xx", xSettingsLineItemPath, settingsRoute{status: 500, body: `{}`}},
		{"another line item echoed", xSettingsLineItemPath, settingsRoute{status: 200, body: `{"data":{"id":"li2","campaign_id":"cmp1"}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := xSettingsRoutes()
			routes[tc.path] = tc.rep
			d, api := xSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderTwitterAds, xSettingsRow())
			if rb != nil {
				t.Fatalf("a failed read still returned a readback: %+v", rb)
			}
			assertSettings503(t, err)
			assertSettingsReadOnly(t, api.requests(), "")
		})
	}
}
