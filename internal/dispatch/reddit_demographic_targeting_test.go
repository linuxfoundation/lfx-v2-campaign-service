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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const demographicExtra = `,"gender":"female","languages":["en","de"],"schedule":[{"startDay":1,"startHour":9,"endDay":5,"endHour":17}]`

func TestRedditDemographicTargeting_PreflightRefusesWhileTheDeploymentGateIsOff(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, flag := range []string{"", "TRUE"} { // a typo fails closed
		t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, flag)
		for _, extra := range []string{`,"gender":"MALE"`, `,"languages":["EN"]`, `,"schedule":[{"startDay":0,"startHour":0,"endDay":6,"endHour":23}]`} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(extra))
			requireConversionEventRefusal(t, err, constants.EnvRedditGenderLanguageDaypartingEnabled)
		}
	}
}

func TestRedditDemographicTargeting_AbsentOrEmptyIsNeverRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, flag)
		d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
		for _, extra := range []string{``, `,"languages":[],"schedule":[],"gender":null`} {
			if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
				t.Fatalf("flag %q %s: want admitted, got %v", flag, extra, err)
			}
		}
	}
}

func TestRedditDemographicTargeting_PreflightRefusesBadValues(t *testing.T) {
	t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, extra, want string }{
		{"gender number", `,"gender":1`, "redditConfig.gender: must be a string"},
		{"gender blank", `,"gender":""`, "redditConfig.gender: an empty value is refused"},
		{"gender unknown", `,"gender":"ALL"`, "redditConfig.gender: \"ALL\" is not one of"},
		{"languages string", `,"languages":"EN"`, "redditConfig.languages: must be an array of strings"},
		{"language unknown", `,"languages":["EN","XX"]`, "redditConfig.languages: entry 2"},
		{"schedule object", `,"schedule":{"startDay":1}`, "redditConfig.schedule: must be an array"},
		{"schedule string hour", `,"schedule":[{"startDay":1,"startHour":"9","endDay":5,"endHour":17}]`, "redditConfig.schedule: must be an array"},
		{"schedule missing", `,"schedule":[{"startDay":1,"startHour":9,"endDay":5}]`, "redditConfig.schedule: entry 1 needs"},
		{"schedule range", `,"schedule":[{"startDay":1,"startHour":9,"endDay":5,"endHour":24}]`, "redditConfig.schedule: entry 1: hours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(tc.extra))
			requireConversionEventRefusal(t, err, tc.want)
		})
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(demographicExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("valid choices: want admitted, got %v", err)
	}
}

// demographicAPI records the campaign create's schedule and the ad-group create's targeting.
type demographicAPI struct {
	mu        sync.Mutex
	requests  int
	campaign  map[string]any
	targeting map[string]any
}

func (a *demographicAPI) dispatcher(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests++
		var body struct {
			Data map[string]any `json:"data"`
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			a.campaign = body.Data
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"):
			a.targeting, _ = body.Data["targeting"].(map[string]any)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
}

func TestRedditDemographicTargeting_DispatchSendsTheChoices(t *testing.T) {
	t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, "true")
	a := &demographicAPI{}
	if _, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(demographicExtra)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if a.targeting["gender"] != "FEMALE" || !reflect.DeepEqual(a.targeting["languages"], []any{"DE", "EN"}) {
		t.Fatalf("targeting = %#v; want gender FEMALE, languages [DE EN]", a.targeting)
	}
	want := []any{map[string]any{"start_day": float64(1), "start_hour": float64(9), "end_day": float64(5), "end_hour": float64(17)}}
	if !reflect.DeepEqual(a.campaign["schedule"], want) {
		t.Fatalf("campaign schedule = %#v, want %#v", a.campaign["schedule"], want)
	}
}

// Dispatch re-checks for any path that skipped PreflightCreate, before any upstream request.
func TestRedditDemographicTargeting_DispatchRefusesBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra, want string }{
		{"gate off", "", `,"gender":"MALE"`, constants.EnvRedditGenderLanguageDaypartingEnabled},
		{"unknown language", "true", `,"languages":["XX"]`, "redditConfig.languages"},
		// A wrong type fails the config decode too; the field-specific refusal must win.
		{"mistyped schedule", "true", `,"schedule":"weekdays"`, "redditConfig.schedule: must be an array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, tc.flag)
			a := &demographicAPI{}
			camp, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(tc.extra))
			if camp != nil {
				t.Fatalf("no campaign may be returned, got %+v", camp)
			}
			requirePreCreate(t, err)
			requireConversionEventRefusal(t, err, tc.want)
			if msg := service.DispatchFailureMessage(model.ProviderRedditAds, err, true); !strings.Contains(msg, tc.want) {
				t.Fatalf("job message %q lost the refusal", msg)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.requests != 0 {
				t.Fatalf("%d Reddit requests were sent before the refusal", a.requests)
			}
		})
	}
}

// A stored config_snapshot replays: omitted choices stay absent, set ones replay unchanged.
func TestRedditDemographicTargeting_ConfigSnapshotReplays(t *testing.T) {
	for _, tc := range []struct{ flag, extra string }{{"", ``}, {"true", demographicExtra}} {
		t.Setenv(constants.EnvRedditGenderLanguageDaypartingEnabled, tc.flag)
		var cfg redditConfig
		mustDecode(t, conversionEventCfg(tc.extra), "redditConfig", &cfg)
		snapshot := campaignFromReddit(context.Background(), &reddit.CampaignResult{}, cfg).ConfigSnapshot
		if tc.extra == `` {
			for _, k := range []string{`"gender"`, `"languages"`, `"schedule"`} {
				if strings.Contains(string(snapshot), k) {
					t.Fatalf("snapshot %s records an omitted %s", snapshot, k)
				}
			}
		}
		var again redditConfig
		mustDecode(t, json.RawMessage(`{"redditConfig":`+string(snapshot)+`}`), "redditConfig", &again)
		if err := checkRedditDemographicTargeting(again); err != nil {
			t.Fatalf("snapshot %s does not replay: %v", snapshot, err)
		}
		if !reflect.DeepEqual(again.Gender, cfg.Gender) || !reflect.DeepEqual(again.Languages, cfg.Languages) || !reflect.DeepEqual(again.Schedule, cfg.Schedule) {
			t.Fatalf("replayed %+v, want %+v", again, cfg)
		}
	}
}
