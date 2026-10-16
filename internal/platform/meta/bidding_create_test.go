// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func biddingClient(srvURL string, offset int64) *Client {
	return NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: offset},
		WithBaseURL(srvURL), WithClock(fixedMetaClock()),
	)
}

// TestCreateCampaign_AutomaticBiddingKeepsTheLowestCostAdSet pins the automatic request
// (LFXV2-2665): BidCap zero leaves the ad set exactly as before — LOWEST_COST_WITHOUT_CAP, billed
// on IMPRESSIONS, no bid_amount — which the bid lever does not read as a max CPC.
func TestCreateCampaign_AutomaticBiddingKeepsTheLowestCostAdSet(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := metaFieldServer(t, adsetCap, newBodyCapture())
	defer srv.Close()
	if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), metaFieldInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	body := adsetCap.get()
	if body["bid_strategy"] != "LOWEST_COST_WITHOUT_CAP" || body["billing_event"] != "IMPRESSIONS" {
		t.Fatalf("automatic ad set: bid_strategy=%v billing_event=%v", body["bid_strategy"], body["billing_event"])
	}
	if _, has := body["bid_amount"]; has {
		t.Fatalf("an automatic ad set must not send bid_amount: %v", body["bid_amount"])
	}
	got := AdSetBid{BidStrategy: body["bid_strategy"].(string), BillingEvent: body["billing_event"].(string), OptimizationGoal: body["optimization_goal"].(string)}
	if got.CPCBidCap() {
		t.Fatal("an automatic ad set must not read as a link-click bid cap")
	}
}

// TestCreateCampaign_ManualBiddingCreatesTheLinkClickBidCapTheLeverAccepts pins the manual
// request: LOWEST_COST_WITH_BID_CAP, billing_event and optimization_goal LINK_CLICKS, bid_amount
// in minor units — and that the lever's own predicate (CPCBidCap) accepts it.
func TestCreateCampaign_ManualBiddingCreatesTheLinkClickBidCapTheLeverAccepts(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := metaFieldServer(t, adsetCap, newBodyCapture())
	defer srv.Close()
	in := metaFieldInput()
	in.BidCap = 1.25
	if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	body := adsetCap.get()
	if body["bid_amount"] != float64(125) {
		t.Fatalf("bid_amount = %v, want 125 minor units", body["bid_amount"])
	}
	got := AdSetBid{BidStrategy: body["bid_strategy"].(string), BillingEvent: body["billing_event"].(string), OptimizationGoal: body["optimization_goal"].(string)}
	if !got.CPCBidCap() {
		t.Fatalf("manual ad set %+v is not the link-click bid cap the bid lever accepts", got)
	}
}

// TestCreateCampaign_ManualBidRefusalsSendNoMutation: a fractional minor unit (here a whole-yen
// account asked for 1.5), and a manual bid on an objective that does not optimize for link
// clicks, are refused before the first mutating call — never rounded, nothing created.
func TestCreateCampaign_ManualBidRefusalsSendNoMutation(t *testing.T) {
	cases := []struct {
		name      string
		offset    int64
		objective string
		bid       float64
		want      string
	}{
		{"fractional minor unit (zero-decimal currency)", 1, "traffic", 1.5, "not a whole number"},
		{"fractional cent", 100, "traffic", 1.255, "not a whole number"},
		{"awareness optimizes for reach", 100, "awareness", 2, "link clicks"},
		{"conversions optimizes for conversions", 100, "conversions", 2, "link clicks"},
		{"daily budget under five times the bid", 100, "traffic", 101, "at least 5 times the bid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("no mutating request may be sent, got %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.RawQuery, "filtering") {
					_, _ = w.Write([]byte(`{"data":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"name":"LF Core","account_status":1}`))
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.BidCap, in.PixelID = tc.objective, tc.bid, "123"
			res, err := biddingClient(srv.URL, tc.offset).CreateCampaign(context.Background(), in)
			if err == nil || !errors.Is(err, ErrBidAmountInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an ErrBidAmountInvalid naming %q", err, tc.want)
			}
			if res != nil {
				t.Fatalf("result = %+v, want nil (nothing created)", res)
			}
		})
	}
}

func TestManualBidSupported(t *testing.T) {
	for objective, want := range map[string]bool{
		"": true, "traffic": true, " Leads ": true, "awareness": false, "engagement": false, "conversions": false, "bogus": false,
	} {
		if got := ManualBidSupported(objective); got != want {
			t.Errorf("ManualBidSupported(%q) = %v, want %v", objective, got, want)
		}
	}
}

// TestCreateCampaign_BidCapBudgetMinimumIsComparedInEncodedMinorUnits (PR #332 review): Meta's
// daily-budget minimum for a click-billed bid cap (5 x bid_amount) is compared in the ENCODED minor
// units of the account currency, so a JPY account (offset 1) with bid 1 passes at budget 5 and is
// refused at 4; USD (offset 100) compares in cents. (A fractional JPY budget is refused by the
// budget encoder itself since #235, before this minimum is checked.)
func TestCreateCampaign_BidCapBudgetMinimumIsComparedInEncodedMinorUnits(t *testing.T) {
	cases := []struct {
		name        string
		offset      int64
		budget, bid float64
		refused     bool
	}{
		{"JPY 5 = 5 x 1", 1, 5, 1, false},
		{"JPY 4 < 5 x 1", 1, 4, 1, true},
		{"USD 6.25 = 5 x 1.25", 100, 6.25, 1.25, false},
		{"USD 6.24 < 5 x 1.25", 100, 6.24, 1.25, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adsetCap := newBodyCapture()
			srv := metaFieldServer(t, adsetCap, newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.Budget, in.BidCap = tc.budget, tc.bid
			_, err := biddingClient(srv.URL, tc.offset).CreateCampaign(context.Background(), in)
			if tc.refused {
				if !errors.Is(err, ErrBidAmountInvalid) || !strings.Contains(err.Error(), "at least 5 times the bid") || adsetCap.get() != nil {
					t.Fatalf("err = %v (ad set sent: %v), want the 5x refusal before the ad set", err, adsetCap.get() != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
		})
	}
}

// TestBidEncoders_RefuseAProductPastTheExactBound (PR #332 review, PR #316): both bid encoders
// refuse an amount whose minor-unit product exceeds MaxExactMinorUnits BEFORE any conversion — past
// it a float64 cannot carry the product exactly — rather than converting an out-of-range value.
func TestBidEncoders_RefuseAProductPastTheExactBound(t *testing.T) {
	const hugeOffset = int64(1e13) // 1e6 x 1e13 = 1e19, past MaxExactMinorUnits and int64
	if _, err := CreateBidMinorUnits(1e6, hugeOffset); !errors.Is(err, ErrBidAmountInvalid) || !strings.Contains(err.Error(), "too large to represent exactly") {
		t.Fatalf("CreateBidMinorUnits = %v, want a too-large refusal", err)
	}
	if _, err := bidToMinorUnits(1e6, hugeOffset); !errors.Is(err, ErrBidAmountInvalid) || !strings.Contains(err.Error(), "too large to represent exactly") {
		t.Fatalf("bidToMinorUnits = %v, want a too-large refusal", err)
	}
	// Just inside the bound still encodes.
	if minor, err := CreateBidMinorUnits(1e6, int64(1e8)); err != nil || minor != 1e14 {
		t.Fatalf("CreateBidMinorUnits(1e6, 1e8) = %d, %v; want 1e14", minor, err)
	}
}

// TestCreateCampaign_BidCapBudgetMinimumCannotOverflow (PR #332 review): with a fallback offset of
// 2e12, a 1e6 bid would encode to 2e18 minor units and 5x that would wrap int64 negative — under
// which ANY budget would pass. The exact encoder (PR #316) now refuses that bid itself, past
// MaxExactMinorUnits, before the 5x minimum is formed and before the ad set is created.
func TestCreateCampaign_BidCapBudgetMinimumCannotOverflow(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := metaFieldServer(t, adsetCap, newBodyCapture())
	defer srv.Close()
	in := metaFieldInput()
	in.Budget, in.BidCap = 1, 1e6
	_, err := biddingClient(srv.URL, int64(2e12)).CreateCampaign(context.Background(), in)
	if !errors.Is(err, ErrBidAmountInvalid) || !strings.Contains(err.Error(), "too large to represent exactly") {
		t.Fatalf("err = %v, want the too-large bid refusal", err)
	}
	if adsetCap.get() != nil {
		t.Fatal("an ad set was sent for a refused bid")
	}
}
