// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the Display cascade
// ---------------------------------------------------------------------------

// displayCreativeAt points the creative at the TLS test server, which serves BOTH the
// images and the Google Ads API — the creative URLs have to be https to survive the
// pure validator's scheme rule, which a plain httptest server could not give them.
func displayCreativeAt(base string) DisplayCreative {
	return DisplayCreative{
		MarketingImages:  []string{base + "/m.png"},
		LogoImages:       []string{base + "/logo.png"},
		Headlines:        []string{"Join us at KubeCon"},
		LongHeadline:     "KubeCon + CloudNativeCon Europe 2026",
		Descriptions:     []string{"Three days of talks."},
		BusinessName:     "Linux Foundation",
		CallToActionText: "Register",
	}
}

// displayImages are the two bodies displayCreativeAt asks for, at sizes satisfying the
// landscape marketing slot (1.91:1, min 600x314) and the landscape logo slot (4:1, min
// 512x128) respectively. Real image bytes, because the geometry rules are the point.
func displayImages(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"/m.png":    pngOf(t, 1200, 628),
		"/logo.png": pngOf(t, 1024, 256),
	}
}

func displayInput(base string) CampaignInput {
	in := demandGenInput()
	in.DisplayCreative = displayCreativeAt(base)
	return in
}

func displayBudgetResource(int) string   { return "customers/1234567890/campaignBudgets/111" }
func displayCampaignResource(int) string { return "customers/1234567890/campaigns/222" }
func displayAdGroupResource(int) string  { return "customers/1234567890/adGroups/333" }
func displayAdResource(int) string       { return "customers/1234567890/adGroupAds/333~444" }

// displayAssets answers assets:mutate with one resource name per image, in order.
func displayAssets() (http.HandlerFunc, func() string) {
	return capturedMutate(2, func(i int) string {
		return "customers/1234567890/assets/" + []string{"900", "901"}[i]
	})
}

// displayCascade routes the campaign-shell mutates to their happy handlers, serves any
// other path as an image, and leaves the two creative mutates to the caller. An
// unexpected API path is a FAILURE rather than a default: Display creates no asset
// group, and a cascade that grew one would otherwise pass silently.
func displayCascade(t *testing.T, images map[string][]byte, adGroupH, assetH, adH http.HandlerFunc) http.HandlerFunc {
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
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			adGroupH(w, r)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			adH(w, r)
		default:
			t.Errorf("unexpected path %s — Display creates no asset group", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestCreateDisplayCampaign_HappyPath(t *testing.T) {
	budgetH, readBudget := capturedMutate(1, displayBudgetResource)
	campaignH, readCampaign := capturedMutate(1, displayCampaignResource)
	adGroupH, readAdGroup := capturedMutate(1, displayAdGroupResource)
	assetH, readAssets := displayAssets()
	adH, readAd := capturedMutate(1, displayAdResource)

	images := displayImages(t)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			budgetH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			campaignH(w, r)
		default:
			displayCascade(t, images, adGroupH, assetH, adH)(w, r)
		}
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result on success")
	}
	if res.CampaignBudgetID != "111" || res.CampaignID != "222" {
		t.Errorf("budget %q / campaign %q, want 111/222", res.CampaignBudgetID, res.CampaignID)
	}
	if res.AdGroupID != "333" || res.AdID != "444" {
		t.Errorf("ad group %q / ad %q, want 333/444", res.AdGroupID, res.AdID)
	}
	// Display has no asset group, so the field may not be invented.
	if res.AssetGroupID != "" {
		t.Errorf("AssetGroupID = %q, want empty — Display has no asset group", res.AssetGroupID)
	}
	if len(res.CreativeAssetIDs) != 2 {
		t.Errorf("CreativeAssetIDs = %v, want the two image assets", res.CreativeAssetIDs)
	}

	// budgetKindFor gives Display its own budget-name segment, so a Display campaign on
	// a brief that already has a Search one does not compose the same budget name and
	// fail at the budget mutate with DUPLICATE_NAME.
	if name, _ := mutateCreate(t, readBudget())["name"].(string); !strings.Contains(name, "Display Budget") {
		t.Errorf("budget name = %q, want one carrying the Display segment", name)
	}

	campaign := mutateCreate(t, readCampaign())
	if campaign["advertisingChannelType"] != advertisingChannelDisplay {
		t.Errorf("advertisingChannelType = %v, want %q", campaign["advertisingChannelType"], advertisingChannelDisplay)
	}
	// Unlike Video, this channel deliberately sends NO sub-type: a bare DISPLAY campaign
	// is already the standard product, and the sub-types that exist are different ad
	// shapes this cascade does not build. An added one would change the product silently.
	if _, ok := campaign["advertisingChannelSubType"]; ok {
		t.Errorf("advertisingChannelSubType must be ABSENT on DISPLAY, got %v", campaign["advertisingChannelSubType"])
	}
	if campaign["status"] != "PAUSED" {
		t.Errorf("campaign status = %v, want PAUSED", campaign["status"])
	}
	// maximize-conversions is this channel's default and must reach the payload as the
	// bidding oneof rather than being silently dropped.
	if _, ok := campaign["maximizeConversions"]; !ok {
		t.Errorf("campaign create carries no bidding strategy: %v", campaign)
	}
	// Refused on this channel because the Display ad-group payload carries no bid — a
	// manual-CPC Display campaign created here would go live bidding a number nobody
	// supplied. See displayBiddingStrategies.
	if _, ok := campaign["manualCpc"]; ok {
		t.Errorf("manualCpc must never be sent on DISPLAY: %v", campaign)
	}
	// Display attaches its geo at the CAMPAIGN level, so the setting governs criteria
	// this campaign actually carries.
	if _, ok := campaign["geoTargetTypeSetting"]; !ok {
		t.Errorf("campaign create carries no geoTargetTypeSetting: %v", campaign)
	}
	// networkSettings, and the exact flags — Display is the ONLY non-Search channel that
	// sends this, and omitting it is not a harmless default. The flags are proto3 bools,
	// so an absent networkSettings is a campaign targeting NO network, which Google
	// refuses with CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK *after* the budget mutate
	// has committed — a stranded billable budget on every Display create, and a retry
	// composes the same budget name and dies at DUPLICATE_NAME. Asserted field by field
	// rather than just "is present": a Display campaign that quietly opted itself into
	// Search inventory would spend on the wrong network, and all-false would present as
	// present-and-correct while being exactly the rejected state.
	network, ok := campaign["networkSettings"].(map[string]any)
	if !ok {
		t.Fatalf("campaign create carries no networkSettings; an omitted one targets NO network and strands the budget: %v", campaign)
	}
	if network["targetContentNetwork"] != true {
		t.Errorf("targetContentNetwork = %v, want true — the Display Network IS the content network", network["targetContentNetwork"])
	}
	for _, off := range []string{"targetGoogleSearch", "targetSearchNetwork"} {
		if network[off] != false {
			t.Errorf("%s = %v, want false — a Display campaign must not opt itself into Search inventory", off, network[off])
		}
	}

	// The ad group must be typed, or the responsive display ad lands in a group that
	// refuses it — three resources after the mistake was made.
	adGroup := mutateCreate(t, readAdGroup())
	if adGroup["type"] != adGroupTypeDisplayStandard {
		t.Errorf("ad group type = %v, want %q", adGroup["type"], adGroupTypeDisplayStandard)
	}
	// ENABLED, as on Demand Gen and Video. Search is the only channel here that pauses
	// its ad group, and the campaign is PAUSED regardless.
	if adGroup["status"] != "ENABLED" {
		t.Errorf("ad group status = %v, want ENABLED", adGroup["status"])
	}

	// The asset mutate must carry the BYTES, base64 encoded — Google's image asset takes
	// data, not an address, which is the whole reason the fetch phase exists.
	assetReq := readAssets()
	if strings.Contains(assetReq, srv.URL) {
		t.Error("the asset mutate sent the image URL; Google cannot fetch it")
	}
	wantPrefix := base64.StdEncoding.EncodeToString(images["/m.png"])[:32]
	if !strings.Contains(assetReq, wantPrefix) {
		t.Error("asset mutate does not carry the fetched image bytes")
	}

	// The ad must reference the resource names GOOGLE returned, each in the slot its
	// image came from — the marketing image as a marketing image, the logo as a logo.
	adReq := readAd()
	if !strings.Contains(adReq, `"marketingImages":[{"asset":"customers/1234567890/assets/900"}]`) {
		t.Errorf("ad does not reference the marketing asset in its own slot: %s", adReq)
	}
	if !strings.Contains(adReq, `"logoImages":[{"asset":"customers/1234567890/assets/901"}]`) {
		t.Errorf("ad does not reference the logo asset in its own slot: %s", adReq)
	}
	// The two square arrays were not supplied and are omitempty, so they must be absent
	// rather than sent as empty lists.
	if strings.Contains(adReq, "squareMarketingImages") || strings.Contains(adReq, "squareLogoImages") {
		t.Errorf("unsupplied image arrays must be omitted, not sent empty: %s", adReq)
	}

	ad := mutateCreate(t, readAd())
	if ad["status"] != "PAUSED" {
		t.Errorf("adGroupAd status = %v, want PAUSED — nothing this client creates may serve unreviewed", ad["status"])
	}
	inner, _ := ad["ad"].(map[string]any)
	responsive, _ := inner["responsiveDisplayAd"].(map[string]any)
	if responsive == nil {
		t.Fatalf("ad create = %v, want a responsiveDisplayAd", ad)
	}
	// The long headline is a SINGLE text asset, not a list. Sending it as a list is the
	// shape error this channel is most likely to inherit from its siblings.
	long, ok := responsive["longHeadline"].(map[string]any)
	if !ok {
		t.Fatalf("longHeadline = %v, want a single text asset object", responsive["longHeadline"])
	}
	if long["text"] != "KubeCon + CloudNativeCon Europe 2026" {
		t.Errorf("longHeadline text = %v, want the supplied headline", long["text"])
	}
	if headlines, _ := responsive["headlines"].([]any); len(headlines) != 1 {
		t.Errorf("headlines = %v, want the one supplied short headline", responsive["headlines"])
	}
	if responsive["businessName"] != "Linux Foundation" {
		t.Errorf("businessName = %v, want the required brand name", responsive["businessName"])
	}
	if responsive["callToActionText"] != "Register" {
		t.Errorf("callToActionText = %v, want the supplied text", responsive["callToActionText"])
	}
}

// A creative with no call to action must send no callToActionText at all — the field is
// omitempty precisely so Google supplies its own button rather than seeing an empty
// string.
func TestCreateDisplayCampaign_CallToActionIsOptional(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := displayAssets()
	adH, readAd := capturedMutate(1, displayAdResource)
	srv := demandGenTLSServer(t, displayCascade(t, displayImages(t), adGroupH, assetH, adH))
	c := demandGenClient(t, srv)

	in := displayInput(srv.URL)
	creative := in.DisplayCreative
	creative.CallToActionText = ""
	in.DisplayCreative = creative

	if _, err := c.CreateDisplayCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(readAd(), "callToActionText") {
		t.Errorf("callToActionText must be ABSENT when none was supplied: %s", readAd())
	}
}

// The documented no-creative shape: campaign + ad group and NO ad, with a closing step
// that says the campaign cannot serve rather than reporting a clean success.
func TestCreateDisplayCampaign_NoAdWithoutACreative(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	srv := demandGenTLSServer(t, displayCascade(t, nil, adGroupH,
		failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate")))
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), demandGenInput()) // no DisplayCreative
	if err != nil {
		t.Fatalf("a Display campaign with no creative must still create: %v", err)
	}
	if res.AdID != "" || len(res.CreativeAssetIDs) != 0 {
		t.Errorf("ad %q / assets %v, want neither", res.AdID, res.CreativeAssetIDs)
	}
	if res.AdGroupID != "333" {
		t.Errorf("AdGroupID = %q — the ad group is created whether or not there is an ad", res.AdGroupID)
	}
	closing := res.Steps[len(res.Steps)-1]
	if !strings.Contains(closing, "NO AD") {
		t.Errorf("closing step = %q, want one saying the campaign cannot serve", closing)
	}
}

// The fetch phase runs BEFORE the first mutate, so a bad image costs nothing. This is
// the guarantee that separates Display from a cascade that discovers a 404 after three
// paid resources exist — and the one Video does not need, having no images at all.
func TestCreateDisplayCampaign_BadImageCreatesNothing(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":mutate") {
			t.Errorf("nothing may be created when an image cannot be fetched, got %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound) // every image 404s
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil: nothing was created, so the claim must be released", res)
	}
}

// BEFORE the campaign exists there is nothing to reconcile, so a DEFINITE budget
// failure returns (nil, err) and the orchestrator releases its claim.
func TestCreateDisplayCampaign_BudgetFailureReturnsNoResult(t *testing.T) {
	images := displayImages(t)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"invalid budget"}}`))
			return
		}
		t.Errorf("nothing may be sent after a definite budget failure, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil: nothing was created, so the claim must be released", res)
	}
}

// The same step with an AMBIGUOUS outcome: the budget may exist, so the error arrives
// with a partial and says UNCONFIRMED. Reporting it as a clean failure would invite a
// retry into a second paid budget.
func TestCreateDisplayCampaign_AmbiguousBudgetKeepsTheClaim(t *testing.T) {
	images := displayImages(t)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			serverError(w, r)
			return
		}
		t.Errorf("nothing may be sent after an unconfirmed budget, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	if res == nil {
		t.Fatal("a budget that MAY exist must come back as a partial, not as nil")
	}
	if res.CampaignBudgetName == "" {
		t.Error("the partial must carry the budget NAME — it is the only handle on a budget whose id is unknown")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx on a mutating POST is an unknown outcome, not a failure: %v", err)
	}
}

// Past the campaign create the error must arrive ALONGSIDE a result carrying the
// campaign id and the deterministic ad group NAME a reconciler needs.
func TestCreateDisplayCampaign_AdGroupFailureKeepsThePartial(t *testing.T) {
	srv := demandGenTLSServer(t, displayCascade(t, displayImages(t), serverError,
		failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate")))
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	if res == nil {
		t.Fatal("the campaign exists and spends — the partial must not be dropped")
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want 222", res.CampaignID)
	}
	if res.AdGroupName == "" {
		t.Error("the partial must carry AdGroupName so the possibly-created ad group can be found")
	}
	if res.AdGroupID != "" {
		t.Errorf("AdGroupID = %q — an unconfirmed ad group has no id to persist", res.AdGroupID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx on a mutating POST is an unknown outcome, not a failure: %v", err)
	}
}

// Image assets created with nothing referencing them are account-level litter, so the
// ids an operator needs in order to find them must survive a failure of the ad half.
func TestCreateDisplayCampaign_AdFailureReportsTheAssets(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := displayAssets()
	srv := demandGenTLSServer(t, displayCascade(t, displayImages(t), adGroupH, assetH, serverError))
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err == nil {
		t.Fatal("want an error")
	}
	if res == nil {
		t.Fatal("the campaign, ad group and assets exist — the partial must not be dropped")
	}
	if len(res.CreativeAssetIDs) != 2 {
		t.Errorf("CreativeAssetIDs = %v, want the created assets: they are the only handle on account-level litter", res.CreativeAssetIDs)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty", res.AdID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("want an UNCONFIRMED classification on a 5xx: %v", err)
	}
}

func TestDisplayClosingStep(t *testing.T) {
	cases := []struct {
		hasGeo, hasAd bool
		wantNoAd      bool
		wantNoGeo     bool
	}{
		{true, true, false, false},
		{false, true, false, true},
		{true, false, true, false},
		{false, false, true, true},
	}
	for _, tc := range cases {
		got := displayClosingStep(tc.hasGeo, tc.hasAd)
		if strings.Contains(got, "NO AD") != tc.wantNoAd {
			t.Errorf("displayClosingStep(%v,%v) = %q, NO AD mismatch", tc.hasGeo, tc.hasAd, got)
		}
		if strings.Contains(got, "no geo targeting") != tc.wantNoGeo {
			t.Errorf("displayClosingStep(%v,%v) = %q, geo mismatch", tc.hasGeo, tc.hasAd, got)
		}
	}
}
