// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

const (
	metaSettingsCampaignPath = "/555"
	metaSettingsAdSetPath    = "/888"
	metaSettingsAccountPath  = "/act_777"
	metaSettingsCampaignBody = `{"id":"555","name":"KubeCon — Leads","status":"PAUSED","account_id":"777","objective":"OUTCOME_LEADS"}`
	// end_time is the create path's endDate + "T23:59:59+0000", echoed in the account's timezone.
	metaSettingsAdSetBody = `{"id":"888","campaign_id":"555","daily_budget":"5000","start_time":"2026-08-01T00:00:00+0000","end_time":"2026-08-31T16:59:59-0700","bid_strategy":"LOWEST_COST_WITHOUT_CAP"}`
	metaSettingsMissing   = `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`
)

func metaSettingsRoutes() map[string]settingsRoute {
	return map[string]settingsRoute{
		metaSettingsCampaignPath: {status: 200, body: metaSettingsCampaignBody},
		metaSettingsAdSetPath:    {status: 200, body: metaSettingsAdSetBody},
		metaSettingsAccountPath:  {status: 200, body: `{"id":"act_777","currency":"USD"}`},
	}
}

func metaSettingsDispatcher(t *testing.T, routes map[string]settingsRoute) (*MetaDispatcher, *settingsAPI) {
	t.Helper()
	api := newSettingsAPI(t, routes)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(api.srv.URL))
	d.settingsNow = pinnedSettingsClock
	return d, api
}

// metaSettingsRow is a created row: provenance matching activeMetaConn (act_777), the ad set the
// create path made, a daily budget and a flight.
func metaSettingsRow() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMetaAds, PlatformCampaignID: "555",
		CampaignName: "KubeCon — Leads",
		BudgetAmount: floatPtr(50), BudgetType: budgetTypePtr(model.BudgetDaily),
		StartDate: dayPtr("2026-08-01"), EndDate: dayPtr("2026-08-31"),
		CreatedAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		Result:    json.RawMessage(`{"AccountID":"act_777","CampaignID":"555","AdSetID":"888"}`),
	}
}

func TestMeta_ReadSettings_MatchWhenBothAgree(t *testing.T) {
	d, api := metaSettingsDispatcher(t, metaSettingsRoutes())
	row := metaSettingsRow()
	before := cloneCampaign(row)

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"50.00", "50.00", model.SettingsMatch},
		settingsFieldBudgetType:      {"daily", "daily", model.SettingsMatch},
		settingsFieldName:            {"KubeCon — Leads", "KubeCon — Leads", model.SettingsMatch},
		settingsFieldStatus:          {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:       {"2026-08-01", "2026-08-01", model.SettingsMatch},
		settingsFieldEndDate:         {"2026-08-31", "2026-08-31", model.SettingsMatch},
		settingsFieldBiddingStrategy: {"", "LOWEST_COST_WITHOUT_CAP", model.SettingsUnknown},
	})
	if rb.PlatformCampaignID != "555" || !rb.ReadAt.Equal(settingsClock) {
		t.Errorf("readback header = %+v", rb)
	}
	reqs := api.requests()
	if len(reqs) != 3 {
		t.Fatalf("want the campaign, ad set and currency reads, got %+v", reqs)
	}
	assertSettingsReadOnly(t, reqs, "")
	assertRowUntouched(t, before, row)
}

func TestMeta_ReadSettings_Cases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(row *model.Campaign, routes map[string]settingsRoute)
		want   map[string]fieldWant
	}{
		{
			name: "budget and name diverge",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsCampaignPath] = settingsRoute{status: 200, body: `{"id":"555","name":"Renamed","status":"ACTIVE","account_id":"777"}`}
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","lifetime_budget":"75050"}`}
			},
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "750.50", model.SettingsDiverged},
				settingsFieldBudgetType:   {"daily", "lifetime", model.SettingsDiverged},
				settingsFieldName:         {"KubeCon — Leads", "Renamed", model.SettingsDiverged},
			},
		},
		{
			// A zero-decimal account: the create path sent round(1000.4 × 1) = 1000, so the
			// recorded side is compared as that encoded amount, not as a false divergence.
			name: "a zero-decimal currency compares as the create path encoded it",
			mutate: func(row *model.Campaign, r map[string]settingsRoute) {
				row.BudgetAmount = floatPtr(1000.4)
				r[metaSettingsAccountPath] = settingsRoute{status: 200, body: `{"currency":"JPY"}`}
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"1000"}`}
			},
			want: map[string]fieldWant{settingsFieldBudgetAmount: {"1000.00", "1000.00", model.SettingsMatch}},
		},
		{
			name: "an unmapped currency leaves the amount unknown, never guessed",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsAccountPath] = settingsRoute{status: 200, body: `{"currency":"ZZZ"}`}
			},
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:   {"daily", "daily", model.SettingsMatch},
			},
		},
		{
			// Campaign Budget Optimization: the ad set holds no budget, and the shared campaign
			// budget is not its counterpart.
			name: "a CBO campaign's ad-set budget is unknown",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","start_time":"2026-08-01T00:00:00+0000"}`}
			},
			want: map[string]fieldWant{
				settingsFieldBudgetAmount: {"50.00", "", model.SettingsUnknown},
				settingsFieldBudgetType:   {"daily", "", model.SettingsUnknown},
				settingsFieldStartDate:    {"2026-08-01", "2026-08-01", model.SettingsMatch},
				settingsFieldEndDate:      {"2026-08-31", "", model.SettingsUnknown},
			},
		},
		{
			// The create path nudged a start that had begun to dispatch time + buffer, which
			// crossed UTC midnight: both sides shown, verdict unknown, never a false divergence.
			name: "a start nudged across midnight at dispatch is unknown, not diverged",
			mutate: func(row *model.Campaign, r map[string]settingsRoute) {
				row.CreatedAt = time.Date(2026, 8, 1, 23, 55, 0, 0, time.UTC)
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"5000","start_time":"2026-08-02T00:05:00+0000","end_time":"2026-08-31T23:59:59+0000"}`}
			},
			want: map[string]fieldWant{settingsFieldStartDate: {"2026-08-01", "2026-08-02", model.SettingsUnknown}},
		},
		{
			name: "a later start far from dispatch is a real divergence",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"5000","start_time":"2026-08-10T00:00:00+0000","end_time":"2026-09-15T23:59:59+0000"}`}
			},
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "2026-08-10", model.SettingsDiverged},
				settingsFieldEndDate:   {"2026-08-31", "2026-09-15", model.SettingsDiverged},
			},
		},
		{
			// A date-only value does not parse against the documented layout, so it is withheld
			// rather than passed through to compare byte-equal with the recorded date.
			name: "an unparseable flight time is absent, not a fabricated match",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"5000","start_time":"2026-08-01","end_time":"2026-08-31"}`}
			},
			want: map[string]fieldWant{
				settingsFieldStartDate: {"2026-08-01", "", model.SettingsUnknown},
				settingsFieldEndDate:   {"2026-08-31", "", model.SettingsUnknown},
			},
		},
		{
			name: "a missing ad set leaves its fields unknown",
			mutate: func(_ *model.Campaign, r map[string]settingsRoute) {
				r[metaSettingsAdSetPath] = settingsRoute{status: 400, body: metaSettingsMissing}
			},
			want: map[string]fieldWant{
				settingsFieldBudgetAmount:    {"50.00", "", model.SettingsUnknown},
				settingsFieldStartDate:       {"2026-08-01", "", model.SettingsUnknown},
				settingsFieldBiddingStrategy: {"", "", model.SettingsUnknown},
				settingsFieldName:            {"KubeCon — Leads", "KubeCon — Leads", model.SettingsMatch},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := metaSettingsRoutes()
			row := metaSettingsRow()
			tc.mutate(row, routes)
			d, _ := metaSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
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

// An ADOPTED row records provenance only — no ad set — so it reads the campaign alone, with the
// ad-set fields absent rather than an error.
func TestMeta_ReadSettings_AdoptedRowReadsAtTheCampaignLevel(t *testing.T) {
	ref, err := adoptedRef("555", "KubeCon — Leads", &meta.CampaignResult{Platform: string(model.ProviderMetaAds), CampaignName: "KubeCon — Leads", CampaignID: "555", AccountID: "act_777"})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Campaign{ID: "camp-1", Platform: model.ProviderMetaAds, PlatformCampaignID: "555", CampaignName: ref.Name, Result: ref.Result}
	d, api := metaSettingsDispatcher(t, metaSettingsRoutes())
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"", "", model.SettingsUnknown},
		settingsFieldBudgetType:      {"", "", model.SettingsUnknown},
		settingsFieldName:            {"KubeCon — Leads", "KubeCon — Leads", model.SettingsMatch},
		settingsFieldStatus:          {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:       {"", "", model.SettingsUnknown},
		settingsFieldEndDate:         {"", "", model.SettingsUnknown},
		settingsFieldBiddingStrategy: {"", "", model.SettingsUnknown},
	})
	if reqs := api.requests(); len(reqs) != 1 || reqs[0].Path != metaSettingsCampaignPath {
		t.Fatalf("an adopted row must read the campaign alone, got %+v", reqs)
	}
}

func TestMeta_ReadSettings_UnknownProvenanceRefusedBeforeAnyRequest(t *testing.T) {
	var consulted atomic.Bool
	api := newSettingsAPI(t, metaSettingsRoutes())
	d := NewMetaDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{}, meta.WithBaseURL(api.srv.URL))
	row := metaSettingsRow()
	row.Result = json.RawMessage(`{"CampaignID":"555","AdSetID":"888"}`)
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
	assertProvenanceUnknown(t, err)
	if consulted.Load() || len(api.requests()) != 0 {
		t.Fatalf("unknown provenance still resolved a connection (%v) or sent %d request(s)", consulted.Load(), len(api.requests()))
	}
}

func TestMeta_ReadSettings_AccountMismatches(t *testing.T) {
	t.Run("recorded account differs from the connection", func(t *testing.T) {
		d, api := metaSettingsDispatcher(t, metaSettingsRoutes())
		row := metaSettingsRow()
		row.Result = json.RawMessage(`{"AccountID":"act_999","CampaignID":"555","AdSetID":"888"}`)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("a mismatched account still sent %d request(s)", n)
		}
	})
	// GET /{id} is not account-scoped, so the account Meta reports is the read's own proof.
	t.Run("Meta reports the campaign under another account", func(t *testing.T) {
		routes := metaSettingsRoutes()
		routes[metaSettingsCampaignPath] = settingsRoute{status: 200, body: `{"id":"555","name":"n","status":"PAUSED","account_id":"999"}`}
		d, _ := metaSettingsDispatcher(t, routes)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, metaSettingsRow())
		assertUpstreamIdentityMismatch(t, err)
	})
	t.Run("the recorded ad set belongs to another campaign", func(t *testing.T) {
		routes := metaSettingsRoutes()
		routes[metaSettingsAdSetPath] = settingsRoute{status: 200, body: `{"id":"888","campaign_id":"556","daily_budget":"5000"}`}
		d, _ := metaSettingsDispatcher(t, routes)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, metaSettingsRow())
		assertUpstreamIdentityMismatch(t, err)
	})
}

// Graph 100/33 on the campaign cannot tell a deleted campaign from one this token cannot load, on
// any HTTP status, so it is a 503 — never a 404 — exactly as on the adoption read. The account is
// routed as loadable, so an account probe that "proved" absence would succeed; none may be sent.
func TestMeta_ReadSettings_ObjectMissingIs503NeverAbsent(t *testing.T) {
	for _, status := range []int{400, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			routes := metaSettingsRoutes()
			routes[metaSettingsCampaignPath] = settingsRoute{status: status, body: metaSettingsMissing}
			d, api := metaSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, metaSettingsRow())
			if rb != nil {
				t.Fatalf("an unverifiable read still returned a readback: %+v", rb)
			}
			if errors.Is(err, domain.ErrPlatformCampaignAbsent) {
				t.Fatalf("100/33 was reported as absence (404): %v", err)
			}
			assertSettings503(t, err)
			for _, r := range api.requests() {
				if r.Path == metaSettingsAccountPath {
					t.Fatalf("the readback probed the ad account to settle 100/33: %+v", api.requests())
				}
			}
		})
	}
}

// A campaign Meta has DELETED still answers with that status: it is reported as status, not 404.
func TestMeta_ReadSettings_DeletedCampaignReportsItsStatus(t *testing.T) {
	routes := metaSettingsRoutes()
	routes[metaSettingsCampaignPath] = settingsRoute{status: 200, body: `{"id":"555","name":"KubeCon — Leads","status":"DELETED","account_id":"777"}`}
	d, _ := metaSettingsDispatcher(t, routes)
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, metaSettingsRow())
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if f := settingsField(t, rb, settingsFieldStatus); f.Upstream == nil || *f.Upstream != "DELETED" || f.Comparison != model.SettingsUnknown {
		t.Fatalf("status = %+v, want upstream DELETED reported upstream-only", f)
	}
}

func TestMeta_ReadSettings_UnreachableOrUntrustworthyIs503(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		rep  settingsRoute
	}{
		{"campaign 5xx", metaSettingsCampaignPath, settingsRoute{status: 500, body: `{"error":{"message":"boom","code":1}}`}},
		{"campaign 401", metaSettingsCampaignPath, settingsRoute{status: 401, body: `{"error":{"message":"bad token","code":190}}`}},
		{"campaign 403", metaSettingsCampaignPath, settingsRoute{status: 403, body: `{"error":{"message":"denied","code":200}}`}},
		{"malformed campaign body", metaSettingsCampaignPath, settingsRoute{status: 200, body: `{"id":`}},
		{"another campaign echoed", metaSettingsCampaignPath, settingsRoute{status: 200, body: `{"id":"556","name":"n","status":"PAUSED","account_id":"777"}`}},
		{"duplicated campaign keys", metaSettingsCampaignPath, settingsRoute{status: 200, body: `{"id":"555","name":"a","name":"b","status":"PAUSED","account_id":"777"}`}},
		{"no account_id", metaSettingsCampaignPath, settingsRoute{status: 200, body: `{"id":"555","name":"n","status":"PAUSED"}`}},
		{"ad set 5xx", metaSettingsAdSetPath, settingsRoute{status: 500, body: `{"error":{"message":"boom","code":1}}`}},
		{"ad set with both budgets", metaSettingsAdSetPath, settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"1","lifetime_budget":"2"}`}},
		{"ad set with a non-integer budget", metaSettingsAdSetPath, settingsRoute{status: 200, body: `{"id":"888","campaign_id":"555","daily_budget":"1.5"}`}},
		{"currency preflight 5xx", metaSettingsAccountPath, settingsRoute{status: 500, body: `{"error":{"message":"boom","code":1}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := metaSettingsRoutes()
			routes[tc.path] = tc.rep
			d, api := metaSettingsDispatcher(t, routes)
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, metaSettingsRow())
			if rb != nil {
				t.Fatalf("a failed read still returned a readback: %+v", rb)
			}
			assertSettings503(t, err)
			assertSettingsReadOnly(t, api.requests(), "")
		})
	}
}
