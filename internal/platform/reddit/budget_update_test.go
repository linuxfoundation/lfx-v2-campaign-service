// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// budgetTestClient wires a client against an Ads API stub that answers every request with
// status/body and records each request. The handler never calls t.Fatal.
func budgetTestClient(t *testing.T, status int, body string, opts ...Option) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	all := append([]Option{WithBaseURL(api.URL + "/api/v3"), WithTokenURL(tok.URL), WithNowFunc(fixedRedditClock())}, opts...)
	return NewClient(testCreds, testAccount, all...), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestBudgetMicros_SharesTheCreatePathsBoundsAndRounding(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		want   int64
		ok     bool
	}{
		{250, 250_000_000, true},
		{1.5, 1_500_000, true},
		{99.9999996, 100_000_000, true}, // rounded, not truncated
		{0.0000006, 1, true},            // rounds up to one micro
		{0.0000004, 0, false},           // rounds to zero micros
		{redditMaxBudgetUSD, int64(redditMaxBudgetUSD) * 1_000_000, true},
		{redditMaxBudgetUSD + 1, 0, false}, // the overflow guard
		{0, 0, false},
		{-1, 0, false},
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
	} {
		got, err := BudgetMicros(tc.amount)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("BudgetMicros(%g) = %d, %v; want %d", tc.amount, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("BudgetMicros(%g) = %d, want a refusal", tc.amount, got)
			continue
		}
		if !errors.Is(err, ErrBudgetAmountInvalid) {
			t.Errorf("BudgetMicros(%g) refusal must wrap ErrBudgetAmountInvalid: %v", tc.amount, err)
		}
		if reason, ok := BudgetAmountReason(err); !ok || reason == "" {
			t.Errorf("BudgetMicros(%g) refusal must carry a client-safe reason", tc.amount)
		}
	}
}

func TestGetCampaignBudget_ReadsTheCampaignsGoal(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK,
		`{"data":{"id":"t3_camp","ad_account_id":"t2_test","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":"7000000"}}`)
	b, err := c.GetCampaignBudget(context.Background(), "t3_camp")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	if got := seen(); len(got) != 1 || got[0] != "GET /api/v3/ad_accounts/t2_test/campaigns/t3_camp " {
		t.Fatalf("requests = %q, want one GET of the campaign", got)
	}
	if b.GoalType != GoalTypeLifetimeSpend || b.GoalValueMicros == nil || *b.GoalValueMicros != 7_000_000 ||
		b.CampaignBudgetOptimization == nil || !*b.CampaignBudgetOptimization || b.AdAccountID != "t2_test" {
		t.Errorf("unexpected budget read: %+v", b)
	}
}

func TestGetCampaignBudget_404IsAbsentNotAnError(t *testing.T) {
	c, _ := budgetTestClient(t, http.StatusNotFound, `{}`)
	b, err := c.GetCampaignBudget(context.Background(), "t3_camp")
	if err != nil || b != nil {
		t.Fatalf("a 404 must read as (nil, nil), got %+v, %v", b, err)
	}
}

func TestCampaignBudget_InvalidCampaignIDRefusedBeforeAnyRequest(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{}}`)
	for _, id := range []string{"", "t3/../x", "t3?x=1", "t3#f"} {
		if _, err := c.GetCampaignBudget(context.Background(), id); !errors.Is(err, ErrInvalidCampaignID) {
			t.Errorf("GetCampaignBudget(%q) = %v, want ErrInvalidCampaignID", id, err)
		}
		if err := c.UpdateCampaignBudget(context.Background(), id, 1); !errors.Is(err, ErrInvalidCampaignID) {
			t.Errorf("UpdateCampaignBudget(%q) = %v, want ErrInvalidCampaignID", id, err)
		}
	}
	if err := c.UpdateCampaignBudget(context.Background(), "t3_camp", 0); err == nil {
		t.Error("a non-positive micro amount must be refused before any request")
	}
	if n := len(seen()); n != 0 {
		t.Errorf("an invalid input must reach no endpoint, got %d request(s)", n)
	}
}

func TestUpdateCampaignBudget_PatchesExactlyGoalValue(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t3_camp","goal_value":42000000}}`)
	if err := c.UpdateCampaignBudget(context.Background(), "t3_camp", 42_000_000); err != nil {
		t.Fatalf("UpdateCampaignBudget: %v", err)
	}
	want := `PATCH /api/v3/ad_accounts/t2_test/campaigns/t3_camp {"data":{"goal_value":42000000}}`
	if got := seen(); len(got) != 1 || got[0] != want {
		t.Fatalf("requests = %q, want [%q]", got, want)
	}
}

// A PATCH setting the same amount converges, so a throttle IS retried — but an exhausted one is
// UNCONFIRMED, never a definite failure.
func TestUpdateCampaignBudget_ExhaustedThrottleIsUnconfirmed(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff))

	err := c.UpdateCampaignBudget(context.Background(), "t3_camp", 1_000_000)
	if err == nil || !IsOutcomeUnconfirmed(err) {
		t.Fatalf("an exhausted 429 on the budget PATCH must be UNCONFIRMED, got %v", err)
	}
	if n := calls.Load(); n != retryMax+1 {
		t.Errorf("an idempotent PATCH should be retried on a 429: %d attempts, want %d", n, retryMax+1)
	}
}

func TestUpdateCampaignBudget_OutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		unconfirmed bool
	}{
		{"2xx with no body confirms", http.StatusOK, ``, false, false},
		{"2xx naming neither id nor amount confirms", http.StatusOK, `{"data":{}}`, false, false},
		{"2xx echoing the sent amount confirms", http.StatusOK, `{"data":{"id":"t3_camp","goal_value":1000000}}`, false, false},
		{"2xx echoing another campaign", http.StatusOK, `{"data":{"id":"t3_x"}}`, true, true},
		{"2xx echoing another amount", http.StatusOK, `{"data":{"goal_value":2}}`, true, true},
		{"2xx echoing an unreadable amount", http.StatusOK, `{"data":{"goal_value":"many"}}`, true, true},
		{"400 is a definite refusal", http.StatusBadRequest, `{}`, true, false},
		{"404 is a definite refusal", http.StatusNotFound, `{}`, true, false},
		{"500 is ambiguous", http.StatusInternalServerError, `{}`, true, true},
		{"503 is ambiguous", http.StatusServiceUnavailable, `{}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetTestClient(t, tc.status, tc.body)
			err := c.UpdateCampaignBudget(context.Background(), "t3_camp", 1_000_000)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && IsOutcomeUnconfirmed(err) != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v: %v", !tc.unconfirmed, tc.unconfirmed, err)
			}
		})
	}
}
