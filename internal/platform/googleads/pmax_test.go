// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the Performance Max cascade
// ---------------------------------------------------------------------------

// pmaxCreativeAt is the minimum asset group pointed at a test server: three
// headlines, one long headline, two descriptions, one marketing image, one square
// marketing image, one logo and a business name. Deliberately the MINIMUM, so a
// cascade test that counts assets counts a number the validator's bounds pin.
func pmaxCreativeAt(base string) PerformanceMaxCreative {
	return PerformanceMaxCreative{
		MarketingImages:       []string{base + "/m.png"},
		SquareMarketingImages: []string{base + "/sq.png"},
		LogoImages:            []string{base + "/logo.png"},
		Headlines:             []string{"Join us at KubeCon", "Three days in Amsterdam", "Meet the maintainers"},
		LongHeadlines:         []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		Descriptions:          []string{"Talks, workshops and hallway track.", "Register now and save."},
		BusinessName:          "Linux Foundation",
	}
}

// pmaxImages is the trio pmaxCreativeAt asks for, each at a size that satisfies
// its slot's ratio and minimum.
func pmaxImages(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"/m.png":    pngOf(t, 1200, 628),
		"/sq.png":   pngOf(t, 400, 400),
		"/logo.png": pngOf(t, 256, 256),
	}
}

// pmaxAssetCount is what pmaxCreativeAt builds: 3 headlines + 1 long headline +
// 2 descriptions + 1 business name + 3 images.
const pmaxAssetCount = 10

// pmaxCascade routes the two campaign-shell mutates to their happy handlers and
// leaves the three asset-group mutates to the caller, so each test varies only the
// part it is about. Any ad-group or ad path is a FAILURE rather than a default:
// Performance Max has neither, and a cascade that grew one would otherwise pass.
func pmaxCascade(t *testing.T, images map[string][]byte, assetH, groupH, linkH http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "assetGroupAssets:mutate"):
			linkH(w, r)
		case strings.HasSuffix(r.URL.Path, "assetGroups:mutate"):
			groupH(w, r)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetH(w, r)
		default:
			t.Errorf("unexpected path: %s — Performance Max has no ad group and no ad", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func okPMaxAssets() (http.HandlerFunc, func() string) {
	return capturedMutate(pmaxAssetCount, func(i int) string {
		return "customers/1234567890/assets/" + []string{"900", "901", "902", "903", "904", "905", "906", "907", "908", "909"}[i]
	})
}

func okPMaxGroup() (http.HandlerFunc, func() string) {
	return capturedMutate(1, func(int) string { return "customers/1234567890/assetGroups/555" })
}

func okPMaxLinks() (http.HandlerFunc, func() string) {
	return capturedMutate(pmaxAssetCount, func(i int) string {
		return "customers/1234567890/assetGroupAssets/555~" + []string{"900", "901", "902", "903", "904", "905", "906", "907", "908", "909"}[i] + "~HEADLINE"
	})
}

func TestCreatePerformanceMaxCampaign_HappyPath(t *testing.T) {
	assetH, readAssets := okPMaxAssets()
	groupH, readGroup := okPMaxGroup()
	linkH, readLinks := okPMaxLinks()
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH, linkH))
	c := demandGenClient(t, srv)

	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result on success")
	}
	if res.CampaignID == "" || res.CampaignBudgetID == "" {
		t.Errorf("campaign %q / budget %q — both must be set", res.CampaignID, res.CampaignBudgetID)
	}
	if res.AssetGroupID != "555" {
		t.Errorf("AssetGroupID = %q, want 555", res.AssetGroupID)
	}
	if len(res.CreativeAssetIDs) != pmaxAssetCount {
		t.Errorf("CreativeAssetIDs = %d, want %d", len(res.CreativeAssetIDs), pmaxAssetCount)
	}
	// Performance Max has no ad group and no ad, so neither id may be invented.
	if res.AdGroupID != "" || res.AdID != "" {
		t.Errorf("AdGroupID = %q, AdID = %q — Performance Max has neither", res.AdGroupID, res.AdID)
	}

	group := readGroup()
	if !strings.Contains(group, `"status":"PAUSED"`) {
		t.Errorf("asset group create = %s, want status PAUSED", group)
	}
	if !strings.Contains(group, "KubeCon Europe 2026"+performanceMaxAssetGroupSuffix) {
		t.Errorf("asset group create = %s, want the default asset group name", group)
	}

	assets := readAssets()
	for _, want := range []string{"Join us at KubeCon", "Linux Foundation", "Talks, workshops and hallway track."} {
		if !strings.Contains(assets, want) {
			t.Errorf("assets:mutate body is missing %q: %s", want, assets)
		}
	}

	// Every link names the resource name Google returned, not one rebuilt from a
	// parsed id, and carries a field type.
	links := readLinks()
	for _, want := range []string{"customers/1234567890/assets/900", assetFieldHeadline, assetFieldBusinessName, assetFieldLogo} {
		if !strings.Contains(links, want) {
			t.Errorf("assetGroupAssets:mutate body is missing %q: %s", want, links)
		}
	}

	last := res.Steps[len(res.Steps)-1]
	if !strings.Contains(last, "Performance Max campaign created") || strings.Contains(last, "NO ASSET GROUP") {
		t.Errorf("closing step = %q, want the asset-group-present wording", last)
	}
}

// TestCreatePerformanceMaxCampaign_CampaignShape captures the campaigns:mutate
// body, which the happy-path cascade cannot: okCampaign does not record its
// request, and the shape of a PERFORMANCE_MAX campaign is the single most
// load-bearing thing in this file.
func TestCreatePerformanceMaxCampaign_CampaignShape(t *testing.T) {
	campaignH, readCampaign := capturedMutate(1, func(int) string { return "customers/1234567890/campaigns/222" })
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	linkH, _ := okPMaxLinks()

	// Built on the TEST goroutine: pmaxImages renders through pngOf, which ends in
	// t.Fatalf, and FailNow from a handler does not stop the test — it can leave the
	// server blocked while a deferred Close runs.
	cascade := pmaxCascade(t, pmaxImages(t), assetH, groupH, linkH)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "campaigns:mutate") {
			campaignH(w, r)
			return
		}
		cascade(w, r)
	})
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)
	if _, err := c.CreatePerformanceMaxCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := readCampaign()
	for _, want := range []string{
		`"advertisingChannelType":"` + advertisingChannelPerformanceMax + `"`,
		`"status":"PAUSED"`,
		`"urlExpansionOptOut":true`,
		`"maximizeConversions"`,
		"geoTargetTypeSetting",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("campaigns:mutate body is missing %q: %s", want, body)
		}
	}
	// Search-only shapes that must never appear on this channel.
	for _, forbidden := range []string{"networkSettings", "manualCpc"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("campaigns:mutate body carries %q, which Performance Max does not take: %s", forbidden, body)
		}
	}
}

func TestCreatePerformanceMaxCampaign_BadImageRefusesBeforeAnyMutate(t *testing.T) {
	tooSmall := pngOf(t, 100, 52) // under Google's minimum for the marketing slot
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".png") {
			_, _ = w.Write(tooSmall)
			return
		}
		t.Errorf("a mutate was sent despite an unusable image: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("an under-size marketing image must refuse the create")
	}
	// Pre-create: nothing was sent, so the claim must be RELEASED — which Dispatch
	// keys on result == nil alone.
	if res != nil {
		t.Errorf("result = %+v, want nil before the first mutate", res)
	}
}

func TestCreatePerformanceMaxCampaign_LinkFailureStillReportsWhatExists(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	// A DEFINITE 4xx, not a 5xx: an ambiguous failure would be retried and reported
	// UNCONFIRMED, and this test is about the partial result, not the wording.
	linkH := gaqlError(http.StatusBadRequest, "assetGroupAssetError", "DUPLICATE_RESOURCE")
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH, linkH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a failed link mutate must be reported")
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come ALONGSIDE a result, never (nil, err)")
	}
	// Both halves have to survive: a group that exists with no assets attached is
	// the state an operator most needs to find, and the assets are orphaned.
	if res.AssetGroupID != "555" {
		t.Errorf("AssetGroupID = %q, want 555 — the group exists", res.AssetGroupID)
	}
	if len(res.CreativeAssetIDs) != pmaxAssetCount {
		t.Errorf("CreativeAssetIDs = %d, want %d — the assets exist", len(res.CreativeAssetIDs), pmaxAssetCount)
	}
	if res.CampaignID == "" {
		t.Error("CampaignID must be carried on the partial result")
	}
}

func TestCreatePerformanceMaxCampaign_ShortAssetResponseIsUnconfirmed(t *testing.T) {
	// A 2xx describing FEWER assets than were sent leaves assets unaccounted for.
	shortH := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{{"resourceName": "customers/1234567890/assets/900"}},
		})
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), shortH,
		failHandler(t, "assetGroups:mutate"), failHandler(t, "assetGroupAssets:mutate")))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a short assets:mutate response must not be accepted as confirmed")
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come alongside a result")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %q, want it to say UNCONFIRMED — the assets may exist", err)
	}
	// The body PARSED — the one id in it is real, and is the only handle an operator
	// has on an account-level asset that may already exist. An UNCONFIRMED outcome
	// must not take it down with the error.
	if len(res.CreativeAssetIDs) != 1 || res.CreativeAssetIDs[0] != "900" {
		t.Errorf("the ids the short response DID carry must survive the error, got %v", res.CreativeAssetIDs)
	}
}

func TestCreatePerformanceMaxCampaign_WrongAccountAssetIsUnconfirmed(t *testing.T) {
	wrongH, _ := capturedMutate(pmaxAssetCount, func(i int) string {
		if i == 2 {
			return "customers/9999999999/assets/902" // another customer's account
		}
		return "customers/1234567890/assets/90" + string(rune('0'+i))
	})
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), wrongH,
		failHandler(t, "assetGroups:mutate"), failHandler(t, "assetGroupAssets:mutate")))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a wrong-account asset resource name must not be accepted")
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come alongside a result")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %q, want UNCONFIRMED", err)
	}
	// The ids parsed BEFORE the bad one are still reported: those assets exist.
	if len(res.CreativeAssetIDs) != 2 {
		t.Errorf("CreativeAssetIDs = %v, want the two ids parsed before the malformed one", res.CreativeAssetIDs)
	}
}

func TestCreatePerformanceMaxCampaign_NoAssetGroupSaysSoLoudly(t *testing.T) {
	// No creative at all — legal, because adoption of a hand-built asset group must
	// not be refused — but the campaign cannot serve, and the step list has to say it.
	srv := demandGenTLSServer(t, pmaxCascade(t, nil,
		failHandler(t, "assets:mutate"), failHandler(t, "assetGroups:mutate"), failHandler(t, "assetGroupAssets:mutate")))
	c := demandGenClient(t, srv)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), demandGenInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.AssetGroupID != "" || len(res.CreativeAssetIDs) != 0 {
		t.Errorf("asset group %q / %d asset(s) — nothing should have been created", res.AssetGroupID, len(res.CreativeAssetIDs))
	}
	last := res.Steps[len(res.Steps)-1]
	if !strings.Contains(last, "NO ASSET GROUP") {
		t.Errorf("closing step = %q, want it to say NO ASSET GROUP", last)
	}
}

// TestPreflightPerformanceMaxSplitsTheCriteria pins the channel split that is the
// easiest thing in this path to get wrong by copying Demand Gen's fence: Demand
// Gen refuses all five campaign criteria kinds, Performance Max refuses only two.
func TestPreflightPerformanceMaxSplitsTheCriteria(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}

	accepted := map[string]func(*CampaignInput){
		"languages": func(in *CampaignInput) { in.Languages = []string{"en"} },
		"ad schedules": func(in *CampaignInput) {
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17}}
		},
	}
	for name, mutate := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			in := demandGenInput()
			in.PerformanceMaxCreative = fullPMaxCreative()
			mutate(&in)
			pf, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
			if err != nil {
				t.Fatalf("Performance Max takes %s at campaign level: %v", name, err)
			}
			if pf.criteria.empty() {
				t.Errorf("%s was accepted but produced no criteria — that is a silent drop", name)
			}
		})
	}

	refused := map[string]func(*CampaignInput){
		"device bid modifiers": func(in *CampaignInput) {
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "MOBILE", BidModifier: 1.2}}
		},
		"excluded age ranges": func(in *CampaignInput) { in.ExcludedAgeRanges = []string{"AGE_RANGE_18_24"} },
		"excluded genders":    func(in *CampaignInput) { in.ExcludedGenders = []string{"MALE"} },
		"ad groups":           func(in *CampaignInput) { in.AdGroups = []AdGroupSpec{{Name: "extra"}} },
	}
	for name, mutate := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			in := demandGenInput()
			in.PerformanceMaxCreative = fullPMaxCreative()
			mutate(&in)
			if _, err := c.preflightCampaignKind(campaignKindPerformanceMax, in); err == nil {
				t.Fatalf("Performance Max accepted %s; it must refuse rather than drop", name)
			}
		})
	}
}
