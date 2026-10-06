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
	"strings"
	"sync/atomic"
	"testing"
)

func TestBidMicros_BoundsAndRounding(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		want   int64
		ok     bool
	}{
		{2.35, 2_350_000, true},
		{0.0000006, 1, true},
		{0.0000004, 0, false},
		{redditMaxBid, int64(redditMaxBid) * 1_000_000, true},
		{redditMaxBid + 1, 0, false},
		{0, 0, false}, {-1, 0, false}, {math.NaN(), 0, false}, {math.Inf(1), 0, false},
	} {
		got, err := BidMicros(tc.amount)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("BidMicros(%v) = %d, %v; want %d", tc.amount, got, err, tc.want)
			}
			continue
		}
		if err == nil || !errors.Is(err, ErrBidAmountInvalid) {
			t.Errorf("BidMicros(%v) = %d, %v; want an ErrBidAmountInvalid", tc.amount, got, err)
		}
		if _, ok := BidAmountReason(err); !ok {
			t.Errorf("BidMicros(%v) refusal carries no client-safe reason", tc.amount)
		}
	}
}

func TestGetAdGroupBid_ReadsTheAdGroup(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_strategy":"MANUAL_BIDDING","bid_type":"CPC","bid_value":1500000}}`)
	got, err := c.GetAdGroupBid(context.Background(), "t5_ag")
	if err != nil {
		t.Fatalf("GetAdGroupBid: %v", err)
	}
	if got.CampaignID != "t3_c" || got.BidStrategy != BidStrategyManual || got.BidType != BidTypeCPC ||
		got.BidValueMicros == nil || *got.BidValueMicros != 1_500_000 || got.BidValueUnparseable {
		t.Errorf("unexpected shape: %+v", got)
	}
	if reqs := seen(); len(reqs) != 1 || !strings.HasPrefix(reqs[0], "GET /api/v3/ad_accounts/t2_test/ad_groups/t5_ag") {
		t.Errorf("want one GET of the ad group, got %v", reqs)
	}
}

func TestGetAdGroupBid_Shapes(t *testing.T) {
	t.Run("404 is no such ad group", func(t *testing.T) {
		c, _ := budgetTestClient(t, http.StatusNotFound, `{}`)
		got, err := c.GetAdGroupBid(context.Background(), "t5_ag")
		if err != nil || got != nil {
			t.Fatalf("want (nil, nil), got %+v, %v", got, err)
		}
	})
	t.Run("another ad group's answer is an error", func(t *testing.T) {
		c, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_other"}}`)
		if _, err := c.GetAdGroupBid(context.Background(), "t5_ag"); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("an id that cannot address a path never leaves the client", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, `{}`)
		_, err := c.GetAdGroupBid(context.Background(), "t5/../x")
		if !errors.Is(err, ErrInvalidAdGroupID) {
			t.Fatalf("want ErrInvalidAdGroupID, got %v", err)
		}
		if n := len(seen()); n != 0 {
			t.Errorf("want no request, got %d", n)
		}
	})
}

func TestUpdateAdGroupBid_PatchesBidValueOnly(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag","bid_value":2350000}}`)
	if err := c.UpdateAdGroupBid(context.Background(), "t5_ag", 2_350_000); err != nil {
		t.Fatalf("UpdateAdGroupBid: %v", err)
	}
	reqs := seen()
	if len(reqs) != 1 || reqs[0] != `PATCH /api/v3/ad_accounts/t2_test/ad_groups/t5_ag {"data":{"bid_value":2350000}}` {
		t.Errorf("want one PATCH naming bid_value only, got %v", reqs)
	}
}

func TestUpdateAdGroupBid_OutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		amount      bool
		unconfirmed bool
	}{
		{name: "400 with a bid_value field error", status: 400, body: `{"error":{"fields":[{"field":"bid_value","message":"too low"}]}}`, amount: true},
		{name: "400 mentioning bid_value outside fields[]", status: 400, body: `{"error":{"message":"bid_value is not allowed with BIDLESS","fields":[{"field":"bid_strategy"}]},"data":{"bid_value":2000000}}`},
		{name: "400 whose field error is another field", status: 400, body: `{"error":{"fields":[{"field":"bid_type"}]}}`},
		{name: "400 about something else", status: 400, body: `{"error":"nope"}`},
		{name: "5xx", status: 502, body: `{}`, unconfirmed: true},
		{name: "echo of another bid", status: 200, body: `{"data":{"id":"t5_ag","bid_value":7}}`, unconfirmed: true},
		{name: "echo of another ad group", status: 200, body: `{"data":{"id":"t5_x"}}`, unconfirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetTestClient(t, tc.status, tc.body)
			err := c.UpdateAdGroupBid(context.Background(), "t5_ag", 2_000_000)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrBidAmountInvalid); got != tc.amount {
				t.Errorf("ErrBidAmountInvalid = %v, want %v: %v", got, tc.amount, err)
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if tc.amount {
				reason, _ := BidAmountReason(err)
				if !strings.Contains(reason, "2") || strings.Contains(reason, "bid_value\"") {
					t.Errorf("reason must be this package's sentence naming the bid: %q", reason)
				}
			}
		})
	}
}

func TestUpdateAdGroupBid_RefusesANonPositiveMicroAmount(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{}`)
	if err := c.UpdateAdGroupBid(context.Background(), "t5_ag", 0); !errors.Is(err, ErrBidAmountInvalid) {
		t.Fatalf("want ErrBidAmountInvalid, got %v", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("want no request, got %d", n)
	}
}

// The campaign read also reports the campaign-level bid_strategy the bid write needs under CBO.
func TestGetCampaignBudget_ReportsTheCampaignBidStrategy(t *testing.T) {
	c, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"bid_strategy":"BIDLESS"}}`)
	got, err := c.GetCampaignBudget(context.Background(), "t3_c")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	if got.BidStrategy != "BIDLESS" {
		t.Errorf("BidStrategy = %q, want BIDLESS", got.BidStrategy)
	}
}

// throttledThenClient answers the FIRST request with a 429 and every later one with
// (status, body), counting attempts.
func throttledThenClient(t *testing.T, status int, body string) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff)), &calls
}

// A refusal that answers a PATCH retried after a 429 cannot speak for the 429'd attempt, which
// may have applied: even a structured bid_value 400 is UNCONFIRMED, never an amount refusal.
func TestUpdateAdGroupBid_RefusalAfterARetried429IsUnconfirmed(t *testing.T) {
	c, calls := throttledThenClient(t, http.StatusBadRequest, `{"error":{"fields":[{"field":"bid_value"}]}}`)
	err := c.UpdateAdGroupBid(context.Background(), "t5_ag", 2_000_000)
	if err == nil || !IsOutcomeUnconfirmed(err) {
		t.Fatalf("want UNCONFIRMED, got %v", err)
	}
	if errors.Is(err, ErrBidAmountInvalid) {
		t.Errorf("a refusal after a retried 429 must not be an amount refusal: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("want the 429 retried once (2 attempts), got %d", n)
	}
}

func TestUpdateAdGroupBid_SuccessAfterARetried429IsASuccess(t *testing.T) {
	c, calls := throttledThenClient(t, http.StatusOK, `{"data":{"id":"t5_ag","bid_value":2000000}}`)
	if err := c.UpdateAdGroupBid(context.Background(), "t5_ag", 2_000_000); err != nil {
		t.Fatalf("a throttled-then-accepted PATCH must succeed: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("want 2 attempts, got %d", n)
	}
}
