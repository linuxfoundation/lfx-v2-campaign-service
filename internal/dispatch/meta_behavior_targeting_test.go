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

// TestMeta_PreflightCreate_BehaviorTargetingFlag: targeting.behaviors and targeting.workPositions
// share their own flag; off, each is a 400 naming it; on, a block of only those needs no audience
// flag, a name is a 400 and a wrong type is refused on its type.
func TestMeta_PreflightCreate_BehaviorTargetingFlag(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaBehaviorTargetingEnabled, "on")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "behaviors", config: `{"metaConfig":{"targeting":{"behaviors":["6002714895372"]}}}`,
				want: "metaConfig.targeting.behaviors is disabled on this deployment (META_BEHAVIOR_TARGETING_ENABLED is not \"true\")"},
			{name: "work positions", config: `{"metaConfig":{"targeting":{"workPositions":["105763692790962"]}}}`,
				want: "metaConfig.targeting.workPositions is disabled on this deployment (META_BEHAVIOR_TARGETING_ENABLED is not \"true\")"},
		})
	})
	t.Run("on, audience flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaBehaviorTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "behaviors and work positions", config: `{"metaConfig":{"targeting":{"behaviors":["6002714895372"],"workPositions":["105763692790962"]}}}`},
			{name: "a behavior and a gender", config: `{"metaConfig":{"targeting":{"behaviors":["6002714895372"],"genders":["female"]}}}`,
				want: "META_AUDIENCE_TARGETING_ENABLED"},
			{name: "a work position name", config: `{"metaConfig":{"targeting":{"workPositions":["Engineer"]}}}`,
				want: `invalid campaign config: metaConfig.targeting: workPositions entry "Engineer" is not a Meta work position id`},
			{name: "numbers instead of strings", config: `{"metaConfig":{"targeting":{"behaviors":[6002714895372]}}}`,
				want: `"targeting" must be an object of`},
		})
	})
}

// TestMeta_DispatchCarriesBehaviorsToTheAdSet: with only the behavior flag on, Dispatch validates
// the ids in one read and sends them in flexible_spec with an explicit Advantage+ flag of 0; off,
// it refuses before any request.
func TestMeta_DispatchCarriesBehaviorsToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaBehaviorTargetingEnabled, "true")
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	var (
		mu        sync.Mutex
		requests  []string
		validated string
		adSet     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/targetingvalidation"):
			validated = r.URL.Query().Get("targeting_list")
			_, _ = io.WriteString(w, `{"data":[{"id":6002714895372,"type":"behaviors","valid":true},{"id":"105763692790962","type":"work_positions","valid":true}]}`)
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
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"targeting":{"behaviors":["6002714895372"],"workPositions":[" 105763692790962"]},"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	targeting, _ := adSet["targeting"].(map[string]any)
	gotValidated := validated
	mu.Unlock()
	if want := `[{"type":"behaviors","id":6002714895372},{"type":"work_positions","id":105763692790962}]`; gotValidated != want {
		t.Errorf("targeting_list = %s, want %s", gotValidated, want)
	}
	want := []any{map[string]any{
		"behaviors":      []any{map[string]any{"id": "6002714895372"}},
		"work_positions": []any{map[string]any{"id": "105763692790962"}},
	}}
	if !reflect.DeepEqual(targeting["flexible_spec"], want) {
		t.Errorf("flexible_spec = %#v, want %#v", targeting["flexible_spec"], want)
	}
	if !reflect.DeepEqual(targeting["targeting_automation"], map[string]any{"advantage_audience": float64(0)}) {
		t.Errorf("targeting_automation = %#v, want advantage_audience 0", targeting["targeting_automation"])
	}

	t.Setenv(constants.EnvMetaBehaviorTargetingEnabled, "false")
	mu.Lock()
	requests = nil
	mu.Unlock()
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !strings.Contains(err.Error(), constants.EnvMetaBehaviorTargetingEnabled) {
		t.Fatalf("Dispatch with the flag off = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("requests with the flag off = %v, want none", requests)
	}
}
