// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetCampaignBidStrategy_Shapes(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantNil        bool
		wantErr        bool
		scheme         string
		usesAdGroupBid bool
		portfolio      string
	}{
		{name: "EnhancedCpc, REST spelling", body: `{"Campaigns":[{"Id":321,"BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`, scheme: "EnhancedCpc", usesAdGroupBid: true},
		{name: "EnhancedCpc, retrieval spelling", body: `{"Campaigns":[{"Id":321,"BiddingScheme":{"Type":"EnhancedCpcBiddingScheme"}}],"PartialErrors":[]}`, scheme: "EnhancedCpc", usesAdGroupBid: true},
		{name: "ManualCpc", body: `{"Campaigns":[{"Id":321,"BiddingScheme":{"Type":"ManualCpc","ManualCpc":1.2}}],"PartialErrors":[]}`, scheme: "ManualCpc", usesAdGroupBid: true},
		{name: "MaxClicks", body: `{"Campaigns":[{"Id":321,"BiddingScheme":{"Type":"MaxClicks","MaxCpc":{"Amount":3}}}],"PartialErrors":[]}`, scheme: "MaxClicks"},
		{name: "no scheme", body: `{"Campaigns":[{"Id":321}],"PartialErrors":[]}`},
		{name: "portfolio", body: `{"Campaigns":[{"Id":321,"BidStrategyId":"77","BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`, scheme: "EnhancedCpc", portfolio: "77"},
		{name: "own strategy, zero portfolio id", body: `{"Campaigns":[{"Id":321,"BidStrategyId":0,"BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`, scheme: "EnhancedCpc", usesAdGroupBid: true},
		{name: "absent campaign", body: `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`, wantNil: true},
		{name: "another campaign's answer", body: `{"Campaigns":[{"Id":999,"BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) })
			got, err := c.GetCampaignBidStrategy(context.Background(), "321")
			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			case err != nil:
				t.Fatalf("GetCampaignBidStrategy: %v", err)
			case tc.wantNil:
				if got != nil {
					t.Fatalf("want nil for an absent campaign, got %+v", got)
				}
				return
			}
			if got.SchemeType != tc.scheme || got.UsesAdGroupBid() != tc.usesAdGroupBid || got.PortfolioBidStrategyID != tc.portfolio {
				t.Errorf("got %+v (UsesAdGroupBid=%v)", got, got.UsesAdGroupBid())
			}
		})
	}
}

// The new raw fields share a decode with the budget read. A scheme shape this package does not
// expect must fail the BID read's guard, never the budget read.
func TestGetCampaignBudget_UnexpectedBidFieldsDoNotBreakTheBudgetRead(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetStandard","BiddingScheme":"weird","BidStrategyId":{"x":1}}],"PartialErrors":[]}`)
	})
	if _, err := c.GetCampaignBudget(context.Background(), "321"); err != nil {
		t.Fatalf("the budget read must ignore the bid fields: %v", err)
	}
	s, err := c.GetCampaignBidStrategy(context.Background(), "321")
	if err != nil {
		t.Fatalf("GetCampaignBidStrategy: %v", err)
	}
	if !s.SchemeUnreadable || !s.PortfolioUnreadable || s.UsesAdGroupBid() {
		t.Errorf("unreadable fields must be flagged and never read as manual: %+v", s)
	}
}

func TestValidateMaxCPCBid(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		ok     bool
	}{
		{minCpcBid, true}, {maxCpcBid, true}, {2.5, true},
		{minCpcBid / 2, false}, {maxCpcBid + 0.01, false}, {0, false}, {-1, false},
		{math.NaN(), false}, {math.Inf(1), false},
	} {
		err := ValidateMaxCPCBid(tc.amount)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateMaxCPCBid(%v) = %v, want ok=%v", tc.amount, err, tc.ok)
		}
		if err != nil {
			if _, has := BidAmountReason(err); !has || !errors.Is(err, ErrBidAmountInvalid) {
				t.Errorf("a bound refusal must be a reasoned ErrBidAmountInvalid: %v", err)
			}
		}
	}
}

func TestUpdateAdGroupCpcBid_SendsIdAndCpcBidOnly(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	if err := c.UpdateAdGroupCpcBid(context.Background(), "321", "654", 1.5); err != nil {
		t.Fatalf("UpdateAdGroupCpcBid: %v", err)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPut || !strings.HasSuffix(reqs[0].path, "/CampaignManagement/v13/AdGroups") {
		t.Fatalf("want one PUT .../AdGroups, got %+v", reqs)
	}
	want := `{"CampaignId":321,"AdGroups":[{"Id":654,"CpcBid":{"Amount":1.5}}],"UpdateAudienceAdsBidAdjustment":false,"ReturnInheritedBidStrategyTypes":false}`
	if reqs[0].body != want {
		t.Errorf("body = %s\nwant   %s", reqs[0].body, want)
	}
}

func TestUpdateAdGroupCpcBid_RejectsBadInputBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	for _, tc := range []struct {
		campaign, adGroup string
		amount            float64
	}{
		{"x", "654", 1}, {"321", "", 1}, {"321", "6.5", 1}, {"321", "654", 0}, {"321", "654", 5000},
	} {
		if err := c.UpdateAdGroupCpcBid(context.Background(), tc.campaign, tc.adGroup, tc.amount); err == nil {
			t.Errorf("want a refusal for %+v", tc)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("bad input reached Microsoft %d times", n)
	}
}

// A 429 is retried (the PUT is idempotent) and a later success is a success.
func TestUpdateAdGroupCpcBid_RetriesThrottle(t *testing.T) {
	var hits atomic.Int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	if err := c.UpdateAdGroupCpcBid(context.Background(), "321", "654", 1.5); err != nil {
		t.Fatalf("a throttled-then-accepted PUT must succeed: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("want 2 attempts, got %d", n)
	}
}

func TestUpdateAdGroupCpcBid_ClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		amount      bool
		notSettable bool
		unconfirmed bool
	}{
		{name: "floor", status: 200, body: `{"PartialErrors":[{"Code":1515,"Index":0}]}`, amount: true},
		{name: "legacy floor code", status: 200, body: `{"PartialErrors":[{"ErrorCode":"BidAmountsLessThanFloorPrice","Index":0}]}`, amount: true},
		{name: "ceiling", status: 200, body: `{"PartialErrors":[{"ErrorCode":"CampaignServiceBidAmountsGreaterThanCeilingPrice","Index":0}]}`, amount: true},
		{name: "invalid search bids", status: 200, body: `{"PartialErrors":[{"Code":1017,"Index":0}]}`, amount: true},
		{name: "cannot set search bid", status: 200, body: `{"PartialErrors":[{"Code":1229,"Index":0}]}`, notSettable: true},
		{name: "invalid ad group id", status: 400, body: `{"Errors":[{"Code":1201,"ErrorCode":"CampaignServiceInvalidAdGroupId"}]}`, notSettable: true},
		{name: "5xx", status: 503, body: ``, unconfirmed: true},
		{name: "other partial error", status: 200, body: `{"PartialErrors":[{"Code":4242,"Index":0}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := c.UpdateAdGroupCpcBid(context.Background(), "321", "654", 1.5)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrBidAmountInvalid); got != tc.amount {
				t.Errorf("ErrBidAmountInvalid = %v, want %v: %v", got, tc.amount, err)
			}
			if got := errors.Is(err, ErrBidNotSettable); got != tc.notSettable {
				t.Errorf("ErrBidNotSettable = %v, want %v: %v", got, tc.notSettable, err)
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if tc.amount {
				if reason, ok := BidAmountReason(err); !ok || !strings.Contains(reason, "1.5") {
					t.Errorf("an amount refusal must carry a sentence naming the bid, got %q", reason)
				}
			}
		})
	}
}

// BidStrategyId is a CampaignAdditionalField — Microsoft returns it only when asked — so the bid
// read must request it, or the portfolio guard never sees a portfolio. The budget read must not.
func TestGetCampaignBidStrategy_RequestsBidStrategyID(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetStandard","BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`)
	})
	if _, err := c.GetCampaignBidStrategy(context.Background(), "321"); err != nil {
		t.Fatalf("GetCampaignBidStrategy: %v", err)
	}
	if _, err := c.GetCampaignBudget(context.Background(), "321"); err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("want two reads, got %+v", reqs)
	}
	if want := `{"AccountId":1234567,"CampaignIds":[321],"CampaignType":"Search","ReturnAdditionalFields":"BidStrategyId"}`; reqs[0].body != want {
		t.Errorf("bid read body = %s, want %s", reqs[0].body, want)
	}
	if want := `{"AccountId":1234567,"CampaignIds":[321],"CampaignType":"Search"}`; reqs[1].body != want {
		t.Errorf("budget read body = %s, want %s (unchanged)", reqs[1].body, want)
	}
}
