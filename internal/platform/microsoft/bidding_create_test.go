// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// msBodyCapture records the campaign and ad group create bodies under a mutex: the handler runs on
// the server's goroutine, so the test reads them only through the locked accessors.
type msBodyCapture struct {
	mu                   sync.Mutex
	campaign, adGroup    []byte
	campaignPosts, posts int
}

func (c *msBodyCapture) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		c.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/Campaigns"):
			c.campaign = body
			c.campaignPosts++
		case strings.HasSuffix(r.URL.Path, "/AdGroups"):
			c.adGroup = body
		}
		c.posts++
		c.mu.Unlock()
		next(w, r)
	}
}

func (c *msBodyCapture) campaignReq(t *testing.T) createCampaignsRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var req createCampaignsRequest
	if err := json.Unmarshal(c.campaign, &req); err != nil {
		t.Fatalf("decode campaign body %q: %v", c.campaign, err)
	}
	return req
}

func (c *msBodyCapture) adGroupReq(t *testing.T) createAdGroupsRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var req createAdGroupsRequest
	if err := json.Unmarshal(c.adGroup, &req); err != nil {
		t.Fatalf("decode ad group body %q: %v", c.adGroup, err)
	}
	return req
}

func (c *msBodyCapture) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts
}

func (c *msBodyCapture) campaignCreates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.campaignPosts
}

// TestCreateCampaign_AutomaticBiddingSendsMaxClicksAndNoAdGroupBid pins the automatic request
// (LFXV2-2665): the campaign carries BiddingScheme {"Type":"MaxClicks"} and the ad group no
// CpcBid — a bid MaxClicks would ignore. The scheme MaxClicks is not one the bid lever accepts.
func TestCreateCampaign_AutomaticBiddingSendsMaxClicksAndNoAdGroupBid(t *testing.T) {
	cap := &msBodyCapture{}
	c := newAPIClient(t, cap.wrap((&campaignsAPI{}).handler(t)))
	in := validInput()
	in.BiddingScheme = BidSchemeMaxClicks
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if bs := cap.campaignReq(t).Campaigns[0].BiddingScheme; bs == nil || bs.Type != BidSchemeMaxClicks {
		t.Fatalf("campaign BiddingScheme = %+v, want MaxClicks", bs)
	}
	if ag := cap.adGroupReq(t).AdGroups; len(ag) != 1 || ag[0].CpcBid != nil {
		t.Fatalf("ad group = %+v, want one with no CpcBid", ag)
	}
	if !res.BidApplied {
		t.Error("BidApplied = false on a run that created the campaign and ad group")
	}
	if (&CampaignBidStrategy{SchemeType: BidSchemeMaxClicks}).UsesAdGroupBid() {
		t.Fatal("MaxClicks must not read as a strategy that uses the ad group bid")
	}
}

// TestCreateCampaign_ManualBiddingSendsEnhancedCpcWithTheAdGroupBid pins the manual request:
// BiddingScheme EnhancedCpc (ManualCpc is silently converted to EnhancedCpc on a Search
// campaign) and the ad group CpcBid = the bid — the scheme the bid lever accepts.
func TestCreateCampaign_ManualBiddingSendsEnhancedCpcWithTheAdGroupBid(t *testing.T) {
	cap := &msBodyCapture{}
	c := newAPIClient(t, cap.wrap((&campaignsAPI{}).handler(t)))
	in := validInput()
	in.BiddingScheme = BidSchemeEnhancedCpc
	in.CpcBid = 1.25
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	bs := cap.campaignReq(t).Campaigns[0].BiddingScheme
	if bs == nil || bs.Type != BidSchemeEnhancedCpc {
		t.Fatalf("campaign BiddingScheme = %+v, want EnhancedCpc", bs)
	}
	if ag := cap.adGroupReq(t).AdGroups; len(ag) != 1 || ag[0].CpcBid == nil || ag[0].CpcBid.Amount != 1.25 {
		t.Fatalf("ad group = %+v, want CpcBid 1.25", ag)
	}
	if !(&CampaignBidStrategy{SchemeType: bs.Type}).UsesAdGroupBid() {
		t.Fatal("the bid lever must accept the scheme a manual create sends")
	}
	if !res.BidApplied || !strings.Contains(strings.Join(res.Steps, "\n"), "Campaign bid strategy: EnhancedCpc") {
		t.Errorf("BidApplied=%v steps=%v, want true and the bid strategy step", res.BidApplied, res.Steps)
	}
}

// TestCreateCampaign_ReusedCampaignOrAdGroupDoesNotApplyTheBid: a campaign found by name (or the
// duplicate-name self-heal), or an ad group found by name, keeps its existing bidding. The result
// must say so — BidApplied false and the warning step — so the dispatcher records no bid.
func TestCreateCampaign_ReusedCampaignOrAdGroupDoesNotApplyTheBid(t *testing.T) {
	in := validInput()
	in.BiddingScheme, in.CpcBid = BidSchemeEnhancedCpc, 1.25
	name := composeName(in)
	cases := map[string]struct {
		api  *campaignsAPI
		want string
	}{
		"campaign found by name": {
			api:  &campaignsAPI{getBody: `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`},
			want: "bid NOT applied to reused campaign 999",
		},
		"ad group found by name": {
			api:  &campaignsAPI{adGroupGetBody: `{"AdGroups":[{"Id":654,"Name":` + jsonString(composeAdGroupName(in)) + `}]}`},
			want: "bid NOT applied to reused ad group 654",
		},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			cap := &msBodyCapture{}
			c := newAPIClient(t, cap.wrap(tc.api.handler(t)))
			res, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if res.BidApplied {
				t.Fatal("BidApplied = true on a run that reused a bid-carrying entity")
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), tc.want) {
				t.Fatalf("steps %v lack the warning %q", res.Steps, tc.want)
			}
		})
	}
}

// TestCreateCampaign_OmittedSchemeKeepsTheLegacyBody: "" sends no BiddingScheme at all, the body
// this client sent before LFXV2-2665.
func TestCreateCampaign_OmittedSchemeKeepsTheLegacyBody(t *testing.T) {
	cap := &msBodyCapture{}
	c := newAPIClient(t, cap.wrap((&campaignsAPI{}).handler(t)))
	if _, err := c.CreateCampaign(context.Background(), validInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	cap.mu.Lock()
	raw := string(cap.campaign)
	cap.mu.Unlock()
	if strings.Contains(raw, "BiddingScheme") {
		t.Fatalf("an omitted scheme must not be sent: %s", raw)
	}
}

// TestCreateCampaign_BiddingSchemeConflictsAreRefusedBeforeAnyRequest: MaxClicks with a CpcBid
// (an ignored bid), EnhancedCpc without one (a bid nobody chose) and an unknown scheme are all
// refused with nothing sent.
func TestCreateCampaign_BiddingSchemeConflictsAreRefusedBeforeAnyRequest(t *testing.T) {
	cases := map[string]func(*CampaignInput){
		"maxclicks with a bid": func(in *CampaignInput) { in.BiddingScheme, in.CpcBid = BidSchemeMaxClicks, 2 },
		"enhanced without bid": func(in *CampaignInput) { in.BiddingScheme = BidSchemeEnhancedCpc },
		"unknown scheme":       func(in *CampaignInput) { in.BiddingScheme = "TargetCpa" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cap := &msBodyCapture{}
			c := newAPIClient(t, cap.wrap((&campaignsAPI{}).handler(t)))
			in := validInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), "bid strategy invalid") {
				t.Fatalf("err = %v, want a bid strategy refusal", err)
			}
			if res != nil || cap.requestCount() != 0 || cap.campaignCreates() != 0 {
				t.Fatalf("result=%+v requests=%d, want nil and 0 (nothing created)", res, cap.requestCount())
			}
		})
	}
}
