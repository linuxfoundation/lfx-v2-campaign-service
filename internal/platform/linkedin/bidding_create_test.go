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

// testManualBid is the manual bid the other LinkedIn create tests send: LinkedIn creates refuse
// automatic bidding (LFXV2-2665), so every create that is meant to reach the API names one.
const testManualBid = 1.0

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
	if b["costType"] != "CPC" || b["optimizationTargetType"] != "NONE" {
		t.Fatalf("manual campaign: costType=%v optimizationTargetType=%v, want CPC/NONE", b["costType"], b["optimizationTargetType"])
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

// TestCreateCampaign_AutomaticBiddingIsRefusedBeforeAnyPost: the objective this client sends,
// WEBSITE_CONVERSION, has one auto-bidding target, MAX_CONVERSION, which LinkedIn will not activate
// without a conversion association this service never makes. So a create with no manual bid is
// refused with nothing created — not even the campaign group — rather than leaving a campaign that
// cannot go live.
func TestCreateCampaign_AutomaticBiddingIsRefusedBeforeAnyPost(t *testing.T) {
	srv, _, posts := biddingCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), biddingCreateInput(0))
	if !errors.Is(err, ErrAutomaticBiddingUnavailable) {
		t.Fatalf("err = %v, want ErrAutomaticBiddingUnavailable", err)
	}
	if res != nil || posts() != 0 {
		t.Fatalf("result=%+v posts=%d, want nil and 0", res, posts())
	}
	// And the bid lever keeps refusing an auto-bid campaign (one an operator switched, or one
	// from before this change): MAX_CONVERSION/CPM is not a manual max CPC.
	cpm, maxConv := "CPM", "MAX_CONVERSION"
	if (&CampaignSettings{CostType: &cpm, OptimizationTargetType: &maxConv}).ManualCPC() {
		t.Fatal("an auto-bid campaign must not read as a manual max CPC")
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
