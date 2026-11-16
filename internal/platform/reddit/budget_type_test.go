// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestResolveBudgetGoalType(t *testing.T) {
	for _, tc := range []struct{ in, want, errHas string }{
		{"", GoalTypeLifetimeSpend, ""},
		{"lifetime", GoalTypeLifetimeSpend, ""},
		{" Daily ", GoalTypeDailySpend, ""},
		{"DAILY", GoalTypeDailySpend, ""},
		{"weekly", "", "is not one of"},
		{"DAILY_SPEND", "", "is not one of"},
	} {
		got, err := ResolveBudgetGoalType(tc.in)
		if tc.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("%q: err = %v, want one naming %q", tc.in, err, tc.errHas)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestCreateCampaign_BudgetTypeSetsGoalType(t *testing.T) {
	for _, tc := range []struct{ budgetType, wantGoal, wantStep string }{
		{"", "LIFETIME_SPEND", "$1000 lifetime"},
		{"lifetime", "LIFETIME_SPEND", "$1000 lifetime"},
		{"daily", "DAILY_SPEND", "$1000 daily"},
	} {
		t.Run(tc.budgetType, func(t *testing.T) {
			c, bodies, cleanup := newBodyCaptureServers(t)
			defer cleanup()
			in := validRedditInput()
			in.BudgetUSD = 1000
			in.BudgetType = tc.budgetType
			res, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			campaign, _ := bodies()
			if campaign["goal_type"] != tc.wantGoal {
				t.Errorf("goal_type = %v, want %s", campaign["goal_type"], tc.wantGoal)
			}
			if campaign["goal_value"] != float64(1000*1_000_000) {
				t.Errorf("goal_value = %v, want the budget in micro-units whatever the pacing", campaign["goal_value"])
			}
			if joined := strings.Join(res.Steps, " | "); !strings.Contains(joined, tc.wantStep) {
				t.Errorf("steps %s do not report %q", joined, tc.wantStep)
			}
		})
	}
}

func TestCreateCampaign_RefusesBadBudgetTypeBeforeAnyCall(t *testing.T) {
	c, calls, cleanup := newCountingServers(t)
	defer cleanup()
	in := validRedditInput()
	in.BudgetType = "weekly"
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil || res != nil {
		t.Fatalf("want a refusal and no result, got res=%v err=%v", res, err)
	}
	var refusal *domain.PreflightRefusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "budgetType") {
		t.Errorf("want a PreflightRefusal naming budgetType, got %T: %v", err, err)
	}
	if n := calls(); n != 0 {
		t.Errorf("%d upstream calls before the refusal; want 0", n)
	}
}
