// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeConversionGoal(t *testing.T) {
	for _, tc := range []struct{ goal, tag, wantGoal, wantTag string }{
		{"", "", "", ""},
		{" ", "  ", "", ""},
		{"WEBSITE_CONVERSIONS", "o3bk1", "WEBSITE_CONVERSIONS", "o3bk1"},
		{" website_conversions ", " O3BK1 ", "WEBSITE_CONVERSIONS", "o3bk1"},
		{"WEBSITE_CONVERSIONS", strings.Repeat("a", 32), "WEBSITE_CONVERSIONS", strings.Repeat("a", 32)},
	} {
		g, tg, err := NormalizeConversionGoal(tc.goal, tc.tag)
		if err != nil || g != tc.wantGoal || tg != tc.wantTag {
			t.Errorf("NormalizeConversionGoal(%q, %q) = (%q, %q, %v), want (%q, %q)", tc.goal, tc.tag, g, tg, err, tc.wantGoal, tc.wantTag)
		}
	}
	for _, tc := range []struct{ goal, tag, field, msg string }{
		{"", "o3bk1", FieldPrimaryWebEventTag, "needs goal WEBSITE_CONVERSIONS"},
		{"LINK_CLICKS", "o3bk1", FieldGoal, "must be WEBSITE_CONVERSIONS"},
		{"SITE_VISITS", "", FieldGoal, "must be WEBSITE_CONVERSIONS"},
		{"WEBSITE_CONVERSIONS", "", FieldPrimaryWebEventTag, "is required with goal WEBSITE_CONVERSIONS"},
		{"WEBSITE_CONVERSIONS", "tw-o8z6j-o3bk1", FieldPrimaryWebEventTag, "must be 1-32 letters or digits"},
		{"WEBSITE_CONVERSIONS", "o3 bk1", FieldPrimaryWebEventTag, "must be 1-32 letters or digits"},
		{"WEBSITE_CONVERSIONS", strings.Repeat("a", 33), FieldPrimaryWebEventTag, "must be 1-32 letters or digits"},
		{"WEBSITE_CONVERSIONS", "ö3bk1", FieldPrimaryWebEventTag, "must be 1-32 letters or digits"},
	} {
		var tErr *TargetingError
		_, _, err := NormalizeConversionGoal(tc.goal, tc.tag)
		if !errors.As(err, &tErr) || tErr.Field != tc.field || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("NormalizeConversionGoal(%q, %q) err = %v, want %s refusal containing %q", tc.goal, tc.tag, err, tc.field, tc.msg)
		}
	}
}

// The line item THIS call creates carries goal and primary_web_event_tag; without them, neither
// parameter is sent and the objective stays WEBSITE_CLICKS either way.
func TestCreateCampaign_ConversionGoalRidesOnTheNewLineItem(t *testing.T) {
	for _, tc := range []struct{ goal, tag, wantGoal, wantTag string }{
		{"website_conversions", "O3BK1", "WEBSITE_CONVERSIONS", "o3bk1"},
		{"", "", "", ""},
	} {
		f := newTargetingFakeX()
		c := targetingClient(t, f)
		ci := targetingInput(nil, nil)
		ci.Goal, ci.PrimaryWebEventTag = tc.goal, tc.tag
		res, err := c.CreateCampaign(context.Background(), ci)
		if err != nil {
			t.Fatalf("CreateCampaign(%q, %q): %v", tc.goal, tc.tag, err)
		}
		f.mu.Lock()
		raw := f.lineItemQuery
		f.mu.Unlock()
		q, _ := url.ParseQuery(raw)
		if q.Get("goal") != tc.wantGoal || q.Has("goal") != (tc.wantGoal != "") ||
			q.Get("primary_web_event_tag") != tc.wantTag || q.Has("primary_web_event_tag") != (tc.wantTag != "") ||
			q.Get("objective") != "WEBSITE_CLICKS" {
			t.Errorf("goal %q tag %q: line-item query %q", tc.goal, tc.tag, raw)
		}
		steps := strings.Join(res.Steps, "\n")
		if tc.wantGoal != "" && !strings.Contains(steps, "Line item li1 sent with goal WEBSITE_CONVERSIONS and primary web event tag o3bk1") ||
			tc.wantGoal == "" && strings.Contains(steps, "web event tag") {
			t.Errorf("goal %q: steps = %s", tc.goal, steps)
		}
	}
}

// A reused line item keeps its own goal: nothing is sent for it, and a warning step says so.
func TestCreateCampaign_ConversionGoalNotAppliedToAReusedLineItem(t *testing.T) {
	f := newTargetingFakeX()
	f.reuseLineItem = "Events | KubeCon | Promoted Tweets | AUTO"
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.Goal, in.PrimaryWebEventTag = "WEBSITE_CONVERSIONS", "o3bk1"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if writes, _ := f.snapshot(); strings.Contains(strings.Join(writes, " "), "line_items") {
		t.Fatalf("writes = %v, want no line-item POST", writes)
	}
	if !res.Reused || !strings.Contains(strings.Join(res.Steps, "\n"), "goal WEBSITE_CONVERSIONS with web event tag o3bk1 NOT applied to reused line item li9") {
		t.Errorf("reused=%v steps = %v", res.Reused, res.Steps)
	}
}

// A goal without its tag is refused before any request.
func TestCreateCampaign_BadConversionGoalSendsNothing(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.Goal = "WEBSITE_CONVERSIONS"
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), "invalid twitterConfig.primaryWebEventTag") {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}
