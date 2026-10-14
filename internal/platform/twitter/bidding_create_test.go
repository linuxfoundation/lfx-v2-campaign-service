// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// twitterBiddingClient answers a CreateCampaign, capturing the line item's query parameters and
// counting mutating requests.
func twitterBiddingClient(t *testing.T) (*Client, func() url.Values, func() int) {
	t.Helper()
	var mu sync.Mutex
	var lineItem url.Values
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			posts++
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			lineItem = r.URL.Query()
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL), WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	return c,
		func() url.Values { mu.Lock(); defer mu.Unlock(); return lineItem },
		func() int { mu.Lock(); defer mu.Unlock(); return posts }
}

func twitterBiddingInput(maxBid float64) CampaignInput {
	return CampaignInput{
		EventName: "KubeCon", Project: "CNCF", BudgetUsd: 500,
		StartDate: "2099-03-01", EndDate: "2099-03-10", TweetID: "1234567890",
		RegistrationURL: "https://events.lf.org/kubecon", MaxBid: maxBid,
	}
}

// TestCreateCampaign_AutomaticBiddingIsAuto pins the automatic line item (LFXV2-2665):
// bid_strategy AUTO with no pay_by and no bid amount — exactly as before.
func TestCreateCampaign_AutomaticBiddingIsAuto(t *testing.T) {
	c, li, _ := twitterBiddingClient(t)
	if _, err := c.CreateCampaign(context.Background(), twitterBiddingInput(0)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	q := li()
	if q.Get("bid_strategy") != "AUTO" || q.Has("pay_by") || q.Has("bid_amount_local_micro") {
		t.Fatalf("automatic line item params = %v, want bid_strategy AUTO only", q)
	}
	if (&LineItemBid{BidStrategy: q.Get("bid_strategy"), PayBy: q.Get("pay_by")}).ManualCPC() {
		t.Fatal("an AUTO line item must not read as a manual CPC")
	}
}

// TestCreateCampaign_ManualBiddingIsWhatTheBidLeverAccepts pins the manual line item: MAX,
// pay_by LINK_CLICK, bid_amount_local_micro — LineItemBid.ManualCPC, the lever's guard.
func TestCreateCampaign_ManualBiddingIsWhatTheBidLeverAccepts(t *testing.T) {
	c, li, _ := twitterBiddingClient(t)
	res, err := c.CreateCampaign(context.Background(), twitterBiddingInput(2.5))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	q := li()
	if q.Get("bid_amount_local_micro") != "2500000" {
		t.Fatalf("bid_amount_local_micro = %q, want 2500000", q.Get("bid_amount_local_micro"))
	}
	if !(&LineItemBid{BidStrategy: q.Get("bid_strategy"), PayBy: q.Get("pay_by")}).ManualCPC() {
		t.Fatalf("manual line item params %v are not the MAX/LINK_CLICK the bid lever accepts", q)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "MAX bid 2.5 per link click") {
		t.Errorf("steps do not record the manual bid: %v", res.Steps)
	}
}

// TestCreateCampaign_SubMicroManualBidIsRefusedBeforeAnyPost: refused, never rounded.
func TestCreateCampaign_SubMicroManualBidIsRefusedBeforeAnyPost(t *testing.T) {
	c, _, posts := twitterBiddingClient(t)
	res, err := c.CreateCampaign(context.Background(), twitterBiddingInput(0.0000015))
	if err == nil || !errors.Is(err, ErrBidAmountInvalid) {
		t.Fatalf("err = %v, want ErrBidAmountInvalid", err)
	}
	if res != nil || posts() != 0 {
		t.Fatalf("result=%+v posts=%d, want nil and 0", res, posts())
	}
}
