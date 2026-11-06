// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const exclusionsExtra = `,"excludedSubreddits":["r/politics"],"excludedGeoTargets":["ru"]`

// requireConfigInvalid asserts a synchronous-400 refusal whose message contains want.
func requireConfigInvalid(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Fatalf("want domain.ErrCreateConfigInvalid (a 400), got %v", err)
	}
	var refusal *domain.PreflightRefusal
	if !errors.As(err, &refusal) {
		t.Errorf("refusal must be a domain.PreflightRefusal so a job copies it verbatim: %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not contain %q", err.Error(), want)
	}
}

// Off (the default): either list is a synchronous 400 naming the flag, and Dispatch refuses it
// again, pre-create, before any Reddit call.
func TestRedditExclusions_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "")
	h := &redditAssetHarness{}
	d := h.dispatcher(t)

	for _, extra := range []string{exclusionsExtra, `,"excludedSubreddits":["politics"]`, `,"excludedGeoTargets":["RU"]`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(extra))
		requireConfigInvalid(t, err, constants.EnvRedditTargetingExclusionsEnabled)
	}

	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAssetCfg(exclusionsExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvRedditTargetingExclusionsEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("%d Reddit API calls before the gate refused; want 0", n)
	}
}

// An unrelated mistyped field must not let a gated or invalid exclusion list through preflight as
// an accepted job: the checks run on the best-effort decode, gate off and gate on.
func TestRedditExclusions_PreflightRefusesDespiteUnrelatedDecodeError(t *testing.T) {
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":"bad","startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","excludedSubreddits":["a b"]}}`)
	t.Run("gate off", func(t *testing.T) {
		t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "")
		d := (&redditAssetHarness{}).dispatcher(t)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, cfg)
		requireConfigInvalid(t, err, constants.EnvRedditTargetingExclusionsEnabled)
	})
	t.Run("gate on", func(t *testing.T) {
		t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "true")
		d := (&redditAssetHarness{}).dispatcher(t)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, cfg)
		requireConfigInvalid(t, err, "redditConfig.excludedSubreddits")
	})
}

// Only the exact value "true" opens the gate.
func TestRedditExclusions_GateFailsClosedOnTypo(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "TRUE")
	d := (&redditAssetHarness{}).dispatcher(t)
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(exclusionsExtra))
	requireConfigInvalid(t, err, constants.EnvRedditTargetingExclusionsEnabled)
}

// On: a valid config passes preflight; each rule breach is a synchronous 400 naming the field.
func TestRedditExclusions_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "true")
	d := (&redditAssetHarness{}).dispatcher(t)
	ctx := context.Background()

	if err := d.PreflightCreate(ctx, "cncf", model.ProviderRedditAds, redditAssetCfg(exclusionsExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("a valid exclusion config must pass preflight, got %v", err)
	}
	cases := map[string]struct{ extra, want string }{
		"targeted and excluded subreddit": {`,"excludedSubreddits":["Kubernetes"]`, "redditConfig.excludedSubreddits"},
		"bad subreddit name":              {`,"excludedSubreddits":["a b"]`, "redditConfig.excludedSubreddits"},
		"excluding the US default":        {`,"excludedGeoTargets":["US"]`, "redditConfig.excludedGeoTargets"},
		"unassigned country":              {`,"excludedGeoTargets":["ZZ"]`, "redditConfig.excludedGeoTargets"},
		"subreddits not an array":         {`,"excludedSubreddits":"politics"`, "redditConfig.excludedSubreddits: must be an array of strings"},
		"geo entries not strings":         {`,"excludedGeoTargets":[1]`, "redditConfig.excludedGeoTargets: must be an array of strings"},
		"case-variant key, wrong type":    {`,"ExcludedGeoTargets":{"x":1}`, "redditConfig.excludedGeoTargets: must be an array of strings"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := d.PreflightCreate(ctx, "cncf", model.ProviderRedditAds, redditAssetCfg(tc.extra))
			requireConfigInvalid(t, err, tc.want)
		})
	}
	// null reads as absent, as encoding/json reads it.
	if err := d.PreflightCreate(ctx, "cncf", model.ProviderRedditAds, redditAssetCfg(`,"excludedSubreddits":null`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("null must read as absent, got %v", err)
	}
}

// On: Dispatch hands both lists to the client, which sends them in the ad group's targeting.
func TestRedditExclusions_DispatchSendsThem(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "true")
	var mu sync.Mutex
	var targeting map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			var env struct {
				Data struct {
					Targeting map[string]any `json:"targeting"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			targeting = env.Data.Targeting
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAssetCfg(exclusionsExtra)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := targeting["excluded_communities"], []any{"politics"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excluded_communities = %#v, want %#v", got, want)
	}
	if got, want := targeting["excluded_geolocations"], []any{"RU"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excluded_geolocations = %#v, want %#v", got, want)
	}
}
