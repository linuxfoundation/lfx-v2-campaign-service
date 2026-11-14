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

func TestRedditBudgetType_PreflightRefusesWhileTheDeploymentGateIsOff(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, flag := range []string{"", "TRUE"} { // a typo fails closed
		t.Setenv(constants.EnvRedditDailyBudgetEnabled, flag)
		// "lifetime" is the default, but setting it is still setting the field.
		for _, v := range []string{"daily", "lifetime"} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(`,"budgetType":"`+v+`"`))
			requireConversionEventRefusal(t, err, constants.EnvRedditDailyBudgetEnabled)
		}
	}
}

func TestRedditBudgetType_AbsentFieldIsNeverRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvRedditDailyBudgetEnabled, flag)
		d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(``)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("flag %q: a config without budgetType must pass, got %v", flag, err)
		}
	}
}

func TestRedditBudgetType_PreflightRefusesBadValues(t *testing.T) {
	t.Setenv(constants.EnvRedditDailyBudgetEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, extra, want string }{
		{"null", `,"budgetType":null`, "must be a string"},
		{"number", `,"budgetType":1`, "must be a string"},
		{"empty", `,"budgetType":""`, "empty value is refused"},
		{"unknown", `,"budgetType":"weekly"`, "is not one of"},
		{"Reddit's own spelling", `,"budgetType":"DAILY_SPEND"`, "is not one of"},
		{"case variant key", `,"BudgetType":"daily"`, "exact spelling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(tc.extra))
			requireConversionEventRefusal(t, err, tc.want)
		})
	}
	for _, v := range []string{"daily", "Daily", "lifetime"} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, conversionEventCfg(`,"budgetType":"`+v+`"`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("%q: want admitted, got %v", v, err)
		}
	}
}

// budgetTypeAPI is a Reddit API stand-in recording the campaign create's goal_type.
type budgetTypeAPI struct {
	mu       sync.Mutex
	requests int
	goalType any
}

func (a *budgetTypeAPI) dispatcher(t *testing.T) *RedditDispatcher {
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
			a.goalType = body.Data["goal_type"]
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"):
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

// The goal type reaches Reddit, and the campaign row records the same pacing, so the budget lever,
// settings readback and monitor compare the stored budget against the right quantity.
func TestRedditBudgetType_DispatchSendsAndRecordsThePacing(t *testing.T) {
	t.Setenv(constants.EnvRedditDailyBudgetEnabled, "true")
	for _, tc := range []struct {
		extra    string
		wantGoal string
		wantType model.BudgetType
	}{
		{``, reddit.GoalTypeLifetimeSpend, model.BudgetLifetime},
		{`,"budgetType":"lifetime"`, reddit.GoalTypeLifetimeSpend, model.BudgetLifetime},
		{`,"budgetType":"DAILY"`, reddit.GoalTypeDailySpend, model.BudgetDaily},
	} {
		a := &budgetTypeAPI{}
		camp, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, conversionEventCfg(tc.extra))
		if err != nil {
			t.Fatalf("%s: Dispatch: %v", tc.extra, err)
		}
		if a.goalType != tc.wantGoal {
			t.Errorf("%s: goal_type = %v, want %s", tc.extra, a.goalType, tc.wantGoal)
		}
		if camp.BudgetType == nil || *camp.BudgetType != tc.wantType {
			t.Errorf("%s: recorded budget_type = %v, want %s", tc.extra, camp.BudgetType, tc.wantType)
		}
	}
}

// Dispatch re-checks for any path that skipped PreflightCreate, before any upstream request.
func TestRedditBudgetType_DispatchRefusesBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra, want string }{
		{"gate off", "", `,"budgetType":"daily"`, constants.EnvRedditDailyBudgetEnabled},
		{"unknown", "true", `,"budgetType":"weekly"`, "is not one of"},
		// A non-string fails the config decode too; the field-specific refusal must still win.
		{"number", "true", `,"budgetType":7`, "must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditDailyBudgetEnabled, tc.flag)
			a := &budgetTypeAPI{}
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

// A stored config_snapshot replays as a valid config: an omitted budgetType stays absent (so a
// default create's snapshot passes the gate when it is off), and a set one replays unchanged.
func TestRedditBudgetType_ConfigSnapshotReplays(t *testing.T) {
	for _, tc := range []struct{ flag, extra string }{{"", ``}, {"true", `,"budgetType":"daily"`}} {
		t.Setenv(constants.EnvRedditDailyBudgetEnabled, tc.flag)
		var cfg redditConfig
		mustDecode(t, conversionEventCfg(tc.extra), "redditConfig", &cfg)
		snapshot := campaignFromReddit(context.Background(), &reddit.CampaignResult{}, cfg).ConfigSnapshot
		if tc.extra == `` && strings.Contains(string(snapshot), `"budgetType"`) {
			t.Fatalf("snapshot %s records an omitted budgetType", snapshot)
		}
		replay := json.RawMessage(`{"redditConfig":` + string(snapshot) + `}`)
		if err := checkRedditBudgetType(replay); err != nil {
			t.Fatalf("snapshot %s does not replay: %v", snapshot, err)
		}
		var again redditConfig
		mustDecode(t, replay, "redditConfig", &again)
		if again.BudgetType != cfg.BudgetType {
			t.Fatalf("replayed budgetType %q, want %q", again.BudgetType, cfg.BudgetType)
		}
	}
}
