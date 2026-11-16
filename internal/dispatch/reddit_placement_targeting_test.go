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

func TestRedditPlacementTargeting_PreflightRefusesWhileTheDeploymentGateIsOff(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, flag := range []string{"", "TRUE"} { // a typo fails closed
		t.Setenv(constants.EnvRedditPlacementTargetingEnabled, flag)
		for _, extra := range []string{
			`,"placements":["FEED"]`,
			`,"platforms":["DESKTOP"]`,
			`,"expandTargeting":false`,
			// Spelling the default is still setting the field: the gate never reads the value.
			`,"expandTargeting":true`,
		} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(extra))
			requireConversionEventRefusal(t, err, constants.EnvRedditPlacementTargetingEnabled)
		}
	}
}

func TestRedditPlacementTargeting_AbsentFieldsAreNeverRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvRedditPlacementTargetingEnabled, flag)
		d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
		for _, extra := range []string{``, `,"placements":[],"platforms":null,"expandTargeting":null`} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(extra))
			if !errors.Is(err, domain.ErrPreflightNotApplicable) {
				t.Fatalf("flag %q, %s: want admitted, got %v", flag, extra, err)
			}
		}
	}
}

func TestRedditPlacementTargeting_PreflightRefusesBadValues(t *testing.T) {
	t.Setenv(constants.EnvRedditPlacementTargetingEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, extra, want string }{
		{"placements not an array", `,"placements":"FEED"`, "redditConfig.placements: must be an array of strings"},
		{"platforms numbers", `,"platforms":[1]`, "redditConfig.platforms: must be an array of strings"},
		{"case-variant key mistyped", `,"Platforms":"DESKTOP"`, "redditConfig.platforms: must be an array of strings"},
		{"expandTargeting string", `,"expandTargeting":"false"`, "redditConfig.expandTargeting: must be true or false"},
		{"unknown placement", `,"placements":["SIDEBAR"]`, "redditConfig.placements: entry 1"},
		{"blank platform", `,"platforms":["DESKTOP"," "]`, "redditConfig.platforms: entry 2 is blank"},
		{"ALL spelled out", `,"platforms":["ALL"]`, "redditConfig.platforms: entry 1"},
		// An unrelated mistyped field must not turn a bad value into an accepted job.
		{"with an unrelated decode error", `,"keywords":"oops","placements":["SIDEBAR"]`, "redditConfig.placements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(tc.extra))
			requireConversionEventRefusal(t, err, tc.want)
		})
	}
}

// placementAPI is a Reddit API stand-in recording the ad-group create's targeting object.
type placementAPI struct {
	mu        sync.Mutex
	requests  int
	targeting map[string]any
}

func (a *placementAPI) dispatcher(t *testing.T) *RedditDispatcher {
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

func TestRedditPlacementTargeting_DispatchSendsTheChoices(t *testing.T) {
	t.Setenv(constants.EnvRedditPlacementTargetingEnabled, "true")
	a := &placementAPI{}
	cfg := conversionEventCfg(`,"placements":["feed"],"platforms":["mobile_web","desktop"],"expandTargeting":false`)
	if _, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !reflect.DeepEqual(a.targeting["locations"], []any{"FEED"}) ||
		!reflect.DeepEqual(a.targeting["platforms"], []any{"DESKTOP", "MOBILE_WEB"}) ||
		a.targeting["expand_targeting"] != false {
		t.Fatalf("targeting = %#v; want locations [FEED], platforms [DESKTOP MOBILE_WEB], expand_targeting false", a.targeting)
	}
}

// Dispatch re-checks for any path that skipped PreflightCreate, before any upstream request.
func TestRedditPlacementTargeting_DispatchRefusesBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra, want string }{
		{"gate off", "", `,"expandTargeting":false`, constants.EnvRedditPlacementTargetingEnabled},
		{"unknown platform", "true", `,"platforms":["TV"]`, "redditConfig.platforms"},
		// A wrong type fails the config decode too; the field-specific refusal must win.
		{"mistyped expandTargeting", "true", `,"expandTargeting":"no"`, "must be true or false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditPlacementTargetingEnabled, tc.flag)
			a := &placementAPI{}
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

// A stored config_snapshot replays as a valid config: omitted choices stay absent (so a snapshot
// of a default create passes the gate when it is off), and set ones replay to the same values.
func TestRedditPlacementTargeting_ConfigSnapshotReplays(t *testing.T) {
	for _, tc := range []struct{ flag, extra string }{
		{"", ``},
		{"true", `,"placements":["FEED"],"platforms":["DESKTOP"],"expandTargeting":false`},
	} {
		t.Setenv(constants.EnvRedditPlacementTargetingEnabled, tc.flag)
		var cfg redditConfig
		mustDecode(t, conversionEventCfg(tc.extra), "redditConfig", &cfg)
		snapshot := campaignFromReddit(context.Background(), &reddit.CampaignResult{}, cfg).ConfigSnapshot
		if tc.extra == `` {
			for _, k := range []string{`"placements"`, `"platforms"`, `"expandTargeting"`} {
				if strings.Contains(string(snapshot), k) {
					t.Fatalf("snapshot %s records an omitted %s", snapshot, k)
				}
			}
		}
		var again redditConfig
		mustDecode(t, json.RawMessage(`{"redditConfig":`+string(snapshot)+`}`), "redditConfig", &again)
		if err := checkRedditPlacementTargeting(again); err != nil {
			t.Fatalf("snapshot %s does not replay: %v", snapshot, err)
		}
		if !reflect.DeepEqual(again.Placements, cfg.Placements) || !reflect.DeepEqual(again.Platforms, cfg.Platforms) ||
			!reflect.DeepEqual(again.ExpandTargeting, cfg.ExpandTargeting) {
			t.Fatalf("replayed %+v, want %+v", again, cfg)
		}
	}
}
