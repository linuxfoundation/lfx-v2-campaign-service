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
