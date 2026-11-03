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
	goalTestAction1  = "customers/1234567890/conversionActions/11"
	goalTestAction2  = "customers/1234567890/conversionActions/22"
	goalTestGoal     = "customers/1234567890/customConversionGoals/777"
	goalTestHandGoal = "customers/1234567890/customConversionGoals/888"
	goalTestConfig   = "customers/1234567890/conversionGoalCampaignConfigs/222"
	goalTestBiddable = "customers/1234567890/campaignConversionGoals/222~PURCHASE~WEBSITE"
)

// goalTestCampaignGoals is the default answer to the read of the campaign's inherited account
// goals: one still biddable, one not.
var goalTestCampaignGoals = `{"results":[` +
	`{"campaignConversionGoal":{"resourceName":"` + goalTestBiddable + `","biddable":true}},` +
	`{"campaignConversionGoal":{"resourceName":"customers/1234567890/campaignConversionGoals/222~SIGNUP~WEBSITE"}}]}`

// goalFake scripts the calls the goal step makes. searches answers each custom-goal GAQL
// lookup in turn; campaignGoals answers the read of the campaign's own goals (the default
// above when empty). An unset create fails the test. The one update this client may send is a
// status-only re-enable of a removed goal, recorded as "reenable" and answered by reenable or
// an echo of the goal; any other update fails the test. atomic overrides the default 2xx that
// echoes what was sent.
type goalFake struct {
	mu            sync.Mutex
	searches      []string
	campaignGoals string
	create        http.HandlerFunc
	reenable      http.HandlerFunc
	atomic        http.HandlerFunc
	bodies        map[string][]string
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
		case strings.HasSuffix(r.URL.Path, "googleAds:search") && strings.Contains(string(body), "FROM campaign_conversion_goal"):
			f.record("campaign goals", body)
			out := f.campaignGoals
			if out == "" {
				out = goalTestCampaignGoals
			}
			_, _ = io.WriteString(w, out)
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
					Update     json.RawMessage `json:"update"`
					UpdateMask string          `json:"updateMask"`
				} `json:"operations"`
			}
			_ = json.Unmarshal(body, &op)
			if len(op.Operations) == 1 && len(op.Operations[0].Update) > 0 {
				var upd map[string]any
				_ = json.Unmarshal(op.Operations[0].Update, &upd)
				if op.Operations[0].UpdateMask != "status" || len(upd) != 2 || upd["status"] != "ENABLED" {
					t.Errorf("a custom goal was written other than by a status-only re-enable: %s", body)
					http.Error(w, "unexpected", http.StatusInternalServerError)
					return
				}
				f.record("reenable", body)
				if f.reenable != nil {
					f.reenable(w, r)
					return
				}
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+upd["resourceName"].(string)+`"}]}`)
				return
			}
			if f.create == nil {
				t.Errorf("unexpected custom goal create: %s", body)
				http.Error(w, "unexpected", http.StatusInternalServerError)
				return
			}
			f.create(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			f.record("atomic", body)
			if f.atomic != nil {
				f.atomic(w, r)
				return
			}
			_, _ = io.WriteString(w, echoGoalMutate(body))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
}

// echoGoalMutate answers a googleAds:mutate with one result per operation, naming what it
// updated, as Google does.
func echoGoalMutate(body []byte) string {
	var req struct {
		MutateOperations []struct {
			ConversionGoalCampaignConfigOperation *struct {
				Update struct {
					ResourceName string `json:"resourceName"`
				} `json:"update"`
			} `json:"conversionGoalCampaignConfigOperation"`
			CampaignConversionGoalOperation *struct {
				Update struct {
					ResourceName string `json:"resourceName"`
				} `json:"update"`
			} `json:"campaignConversionGoalOperation"`
		} `json:"mutateOperations"`
	}
	_ = json.Unmarshal(body, &req)
	var parts []string
	for _, op := range req.MutateOperations {
		switch {
		case op.ConversionGoalCampaignConfigOperation != nil:
			parts = append(parts, `{"conversionGoalCampaignConfigResult":{"resourceName":"`+op.ConversionGoalCampaignConfigOperation.Update.ResourceName+`"}}`)
		case op.CampaignConversionGoalOperation != nil:
			parts = append(parts, `{"campaignConversionGoalResult":{"resourceName":"`+op.CampaignConversionGoalOperation.Update.ResourceName+`"}}`)
		}
	}
	return `{"mutateOperationResponses":[` + strings.Join(parts, ",") + `]}`
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
	if n := len(f.sent("search")) + len(f.sent("goal")) + len(f.sent("atomic")); n != 0 {
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

	atomic := f.sent("atomic")
	if len(atomic) != 1 {
		t.Fatalf("sent %d googleAds:mutate writes, want 1", len(atomic))
	}
	for _, want := range []string{
		`"conversionGoalCampaignConfigOperation":{"update":{"resourceName":"` + goalTestConfig + `"`,
		`"goalConfigLevel":"CAMPAIGN"`, `"customConversionGoal":"` + goalTestGoal + `"`,
		`"updateMask":"goal_config_level,custom_conversion_goal"`,
	} {
		if !strings.Contains(atomic[0], want) {
			t.Errorf("atomic write %s lacks %s", atomic[0], want)
		}
	}
}

// The custom goal is ADDED to what the campaign bids toward, so every inherited account goal
// still biddable is taken out of bidding in the same atomic write; one already not biddable is
// left alone.
func TestApplyCampaignConversionGoal_TakesTheAccountGoalsOutOfBidding(t *testing.T) {
	f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1)}}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	atomic := f.sent("atomic")
	if len(atomic) != 1 {
		t.Fatalf("sent %d googleAds:mutate writes, want 1", len(atomic))
	}
	want := `{"campaignConversionGoalOperation":{"update":{"resourceName":"` + goalTestBiddable + `","biddable":false},"updateMask":"biddable"}}`
	if !strings.Contains(atomic[0], want) {
		t.Errorf("atomic write %s lacks %s", atomic[0], want)
	}
	if strings.Contains(atomic[0], "SIGNUP") {
		t.Errorf("atomic write %s touches a goal that was not biddable", atomic[0])
	}
	if !strings.Contains(step, "1 account goal(s) set not biddable") {
		t.Errorf("step = %q, want it to count the goals taken out of bidding", step)
	}
}

// A goal row that does not belong to this campaign is refused before anything is written.
func TestApplyCampaignConversionGoal_RefusesAnotherCampaignsGoalRow(t *testing.T) {
	f := &goalFake{
		searches:      []string{goalRow("ENABLED", goalTestAction1)},
		campaignGoals: `{"results":[{"campaignConversionGoal":{"resourceName":"customers/1234567890/campaignConversionGoals/999~PURCHASE~WEBSITE","biddable":true}}]}`,
	}
	c := goalClient(t, f)
	_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err == nil || !strings.Contains(err.Error(), "NOT applied") {
		t.Fatalf("err = %v, want the goal NOT applied", err)
	}
	if n := len(f.sent("atomic")); n != 0 {
		t.Errorf("sent %d writes after refusing the read", n)
	}
}

// A second campaign for the same set reuses the goal: nothing is written to it.
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
	if n := len(f.sent("atomic")); n != 1 {
		t.Errorf("sent %d googleAds:mutate writes, want 1", n)
	}
}

// A goal named for the set but edited by hand to hold something else is never written: it may
// count conversions for live campaigns in other projects. With no other goal holding the set,
// a new one is created under the set's second name.
func TestApplyCampaignConversionGoal_NeverWritesAGoalThatWasEdited(t *testing.T) {
	f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1), `{"results":[]}`}, create: okGoalCreate}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	goal := f.sent("goal")
	wantName := customConversionGoalName([]string{goalTestAction1, goalTestAction2}) + " | 2"
	if len(goal) != 1 || !strings.Contains(goal[0], `"create":`) || !strings.Contains(goal[0], `"name":"`+wantName+`"`) {
		t.Fatalf("goal writes %v, want one create under the second name %q", goal, wantName)
	}
	if !strings.Contains(step, "created under its second name") {
		t.Errorf("step = %q, want it to say the goal was created under its second name", step)
	}
}

// A REMOVED goal holding exactly the set is re-enabled, whether it is the goal named for the set
// or one under another name: Google refuses to remove a goal linked to a campaign, so a removed
// goal counts for none, and re-enabling it changes no live campaign. Only status is written.
func TestApplyCampaignConversionGoal_ReenablesARemovedGoalHoldingTheSet(t *testing.T) {
	removed := goalRow("REMOVED", goalTestAction1, goalTestAction2)
	for name, named := range map[string]string{
		"the goal named for the set": removed,
		"a goal under another name":  goalRow("ENABLED", goalTestAction1),
	} {
		t.Run(name, func(t *testing.T) {
			f := &goalFake{searches: []string{named, removed}}
			c := goalClient(t, f)
			step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
			if err != nil {
				t.Fatalf("applyCampaignConversionGoal: %v", err)
			}
			re := f.sent("reenable")
			want := `{"operations":[{"update":{"resourceName":"` + goalTestGoal + `","status":"ENABLED"},"updateMask":"status"}]}`
			if len(re) != 1 || re[0] != want {
				t.Fatalf("re-enables %v, want exactly %s", re, want)
			}
			if !strings.Contains(step, "custom goal 777, re-enabled: a removed goal held these actions") {
				t.Errorf("step = %q, want the goal named as re-enabled", step)
			}
			atomic := f.sent("atomic")
			if len(atomic) != 1 || !strings.Contains(atomic[0], `"customConversionGoal":"`+goalTestGoal+`"`) {
				t.Errorf("atomic writes %v, want one pointing the campaign at the re-enabled goal", atomic)
			}
		})
	}
}

// An enabled goal holding the set is preferred to a removed one: reusing it writes nothing.
func TestApplyCampaignConversionGoal_PrefersAnEnabledGoalToARemovedOne(t *testing.T) {
	both := `{"results":[` +
		`{"customConversionGoal":{"resourceName":"` + goalTestGoal + `","status":"REMOVED","conversionActions":["` + goalTestAction1 + `"]}},` +
		`{"customConversionGoal":{"resourceName":"` + goalTestHandGoal + `","status":"ENABLED","conversionActions":["` + goalTestAction1 + `"]}}]}`
	f := &goalFake{searches: []string{goalRow("REMOVED", goalTestAction1), both}}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if n := len(f.sent("goal")); n != 0 {
		t.Errorf("sent %d custom goal writes, want none", n)
	}
	if !strings.Contains(step, "custom goal 888, reused") {
		t.Errorf("step = %q, want the enabled goal reused", step)
	}
}

// Google refuses a create when a goal holds the same action list. When that goal is REMOVED, it
// is re-enabled and used, instead of the refusal standing.
func TestApplyCampaignConversionGoal_ADuplicateActionListHeldByARemovedGoalReenablesIt(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, goalRow("REMOVED", goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "DUPLICATE_CONVERSION_ACTION_LIST"),
	}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if n := len(f.sent("reenable")); n != 1 {
		t.Errorf("sent %d re-enables, want 1", n)
	}
	if !strings.Contains(step, "re-enabled") {
		t.Errorf("step = %q, want the removed goal re-enabled", step)
	}
}

// A re-enable is confirmed for THIS goal before the campaign is pointed at it. One answered 2xx
// naming another goal, or refused after a retried 429, may or may not have applied: the campaign
// is not pointed at the goal, and the error is unconfirmed about it.
func TestApplyCampaignConversionGoal_AnUnconfirmedReenableIsNotUsed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	for name, reenable := range map[string]http.HandlerFunc{
		"2xx naming another goal": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+goalTestHandGoal+`"}]}`)
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
			gaqlError(http.StatusBadRequest, "customConversionGoalError", "INVALID_CONVERSION_ACTION")(w, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			removed := goalRow("REMOVED", goalTestAction1)
			f := &goalFake{searches: []string{removed, removed}, reenable: reenable}
			c := goalClient(t, f)
			_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
			if err == nil || !IsOutcomeUnconfirmed(err) || !strings.Contains(err.Error(), "NOT applied to campaign 222") {
				t.Fatalf("err = %v, want an unconfirmed re-enable and the goal NOT applied to the campaign", err)
			}
			if n := len(f.sent("atomic")); n != 0 {
				t.Errorf("pointed the campaign at a goal whose re-enable was not confirmed (%d writes)", n)
			}
		})
	}
}

// When the goal named for the set changed but another enabled goal holds exactly the set (the
// second-name goal an earlier campaign created, or one made by hand), that goal is reused.
func TestApplyCampaignConversionGoal_AChangedGoalFallsBackToOneHoldingTheSet(t *testing.T) {
	f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1), handGoalRows(goalTestAction2, goalTestAction1)}}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if n := len(f.sent("goal")); n != 0 {
		t.Errorf("sent %d custom goal writes, want none", n)
	}
	if !strings.Contains(step, "custom goal 888, reused: an existing goal already holds these actions") {
		t.Errorf("step = %q, want the goal holding the set named as reused", step)
	}
}

// When the second name is also held by a goal edited to hold something else, nothing is written
// and the goal is NOT applied: there is no third name to fall back to.
func TestApplyCampaignConversionGoal_ASecondNameThatWasAlsoEditedIsNotApplied(t *testing.T) {
	f := &goalFake{
		searches: []string{goalRow("ENABLED", goalTestAction1), `{"results":[]}`, goalRow("ENABLED", goalTestAction2)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "CUSTOM_GOAL_DUPLICATE_NAME"),
	}
	c := goalClient(t, f)
	_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err == nil || !strings.Contains(err.Error(), "NOT applied") {
		t.Fatalf("err = %v, want the goal NOT applied", err)
	}
	if n := len(f.sent("atomic")); n != 0 {
		t.Errorf("sent %d writes with no goal to point at", n)
	}
}

// A create that loses the name race to a REMOVED goal holding the set re-enables that goal, as
// any removed holder is.
func TestApplyCampaignConversionGoal_ANameRaceLostToARemovedGoalReenablesIt(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, goalRow("REMOVED", goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "CUSTOM_GOAL_DUPLICATE_NAME"),
	}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if n := len(f.sent("reenable")); n != 1 || !strings.Contains(step, "re-enabled") {
		t.Errorf("re-enables %d, step %q: want the removed winner re-enabled", n, step)
	}
}

// A goal create answered 2xx without a usable name may still have created the goal. The
// campaign is not pointed at anything, and the error says a retry finds the goal by name.
func TestApplyCampaignConversionGoal_ACreateWithoutAUsableNameSaysARetryFindsIt(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`},
		create: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{}]}`)
		},
	}
	c := goalClient(t, f)
	_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err == nil || !strings.Contains(err.Error(), "the goal may exist; a retry finds it by name and reuses it") {
		t.Fatalf("err = %v, want it to say the goal may exist and a retry reuses it", err)
	}
	if !IsOutcomeUnconfirmed(err) || !strings.Contains(err.Error(), "NOT applied to campaign 222") {
		t.Errorf("err = %v, want it unconfirmed about the goal and NOT applied to the campaign", err)
	}
	if n := len(f.sent("atomic")); n != 0 {
		t.Errorf("sent %d writes with no confirmed goal", n)
	}
}

// Two creates for one set race: the loser looks the winner's goal up and uses it.
func TestApplyCampaignConversionGoal_ADuplicateNameUsesTheWinnersGoal(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, goalRow("ENABLED", goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "CUSTOM_GOAL_DUPLICATE_NAME"),
	}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if !strings.Contains(step, "reused") {
		t.Errorf("step = %q, want the winner's goal reported as reused", step)
	}
	if n := len(f.sent("atomic")); n != 1 {
		t.Errorf("sent %d googleAds:mutate writes, want 1", n)
	}
}

func handGoalRows(actions ...string) string {
	raw, _ := json.Marshal(actions)
	return `{"results":[{"customConversionGoal":{"resourceName":"` + goalTestHandGoal + `","status":"ENABLED","conversionActions":` + string(raw) + `}}]}`
}

// Google refuses a second goal holding the same action list under any name. A goal made by
// hand for exactly that set is reused as it stands: never edited, and named as such.
func TestApplyCampaignConversionGoal_ReusesAHandMadeGoalHoldingTheSameActions(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, handGoalRows(goalTestAction2, goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "DUPLICATE_CONVERSION_ACTION_LIST"),
	}
	c := goalClient(t, f)
	step, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err != nil {
		t.Fatalf("applyCampaignConversionGoal: %v", err)
	}
	if !strings.Contains(step, "custom goal 888, reused: an existing goal already holds these actions") {
		t.Errorf("step = %q, want the hand-made goal named as reused", step)
	}
	if n := len(f.sent("goal")); n != 1 {
		t.Errorf("sent %d custom goal writes, want only the refused create", n)
	}
	atomic := f.sent("atomic")
	if len(atomic) != 1 || !strings.Contains(atomic[0], `"customConversionGoal":"`+goalTestHandGoal+`"`) {
		t.Errorf("atomic writes %v, want one pointing the campaign at the hand-made goal", atomic)
	}
}

// When the refusal names a duplicate list but no goal the lookup finds holds exactly the set,
// there is nothing to use, and the refusal stands.
func TestApplyCampaignConversionGoal_ADuplicateActionListWithNoGoalHoldingTheSetIsNotApplied(t *testing.T) {
	f := &goalFake{
		searches: []string{`{"results":[]}`, handGoalRows(goalTestAction1)},
		create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "DUPLICATE_CONVERSION_ACTION_LIST"),
	}
	c := goalClient(t, f)
	_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1, goalTestAction2))
	if err == nil || !strings.Contains(err.Error(), "NOT applied") {
		t.Fatalf("err = %v, want the goal NOT applied", err)
	}
	if n := len(f.sent("atomic")); n != 0 {
		t.Errorf("sent %d writes with no goal to point at", n)
	}
}

// A failed write is a partial result that says the goal was NOT applied, never success.
func TestApplyCampaignConversionGoal_FailuresSayTheGoalWasNotApplied(t *testing.T) {
	for name, f := range map[string]*goalFake{
		"goal create refused": {
			searches: []string{`{"results":[]}`},
			create:   gaqlError(http.StatusBadRequest, "customConversionGoalError", "INVALID_CONVERSION_ACTION"),
		},
		"atomic write refused": {
			searches: []string{goalRow("ENABLED", goalTestAction1)},
			atomic:   gaqlError(http.StatusBadRequest, "conversionGoalCampaignConfigError", "INVALID_GOAL_CONFIG_LEVEL"),
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
			// The goal step runs first and every channel returns on its failure, so the error
			// must say nothing else was built.
			if !strings.Contains(err.Error(), "nothing past the campaign was built") {
				t.Errorf("error %q does not say the create stopped here", err)
			}
		})
	}
}

// An atomic write whose outcome is unknown may already have pointed the campaign at the goal,
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
			f := &goalFake{searches: []string{goalRow("ENABLED", goalTestAction1)}, atomic: config}
			c := goalClient(t, f)
			_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
			if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") || strings.Contains(err.Error(), "NOT applied") {
				t.Fatalf("err = %v, want an UNCONFIRMED goal, not NOT applied", err)
			}
		})
	}
}

// A 2xx whose results do not name this campaign's config and each goal taken out of bidding,
// in order, is not taken as this campaign's goal.
func TestApplyCampaignConversionGoal_AResultNamingAnythingElseIsUnconfirmed(t *testing.T) {
	config := `{"conversionGoalCampaignConfigResult":{"resourceName":"` + goalTestConfig + `"}}`
	goal := `{"campaignConversionGoalResult":{"resourceName":"` + goalTestBiddable + `"}}`
	for name, resp := range map[string]string{
		"another campaign's config": `{"mutateOperationResponses":[{"conversionGoalCampaignConfigResult":{"resourceName":"customers/1234567890/conversionGoalCampaignConfigs/999"}},` + goal + `]}`,
		"another goal":              `{"mutateOperationResponses":[` + config + `,{"campaignConversionGoalResult":{"resourceName":"customers/1234567890/campaignConversionGoals/222~SIGNUP~WEBSITE"}}]}`,
		"a goal result missing":     `{"mutateOperationResponses":[` + config + `]}`,
		"no results":                `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &goalFake{
				searches: []string{goalRow("ENABLED", goalTestAction1)},
				atomic: func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, resp)
				},
			}
			c := goalClient(t, f)
			_, err := c.applyCampaignConversionGoal(context.Background(), "222", goalPlan(biddingMaximizeConversions, goalTestAction1))
			if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") || !IsOutcomeUnconfirmed(err) {
				t.Fatalf("err = %v, want an UNCONFIRMED goal that IsOutcomeUnconfirmed reports", err)
			}
		})
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

// The goal is applied before the Demand Gen ad group, so a partial from a later failure must
// still report it: the campaign's goals already changed in the account.
func TestCreateDemandGenCampaign_APartialAfterTheGoalReportsIt(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			emptySearchPage(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "customConversionGoals:mutate"):
			okGoalCreate(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			_, _ = io.WriteString(w, echoGoalMutate(body))
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			gaqlError(http.StatusBadRequest, "adGroupError", "INVALID_ADGROUP")(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	in := demandGenInput()
	in.ConversionActions = []string{"11"}
	res, err := demandGenClient(t, srv).CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("want the ad group failure")
	}
	if res == nil || !strings.Contains(strings.Join(res.Steps, "\n"), "Conversion goal applied") {
		t.Errorf("partial steps %v do not report the goal that was applied", res)
	}
}
