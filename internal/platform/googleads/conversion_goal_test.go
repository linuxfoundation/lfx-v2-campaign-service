// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	goalTestAction1 = "customers/1234567890/conversionActions/11"
	goalTestAction2 = "customers/1234567890/conversionActions/22"
	goalTestGoal    = "customers/1234567890/customConversionGoals/777"
	goalTestConfig  = "customers/1234567890/conversionGoalCampaignConfigs/222"
)

// goalFake scripts the four calls the goal step makes. searches answers each GAQL lookup in
// turn; an unset handler fails the test, so a call the scenario does not expect is caught.
type goalFake struct {
	mu       sync.Mutex
	searches []string
	create   http.HandlerFunc
	config   http.HandlerFunc
	bodies   map[string][]string
}

func (f *goalFake) record(path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[path] = append(f.bodies[path], string(body))
}

func (f *goalFake) sent(suffix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[suffix]
}

func goalClient(t *testing.T, f *goalFake) *Client {
	t.Helper()
	f.bodies = map[string][]string{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			f.record("search", body)
			f.mu.Lock()
			if len(f.searches) == 0 {
				f.mu.Unlock()
				t.Errorf("unexpected extra GAQL lookup: %s", body)
				http.Error(w, "unexpected", http.StatusInternalServerError)
				return
			}
			out := f.searches[0]
			f.searches = f.searches[1:]
			f.mu.Unlock()
			_, _ = io.WriteString(w, out)
		case strings.HasSuffix(r.URL.Path, "customConversionGoals:mutate"):
			f.record("goal", body)
			var op struct {
				Operations []struct {
					Update json.RawMessage `json:"update"`
				} `json:"operations"`
			}
			_ = json.Unmarshal(body, &op)
			if len(op.Operations) == 1 && len(op.Operations[0].Update) > 0 {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+goalTestGoal+`"}]}`)
				return
			}
			if f.create == nil {
				t.Errorf("unexpected custom goal create: %s", body)
				http.Error(w, "unexpected", http.StatusInternalServerError)
				return
			}
			f.create(w, r)
		case strings.HasSuffix(r.URL.Path, "conversionGoalCampaignConfigs:mutate"):
			f.record("config", body)
			if f.config == nil {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+goalTestConfig+`"}]}`)
				return
			}
			f.config(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
}

func goalRow(status string, actions ...string) string {
	raw, _ := json.Marshal(actions)
	return `{"results":[{"customConversionGoal":{"resourceName":"` + goalTestGoal + `","status":"` + status + `","conversionActions":` + string(raw) + `}}]}`
}

func goalPlan(strategy string, actions ...string) biddingPlan {
	return biddingPlan{strategy: strategy, conversionActions: actions}
}

func okGoalCreate(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+goalTestGoal+`"}]}`)
}

// No actions named: nothing is written, and the campaign keeps the account's goals.
func TestApplyCampaignConversionGoal_NoActionsWritesNothing(t *testing.T) {
	f := &goalFake{}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions))
	if err != nil || step != "" {
		t.Fatalf("got (%q, %v), want no step and no error", step, err)
	}
	if n := len(f.sent("search")) + len(f.sent("goal")) + len(f.sent("config")); n != 0 {
		t.Errorf("made %d requests with no actions named", n)
	}
}

// The first campaign for a set creates the goal, then points the campaign's config at it.
func TestApplyCampaignConversionGoal_CreatesThenPointsTheCampaignAtIt(t *testing.T) {
	f := &goalFake{searches: []string{`{"results":[]}`}, create: okGoalCreate}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if !strings.Contains(step, "optimizes toward 2 conversion action(s)") || !strings.Contains(step, "custom goal 777, created") {
		t.Errorf("step = %q, want it to name both actions and the created goal", step)
	}

	goal := f.sent("goal")
	if len(goal) != 1 {
		t.Fatalf("sent %d custom goal writes, want 1 create", len(goal))
	}
	wantName := customConversionGoalName([]string{goalTestAction1, goalTestAction2})
	for _, want := range []string{`"name":"` + wantName + `"`, goalTestAction1, goalTestAction2, `"status":"ENABLED"`} {
		if !strings.Contains(goal[0], want) {
			t.Errorf("goal create %s lacks %s", goal[0], want)
		}
	}

	config := f.sent("config")
	if len(config) != 1 {
		t.Fatalf("sent %d config updates, want 1", len(config))
	}
	for _, want := range []string{`"resourceName":"` + goalTestConfig + `"`, `"goalConfigLevel":"CAMPAIGN"`, `"customConversionGoal":"` + goalTestGoal + `"`, `"updateMask":"goal_config_level,custom_conversion_goal"`} {
		if !strings.Contains(config[0], want) {
			t.Errorf("config update %s lacks %s", config[0], want)
		}
	}
}

// A second campaign for the same set reuses the goal: no create, no restore.
func TestApplyCampaignConversionGoal_ReusesAnEnabledGoalForTheSameSet(t *testing.T) {
	f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction2, goalTestAction1)}}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingTargetCPA, goalTestAction1, goalTestAction2))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if !strings.Contains(step, "reused") {
		t.Errorf("step = %q, want it to say the goal was reused", step)
	}
	if n := len(f.sent("goal")); n != 0 {
		t.Errorf("sent %d custom goal writes for a goal that already matched", n)
	}
	if n := len(f.sent("config")); n != 1 {
		t.Errorf("sent %d config updates, want 1", n)
	}
}

// A goal found removed, or holding a set edited by hand, is put back before it is used.
func TestApplyCampaignConversionGoal_RestoresARemovedOrEditedGoal(t *testing.T) {
	for name, row := range map[string]string{
		"removed": goalRow("REMOVED", goalTestAction1, goalTestAction2),
		"edited":  goalRow("ENABLED", goalTestAction1),
	} {
		t.Run(name, func(t *testing.T) {
			f := &goalFake{searches: []string{row}}
			c := goalClient(t, f)
			if _, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2)); err != nil {
				t.Fatalf("applyCampaignConversionGoal: %v", err)
			}
			goal := f.sent("goal")
			if len(goal) != 1 {
				t.Fatalf("sent %d custom goal writes, want 1 restore", len(goal))
			}
			for _, want := range []string{`"update":`, `"resourceName":"` + goalTestGoal + `"`, `"status":"ENABLED"`, goalTestAction2, `"updateMask":"conversion_actions,status"`} {
				if !strings.Contains(goal[0], want) {
					t.Errorf("restore %s lacks %s", goal[0], want)
				}
			}
		})
	}
}

// Two creates for one set race: the loser looks the winner's goal up and uses it.
func TestApplyCampaignConversionGoal_ADuplicateNameUsesTheWinnersGoal(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, goalRow("ENABLED", goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "DUPLICATE_NAME"),
	}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if !strings.Contains(step, "reused") {
		t.Errorf("step = %q, want the winner's goal reported as reused", step)
	}
	if n := len(f.sent("config")); n != 1 {
		t.Errorf("sent %d config updates, want 1", n)
	}
}

// A failed write is a partial result that says the goal was NOT applied, never success.
func TestApplyCampaignConversionGoal_FailuresSayTheGoalWasNotApplied(t *testing.T) {
	for name, f := range map[string]*goalFake{
		"goal create refused": {
			searches: []string{`{"results":[]}`},
			create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "INVALID_CONVERSION_ACTION"),
		},
		"config update refused": {
			searches: []string{goalRow("ENABLED", goalTestAction1)},
			config:   gaqlError(http.StatusBadRequest, "conversionGoalCampaignConfigError", "INVALID_GOAL_CONFIG_LEVEL"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := goalClient(t, f)
			step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
			if err == nil {
				t.Fatalf("got step %q and no error", step)
			}
			if !strings.Contains(err.Error(), "conversion goal NOT applied to campaign 222") {
				t.Errorf("error %q does not say the goal was not applied", err)
			}
		})
	}
}

// A config update whose outcome is unknown may already have pointed the campaign at the goal,
// so it is UNCONFIRMED, never "NOT applied": a 5xx, which is not retried, and a definite 400
// that follows a retried 429, which the first attempt may already have applied.
func TestApplyCampaignConversionGoal_AnUnknownConfigOutcomeIsUnconfirmed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	for name, config := range map[string]http.HandlerFunc{
		"5xx": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":13,"status":"INTERNAL"}}`)
		},
		"400 after a retried 429": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`)
				return
			}
			gaqlError(http.StatusBadRequest, "conversionGoalCampaignConfigError", "INVALID_GOAL_CONFIG_LEVEL")(w, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1)}, config: config}
			c := goalClient(t, f)
			_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
			if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") || strings.Contains(err.Error(), "NOT applied") {
				t.Fatalf("err = %v, want an UNCONFIRMED goal, not NOT applied", err)
			}
		})
	}
}

// A 2xx that names another campaign's config is not taken as this campaign's goal.
func TestApplyCampaignConversionGoal_AConfigResultForAnotherCampaignIsUnconfirmed(t *testing.T) {
	f := &goalFake{
		searches: []string{goalRow("ENABLED", goalTestAction1)},
		config: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/conversionGoalCampaignConfigs/999"}]}`)
		},
	}
	c := goalClient(t, f)
	_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("err = %v, want an UNCONFIRMED goal", err)
	}
}

// Under a click strategy the goal still decides what counts as a conversion, but the step
// must not claim the campaign bids toward it.
func TestApplyCampaignConversionGoal_AClickStrategyIsNotDescribedAsOptimizing(t *testing.T) {
	f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1)}}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeClicks, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if strings.Contains(step, "optimizes toward") || !strings.Contains(step, "does not optimize toward conversions") {
		t.Errorf("step = %q, want it to say this bidding does not optimize toward conversions", step)
	}
}

// One goal per SET: the order the operator listed them in must not create a second goal.
func TestCustomConversionGoalName_IgnoresOrder(t *testing.T) {
	a := customConversionGoalName([]string{goalTestAction1, goalTestAction2})
	b := customConversionGoalName([]string{goalTestAction2, goalTestAction1})
	if a != b {
		t.Errorf("names differ by order: %q vs %q", a, b)
	}
	if a == customConversionGoalName([]string{goalTestAction1}) {
		t.Error("a different set produced the same name")
	}
	if !strings.HasPrefix(a, customConversionGoalNamePrefix) {
		t.Errorf("name %q lacks the LFX prefix", a)
	}
}

// Every channel applies the goal right after its campaign create (#436). The goal create is
// refused here, so each channel must come back with its campaign id and an error saying the
// goal was not applied, and must build nothing further on the campaign. One subtest per
// channel, so dropping the call from any one of them fails by name. Video runs through the
// unexported cascade, since CreateVideoCampaign refuses before any mutate (the Google Ads API
// cannot create Video campaigns) and the cascade is kept tested against the day it can.
func TestEveryChannelAppliesTheConversionGoalAfterItsCampaign(t *testing.T) {
	for _, tc := range []struct {
		name   string
		images func(*testing.T) map[string][]byte
		create func(*Client, string) (*CampaignResult, error)
	}{
		{"search", nil, func(c *Client, _ string) (*CampaignResult, error) {
			in := sampleInput()
			in.ConversionActions = []string{"11"}
			return c.CreateCampaign(context.Background(), in)
		}},
		{"display", displayImages, func(c *Client, base string) (*CampaignResult, error) {
			in := displayInput(base)
			in.ConversionActions = []string{"11"}
			return c.CreateDisplayCampaign(context.Background(), in)
		}},
		{"video", nil, func(c *Client, _ string) (*CampaignResult, error) {
			in := videoInput()
			in.ConversionActions = []string{"11"}
			return c.createVideoCampaignCascade(context.Background(), in)
		}},
		{"demand gen", nil, func(c *Client, _ string) (*CampaignResult, error) {
			in := demandGenInput()
			in.ConversionActions = []string{"11"}
			return c.CreateDemandGenCampaign(context.Background(), in)
		}},
		{"performance max", pmaxImages, func(c *Client, base string) (*CampaignResult, error) {
			in := demandGenInput()
			in.PerformanceMaxCreative = pmaxCreativeAt(base)
			in.ConversionActions = []string{"11"}
			return c.CreatePerformanceMaxCampaign(context.Background(), in)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var images map[string][]byte
			if tc.images != nil {
				images = tc.images(t)
			}
			srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				if body, ok := images[r.URL.Path]; ok {
					_, _ = w.Write(body)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "googleAds:search"):
					emptySearchPage(w, r)
				case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
					okBudget(w, r)
				case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
					okCampaign(w, r)
				case strings.HasSuffix(r.URL.Path, "customConversionGoals:mutate"):
					gaqlError(http.StatusBadRequest, "customConversionGoalError", "INVALID_CONVERSION_ACTION")(w, r)
				default:
					t.Errorf("%s reached %s after the conversion goal failed", tc.name, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})

			res, err := tc.create(demandGenClient(t, srv), srv.URL)
			if err == nil || !strings.Contains(err.Error(), "conversion goal NOT applied to campaign 222") {
				t.Fatalf("err = %v, want the goal reported as not applied to campaign 222", err)
			}
			if res == nil || res.CampaignID != "222" {
				t.Errorf("partial = %+v, want it to carry the created campaign", res)
			}
		})
	}
}
