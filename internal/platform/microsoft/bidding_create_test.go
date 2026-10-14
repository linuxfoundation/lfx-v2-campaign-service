// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCreateCampaign_AutomaticBiddingSendsMaxClicksAndNoAdGroupBid pins the automatic request
// (LFXV2-2665): the campaign carries BiddingScheme {"Type":"MaxClicks"} and the ad group no
// CpcBid — a bid MaxClicks would ignore. The scheme MaxClicks is not one the bid lever accepts.
func TestCreateCampaign_AutomaticBiddingSendsMaxClicksAndNoAdGroupBid(t *testing.T) {
	var seen createCampaignsRequest
	var adGroup createAdGroupsRequest
	api := &campaignsAPI{postSeen: &seen, adGroupSeen: &adGroup}
	c := newAPIClient(t, api.handler(t))
	in := validInput()
	in.BiddingScheme = BidSchemeMaxClicks
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if bs := seen.Campaigns[0].BiddingScheme; bs == nil || bs.Type != BidSchemeMaxClicks {
		t.Fatalf("campaign BiddingScheme = %+v, want MaxClicks", bs)
	}
	if len(adGroup.AdGroups) != 1 || adGroup.AdGroups[0].CpcBid != nil {
		t.Fatalf("ad group = %+v, want one with no CpcBid", adGroup.AdGroups)
	}
	if (&CampaignBidStrategy{SchemeType: BidSchemeMaxClicks}).UsesAdGroupBid() {
		t.Fatal("MaxClicks must not read as a strategy that uses the ad group bid")
	}
}

// TestCreateCampaign_ManualBiddingSendsEnhancedCpcWithTheAdGroupBid pins the manual request:
// BiddingScheme EnhancedCpc (ManualCpc is silently converted to EnhancedCpc on a Search
// campaign) and the ad group CpcBid = the bid — the scheme the bid lever accepts.
func TestCreateCampaign_ManualBiddingSendsEnhancedCpcWithTheAdGroupBid(t *testing.T) {
	var seen createCampaignsRequest
	var adGroup createAdGroupsRequest
	api := &campaignsAPI{postSeen: &seen, adGroupSeen: &adGroup}
	c := newAPIClient(t, api.handler(t))
	in := validInput()
	in.BiddingScheme = BidSchemeEnhancedCpc
	in.CpcBid = 1.25
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	bs := seen.Campaigns[0].BiddingScheme
	if bs == nil || bs.Type != BidSchemeEnhancedCpc {
		t.Fatalf("campaign BiddingScheme = %+v, want EnhancedCpc", bs)
	}
	if len(adGroup.AdGroups) != 1 || adGroup.AdGroups[0].CpcBid == nil || adGroup.AdGroups[0].CpcBid.Amount != 1.25 {
		t.Fatalf("ad group = %+v, want CpcBid 1.25", adGroup.AdGroups)
	}
	if !(&CampaignBidStrategy{SchemeType: bs.Type}).UsesAdGroupBid() {
		t.Fatal("the bid lever must accept the scheme a manual create sends")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Campaign bid strategy: EnhancedCpc") {
		t.Errorf("steps do not record the bid strategy: %v", res.Steps)
	}
}

// TestCreateCampaign_OmittedSchemeKeepsTheLegacyBody: "" sends no BiddingScheme at all, the body
// this client sent before LFXV2-2665.
func TestCreateCampaign_OmittedSchemeKeepsTheLegacyBody(t *testing.T) {
	var seen createCampaignsRequest
	api := &campaignsAPI{postSeen: &seen}
	c := newAPIClient(t, api.handler(t))
	if _, err := c.CreateCampaign(context.Background(), validInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	raw, _ := json.Marshal(seen.Campaigns[0])
	if strings.Contains(string(raw), "BiddingScheme") {
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
			api := &campaignsAPI{}
			c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.Path)
				api.handler(t)(w, r)
			})
			in := validInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), "bid strategy invalid") {
				t.Fatalf("err = %v, want a bid strategy refusal", err)
			}
			if res != nil {
				t.Fatalf("result = %+v, want nil (nothing created)", res)
			}
		})
	}
}
