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

// biddingCreateServer answers a full CreateCampaign and captures the campaign POST body. posts
// counts every mutating request, so a refusal can prove nothing was sent.
func biddingCreateServer(t *testing.T) (*httptest.Server, func() map[string]any, func() int) {
	t.Helper()
	var mu sync.Mutex
	var campaignBody map[string]any
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
		func() map[string]any { mu.Lock(); defer mu.Unlock(); return campaignBody },
		func() int { mu.Lock(); defer mu.Unlock(); return posts }
}

func biddingCreateInput(manualBid float64) CampaignInput {
	return CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "Join us", Headline: "KubeCon 2099"}},
		Project:          "CNCF",
		ManualCPCBid:     manualBid,
	}
}

// TestCreateCampaign_ManualBidSendsTheShapeTheBidLeverAccepts pins the manual request: costType
// CPC, optimizationTargetType NONE, unitCost = the bid in USD at two decimals. That is exactly
// what CampaignSettings.ManualCPC reads as a manual max CPC, which the test also asserts, so a
// campaign created manual is one the bid lever will re-bid.
func TestCreateCampaign_ManualBidSendsTheShapeTheBidLeverAccepts(t *testing.T) {
	srv, body, _ := biddingCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.CreateCampaign(context.Background(), biddingCreateInput(4.5)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	b := body()
	if b["costType"] != "CPC" || b["optimizationTargetType"] != "NONE" || b["objectiveType"] != ObjectiveWebsiteConversion {
		t.Fatalf("manual campaign: costType=%v optimizationTargetType=%v objectiveType=%v, want CPC/NONE/WEBSITE_CONVERSION",
			b["costType"], b["optimizationTargetType"], b["objectiveType"])
	}
	unit, ok := b["unitCost"].(map[string]any)
	if !ok || unit["amount"] != "4.50" || unit["currencyCode"] != "USD" {
		t.Fatalf("manual campaign unitCost = %v, want {amount 4.50, currencyCode USD}", b["unitCost"])
	}
	// The bid lever's own predicate accepts what was sent.
	costType, target := b["costType"].(string), b["optimizationTargetType"].(string)
	if !(&CampaignSettings{CostType: &costType, OptimizationTargetType: &target}).ManualCPC() {
		t.Fatal("the bid lever's ManualCPC refuses the setup a manual create sends")
	}
}

// TestCreateCampaign_AutomaticBiddingIsWebsiteVisitMaxClick pins the automatic request (decision
// 2026-10-13, Misha): objectiveType WEBSITE_VISIT, optimizationTargetType MAX_CLICK, costType CPM
// (the "Create and Manage Campaigns" Website Traffic row STANDARD_UPDATE | AUTO | MAX_CLICK), NO
// unitCost, and the name's Objective segment "Traffic" — and that the bid lever's own predicate
// refuses it as not a manual max CPC.
func TestCreateCampaign_AutomaticBiddingIsWebsiteVisitMaxClick(t *testing.T) {
	srv, body, _ := biddingCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), biddingCreateInput(0))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	b := body()
	if b["objectiveType"] != ObjectiveWebsiteVisit || b["optimizationTargetType"] != OptimizationTargetMaxClick || b["costType"] != "CPM" {
		t.Fatalf("automatic campaign: objectiveType=%v optimizationTargetType=%v costType=%v, want WEBSITE_VISIT/MAX_CLICK/CPM",
			b["objectiveType"], b["optimizationTargetType"], b["costType"])
	}
	if _, has := b["unitCost"]; has {
		t.Fatalf("an automatic campaign must not send unitCost: %v", b["unitCost"])
	}
	if !strings.Contains(res.CampaignName, "| LinkedIn | Traffic |") {
		t.Errorf("campaign name %q lacks the Traffic objective segment", res.CampaignName)
	}
	costType, target := b["costType"].(string), b["optimizationTargetType"].(string)
	if (&CampaignSettings{CostType: &costType, OptimizationTargetType: &target}).ManualCPC() {
		t.Fatal("an automatic MAX_CLICK campaign must not read as a manual max CPC")
	}
}

// TestCreateCampaign_SubCentManualBidIsRefusedBeforeAnyPost: a bid that is not whole cents is
// refused, never rounded, and nothing — not even the campaign group — is created.
func TestCreateCampaign_SubCentManualBidIsRefusedBeforeAnyPost(t *testing.T) {
	srv, _, posts := biddingCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), biddingCreateInput(2.345))
	if err == nil || !errors.Is(err, ErrBidAmountInvalid) {
		t.Fatalf("err = %v, want ErrBidAmountInvalid", err)
	}
	if res != nil {
		t.Fatalf("a pre-create refusal must return a nil result, got %+v", res)
	}
	if n := posts(); n != 0 {
		t.Fatalf("%d mutating requests were sent for a refused bid, want 0", n)
	}
}

// TestFindCampaignByNameInGroup_RefusesAnObjectiveMismatch: a same-name campaign in the same group
// is reused only when its objectiveType is the one this bidding strategy creates (or is
// unreported); a reported mismatch aborts rather than reuse it or create a duplicate beside it.
func TestFindCampaignByNameInGroup_RefusesAnObjectiveMismatch(t *testing.T) {
	const name = "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | tlf | MoFU"
	for label, tc := range map[string]struct {
		reported string
		wantID   string
		wantErr  bool
	}{
		"same objective is reused":       {reported: `,"objectiveType":"WEBSITE_VISIT"`, wantID: "200"},
		"unreported objective is reused": {reported: ``, wantID: "200"},
		"different objective aborts":     {reported: `,"objectiveType":"WEBSITE_CONVERSION"`, wantErr: true},
	} {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[{"name":"`+name+`","status":"PAUSED","id":"urn:li:sponsoredCampaign:200","campaignGroup":"urn:li:sponsoredCampaignGroup:555"`+tc.reported+`}],"metadata":{"nextPageToken":""}}`)
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", name, "555", ObjectiveWebsiteVisit)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "objectiveType WEBSITE_CONVERSION, not WEBSITE_VISIT") {
					t.Fatalf("id=%q err=%v, want an objective-mismatch abort", id, err)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("id=%q err=%v, want %q", id, err, tc.wantID)
			}
		})
	}
}
