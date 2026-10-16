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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// adReadRoutes is a happy list-meta-ads exchange for ad set 888 of campaign 555 under act_777.
func adReadRoutes() map[string]settingsRoute {
	return map[string]settingsRoute{
		"GET /888": {status: 200, body: adSetStateActive},
		"GET /888/ads": {status: 200, body: `{"data":[` +
			`{"id":"901","name":"Image A","status":"ACTIVE","effective_status":"ACTIVE","creative":{"id":"701","name":"Creative A"},"adset_id":"888","campaign_id":"555","account_id":"777"},` +
			`{"id":"902","name":"Image B","status":"PAUSED","effective_status":"PAUSED","adset_id":"888","campaign_id":"555","account_id":"777"}` +
			`],"paging":{}}`},
		"GET /act_777": {status: 200, body: `{"currency":"USD","id":"act_777"}`},
		"GET /act_777/insights": {status: 200, body: `{"data":[` +
			`{"ad_id":"901","adset_id":"888","campaign_id":"555","account_currency":"USD","impressions":"1000","clicks":"20","spend":"7.25"}` +
			`],"paging":{}}`},
	}
}

func TestMeta_ReadMetaAds_RendersAdsCreativesAndMetrics(t *testing.T) {
	d, api := adSetDispatcher(t, adReadRoutes())
	got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadMetaAds: %v", err)
	}
	if got.CampaignID != "camp-1" || got.PlatformCampaignID != "555" || got.AdSetID != "888" || got.Currency != "USD" ||
		got.Window != model.MetricsWindowLast7Days || !got.ReadAt.Equal(settingsClock) || got.Truncated || len(got.Ads) != 2 {
		t.Fatalf("header = %+v", got)
	}
	a, b := got.Ads[0], got.Ads[1]
	if a.ID != "901" || !a.Listed || derefOr(a.CreativeID) != "701" || derefOr(a.CreativeName) != "Creative A" ||
		a.Impressions != 1000 || a.Clicks != 20 || a.CostMicros != 7_250_000 || a.Ctr != 0.02 {
		t.Errorf("901 = %+v", a)
	}
	if b.ID != "902" || b.CreativeID != nil || b.Impressions != 0 || derefOr(b.Status) != "PAUSED" {
		t.Errorf("902 = %+v", b)
	}
	reqs := api.requests()
	assertSettingsReadOnly(t, reqs, "")
	// The ad set's ownership is proven FIRST, before any of its ads is read.
	if len(reqs) == 0 || reqs[0].Path != "/888" {
		t.Fatalf("first request = %+v, want the ad set's ownership read", reqs)
	}
}

// An ad set that is not this campaign's (or is under another account) is refused before its ads
// are read: an empty answer must never be given for another campaign's ad set.
func TestMeta_ReadMetaAds_ForeignAdSetIsRefusedBeforeItsAdsAreRead(t *testing.T) {
	for name, state := range map[string]string{
		"another campaign": `{"id":"888","campaign_id":"556","account_id":"777","status":"ACTIVE"}`,
		"another account":  `{"id":"888","campaign_id":"555","account_id":"999","status":"ACTIVE"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := adReadRoutes()
			r["GET /888"] = settingsRoute{status: 200, body: state}
			d, api := adSetDispatcher(t, r)
			got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast30Days)
			if got != nil || !errors.Is(err, domain.ErrMetaAdSetNotInCampaign) {
				t.Fatalf("got %+v, %v; want ErrMetaAdSetNotInCampaign", got, err)
			}
			if reqs := api.requests(); len(reqs) != 1 {
				t.Fatalf("sent %+v; want only the ownership read", reqs)
			}
		})
	}
}

// Every ad's parent ids are checked too: an ad reported under another ad set or campaign is an
// untrustworthy answer (503), and one under another account an identity mismatch (409).
func TestMeta_ReadMetaAds_AdParentIDsAreVerified(t *testing.T) {
	for _, tc := range []struct {
		name, row string
		mismatch  bool
	}{
		{"another ad set", `{"id":"901","adset_id":"889","campaign_id":"555","account_id":"777"}`, false},
		{"another campaign", `{"id":"901","adset_id":"888","campaign_id":"556","account_id":"777"}`, false},
		{"another account", `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"999"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := adReadRoutes()
			r["GET /888/ads"] = settingsRoute{status: 200, body: `{"data":[` + tc.row + `],"paging":{}}`}
			d, _ := adSetDispatcher(t, r)
			got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast30Days)
			if got != nil {
				t.Fatalf("an unproven ad was returned: %+v", got)
			}
			if tc.mismatch {
				assertUpstreamIdentityMismatch(t, err)
				return
			}
			assertSettings503(t, err)
		})
	}
}

// identityjson.Check runs on the raw bodies: a duplicated key cannot smuggle a different parent.
func TestMeta_ReadMetaAds_UntrustedBodyIsRefused(t *testing.T) {
	for name, route := range map[string][2]string{
		"ownership read":  {"GET /888", `{"id":"888","campaign_id":"556","campaign_id":"555","account_id":"777","status":"ACTIVE"}`},
		"ad listing":      {"GET /888/ads", `{"data":[{"id":"901","adset_id":"889","adset_id":"888","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		"ad insights row": {"GET /act_777/insights", `{"data":[{"ad_id":"901","adset_id":"888","campaign_id":"555","account_currency":"USD","spend":"1","spend":"2"}],"paging":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := adReadRoutes()
			r[route[0]] = settingsRoute{status: 200, body: route[1]}
			d, _ := adSetDispatcher(t, r)
			got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast30Days)
			if got != nil {
				t.Fatalf("an untrusted body was accepted: %+v", got)
			}
			assertSettings503(t, err)
		})
	}
}

// Graph 100/33 on the ad set or its ads: 503 on every HTTP status, never absence.
func TestMeta_ReadMetaAds_UpstreamErrorsAre503NeverAbsent(t *testing.T) {
	for _, key := range []string{"GET /888", "GET /888/ads", "GET /act_777/insights"} {
		for _, status := range []int{400, 403, 404, 500} {
			t.Run(fmt.Sprintf("%s %d", key, status), func(t *testing.T) {
				r := adReadRoutes()
				r[key] = settingsRoute{status: status, body: adSetGraphMissing}
				d, _ := adSetDispatcher(t, r)
				got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast30Days)
				if got != nil {
					t.Fatalf("an unverifiable read returned %+v", got)
				}
				assertSettings503(t, err)
			})
		}
	}
}

func TestMeta_ReadMetaAds_RefusalsSendNoRequest(t *testing.T) {
	t.Run("unsupported window", func(t *testing.T) {
		d, api := adSetDispatcher(t, adReadRoutes())
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindow("last_90_days"))
		if !errors.Is(err, domain.ErrMetricsWindowUnsupported) || len(api.requests()) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(api.requests()))
		}
	})
	t.Run("invalid ad set id", func(t *testing.T) {
		d, api := adSetDispatcher(t, adReadRoutes())
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "88/8", model.MetricsWindowLast30Days)
		if !errors.Is(err, domain.ErrMetaAdSetInvalid) || len(api.requests()) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(api.requests()))
		}
	})
	t.Run("invalid stored campaign id", func(t *testing.T) {
		d, api := adSetDispatcher(t, adReadRoutes())
		row := adSetRow()
		row.PlatformCampaignID = "0555"
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, row, "888", model.MetricsWindowLast30Days)
		if !errors.Is(err, domain.ErrStoredPlatformIDInvalid) || errors.Is(err, domain.ErrCampaignUpstreamIdentityMismatch) || len(api.requests()) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(api.requests()))
		}
	})
	t.Run("unknown provenance", func(t *testing.T) {
		var consulted atomic.Bool
		api := newAdSetAPI(t, adReadRoutes())
		d := NewMetaDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{}, meta.WithBaseURL(api.srv.URL))
		row := adSetRow()
		row.Result = json.RawMessage(`{"CampaignID":"555","AdSetID":"888"}`)
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, row, "888", model.MetricsWindowLast30Days)
		assertProvenanceUnknown(t, err)
		if consulted.Load() || len(api.requests()) != 0 {
			t.Fatalf("unknown provenance resolved a connection (%v) or sent %d request(s)", consulted.Load(), len(api.requests()))
		}
	})
	t.Run("account mismatch", func(t *testing.T) {
		d, api := adSetDispatcher(t, adReadRoutes())
		row := adSetRow()
		row.Result = json.RawMessage(`{"AccountID":"act_999","CampaignID":"555","AdSetID":"888"}`)
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, row, "888", model.MetricsWindowLast30Days)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("a mismatched account sent %d request(s)", n)
		}
	})
	t.Run("no connection carries ErrConnectionAbsent", func(t *testing.T) {
		api := newAdSetAPI(t, adReadRoutes())
		d := NewMetaDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, meta.WithBaseURL(api.srv.URL))
		_, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetricsWindowLast30Days)
		if !errors.Is(err, domain.ErrConnectionAbsent) || len(api.requests()) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(api.requests()))
		}
	})
}

// A hand-added ad set of the campaign (not the recorded one) is readable: reading changes nothing.
func TestMeta_ReadMetaAds_AnyAdSetOfTheCampaignIsReadable(t *testing.T) {
	r := adReadRoutes()
	r["GET /889"] = settingsRoute{status: 200, body: `{"id":"889","campaign_id":"555","account_id":"777","status":"PAUSED"}`}
	r["GET /889/ads"] = settingsRoute{status: 200, body: `{"data":[],"paging":{}}`}
	r["GET /act_777/insights"] = settingsRoute{status: 200, body: `{"data":[],"paging":{}}`}
	d, _ := adSetDispatcher(t, r)
	got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, adoptedAdSetRow(t), "889", model.MetricsWindowLast30Days)
	if err != nil || got.AdSetID != "889" || len(got.Ads) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
