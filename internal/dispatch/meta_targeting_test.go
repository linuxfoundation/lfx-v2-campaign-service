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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestMeta_PreflightCreate_TargetingFlagOff: with META_AUDIENCE_TARGETING_ENABLED unset (or any
// value but "true") any targeting block is a synchronous 400 naming the flag; absent and null are
// not targeting at all.
func TestMeta_PreflightCreate_TargetingFlagOff(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMetaAudienceTargetingEnabled, v)
			runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
				{name: "no targeting", config: `{"metaConfig":{"geoTargets":["US"]}}`},
				{name: "null targeting", config: `{"metaConfig":{"targeting":null}}`},
				{name: "age range", config: `{"metaConfig":{"targeting":{"ageMin":25}}}`,
					want: "metaConfig.targeting is disabled on this deployment (META_AUDIENCE_TARGETING_ENABLED is not \"true\")"},
				{name: "empty block", config: `{"metaConfig":{"targeting":{}}}`, want: "is disabled on this deployment"},
			})
		})
	}
}

// TestMeta_PreflightCreate_TargetingFlagOn: with the flag on, the #550 rules are a synchronous 400
// and a valid block is left to Dispatch. A wrong-typed block is refused on its type rather than
// read as absent.
func TestMeta_PreflightCreate_TargetingFlagOn(t *testing.T) {
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
	runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
		{name: "full block", config: `{"metaConfig":{"targeting":{"ageMin":25,"ageMax":54,"genders":["female"],"customAudiences":["111"],"excludedCustomAudiences":["222"],"advantageAudience":false}}}`},
		{name: "explicit ageMin 0", config: `{"metaConfig":{"targeting":{"ageMin":0}}}`, want: "ageMin 0 is out of range"},
		{name: "explicit ageMax 0", config: `{"metaConfig":{"targeting":{"ageMax":0}}}`, want: "ageMax 0 is out of range"},
		{name: "null ages are absent", config: `{"metaConfig":{"targeting":{"ageMin":null,"ageMax":null,"genders":["male"]}}}`},
		{name: "out of range age", config: `{"metaConfig":{"targeting":{"ageMax":70}}}`,
			want: "invalid campaign config: metaConfig.targeting: ageMax 70 is out of range"},
		{name: "advantage with a narrow age", config: `{"metaConfig":{"targeting":{"ageMin":35,"advantageAudience":true}}}`,
			want: "ageMin of at most 25"},
		{name: "audience in both lists", config: `{"metaConfig":{"targeting":{"customAudiences":["111"],"excludedCustomAudiences":["111"]}}}`,
			want: "is in both"},
		{name: "a string block", config: `{"metaConfig":{"targeting":"25-54"}}`,
			want: `invalid campaign config: metaConfig: "targeting" must be an object`},
		{name: "a wrong-typed field, case-variant key", config: `{"metaConfig":{"Targeting":{"ageMin":"25"}}}`,
			want: `"Targeting" must be an object`},
		{name: "two case-variant keys", config: `{"metaConfig":{"targeting":{"ageMin":25},"Targeting":{"ageMin":99}}}`,
			want: `more than one targeting key ("Targeting", "targeting")`},
		// Best-effort decode elsewhere: an unrelated broken field must not let bad targeting through.
		{name: "bad targeting, broken budget", config: `{"metaConfig":{"budget":"lots","targeting":{"genders":["x"]}}}`,
			want: "is not one of male, female"},
	})
}

// TestMeta_DispatchRefusesTargetingWhileTheFlagIsOff: a path that skipped the pre-check is refused
// pre-create with the claim released, and Meta is never called.
func TestMeta_DispatchRefusesTargetingWhileTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"targeting":{"excludedCustomAudiences":["222"]},"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
		!strings.Contains(err.Error(), "META_AUDIENCE_TARGETING_ENABLED") {
		t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
	}
}

// TestMeta_DispatchCarriesTargetingToTheAdSet: with the flag on, the wire block reaches the ad set
// normalized, after the audiences were read under the connection's account.
func TestMeta_DispatchCarriesTargetingToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
	var (
		mu        sync.Mutex
		adSet     map[string]any
		audiences []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("fields") == "id,account_id,subtype":
			id := strings.TrimPrefix(r.URL.Path, "/")
			audiences = append(audiences, id)
			_, _ = io.WriteString(w, `{"id":"`+id+`","account_id":"777","subtype":"CUSTOM"}`)
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
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"targeting":{"ageMin":25,"ageMax":54,"genders":["Male"],"customAudiences":["111"],"excludedCustomAudiences":["222"]},
		"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	targeting, _ := adSet["targeting"].(map[string]any)
	want := map[string]any{
		"age_min": float64(25), "age_max": float64(54), "genders": []any{float64(1)},
		"custom_audiences":          []any{map[string]any{"id": "111"}},
		"excluded_custom_audiences": []any{map[string]any{"id": "222"}},
		"targeting_automation":      map[string]any{"advantage_audience": float64(0)},
	}
	for k, w := range want {
		if !reflect.DeepEqual(targeting[k], w) {
			t.Errorf("ad set targeting[%s] = %#v, want %#v", k, targeting[k], w)
		}
	}
	if !reflect.DeepEqual(audiences, []string{"111", "222"}) {
		t.Errorf("audience reads = %v, want [111 222]", audiences)
	}
}

// TestEveryMetaTargetingFieldIsClassifiedForTheMarketingOpsGate: the service gate names exactly the
// targeting fields that reference an account resource — the two audience lists — and every other
// targeting field is classified as not gated. Adding a field to meta.AudienceTargeting without
// classifying it here fails, so a new account-resource field cannot skip the role check.
func TestEveryMetaTargetingFieldIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	gated := map[string]string{"customAudiences": `["1"]`, "excludedCustomAudiences": `["1"]`}
	notGated := map[string]string{"ageMin": `30`, "ageMax": `40`, "genders": `["male"]`, "advantageAudience": `false`, "locales": `[6]`, "devicePlatforms": `["mobile"]`, "interests": `["6003"]`, "regions": `["3847"]`, "cities": `["2420605"]`}
	typ := reflect.TypeOf(meta.AudienceTargeting{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		value, isGated := gated[name]
		if !isGated {
			var ok bool
			if value, ok = notGated[name]; !ok {
				t.Errorf("metaConfig.targeting.%s is not classified for the marketing-ops gate", name)
				continue
			}
		}
		raw := json.RawMessage(`{"metaConfig":{"targeting":{"` + name + `":` + value + `}}}`)
		got := service.MetaMarketingOpsInputs(raw)
		switch {
		case isGated && !reflect.DeepEqual(got, []string{"targeting." + name}):
			t.Errorf("MetaMarketingOpsInputs(%s) = %v, want [targeting.%s]", name, got, name)
		case !isGated && len(got) != 0:
			t.Errorf("MetaMarketingOpsInputs(%s) = %v, want none", name, got)
		}
	}
}
