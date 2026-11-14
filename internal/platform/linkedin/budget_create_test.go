// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

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
)

// budgetCreateServer answers a full CreateCampaign and captures the campaign group and campaign
// POST bodies as raw JSON, so a test can assert the exact budget and schedule fields on the wire.
// posts counts every mutating request, so a refusal can prove nothing was sent.
func budgetCreateServer(t *testing.T) (*httptest.Server, func() (group, campaign map[string]any), func() int) {
	t.Helper()
	var mu sync.Mutex
	var groupBody, campaignBody map[string]any
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		posts++
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			groupBody = body
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			campaignBody = body
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv,
		func() (map[string]any, map[string]any) { mu.Lock(); defer mu.Unlock(); return groupBody, campaignBody },
		func() int { mu.Lock(); defer mu.Unlock(); return posts }
}

func budgetCreateInput(budget float64, lifetime bool, dailyCap float64) CampaignInput {
	in := biddingCreateInput(0)
	in.BudgetUSD = budget
	in.LifetimeBudget = lifetime
	in.DailyBudgetUSD = dailyCap
	return in
}

func moneyField(t *testing.T, body map[string]any, field string) string {
	t.Helper()
	m, ok := body[field].(map[string]any)
	if !ok {
		return ""
	}
	if m["currencyCode"] != "USD" {
		t.Errorf("%s.currencyCode = %v, want USD", field, m["currencyCode"])
	}
	s, _ := m["amount"].(string)
	return s
}

// TestCreateCampaign_LifetimeWithDailyCapSendsBothBudgets pins #584's wire shape: a lifetime
// budget with a daily cap is ONE campaign carrying totalBudget and dailyBudget, with a
// runSchedule that has a start and no end — LinkedIn's documented "Daily Budget and a Total Budget
// with a Continuous Schedule" option. The campaign GROUP carries the end; group_capped_test.go
// pins the guards that keep the shared group ending exactly there.
func TestCreateCampaign_LifetimeWithDailyCapSendsBothBudgets(t *testing.T) {
	srv, bodies, _ := budgetCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.CreateCampaign(context.Background(), budgetCreateInput(1000, true, 20)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	group, campaign := bodies()
	if got := moneyField(t, campaign, "totalBudget"); got != "1000.00" {
		t.Errorf("totalBudget.amount = %q, want 1000.00", got)
	}
	if got := moneyField(t, campaign, "dailyBudget"); got != "20.00" {
		t.Errorf("dailyBudget.amount = %q, want 20.00", got)
	}
	sched, _ := campaign["runSchedule"].(map[string]any)
	if _, ok := sched["start"]; !ok {
		t.Errorf("campaign runSchedule has no start: %v", campaign["runSchedule"])
	}
	if end, ok := sched["end"]; ok {
		t.Errorf("campaign runSchedule.end = %v, want none: LinkedIn pairs both budgets with a continuous schedule", end)
	}
	gsched, _ := group["runSchedule"].(map[string]any)
	if _, ok := gsched["end"]; !ok {
		t.Errorf("campaign group runSchedule has no end, so nothing bounds the two-budget campaign's flight: %v", group["runSchedule"])
	}
}

// TestCreateCampaign_SingleBudgetUnchanged pins the behaviour #584 must not change: without a
// daily cap exactly one budget field is sent, and the campaign keeps its runSchedule.end.
func TestCreateCampaign_SingleBudgetUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		lifetime  bool
		budget    float64
		wantField string
		notField  string
	}{
		{"lifetime only", true, 1000, "totalBudget", "dailyBudget"},
		{"daily only", false, 50, "dailyBudget", "totalBudget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies, _ := budgetCreateServer(t)
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			if _, err := c.CreateCampaign(context.Background(), budgetCreateInput(tc.budget, tc.lifetime, 0)); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			_, campaign := bodies()
			if moneyField(t, campaign, tc.wantField) == "" {
				t.Errorf("%s missing: %v", tc.wantField, campaign)
			}
			if _, has := campaign[tc.notField]; has {
				t.Errorf("%s sent on a single-budget create: %v", tc.notField, campaign[tc.notField])
			}
			sched, _ := campaign["runSchedule"].(map[string]any)
			if _, ok := sched["end"]; !ok {
				t.Errorf("single-budget campaign lost its runSchedule.end: %v", campaign["runSchedule"])
			}
		})
	}
}

// TestCreateCampaign_BudgetPairRefusedBeforeAnyPost proves every pair rule refuses with nothing
// created — LinkedIn would otherwise reject the campaign only after the campaign group exists.
func TestCreateCampaign_BudgetPairRefusedBeforeAnyPost(t *testing.T) {
	cases := []struct {
		name     string
		budget   float64
		lifetime bool
		cap      float64
		wantText string
	}{
		{"cap without lifetime", 50, false, 20, "needs a lifetime budget"},
		{"cap above total", 150, true, 200, "above the lifetime budget"},
		{"cap below the daily minimum", 1000, true, 5, "below LinkedIn's minimum of $10"},
		{"total below the lifetime minimum", 90, true, 20, "below LinkedIn's minimum of $100"},
		{"negative cap", 1000, true, -20, "greater than zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, posts := budgetCreateServer(t)
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			_, err := c.CreateCampaign(context.Background(), budgetCreateInput(tc.budget, tc.lifetime, tc.cap))
			if err == nil {
				t.Fatal("want a refusal")
			}
			if !errors.Is(err, ErrBudgetAmountInvalid) {
				t.Errorf("error does not wrap ErrBudgetAmountInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
			if n := posts(); n != 0 {
				t.Errorf("refusal issued %d POST(s); nothing may be created", n)
			}
		})
	}
}

// TestValidateBudgetPair checks the pair rule on the ROUNDED wire values: a cap equal to the
// total is accepted, and a cap that only exceeds the total below wire precision is too.
func TestValidateBudgetPair(t *testing.T) {
	cases := []struct {
		total, daily       float64
		wantTotal, wantDay string
		wantErr            bool
	}{
		{1000, 20, "1000.00", "20.00", false},
		{100, 100, "100.00", "100.00", false},
		{100, 100.004, "100.00", "100.00", false},
		{100, 100.01, "", "", true},
		{99.99, 20, "", "", true},
		{1000, 9.99, "", "", true},
	}
	for _, tc := range cases {
		gotTotal, gotDay, err := ValidateBudgetPair(tc.total, tc.daily)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateBudgetPair(%v, %v) err = %v, wantErr %v", tc.total, tc.daily, err, tc.wantErr)
			continue
		}
		if gotTotal != tc.wantTotal || gotDay != tc.wantDay {
			t.Errorf("ValidateBudgetPair(%v, %v) = (%q, %q), want (%q, %q)", tc.total, tc.daily, gotTotal, gotDay, tc.wantTotal, tc.wantDay)
		}
	}
}
