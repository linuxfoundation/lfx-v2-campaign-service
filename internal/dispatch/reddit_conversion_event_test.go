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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// conversionEventCfg is a minimal valid redditConfig envelope plus extra fields (each starting
// with a comma).
func conversionEventCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","subreddits":["kubernetes"]` + extra + `}}`)
}

// requireConversionEventRefusal asserts a synchronous-400 refusal: ErrCreateConfigInvalid (the
// handler's 400) carried as a PreflightRefusal (the job copies it verbatim), naming want.
func requireConversionEventRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Fatalf("want ErrCreateConfigInvalid, got %v", err)
	}
	var pr *domain.PreflightRefusal
	if !errors.As(err, &pr) {
		t.Fatalf("want a PreflightRefusal so the job keeps the sentence, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not name %q", err.Error(), want)
	}
}

func TestRedditConversionEvent_PreflightRefusesWhileTheDeploymentGateIsOff(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(`,"conversionEvent":"SIGN_UP"`))
	requireConversionEventRefusal(t, err, constants.EnvRedditConversionEventEnabled)

	// "TRUE" is not "true": a typo fails closed.
	t.Setenv(constants.EnvRedditConversionEventEnabled, "TRUE")
	err = d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(`,"conversionEvent":"SIGN_UP"`))
	requireConversionEventRefusal(t, err, constants.EnvRedditConversionEventEnabled)
}

func TestRedditConversionEvent_AbsentFieldIsNeverRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvRedditConversionEventEnabled, flag)
		d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(`,"objective":"traffic"`))
		if !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("flag %q: a config without conversionEvent must pass, got %v", flag, err)
		}
	}
}

func TestRedditConversionEvent_PreflightRefusesBadValues(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"null", `,"conversionEvent":null`, "must be a string"},
		{"number", `,"conversionEvent":5`, "must be a string"},
		{"empty", `,"conversionEvent":""`, "empty value is refused"},
		{"blank", `,"conversionEvent":"  "`, "empty value is refused"},
		{"unknown", `,"conversionEvent":"REGISTER"`, "is not one of"},
		{"app event", `,"conversionEvent":"MOBILE_CONVERSION_INSTALL"`, "is not one of"},
		{"clicks goal", `,"conversionEvent":"CLICKS"`, "is not one of"},
		{"wrong objective", `,"objective":"traffic","conversionEvent":"SIGN_UP"`, "objective"},
		{"video objective", `,"objective":"video_views","videoGoal":"VIDEO_VIEW_6S","conversionEvent":"LEAD"`, "objective"},
		{"case variant key", `,"ConversionEvent":"SIGN_UP"`, "exact spelling"},
		{"mis-cased objective", `,"objective":"Conversions","conversionEvent":"SIGN_UP"`, "unsupported Reddit objective"},
		{"unknown objective", `,"objective":"bogus","conversionEvent":"SIGN_UP"`, "unsupported Reddit objective"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(tc.extra))
			requireConversionEventRefusal(t, err, tc.want)
		})
	}
}

func TestRedditConversionEvent_PreflightAdmitsEveryListedEvent(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, ev := range reddit.ConversionEvents() {
		for _, spelled := range []string{ev, strings.ToLower(ev)} {
			// objective omitted (the conversions default) and explicit conversions both admit it.
			for _, obj := range []string{``, `,"objective":"conversions"`} {
				err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(obj+`,"conversionEvent":"`+spelled+`"`))
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("%s %s: want admitted, got %v", spelled, obj, err)
				}
			}
		}
	}
}

// conversionEventAPI is a Reddit API stand-in recording the optimization_goal of the campaign and
// ad-group creates.
type conversionEventAPI struct {
	mu                    sync.Mutex
	requests              int
	campaignGoal, adGroup any
}

func (a *conversionEventAPI) dispatcher(t *testing.T) *RedditDispatcher {
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
			a.campaignGoal = body.Data["optimization_goal"]
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"):
			a.adGroup = body.Data["optimization_goal"]
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

func TestRedditConversionEvent_DispatchSendsTheEventOnCampaignAndAdGroup(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "true")
	a := &conversionEventAPI{}
	if _, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(`,"conversionEvent":"sign_up"`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if a.campaignGoal != "SIGN_UP" || a.adGroup != "SIGN_UP" {
		t.Fatalf("optimization_goal campaign=%v adGroup=%v, want SIGN_UP on both (CBO requires them to match)", a.campaignGoal, a.adGroup)
	}
}

func TestRedditConversionEvent_DispatchWithoutTheFieldKeepsPurchase(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "true")
	a := &conversionEventAPI{}
	if _, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(``)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if a.campaignGoal != "PURCHASE" || a.adGroup != "PURCHASE" {
		t.Fatalf("optimization_goal campaign=%v adGroup=%v, want the unchanged PURCHASE default", a.campaignGoal, a.adGroup)
	}
}

// Dispatch re-checks for any path that skipped PreflightCreate, before any upstream request.
func TestRedditConversionEvent_DispatchRefusesBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra, want string }{
		{"gate off", "", `,"conversionEvent":"SIGN_UP"`, constants.EnvRedditConversionEventEnabled},
		{"wrong objective", "true", `,"objective":"awareness","conversionEvent":"SIGN_UP"`, "objective"},
		{"null", "true", `,"conversionEvent":null`, "must be a string"},
		// A non-string fails the config decode too; the field-specific refusal must still win.
		{"number", "true", `,"conversionEvent":5`, "must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditConversionEventEnabled, tc.flag)
			a := &conversionEventAPI{}
			camp, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(tc.extra))
			if camp != nil {
				t.Fatalf("no campaign may be returned, got %+v", camp)
			}
			requirePreCreate(t, err)
			requireConversionEventRefusal(t, err, tc.want)
			// The refusal reaches the job verbatim, not as the generic failure.
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

// A stored config_snapshot must replay as a valid config: an omitted event stays absent (the
// validator refuses an explicit ""), and a set one replays to the same value.
func TestRedditConversionEvent_ConfigSnapshotReplays(t *testing.T) {
	t.Setenv(constants.EnvRedditConversionEventEnabled, "true")
	for _, extra := range []string{``, `,"conversionEvent":"LEAD"`} {
		config := conversionEventCfg(extra)
		var cfg redditConfig
		mustDecode(t, config, "redditConfig", &cfg)
		snapshot := campaignFromReddit(context.Background(), &reddit.CampaignResult{}, cfg).ConfigSnapshot
		if extra == `` && strings.Contains(string(snapshot), redditConversionEventKey) {
			t.Fatalf("snapshot %s records an omitted conversionEvent", snapshot)
		}
		replay := json.RawMessage(`{"redditConfig":` + string(snapshot) + `}`)
		var again redditConfig
		mustDecode(t, replay, "redditConfig", &again)
		if err := checkRedditConversionEvent(replay, again); err != nil {
			t.Fatalf("snapshot %s does not replay: %v", snapshot, err)
		}
		if again.ConversionEvent != cfg.ConversionEvent {
			t.Fatalf("replayed event %q, want %q", again.ConversionEvent, cfg.ConversionEvent)
		}
	}
}
