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

// TestMeta_PreflightCreate_AdSchedule: off, a schedule is a 400 naming META_AD_SCHEDULE_ENABLED; on,
// a daily budget and bad days or minutes are 400s, and a wrong-typed schedule is refused on its type.
func TestMeta_PreflightCreate_AdSchedule(t *testing.T) {
	const sched = `"adSchedule":{"days":[1,2,3,4,5],"startMinute":540,"endMinute":1020}`
	// lifetime opens every config below; the budget flag is the field adSchedule depends on.
	const lifetime = `{"metaConfig":{"objective":"traffic","lifetimeBudget":true`
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaAdScheduleEnabled, "1")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "absent", config: lifetime + `}}`},
			{name: "null", config: lifetime + `,"adSchedule":null}}`},
			{name: "a schedule", config: lifetime + `,` + sched + `}}`,
				want: "metaConfig.adSchedule is disabled on this deployment (META_AD_SCHEDULE_ENABLED is not \"true\")"},
		})
	})
	t.Run("on", func(t *testing.T) {
		t.Setenv(constants.EnvMetaAdScheduleEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "lifetime", config: lifetime + `,` + sched + `}}`},
			{name: "daily", config: `{"metaConfig":{` + sched + `}}`,
				want: "invalid campaign config: metaConfig.adSchedule: needs a lifetime budget"},
			{name: "a weekend day 7", config: lifetime + `,"adSchedule":{"days":[7],"startMinute":0,"endMinute":60}}}`,
				want: "day 7 is out of range"},
			{name: "half hours", config: lifetime + `,"adSchedule":{"days":[1],"startMinute":30,"endMinute":90}}}`,
				want: "must be whole hours"},
			{name: "a string schedule", config: lifetime + `,"adSchedule":"weekdays"}}`,
				want: `"adSchedule" must be an object of days (an integer list), startMinute and endMinute (integers)`},
			{name: "a wrong-typed field, broken budget", config: `{"metaConfig":{"budget":"lots","lifetimeBudget":true,"AdSchedule":{"days":["mon"],"startMinute":0,"endMinute":60}}}`,
				want: `"AdSchedule" must be an object of`},
		})
	})
}

// TestMeta_DispatchCarriesTheAdSchedule: with the flag on the schedule reaches the lifetime ad set;
// off, Dispatch refuses it before any request.
func TestMeta_DispatchCarriesTheAdSchedule(t *testing.T) {
	t.Setenv(constants.EnvMetaAdScheduleEnabled, "true")
	var (
		mu       sync.Mutex
		requests int
		adSet    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		requests++
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
	cfg := json.RawMessage(`{"metaConfig":{"budget":1000,"lifetimeBudget":true,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"adSchedule":{"days":[2,4],"startMinute":480,"endMinute":1080},"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	gotSched, gotPacing := adSet["adset_schedule"], adSet["pacing_type"]
	mu.Unlock()
	want := []any{map[string]any{"start_minute": float64(480), "end_minute": float64(1080), "days": []any{float64(2), float64(4)}, "timezone_type": "ADVERTISER"}}
	if !reflect.DeepEqual(gotSched, want) || !reflect.DeepEqual(gotPacing, []any{"day_parting"}) {
		t.Fatalf("adset_schedule = %#v, pacing_type = %#v; want %#v and [day_parting]", gotSched, gotPacing, want)
	}

	t.Setenv(constants.EnvMetaAdScheduleEnabled, "false")
	mu.Lock()
	requests = 0
	mu.Unlock()
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !strings.Contains(err.Error(), constants.EnvMetaAdScheduleEnabled) {
		t.Fatalf("Dispatch with the flag off = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("%d requests with the flag off, want none", requests)
	}
}
