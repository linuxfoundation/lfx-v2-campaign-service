// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bidUpdateServers mirrors budgetUpdateServers: a token endpoint and an API server that records
// the mutate body and answers with the ad group that was addressed.
func bidUpdateServers(t *testing.T) (*Client, func() string) {
	t.Helper()
	var mu sync.Mutex
	var body string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/654"}]}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	return c, func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
}

// TestValidateMaxCPCBid_Bounds pins the create path's bounds on the EDIT path, branch by branch.
// The floor matters most: ValidateMaxCPCBid is deliberately NOT validateCPCBid, whose contract
// answers (0, nil) for "no bid supplied". An edit always names a bid, so a zero here is a request
// to bid nothing and must be refused rather than sent as a real instruction to stop bidding.
func TestValidateMaxCPCBid_Bounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		bid  float64
		want int64
		ok   bool
	}{
		{"NaN", math.NaN(), 0, false},
		{"+Inf", math.Inf(1), 0, false},
		{"-Inf", math.Inf(-1), 0, false},
		{"zero", 0, 0, false},
		{"negative", -1, 0, false},
		{"below the floor", 0.009, 0, false},
		{"at the floor", minCPCBid, 10_000, true},
		{"at the ceiling", maxCPCBid, 100_000_000_000, true},
		{"above the ceiling", maxCPCBid + 1, 0, false},
		// 0.07 is 0.07000000000000001 in float64: truncation would bid 69,999 micros — a cent
		// less than asked, on every value of this shape.
		{"rounds rather than truncates", 0.07, 70_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateMaxCPCBid(tc.bid)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateMaxCPCBid(%v): %v", tc.bid, err)
				}
				if got != tc.want {
					t.Errorf("micros = %d, want %d", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMaxCPCBid(%v) = %d, want a refusal", tc.bid, got)
			}
			// Every refusal must carry a reason the dispatcher can turn into a 400; without
			// it the amount falls through to a 503 and invites a retry that cannot succeed.
			if _, ok := BidAmountReason(err); !ok {
				t.Errorf("refusal does not carry a BidAmountReason: %v", err)
			}
		})
	}
}

// TestUpdateAdGroupCPCBid_SendsUpdateMaskAndBid pins the payload. cpcBidMicros must be PRESENT
// even though the update mask names it: the mask without the field asks Google to set the field
// to its default — a zero bid — while the caller reads back a success.
func TestUpdateAdGroupCPCBid_SendsUpdateMaskAndBid(t *testing.T) {
	c, body := bidUpdateServers(t)

	if err := c.UpdateAdGroupCPCBid(context.Background(), "654", 1_250_000); err != nil {
		t.Fatalf("UpdateAdGroupCPCBid: %v", err)
	}

	got := body()
	for _, want := range []string{`"updateMask":"cpc_bid_micros"`, `"cpcBidMicros":1250000`, `customers/1234567890/adGroups/654`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"create"`) {
		t.Errorf("an update operation must not carry a create: %s", got)
	}
	// This method changes the bid and NOTHING else — in particular never the campaign's bidding
	// strategy, which is the one fact that decides whether the bid is used at all.
	for _, forbidden := range []string{`"status"`, `"name"`, `"biddingStrategyType"`, `"type"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("body wrote %s, which this method must never touch: %s", forbidden, got)
		}
	}
}

// TestUpdateAdGroupCPCBid_RejectsBadInput guards the input contract BEFORE any request. The ad
// group id interpolates into a resourceName, so a non-numeric value could alter the resource
// path. A non-positive amount is refused here too even though the dispatcher converts through
// ValidateMaxCPCBid: this method is exported, and Google accepts a zero as a real instruction to
// stop the ad group bidding.
func TestUpdateAdGroupCPCBid_RejectsBadInput(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	for _, tc := range []struct {
		name      string
		adGroupID string
		micros    int64
	}{
		{"empty ad group id", "  ", 1_000_000},
		{"non-numeric ad group id", "654/../999", 1_000_000},
		{"ad group id with a query delimiter", "654?x=1", 1_000_000},
		{"zero micros", "654", 0},
		{"negative micros", "654", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			if err := c.UpdateAdGroupCPCBid(context.Background(), tc.adGroupID, tc.micros); err == nil {
				t.Fatal("expected a rejection")
			}
			mu.Lock()
			sawCall := reached
			mu.Unlock()
			if sawCall {
				t.Error("no API call should be made — the guard is up front")
			}
		})
	}
}

// TestUpdateAdGroupCPCBid_RetriesThrottle pins the idempotent=true choice, the OPPOSITE of the
// create path's: setting a bid twice converges on identical state, so a throttled attempt must be
// retried rather than surfaced as an avoidable failure.
func TestUpdateAdGroupCPCBid_RetriesThrottle(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/654"}]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	if err := c.UpdateAdGroupCPCBid(context.Background(), "654", 1_000_000); err != nil {
		t.Fatalf("a throttled bid update must be retried, not failed: %v", err)
	}
	mu.Lock()
	total := attempts
	mu.Unlock()
	if total < 2 {
		t.Errorf("attempts = %d, want >1 (the 429 must be retried; idempotent=false would abort)", total)
	}
}

// TestUpdateAdGroupCPCBid_UnusableAcknowledgementIsUnconfirmed pins that a 2xx is not by itself a
// confirmed write — the caller persists the new bid on the strength of a nil return. The last two
// cases are the ones nothing else catches: firstResourceName extracts only the trailing segment,
// so another account's ad group and this account's campaign budget both carry the id that was
// addressed and both pass an id-only comparison.
func TestUpdateAdGroupCPCBid_UnusableAcknowledgementIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no results", `{"results":[]}`},
		{"malformed resource name", `{"results":[{"resourceName":"noslash"}]}`},
		{"a different ad group", `{"results":[{"resourceName":"customers/1234567890/adGroups/999"}]}`},
		{"the right id in a different account", `{"results":[{"resourceName":"customers/9999999999/adGroups/654"}]}`},
		{"the right id on a different resource kind", `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/654"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer apiSrv.Close()

			c := NewClient(testCreds(), testAccount(),
				WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
			err := c.UpdateAdGroupCPCBid(context.Background(), "654", 1_000_000)
			if err == nil {
				t.Fatal("a 2xx that does not acknowledge the addressed ad group must not be reported as a confirmed write")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Fatalf("error is not classified unconfirmed: %v — the request was accepted, so the change may have applied", err)
			}
		})
	}
}
