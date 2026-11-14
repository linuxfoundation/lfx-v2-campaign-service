// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
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
