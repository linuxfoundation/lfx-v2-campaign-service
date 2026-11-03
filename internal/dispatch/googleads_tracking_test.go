// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// Adoption binds a campaign that already exists and creates nothing (#476): the
// utmcampaign custom parameter rides only on a campaign CREATE, so an adopted campaign is
// never sent one — not as a create, and not as an update of the campaign it found. Every
// request is recorded, so a later change that pushed the parameter onto an adopted
// campaign through any endpoint would show here.
func TestGoogleAds_Adoption_SendsNoUTMCampaignParameter(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	var (
		mu     sync.Mutex
		paths  []string
		bodies []string
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"999","name":"LFX | DemandGen Campaign | cncf | KubeCon NA 2026 | brief-1","status":"PAUSED","resourceName":"customers/1234567890/campaigns/999"}}]}`)
			return
		}
		http.Error(w, "adoption must send nothing but the lookup", http.StatusBadRequest)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)},
		identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL),
		googleads.WithBaseURL(apiSrv.URL),
	)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","adoptExisting":true}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "999" {
		t.Fatalf("want the adopted campaign 999, got %+v", camp)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("adoption sent no lookup at all")
	}
	for i, p := range paths {
		if !strings.HasSuffix(p, "googleAds:search") {
			t.Errorf("adoption sent %s — it must send only the name lookup", p)
		}
		if strings.Contains(bodies[i], "utmcampaign") || strings.Contains(bodies[i], "urlCustomParameters") {
			t.Errorf("adoption sent the utmcampaign custom parameter to %s: %s", p, bodies[i])
		}
	}
}
