// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestMeta_PreflightCreate_RegionTargetingFlag: targeting.regions and targeting.cities have their own
// flag; off, each is a 400 naming it; on, a region-only block needs no audience flag, a name is a
// 400 and a wrong type is refused on its type.
func TestMeta_PreflightCreate_RegionTargetingFlag(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaRegionTargetingEnabled, "on")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "regions", config: `{"metaConfig":{"targeting":{"regions":["3847"]}}}`,
				want: "metaConfig.targeting.regions is disabled on this deployment (META_REGION_TARGETING_ENABLED is not \"true\")"},
			{name: "cities", config: `{"metaConfig":{"targeting":{"cities":["2420605"]}}}`,
				want: "metaConfig.targeting.cities is disabled on this deployment (META_REGION_TARGETING_ENABLED is not \"true\")"},
		})
	})
	t.Run("on, audience flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaRegionTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "regions and cities", config: `{"metaConfig":{"targeting":{"regions":["3847"],"cities":["2420605"]}}}`},
			{name: "a region and an age", config: `{"metaConfig":{"targeting":{"regions":["3847"],"ageMin":30}}}`,
				want: "META_AUDIENCE_TARGETING_ENABLED"},
			{name: "a region name", config: `{"metaConfig":{"targeting":{"regions":["California"]}}}`,
				want: `invalid campaign config: metaConfig.targeting: regions entry "California" is not a Meta region key`},
			{name: "numbers instead of strings", config: `{"metaConfig":{"targeting":{"cities":[2420605]}}}`,
				want: `"targeting" must be an object of`},
		})
	})
}

// TestMeta_DispatchCarriesRegionsToTheAdSet: with only the region flag on, Dispatch looks the keys
// up and adds them to geo_locations; off, it refuses before any request.
func TestMeta_DispatchCarriesRegionsToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaRegionTargetingEnabled, "true")
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	var (
		mu       sync.Mutex
		requests []string
		adSet    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search" && r.URL.Query().Get("type") == "adgeolocationmeta":
			_, _ = io.WriteString(w, `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"US"}}}}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &adSet); err != nil {
				t.Errorf("decode ad set body: %v", err)
			}
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["CA"],
		"targeting":{"regions":["3847"]},"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	targeting, _ := adSet["targeting"].(map[string]any)
	mu.Unlock()
	want := map[string]any{"countries": []any{"CA"}, "regions": []any{map[string]any{"key": "3847"}}}
	if !reflect.DeepEqual(targeting["geo_locations"], want) {
		t.Fatalf("geo_locations = %#v, want %#v", targeting["geo_locations"], want)
	}

	t.Setenv(constants.EnvMetaRegionTargetingEnabled, "false")
	mu.Lock()
	requests = nil
	mu.Unlock()
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !strings.Contains(err.Error(), constants.EnvMetaRegionTargetingEnabled) {
		t.Fatalf("Dispatch with the flag off = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("requests with the flag off = %v, want none", requests)
	}
}
