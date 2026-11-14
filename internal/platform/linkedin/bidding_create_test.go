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
	res, err := c.CreateCampaign(context.Background(), biddingCreateInput(4.5))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !res.BidApplied {
		t.Error("BidApplied = false on a run that created the campaign")
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
			id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", name, "555", ObjectiveWebsiteVisit, OptimizationTargetMaxClick)
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

// TestCreateCampaign_ReusedCampaignDoesNotApplyTheBid: a same-name campaign found in the group is
// reused with its existing bidding — no campaign POST, BidApplied false and the warning step, so
// the dispatcher records no bid.
func TestCreateCampaign_ReusedCampaignDoesNotApplyTheBid(t *testing.T) {
	const name = "Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | CNCF | MoFU"
	var mu sync.Mutex
	campaignPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "adCampaignGroups") {
				_, _ = io.WriteString(w, `{"elements":[{"name":"`+name+`","status":"PAUSED","id":"urn:li:sponsoredCampaign:777","campaignGroup":"urn:li:sponsoredCampaignGroup:100","objectiveType":"WEBSITE_CONVERSION"}],"metadata":{"nextPageToken":""}}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			campaignPosts++
			mu.Unlock()
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
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), biddingCreateInput(4.5))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	mu.Lock()
	posts := campaignPosts
	mu.Unlock()
	if res.CampaignID != "777" || posts != 0 {
		t.Fatalf("campaign = %q, campaign POSTs = %d; want the reused 777 and none", res.CampaignID, posts)
	}
	if res.BidApplied {
		t.Fatal("BidApplied = true on a run that reused the campaign")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "bid NOT applied to reused campaign 777") {
		t.Fatalf("steps %v lack the bid-not-applied warning", res.Steps)
	}
}

// conversionObjectiveServer answers a full CreateCampaign including the Campaign Conversions PUT,
// and records the campaign POST body and the order of every mutating request (#582).
func conversionObjectiveServer(t *testing.T) (*httptest.Server, func() map[string]any, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var campaignBody map[string]any
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			order = append(order, "group")
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			order = append(order, "campaign")
			_ = json.Unmarshal(raw, &campaignBody)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "campaignConversions"):
			order = append(order, "conversion")
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "posts"):
			order = append(order, "post")
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			order = append(order, "creative")
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv,
		func() map[string]any { mu.Lock(); defer mu.Unlock(); return campaignBody },
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), order...) }
}

// TestCreateCampaign_AutomaticConversionsIsWebsiteConversionMaxConversion pins #582's request:
// objective WEBSITE_CONVERSION with automatic bidding sends objectiveType WEBSITE_CONVERSION,
// optimizationTargetType MAX_CONVERSION and costType CPM ("Create and Manage Campaigns" row
// STANDARD_UPDATE | AUTO | MAX_CONVERSION), status PAUSED, NO unitCost, the Conversions name
// segment — and associates the conversion after the campaign POST, before anything else, which
// is the order LinkedIn documents ("created and then associated with a conversion before
// activating"). The bid lever's predicate must refuse it: it has no manual bid.
func TestCreateCampaign_AutomaticConversionsIsWebsiteConversionMaxConversion(t *testing.T) {
	srv, body, order := conversionObjectiveServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := biddingCreateInput(0)
	in.Objective = ObjectiveWebsiteConversion
	in.Conversions = []string{"104012"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	b := body()
	if b["objectiveType"] != ObjectiveWebsiteConversion || b["optimizationTargetType"] != OptimizationTargetMaxConversion || b["costType"] != "CPM" {
		t.Fatalf("automatic conversions campaign: objectiveType=%v optimizationTargetType=%v costType=%v, want WEBSITE_CONVERSION/MAX_CONVERSION/CPM",
			b["objectiveType"], b["optimizationTargetType"], b["costType"])
	}
	if b["status"] != "PAUSED" {
		t.Fatalf("status = %v, want PAUSED (a MAX_CONVERSION campaign is activated only after its conversion is associated)", b["status"])
	}
	if _, has := b["unitCost"]; has {
		t.Fatalf("an automatic campaign must not send unitCost: %v", b["unitCost"])
	}
	if !strings.Contains(res.CampaignName, "| LinkedIn | Conversions |") {
		t.Errorf("campaign name %q lacks the Conversions objective segment", res.CampaignName)
	}
	if !res.BidApplied || strings.Join(res.ConversionsAssociated, ",") != "104012" || len(res.ConversionsFailed) != 0 {
		t.Fatalf("BidApplied=%v associated=%v failed=%v, want true/[104012]/[]", res.BidApplied, res.ConversionsAssociated, res.ConversionsFailed)
	}
	if got := strings.Join(order(), ","); !strings.HasPrefix(got, "group,campaign,conversion,") {
		t.Fatalf("mutating order = %s, want the conversion associated right after the campaign", got)
	}
	costType, target := b["costType"].(string), b["optimizationTargetType"].(string)
	if (&CampaignSettings{CostType: &costType, OptimizationTargetType: &target}).ManualCPC() {
		t.Fatal("an automatic MAX_CONVERSION campaign must not read as a manual max CPC")
	}
}

// TestCreateCampaign_ObjectiveRefusalsSendNothing: MAX_CONVERSION with no conversion to associate
// (LinkedIn would never activate it), a manual bid under WEBSITE_VISIT, and an objective this
// service does not create are each refused before any POST.
func TestCreateCampaign_ObjectiveRefusalsSendNothing(t *testing.T) {
	for label, tc := range map[string]struct {
		bid       float64
		objective string
		want      string
	}{
		"automatic conversions without a conversion": {0, ObjectiveWebsiteConversion, "requires at least one conversion rule"},
		"manual traffic":    {4.5, ObjectiveWebsiteVisit, "manual max CPC bid is created under objective WEBSITE_CONVERSION only"},
		"unknown objective": {0, "LEAD_GENERATION", "is not one this service creates"},
	} {
		t.Run(label, func(t *testing.T) {
			srv, _, posts := biddingCreateServer(t)
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			in := biddingCreateInput(tc.bid)
			in.Objective = tc.objective
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if res != nil || posts() != 0 {
				t.Fatalf("result %+v, %d mutating requests; want nil and none", res, posts())
			}
		})
	}
	// The sentinel is what the conversion refusal carries.
	srv, _, _ := biddingCreateServer(t)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := biddingCreateInput(0)
	in.Objective = ObjectiveWebsiteConversion
	if _, err := c.CreateCampaign(context.Background(), in); !errors.Is(err, ErrAutomaticConversionsNeedConversion) {
		t.Fatalf("err = %v, want ErrAutomaticConversionsNeedConversion", err)
	}
}

// TestCampaignBidding_ObjectiveMatrix pins every combination campaignBidding offers against the
// "Ad Budget Type Allowable Combinations" rows (li-lms-2026-02): WEBSITE_VISIT allows MAX_CLICK and
// NONE, WEBSITE_CONVERSION allows MAX_CONVERSION and NONE; auto-bidding costType is CPM.
func TestCampaignBidding_ObjectiveMatrix(t *testing.T) {
	for _, tc := range []struct {
		bid                             float64
		objective                       string
		wantObjective, wantTarget, cost string
		wantName                        string
	}{
		{0, "", ObjectiveWebsiteVisit, OptimizationTargetMaxClick, "CPM", "Traffic"},
		{0, ObjectiveWebsiteVisit, ObjectiveWebsiteVisit, OptimizationTargetMaxClick, "CPM", "Traffic"},
		{0, ObjectiveWebsiteConversion, ObjectiveWebsiteConversion, OptimizationTargetMaxConversion, "CPM", "Conversions"},
		{4.5, "", ObjectiveWebsiteConversion, OptimizationTargetNone, "CPC", "Conversions"},
		{4.5, ObjectiveWebsiteConversion, ObjectiveWebsiteConversion, OptimizationTargetNone, "CPC", "Conversions"},
	} {
		plan, err := campaignBidding(tc.bid, tc.objective)
		if err != nil {
			t.Fatalf("campaignBidding(%v, %q): %v", tc.bid, tc.objective, err)
		}
		if plan.objectiveType != tc.wantObjective || plan.optimizationTarget != tc.wantTarget ||
			plan.fields["optimizationTargetType"] != tc.wantTarget || plan.fields["costType"] != tc.cost || plan.nameObjective != tc.wantName {
			t.Errorf("campaignBidding(%v, %q) = %+v, want %s/%s/%s/%s", tc.bid, tc.objective, plan, tc.wantObjective, tc.wantTarget, tc.cost, tc.wantName)
		}
	}
}

// TestFindCampaignByNameInGroup_RefusesABiddingModeMismatch (#582): automatic and manual Website
// conversions campaigns share a name, so a same-name, same-objective campaign is reused only in
// the same bidding MODE — manual (NONE, ENHANCED_CONVERSION) or automated — or when unreported.
func TestFindCampaignByNameInGroup_RefusesABiddingModeMismatch(t *testing.T) {
	const name = "Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | tlf | MoFU"
	for label, tc := range map[string]struct {
		want, reported string
		wantErr        bool
	}{
		"automatic reuses automatic":            {want: OptimizationTargetMaxConversion, reported: `,"optimizationTargetType":"MAX_CONVERSION"`},
		"automatic reuses unreported":           {want: OptimizationTargetMaxConversion, reported: ``},
		"automatic refuses manual NONE":         {want: OptimizationTargetMaxConversion, reported: `,"optimizationTargetType":"NONE"`, wantErr: true},
		"manual refuses automatic":              {want: OptimizationTargetNone, reported: `,"optimizationTargetType":"MAX_CONVERSION"`, wantErr: true},
		"manual reuses ENHANCED_CONVERSION":     {want: OptimizationTargetNone, reported: `,"optimizationTargetType":"ENHANCED_CONVERSION"`},
		"automatic refuses ENHANCED_CONVERSION": {want: OptimizationTargetMaxConversion, reported: `,"optimizationTargetType":"ENHANCED_CONVERSION"`, wantErr: true},
	} {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[{"name":"`+name+`","status":"PAUSED","id":"urn:li:sponsoredCampaign:200","campaignGroup":"urn:li:sponsoredCampaignGroup:555","objectiveType":"WEBSITE_CONVERSION"`+tc.reported+`}],"metadata":{"nextPageToken":""}}`)
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", name, "555", ObjectiveWebsiteConversion, tc.want)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "optimizationTargetType") {
					t.Fatalf("id=%q err=%v, want a bidding-mode abort", id, err)
				}
				return
			}
			if err != nil || id != "200" {
				t.Fatalf("id=%q err=%v, want 200", id, err)
			}
		})
	}
}
