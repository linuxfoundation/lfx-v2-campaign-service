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

// TestMeta_PreflightCreate_OptimizationGoalFlagOff: with META_OPTIMIZATION_GOAL_ENABLED unset or
// not exactly "true", metaConfig.optimizationGoal is a synchronous 400 naming the flag; blank and
// null are absent, and a wrong type is refused on its type rather than read as absent.
func TestMeta_PreflightCreate_OptimizationGoalFlagOff(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMetaOptimizationGoalEnabled, v)
			runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
				{name: "absent", config: `{"metaConfig":{"geoTargets":["US"]}}`},
				{name: "blank and null", config: `{"metaConfig":{"optimizationGoal":"  "}}`},
				{name: "null", config: `{"metaConfig":{"optimizationGoal":null}}`},
				{name: "landing page views", config: `{"metaConfig":{"optimizationGoal":"LANDING_PAGE_VIEWS"}}`,
					want: "metaConfig.optimizationGoal is disabled on this deployment (META_OPTIMIZATION_GOAL_ENABLED is not \"true\")"},
				{name: "a numeric goal, case-variant key", config: `{"metaConfig":{"OptimizationGoal":1}}`, want: `"OptimizationGoal" must be a string`},
			})
		})
	}
}

// TestMeta_PreflightCreate_OptimizationGoalFlagOn: with the flag on, the traffic-only rule, the
// allow-list and the link-click bid cap conflict are synchronous 400s.
func TestMeta_PreflightCreate_OptimizationGoalFlagOn(t *testing.T) {
	t.Setenv(constants.EnvMetaOptimizationGoalEnabled, "true")
	runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
		{name: "traffic", config: `{"metaConfig":{"objective":"traffic","optimizationGoal":"landing_page_views"}}`},
		{name: "default objective", config: `{"metaConfig":{"optimizationGoal":"LINK_CLICKS"}}`},
		{name: "awareness", config: `{"metaConfig":{"objective":"awareness","optimizationGoal":"LANDING_PAGE_VIEWS"}}`,
			want: `invalid campaign config: metaConfig.optimizationGoal: applies only to the traffic objective (objective is "awareness")`},
		{name: "unknown goal", config: `{"metaConfig":{"optimizationGoal":"REACH"}}`, want: `"REACH" is not one of LINK_CLICKS, LANDING_PAGE_VIEWS`},
		{name: "manual bid with landing page views", config: `{"metaConfig":{"optimizationGoal":"landing_page_views","bidding":{"strategy":"manual","bidAmount":1.5}}}`,
			want: "metaConfig.bidding: manual bidding is a cost-per-link-click bid cap, which needs the LINK_CLICKS optimization goal"},
		{name: "manual bid with link clicks", config: `{"metaConfig":{"optimizationGoal":"LINK_CLICKS","bidding":{"strategy":"manual","bidAmount":1.5}}}`},
	})
}

// TestMeta_DispatchOptimizationGoal: a path that skipped the pre-check is refused pre-create while
// the flag is off, with Meta never called; with the flag on the goal reaches the ad set normalized.
func TestMeta_DispatchOptimizationGoal(t *testing.T) {
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"optimizationGoal":"landing_page_views","variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	clock := meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) })
	t.Run("flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaOptimizationGoalEnabled, "false")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL), clock)
		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
		var nuc interface{ NoUpstreamCreate() bool }
		if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
			!strings.Contains(err.Error(), constants.EnvMetaOptimizationGoalEnabled) {
			t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
		}
	})
	t.Run("flag on", func(t *testing.T) {
		t.Setenv(constants.EnvMetaOptimizationGoalEnabled, "true")
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
		d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL), clock)
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if adSet["optimization_goal"] != "LANDING_PAGE_VIEWS" {
			t.Fatalf("optimization_goal = %v, want LANDING_PAGE_VIEWS", adSet["optimization_goal"])
		}
	})
}
