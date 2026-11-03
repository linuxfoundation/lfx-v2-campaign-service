// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Demand Gen conversion bidding, campaign criteria and sitelinks (#474)
// ---------------------------------------------------------------------------

// parityCapture records every request path and body in order and answers each mutate
// with a plausible result. failPath, when set, is answered with failStatus instead.
type parityCapture struct {
	mu         sync.Mutex
	paths      []string
	bodies     map[string]string
	failPath   string
	failStatus int
}

func (p *parityCapture) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		path := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		p.mu.Lock()
		p.paths = append(p.paths, path)
		if p.bodies == nil {
			p.bodies = map[string]string{}
		}
		p.bodies[path] = string(raw)
		p.mu.Unlock()
		if path == p.failPath {
			w.WriteHeader(p.failStatus)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"boom"}}`)
			return
		}
		switch path {
		case "campaignBudgets:mutate":
			okBudget(w, r)
		case "campaigns:mutate":
			okCampaign(w, r)
		case "campaignCriteria:mutate":
			perOperationResults(func(i int) string {
				return "customers/1234567890/campaignCriteria/222~" + strconv.Itoa(700+i)
			})(w, r)
		case "assets:mutate":
			perOperationResults(func(i int) string {
				return "customers/1234567890/assets/" + strconv.Itoa(900+i)
			})(w, r)
		case "campaignAssets:mutate":
			perOperationResults(func(i int) string {
				return "customers/1234567890/campaignAssets/222~" + strconv.Itoa(900+i) + "~SITELINK"
			})(w, r)
		case "adGroups:mutate":
			okAdGroup(w, r)
		case "adGroupCriteria:mutate":
			perOperationResults(func(i int) string {
				return "customers/1234567890/adGroupCriteria/333~" + strconv.Itoa(800+i)
			})(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func parityInput() CampaignInput {
	in := demandGenInput()
	in.BiddingStrategy = biddingTargetCPA
	in.TargetCPA = 25
	in.Languages = []string{"EN"}
	in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17}}
	in.DeviceBidModifiers = []DeviceBidModifier{{Device: "CONNECTED_TV"}}
	in.Sitelinks = []Sitelink{{Text: "Register", FinalURL: "https://events.linuxfoundation.org/kubecon-eu-2026/register/"}}
	return in
}

// The whole #474 path in one create: the target CPA as the standalone `targetCpa`, the ad
// schedule and the CONNECTED_TV exclusion as CAMPAIGN criteria without the language (which
// stays on the ad group), and the sitelink created and linked to the campaign — each sent
// after the campaign and before the ad group, with its ids and step line on the result.
func TestCreateDemandGenCampaign_ConversionBiddingCriteriaAndSitelinks(t *testing.T) {
	cap := &parityCapture{}
	srv := demandGenTLSServer(t, cap.handler(t))
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), parityInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantOrder := "campaignBudgets:mutate,campaigns:mutate,campaignCriteria:mutate,assets:mutate,campaignAssets:mutate,adGroups:mutate,adGroupCriteria:mutate"
	if got := strings.Join(cap.paths, ","); got != wantOrder {
		t.Fatalf("request order = %s, want %s", got, wantOrder)
	}
	campaign := cap.bodies["campaigns:mutate"]
	if !strings.Contains(campaign, `"targetCpa":{"targetCpaMicros":25000000}`) || strings.Contains(campaign, "maximizeConversions") || strings.Contains(campaign, "targetSpend") {
		t.Errorf("campaign body %s, want exactly the standalone targetCpa strategy", campaign)
	}
	wantCriteria := `{"operations":[` +
		`{"create":{"campaign":"customers/1234567890/campaigns/222","adSchedule":{"dayOfWeek":"MONDAY","startHour":9,"startMinute":"ZERO","endHour":17,"endMinute":"ZERO"}}},` +
		`{"create":{"campaign":"customers/1234567890/campaigns/222","bidModifier":0,"device":{"type":"CONNECTED_TV"}}}]}`
	if got := cap.bodies["campaignCriteria:mutate"]; got != wantCriteria {
		t.Errorf("campaign criteria body\n got %s\nwant %s", got, wantCriteria)
	}
	if !strings.Contains(cap.bodies["assets:mutate"], `"sitelinkAsset":{"linkText":"Register"}`) {
		t.Errorf("assets body %s, want the sitelink", cap.bodies["assets:mutate"])
	}
	wantLink := `{"operations":[{"create":{"campaign":"customers/1234567890/campaigns/222","asset":"customers/1234567890/assets/900","fieldType":"SITELINK"}}]}`
	if got := cap.bodies["campaignAssets:mutate"]; got != wantLink {
		t.Errorf("link body %s, want %s", got, wantLink)
	}
	if !strings.Contains(cap.bodies["adGroupCriteria:mutate"], "languageConstants/1000") {
		t.Errorf("the language must stay on the ad group, got %s", cap.bodies["adGroupCriteria:mutate"])
	}

	if strings.Join(res.CampaignTargetingCriterionIDs, ",") != "700,701" {
		t.Errorf("CampaignTargetingCriterionIDs = %v, want [700 701]", res.CampaignTargetingCriterionIDs)
	}
	if strings.Join(res.TargetingCriterionIDs, ",") != "800" {
		t.Errorf("TargetingCriterionIDs = %v, want the ad group's language [800]", res.TargetingCriterionIDs)
	}
	if strings.Join(res.ExtensionAssetIDs, ",") != "900" || strings.Join(res.ExtensionLinkIDs, ",") != "900" {
		t.Errorf("extension ids = %v / %v, want [900] / [900]", res.ExtensionAssetIDs, res.ExtensionLinkIDs)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, want := range []string{
		"Campaign created: 222 (PAUSED, DEMAND_GEN, target-cpa, target CPA 25.00,",
		"Campaign targeting applied: 2 criteria (1 ad schedule(s); 1 device bid modifier(s))",
		"Ad extensions applied: 1 assets (1 sitelinks)",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps missing %q:\n%s", want, steps)
		}
	}
}

// Without schedules, devices or sitelinks the cascade sends neither new mutate: a Demand
// Gen create with languages alone is the #459 request sequence, unchanged.
func TestCreateDemandGenCampaign_NoCampaignCriteriaWhenOnlyLanguages(t *testing.T) {
	cap := &parityCapture{}
	srv := demandGenTLSServer(t, cap.handler(t))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.Languages = []string{"EN"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(cap.paths, ","); got != "campaignBudgets:mutate,campaigns:mutate,adGroups:mutate,adGroupCriteria:mutate" {
		t.Errorf("request order = %s", got)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Campaign created: 222 (PAUSED, DEMAND_GEN, maximize-clicks,") {
		t.Errorf("the default step must name maximize clicks: %v", res.Steps)
	}
}

// Each new step's failure is a PARTIAL result: the campaign exists, the error says what is
// missing, an ambiguous (5xx) outcome is UNCONFIRMED, nothing past the failed step is sent,
// and what the earlier step created is still on the result.
func TestCreateDemandGenCampaign_NewStepsFailAsPartials(t *testing.T) {
	cases := []struct {
		name     string
		failPath string
		status   int
		wantErr  string
		check    func(t *testing.T, res *CampaignResult)
	}{
		{"criteria refused", "campaignCriteria:mutate", http.StatusBadRequest, "it has NO language, schedule, device or demographic criteria", nil},
		{"criteria ambiguous", "campaignCriteria:mutate", http.StatusInternalServerError, "campaign targeting criteria UNCONFIRMED", nil},
		{"sitelink assets ambiguous", "assets:mutate", http.StatusInternalServerError, "ad extension assets UNCONFIRMED", func(t *testing.T, res *CampaignResult) {
			if strings.Join(res.CampaignTargetingCriterionIDs, ",") != "700,701" {
				t.Errorf("the criteria created before the failure were lost: %v", res.CampaignTargetingCriterionIDs)
			}
		}},
		{"sitelink link refused", "campaignAssets:mutate", http.StatusBadRequest, "ad extension linking failed", func(t *testing.T, res *CampaignResult) {
			if strings.Join(res.ExtensionAssetIDs, ",") != "900" || len(res.ExtensionLinkIDs) != 0 {
				t.Errorf("unlinked assets must be reported: assets %v links %v", res.ExtensionAssetIDs, res.ExtensionLinkIDs)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &parityCapture{failPath: tc.failPath, failStatus: tc.status}
			srv := demandGenTLSServer(t, cap.handler(t))
			c := demandGenClient(t, srv)
			res, err := c.CreateDemandGenCampaign(context.Background(), parityInput())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if res == nil || res.CampaignID != "222" || res.CampaignBudgetID != "111" {
				t.Fatalf("partial = %+v, want the campaign and budget", res)
			}
			for _, p := range cap.paths {
				if p == "adGroups:mutate" {
					t.Errorf("the ad group was created after the failed step")
				}
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), "Campaign created: 222") {
				t.Errorf("partial lost its steps: %v", res.Steps)
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Several ads per Demand Gen ad group (#474)
// ---------------------------------------------------------------------------

// severalAdsInput gives the single group two video ads and an image ad.
func severalAdsInput(base string) CampaignInput {
	in := demandGenInput()
	second := fullVideoCreative(base)
	second.YouTubeVideoIDs = []string{"abcdefghijk"}
	in.DemandGenAds = []DemandGenAd{
		{Name: "Cut A", VideoCreative: fullVideoCreative(base)},
		{Name: "Cut B", VideoCreative: second},
		{Name: "Static", Creative: creativeAt(base)},
	}
	return in
}

// numberedVideoAds answers each googleAds:mutate as okGoogleAdsMutate does, but numbers the
// ad by call (444, 445, …) so two ads in one group carry distinct ids; failCall, when not
// negative, is answered 503 instead.
func numberedVideoAds(failCall int) func(call int, body []byte, w http.ResponseWriter) {
	return func(call int, body []byte, w http.ResponseWriter) {
		if call == failCall {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rec := httptest.NewRecorder()
		okGoogleAdsMutate(body, rec)
		_, _ = io.WriteString(w, strings.ReplaceAll(rec.Body.String(), "~444", "~"+strconv.Itoa(444+call)))
	}
}

// Each ad is its own request, in order, under the one ad group: the video ads by one
// googleAds:mutate each, named "<ad group name> - <ad name>", and the image ad by its
// assets:mutate and adGroupAds:mutate. Every ad id lands in AdGroups (so launch flips all
// of them), the scalar AdID is the first, and each step line names its ad.
func TestCreateDemandGenCampaign_SeveralAdsInTheSingleGroup(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"googleAds:mutate": numberedVideoAds(-1),
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), severalAdsInput(base))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(rig.sent("adGroups:mutate")); n != 1 {
		t.Fatalf("sent %d ad group mutates, want 1", n)
	}
	videos := rig.sent("googleAds:mutate")
	if len(videos) != 2 {
		t.Fatalf("sent %d video ad mutates, want 2", len(videos))
	}
	for i, want := range []string{`"name":"KubeCon Europe 2026 - Display - Cut A"`, `"name":"KubeCon Europe 2026 - Display - Cut B"`} {
		if !strings.Contains(string(videos[i]), want) {
			t.Errorf("video ad %d body %s, want %s", i+1, videos[i], want)
		}
	}
	if !strings.Contains(string(videos[1]), "abcdefghijk") {
		t.Errorf("the second video ad must carry its own video: %s", videos[1])
	}
	if n := len(rig.sent("adGroupAds:mutate")); n != 1 {
		t.Errorf("sent %d image ad mutates, want 1", n)
	}
	if res.AdID != "444" || len(res.AdGroups) != 1 || strings.Join(res.AdGroups[0].AdIDs, ",") != "444,445,555" || res.AdGroups[0].ID != "333" {
		t.Errorf("AdID = %q AdGroups = %+v, want 444 and one group with ads 444,445,555", res.AdID, res.AdGroups)
	}
	if len(res.CreativeAssetIDs) == 0 || strings.Join(res.CreativeAssetIDs, ",") != strings.Join(res.AdGroups[0].CreativeAssetIDs, ",") {
		t.Errorf("CreativeAssetIDs %v / group %v, want every ad's assets on both", res.CreativeAssetIDs, res.AdGroups[0].CreativeAssetIDs)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, want := range []string{`Demand Gen video ad created: 444 (PAUSED,`, `[ad "Cut A"]`, `[ad "Cut B"]`, `Demand Gen ad created: 555 (PAUSED,`, `[ad "Static"]`} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps missing %q:\n%s", want, steps)
		}
	}
}

// A failure on the second ad returns the first ad complete, the second's logo asset, and
// an UNCONFIRMED error naming the second ad; the third is never sent.
func TestCreateDemandGenCampaign_SeveralAdsPartialOnTheSecond(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"googleAds:mutate": numberedVideoAds(1),
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), severalAdsInput(base))
	if err == nil || !strings.Contains(err.Error(), `video ad "KubeCon Europe 2026 - Display - Cut B" creation UNCONFIRMED`) {
		t.Fatalf("err = %v, want the second ad UNCONFIRMED", err)
	}
	if res == nil || res.AdID != "444" || len(res.AdGroups) != 1 || strings.Join(res.AdGroups[0].AdIDs, ",") != "444" {
		t.Fatalf("res = %+v, want the first ad complete", res)
	}
	if len(res.AdGroups[0].CreativeAssetIDs) < 2 {
		t.Errorf("the second ad's logo asset was lost: %v", res.AdGroups[0].CreativeAssetIDs)
	}
	if n := len(rig.sent("adGroupAds:mutate")); n != 0 {
		t.Errorf("the third ad was sent after the second failed")
	}
}

// Every refusal of the ad lists lands in the preflight.
func TestValidateDemandGenAds_Refusals(t *testing.T) {
	ad := func(name string) DemandGenAd { return DemandGenAd{Name: name, Creative: fullCreative()} }
	cases := map[string]struct {
		kind    string
		mutate  func(in *CampaignInput)
		wantSub string
	}{
		"off demand gen": {campaignKindSearch, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("A")}
		}, "Demand Gen ads are not supported on Search Campaign"},
		"beside the image creative": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("A")}
			in.DemandGenCreative = fullCreative()
		}, "DemandGenAds replaces the single Demand Gen ad"},
		"beside the video creative": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("A")}
			in.DemandGenVideoCreative = fullVideoCreative("https://cdn.example.org")
		}, "DemandGenAds replaces the single Demand Gen ad"},
		"beside ad groups": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("A")}
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "G", Creative: fullCreative()}}
		}, "DemandGenAds and DemandGenAudience must be empty"},
		"too many": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("A"), ad("B"), ad("C"), ad("D"), ad("E"), ad("F")}
		}, "names 6 Demand Gen ads, limit 5"},
		"no name": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad(" | ")}
		}, "has no name"},
		"duplicate names": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{ad("Cut"), ad("cut")}
		}, "is used more than once"},
		"both creatives": {campaignKindDemandGen, func(in *CampaignInput) {
			a := ad("A")
			a.VideoCreative = fullVideoCreative("https://cdn.example.org")
			in.DemandGenAds = []DemandGenAd{a}
		}, "sets both a Demand Gen image creative and a Demand Gen video creative"},
		"no creative": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{{Name: "A"}}
		}, "has no creative"},
		"video ad name over 255": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAds = []DemandGenAd{{Name: strings.Repeat("v", 230), VideoCreative: fullVideoCreative("https://cdn.example.org")}}
		}, "video ad name"},
		"group ads beside the group creative": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "G", Creative: fullCreative(), Ads: []DemandGenAd{ad("A")}}}
		}, "sets Ads beside Creative or VideoCreative"},
		"group ads duplicate": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "G", Ads: []DemandGenAd{ad("A"), ad("a")}}}
		}, `in Demand Gen ad group "KubeCon Europe 2026 - Display | G"`},
	}
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			tc.mutate(&in)
			_, err := c.preflightCampaignKind(tc.kind, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
	// The 255 boundary: "KubeCon Europe 2026 - Display - " is 32 characters, so a 223-rune
	// label composes exactly 255 and passes, and an image ad's longer name is not sent.
	in := demandGenInput()
	in.DemandGenAds = []DemandGenAd{
		{Name: strings.Repeat("v", 223), VideoCreative: fullVideoCreative("https://cdn.example.org")},
		{Name: strings.Repeat("i", 240), Creative: fullCreative()},
	}
	if _, err := c.preflightCampaignKind(campaignKindDemandGen, in); err != nil {
		t.Errorf("a 255-character video ad name and a long image ad label must pass: %v", err)
	}
}

// A named group takes Ads the way the single group takes DemandGenAds: one plan per ad,
// each named on the GROUP's name.
func TestValidateDemandGenAdGroups_GroupAds(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	in := demandGenInput()
	in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "G", Ads: []DemandGenAd{
		{Name: "One", Creative: fullCreative()},
		{Name: "Two", VideoCreative: fullVideoCreative("https://cdn.example.org")},
	}}}
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	ads := pf.demandGenGroups[0].ads
	if len(ads) != 2 || ads[0].label != "One" || !ads[0].creative.present || ads[1].adName != "KubeCon Europe 2026 - Display | G - Two" || !ads[1].video.present {
		t.Errorf("ads = %+v", ads)
	}
}

// A context that dies between ads stops before the next ad's request, with the earlier
// ads reported.
func TestCreateDemandGenCampaign_ContextDoneBetweenAds(t *testing.T) {
	var flag atomic.Bool
	ads := numberedVideoAds(-1)
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"googleAds:mutate": func(call int, body []byte, w http.ResponseWriter) {
			flag.Store(true)
			ads(call, body, w)
		},
	})
	res, err := c.CreateDemandGenCampaign(errAfterFlagCtx{Context: context.Background(), flag: &flag}, severalAdsInput(base))
	if err == nil || !strings.Contains(err.Error(), `aborted after 1 of 3 ad(s) were created in ad group 333`) {
		t.Fatalf("err = %v, want the between-ads abort", err)
	}
	if res == nil || len(res.AdGroups) != 1 || strings.Join(res.AdGroups[0].AdIDs, ",") != "444" {
		t.Errorf("res = %+v, want the first ad only", res)
	}
	if n := len(rig.sent("googleAds:mutate")); n != 1 {
		t.Errorf("sent %d video ad mutates, want only the first", n)
	}
}
