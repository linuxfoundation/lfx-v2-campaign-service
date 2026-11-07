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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestMeta_PreflightCreate_CreativeOptionsFlagsOff: with either flag unset or not exactly "true",
// its field is a synchronous 400 naming the flag; blank is absent, and a wrong type is refused on
// its type rather than read as absent.
func TestMeta_PreflightCreate_CreativeOptionsFlagsOff(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMetaCallToActionEnabled, v)
			t.Setenv(constants.EnvMetaConversionEventEnabled, v)
			runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
				{name: "neither", config: `{"metaConfig":{"geoTargets":["US"]}}`},
				{name: "blank and null", config: `{"metaConfig":{"callToAction":"  ","conversionEvent":null}}`},
				{name: "call to action", config: `{"metaConfig":{"callToAction":"SIGN_UP"}}`,
					want: "metaConfig.callToAction is disabled on this deployment (META_CALL_TO_ACTION_ENABLED is not \"true\")"},
				{name: "conversion event", config: `{"metaConfig":{"objective":"conversions","conversionEvent":"LEAD"}}`,
					want: "metaConfig.conversionEvent is disabled on this deployment (META_CONVERSION_EVENT_ENABLED is not \"true\")"},
				{name: "a numeric call to action", config: `{"metaConfig":{"callToAction":1}}`, want: `"callToAction" must be a string`},
				{name: "a boolean event, case-variant key", config: `{"metaConfig":{"ConversionEvent":true}}`, want: `"ConversionEvent" must be a string`},
			})
		})
	}
}

// TestMeta_PreflightCreate_CreativeOptionsFlagsOn: each flag gates only its own field, and with it
// on the allow-lists and the conversions-only rule are synchronous 400s.
func TestMeta_PreflightCreate_CreativeOptionsFlagsOn(t *testing.T) {
	t.Run("call to action only", func(t *testing.T) {
		t.Setenv(constants.EnvMetaCallToActionEnabled, "true")
		t.Setenv(constants.EnvMetaConversionEventEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "allowed", config: `{"metaConfig":{"callToAction":"buy_tickets"}}`},
			{name: "not a Meta button", config: `{"metaConfig":{"callToAction":"REGISTER_NOW"}}`, want: "metaConfig.callToAction: \"REGISTER_NOW\" is not one of"},
			{name: "event still gated", config: `{"metaConfig":{"objective":"conversions","conversionEvent":"LEAD"}}`, want: "META_CONVERSION_EVENT_ENABLED"},
		})
	})
	t.Run("conversion event only", func(t *testing.T) {
		t.Setenv(constants.EnvMetaConversionEventEnabled, "true")
		t.Setenv(constants.EnvMetaCallToActionEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "allowed", config: `{"metaConfig":{"objective":"Conversions","conversionEvent":"complete_registration"}}`},
			{name: "on traffic", config: `{"metaConfig":{"objective":"traffic","conversionEvent":"LEAD"}}`, want: "applies only to the conversions objective"},
			{name: "default objective", config: `{"metaConfig":{"conversionEvent":"LEAD"}}`, want: `applies only to the conversions objective (objective is "traffic")`},
			{name: "unknown event", config: `{"metaConfig":{"objective":"conversions","conversionEvent":"ADD_TO_CART"}}`, want: "is not one of PURCHASE"},
			{name: "button still gated", config: `{"metaConfig":{"callToAction":"SIGN_UP"}}`, want: "META_CALL_TO_ACTION_ENABLED"},
		})
	})
}

// TestMeta_DispatchRefusesCreativeOptionsWhileFlagsAreOff: a path that skipped the pre-check is
// refused pre-create with the claim released, and Meta is never called.
func TestMeta_DispatchRefusesCreativeOptionsWhileFlagsAreOff(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"call to action", `"callToAction":"SIGN_UP"`, "META_CALL_TO_ACTION_ENABLED"},
		{"conversion event", `"objective":"conversions","pixelId":"123","conversionEvent":"LEAD"`, "META_CONVERSION_EVENT_ENABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both flags off explicitly, so an exported developer environment cannot change the result.
			t.Setenv(constants.EnvMetaCallToActionEnabled, "false")
			t.Setenv(constants.EnvMetaConversionEventEnabled, "false")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
			cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],` +
				tc.extra + `,"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
			var nuc interface{ NoUpstreamCreate() bool }
			if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate refusal naming %s", camp, err, tc.want)
			}
		})
	}
}

// TestMeta_DispatchCarriesCreativeOptions: with both flags on, the wire values reach the creative's
// button and the ad set's promoted object, normalized.
func TestMeta_DispatchCarriesCreativeOptions(t *testing.T) {
	t.Setenv(constants.EnvMetaCallToActionEnabled, "true")
	t.Setenv(constants.EnvMetaConversionEventEnabled, "true")
	var (
		mu              sync.Mutex
		adSet, creative map[string]any
	)
	decode := func(r *http.Request) map[string]any {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("decode %s body: %v", r.URL.Path, err)
		}
		return m
	}
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
			adSet = decode(r)
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creative = decode(r)
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
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"objective":"conversions","pixelId":"1234567890","conversionEvent":"complete_registration","callToAction":"sign_up",
		"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	po, _ := adSet["promoted_object"].(map[string]any)
	if po["custom_event_type"] != "COMPLETE_REGISTRATION" {
		t.Errorf("promoted_object = %v, want custom_event_type COMPLETE_REGISTRATION", po)
	}
	spec, _ := creative["object_story_spec"].(map[string]any)
	link, _ := spec["link_data"].(map[string]any)
	if cta, _ := link["call_to_action"].(map[string]any); cta["type"] != "SIGN_UP" {
		t.Errorf("call_to_action = %v, want SIGN_UP", link["call_to_action"])
	}
}
