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

// TestMeta_PreflightCreate_LocalesFlag: targeting.locales has its own flag. While it is off the list
// is a 400 naming it; while it is on, a locales-only block needs no META_AUDIENCE_TARGETING_ENABLED,
// but an audience field beside it, or an empty block, still does.
func TestMeta_PreflightCreate_LocalesFlag(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaLocaleTargetingEnabled, "TRUE")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "locales", config: `{"metaConfig":{"targeting":{"locales":[6]}}}`,
				want: "metaConfig.targeting.locales is disabled on this deployment (META_LOCALE_TARGETING_ENABLED is not \"true\")"},
			{name: "empty locales is absent", config: `{"metaConfig":{"targeting":{"ageMin":25,"locales":[]}}}`},
		})
	})
	t.Run("on, audience flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaLocaleTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "locales only", config: `{"metaConfig":{"targeting":{"locales":[6,1001]}}}`},
			{name: "locales and an age", config: `{"metaConfig":{"targeting":{"locales":[6],"ageMin":25}}}`,
				want: "metaConfig.targeting is disabled on this deployment (META_AUDIENCE_TARGETING_ENABLED is not \"true\")"},
			{name: "empty block", config: `{"metaConfig":{"targeting":{}}}`, want: "META_AUDIENCE_TARGETING_ENABLED"},
			{name: "a zero locale", config: `{"metaConfig":{"targeting":{"locales":[0]}}}`,
				want: "invalid campaign config: metaConfig.targeting: locales entry 0 is not a Meta locale key"},
			{name: "a string locale", config: `{"metaConfig":{"targeting":{"locales":["en_US"]}}}`,
				want: `"targeting" must be an object of`},
		})
	})
}

// TestMeta_PreflightCreate_DevicePlatformsFlag: targeting.devicePlatforms has its own flag, like
// locales; with it on, an unknown device and desktop-only with Audience Network are 400s.
func TestMeta_PreflightCreate_DevicePlatformsFlag(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaDeviceTargetingEnabled, "1")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "devices", config: `{"metaConfig":{"targeting":{"devicePlatforms":["mobile"]}}}`,
				want: "metaConfig.targeting.devicePlatforms is disabled on this deployment (META_DEVICE_TARGETING_ENABLED is not \"true\")"},
		})
	})
	t.Run("on, audience flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaDeviceTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
		t.Setenv(constants.EnvMetaLocaleTargetingEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "devices only", config: `{"metaConfig":{"targeting":{"devicePlatforms":["desktop"]}}}`},
			{name: "devices and genders", config: `{"metaConfig":{"targeting":{"devicePlatforms":["mobile"],"genders":["male"]}}}`,
				want: "META_AUDIENCE_TARGETING_ENABLED"},
			{name: "devices and locales", config: `{"metaConfig":{"targeting":{"devicePlatforms":["mobile"],"locales":[6]}}}`,
				want: "META_LOCALE_TARGETING_ENABLED"},
			{name: "unknown device", config: `{"metaConfig":{"targeting":{"devicePlatforms":["tv"]}}}`,
				want: `invalid campaign config: metaConfig.targeting: devicePlatforms entry "tv" is not one of mobile, desktop`},
			{name: "desktop with audience network", config: `{"metaConfig":{"placements":{"audienceNetwork":true},"targeting":{"devicePlatforms":["desktop"]}}}`,
				want: "cannot serve the audienceNetwork placement"},
			{name: "a string list as a string", config: `{"metaConfig":{"targeting":{"devicePlatforms":"mobile"}}}`,
				want: `"targeting" must be an object of`},
		})
	})
}

// TestMeta_DispatchCarriesDevicePlatformsToTheAdSet: with only the device flag on, one device reaches
// the ad set as device_platforms, and Dispatch refuses desktop-only with Audience Network before any
// request.
func TestMeta_DispatchCarriesDevicePlatformsToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaDeviceTargetingEnabled, "true")
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	targeting := dispatchMetaAdSetTargeting(t, `"targeting":{"devicePlatforms":["Mobile"]}`)
	if !reflect.DeepEqual(targeting["device_platforms"], []any{"mobile"}) {
		t.Errorf("ad set targeting.device_platforms = %#v, want [mobile]", targeting["device_platforms"])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"placements":{"audienceNetwork":true},"targeting":{"devicePlatforms":["desktop"]},"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !strings.Contains(err.Error(), "audienceNetwork") {
		t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate refusal naming audienceNetwork", camp, err)
	}
}

// TestMeta_DispatchCarriesLocalesToTheAdSet: with only the locale flag on, a locales-only block
// reaches the ad set de-duplicated, with no Advantage+ flag.
func TestMeta_DispatchCarriesLocalesToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaLocaleTargetingEnabled, "true")
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	targeting := dispatchMetaAdSetTargeting(t, `"targeting":{"locales":[24,6,24]}`)
	if !reflect.DeepEqual(targeting["locales"], []any{float64(24), float64(6)}) {
		t.Errorf("ad set targeting.locales = %#v, want [24 6]", targeting["locales"])
	}
	if _, has := targeting["targeting_automation"]; has {
		t.Errorf("targeting_automation = %v, want it absent", targeting["targeting_automation"])
	}
}

// dispatchMetaAdSetTargeting dispatches a minimal Meta create carrying extra metaConfig fields
// against a fake Graph API and returns the ad set's targeting object.
func dispatchMetaAdSetTargeting(t *testing.T, extra string) map[string]any {
	t.Helper()
	var (
		mu    sync.Mutex
		adSet map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
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
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],` +
		extra + `,"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	targeting, ok := adSet["targeting"].(map[string]any)
	if !ok {
		t.Fatalf("ad set targeting = %#v, want an object", adSet["targeting"])
	}
	return targeting
}
