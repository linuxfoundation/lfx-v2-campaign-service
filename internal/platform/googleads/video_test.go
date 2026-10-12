// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the Video (YouTube) cascade
// ---------------------------------------------------------------------------

// videoCreativeFixture is the MINIMUM responsive video ad: one video and one of each
// required text type. Minimum deliberately, so a test that counts assets counts a
// number validateVideoCreative's own bounds pin.
func videoCreativeFixture() VideoCreative {
	return VideoCreative{
		YouTubeVideoIDs: []string{"dQw4w9WgXcQ"},
		// Display width 12: the Video short headline limit is 15, not the RSA's 30.
		Headlines:     []string{"Register now"},
		LongHeadlines: []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		Descriptions:  []string{"Talks, workshops and the hallway track."},
	}
}

func videoInput() CampaignInput {
	in := demandGenInput()
	in.VideoCreative = videoCreativeFixture()
	return in
}

// videoCascade routes the campaign-shell mutates to their happy handlers and leaves
// the two creative mutates to the caller, so each test varies only the part it is
// about. Anything else is a FAILURE rather than a default: Video creates no asset
// group and fetches no images, and a cascade that grew either would otherwise pass
// silently.
func videoCascade(t *testing.T, adGroupH, assetH, adH http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
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
			t.Errorf("unexpected path %s — Video creates no asset group and fetches no images", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func videoBudgetResource(int) string   { return "customers/1234567890/campaignBudgets/111" }
func videoCampaignResource(int) string { return "customers/1234567890/campaigns/222" }
func videoAdGroupResource(int) string  { return "customers/1234567890/adGroups/333" }
func videoAssetResource(int) string    { return "customers/1234567890/assets/900" }
func videoAdResource(int) string       { return "customers/1234567890/adGroupAds/333~444" }

// serverError is the AMBIGUOUS outcome every mutate in this package has to classify as
// UNCONFIRMED: a 5xx on a mutating POST means the resource may well exist.
func serverError(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":{"code":500,"message":"backend error"}}`))
}

// mutateCreate decodes a captured mutate body and returns its single operation's
// create object.
func mutateCreate(t *testing.T, body string) map[string]any {
	t.Helper()
	var req struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decoding mutate body %q: %v", body, err)
	}
	if len(req.Operations) != 1 {
		t.Fatalf("operations = %d, want 1: %s", len(req.Operations), body)
	}
	return req.Operations[0].Create
}

// TestCreateVideoCampaign_RefusesWithoutSendingAnything is the only test in this file
// that calls the EXPORTED entry point. Every test below it calls
// createVideoCampaignCascade directly, because CreateVideoCampaign no longer runs the
// cascade: the Google Ads API cannot create a Video campaign, so it refuses first and
// the cascade is retained unreachable against the day Google opens creation. See
// CreateVideoCampaign for the full reasoning.
//
// What this pins is the REFUSAL ITSELF, not just an error. The server fails the test if
// it is contacted at all, because "returns an error" is satisfied equally well by a
// cascade that creates a budget and then fails — which is the exact outcome the refusal
// exists to prevent, and which costs real money per attempt.
func TestCreateVideoCampaign_RefusesWithoutSendingAnything(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Google was contacted at %s; a Video create must be refused before any request", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)

	res, err := c.CreateVideoCampaign(context.Background(), videoInput())
	if err == nil {
		t.Fatal("CreateVideoCampaign returned no error; the Google Ads API cannot create Video campaigns")
	}
	if !errors.Is(err, ErrVideoCreateUnsupported) {
		t.Errorf("error = %v, want it to wrap ErrVideoCreateUnsupported so dispatch can tell "+
			"'Google cannot do this' apart from 'this request was malformed'", err)
	}
	// (nil, err) is the pre-create half of the partial-result contract: nothing exists
	// upstream, so the orchestrator must RELEASE its claim rather than record an orphan.
	// A non-nil result here would have dispatch retain a claim on a campaign that was
	// never created and can never be created.
	if res != nil {
		t.Errorf("result = %+v, want nil — nothing was created, so the claim must be released", res)
	}
}

func TestCreateVideoCampaign_HappyPath(t *testing.T) {
	budgetH, readBudget := capturedMutate(1, videoBudgetResource)
	campaignH, readCampaign := capturedMutate(1, videoCampaignResource)
	adGroupH, readAdGroup := capturedMutate(1, videoAdGroupResource)
	assetH, readAssets := capturedMutate(1, videoAssetResource)
	adH, readAd := capturedMutate(1, videoAdResource)

	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			budgetH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			campaignH(w, r)
		default:
			videoCascade(t, adGroupH, assetH, adH)(w, r)
		}
	})
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
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
	// Video has no asset group, so the field may not be invented.
	if res.AssetGroupID != "" {
		t.Errorf("AssetGroupID = %q, want empty — Video has no asset group", res.AssetGroupID)
	}
	if len(res.CreativeAssetIDs) != 1 {
		t.Errorf("CreativeAssetIDs = %v, want the one video asset", res.CreativeAssetIDs)
	}

	// budgetKindFor gives Video its own budget-name segment, so a Video campaign on a
	// brief that already has a Search one does not compose the same budget name and
	// fail at the budget mutate with DUPLICATE_NAME.
	if name, _ := mutateCreate(t, readBudget())["name"].(string); !strings.Contains(name, "Video Budget") {
		t.Errorf("budget name = %q, want one carrying the Video segment", name)
	}

	// The campaign must carry the channel type AND the sub-type. The sub-type is the
	// field most easily lost in a refactor, and the one whose absence is only
	// discovered three resources later, at the ad mutate.
	campaign := mutateCreate(t, readCampaign())
	if campaign["advertisingChannelType"] != advertisingChannelVideo {
		t.Errorf("advertisingChannelType = %v, want %q", campaign["advertisingChannelType"], advertisingChannelVideo)
	}
	if campaign["advertisingChannelSubType"] != advertisingChannelSubTypeVideoAction {
		t.Errorf("advertisingChannelSubType = %v, want %q — an unqualified VIDEO campaign refuses the responsive ad this cascade then sends", campaign["advertisingChannelSubType"], advertisingChannelSubTypeVideoAction)
	}
	if campaign["status"] != "PAUSED" {
		t.Errorf("campaign status = %v, want PAUSED", campaign["status"])
	}
	// maximize-conversions is this channel's default and must reach the payload as the
	// bidding oneof rather than being silently dropped.
	if _, ok := campaign["maximizeConversions"]; !ok {
		t.Errorf("campaign create carries no bidding strategy: %v", campaign)
	}
	// Both are rejected on VIDEO, and either would fail AFTER the budget exists.
	if _, ok := campaign["manualCpc"]; ok {
		t.Errorf("manualCpc must never be sent on VIDEO: %v", campaign)
	}
	if _, ok := campaign["networkSettings"]; ok {
		t.Errorf("networkSettings must not be sent on VIDEO: %v", campaign)
	}

	// The ad group must be typed, or the videoResponsiveAd lands in a
	// VIDEO_TRUE_VIEW_IN_STREAM group that refuses it.
	if got := mutateCreate(t, readAdGroup())["type"]; got != adGroupTypeVideoResponsive {
		t.Errorf("ad group type = %v, want %q", got, adGroupTypeVideoResponsive)
	}

	// The asset mutate must be a YouTube video asset, never an image one.
	asset := mutateCreate(t, readAssets())
	yt, _ := asset["youtubeVideoAsset"].(map[string]any)
	if yt == nil {
		t.Fatalf("asset create = %v, want a youtubeVideoAsset", asset)
	}
	if yt["youtubeVideoId"] != "dQw4w9WgXcQ" {
		t.Errorf("youtubeVideoId = %v, want the id the caller supplied", yt["youtubeVideoId"])
	}

	ad := mutateCreate(t, readAd())
	if ad["status"] != "PAUSED" {
		t.Errorf("adGroupAd status = %v, want PAUSED — nothing this client creates may serve unreviewed", ad["status"])
	}
	inner, _ := ad["ad"].(map[string]any)
	responsive, _ := inner["videoResponsiveAd"].(map[string]any)
	if responsive == nil {
		t.Fatalf("ad create = %v, want a videoResponsiveAd", ad)
	}
	// The ad must reference the asset by the resource name GOOGLE returned, never one
	// rebuilt from the parsed id.
	videos, _ := responsive["videos"].([]any)
	if len(videos) != 1 {
		t.Fatalf("videos = %v, want exactly one", responsive["videos"])
	}
	if first, _ := videos[0].(map[string]any); first["asset"] != "customers/1234567890/assets/900" {
		t.Errorf("video asset reference = %v, want the resource name Google returned", first["asset"])
	}
	// Short and long headlines are DIFFERENT assets to Google — a format that shows one
	// does not show the other — so collapsing them into one list is a real defect.
	headlines, _ := responsive["headlines"].([]any)
	longHeadlines, _ := responsive["longHeadlines"].([]any)
	if len(headlines) != 1 || len(longHeadlines) != 1 {
		t.Errorf("headlines/longHeadlines must be sent as separate lists: %v", responsive)
	}
	// No call to action was supplied, and the field is omitempty precisely so Google
	// supplies its own default button rather than seeing an empty list.
	if _, ok := responsive["callToActions"]; ok {
		t.Errorf("callToActions must be ABSENT when none were supplied, got %v", responsive["callToActions"])
	}
	// Companion banners are deliberately not offered; the field must not appear at all.
	if _, ok := responsive["companionBanners"]; ok {
		t.Errorf("companionBanners is deliberately out of scope: %v", responsive)
	}
}

// TestCreateVideoCampaign_NoAdWithoutACreative pins the documented shape: no creative
// means campaign + ad group and NO ad, and the closing step must say the campaign
// cannot serve rather than reporting a clean success.
func TestCreateVideoCampaign_NoAdWithoutACreative(t *testing.T) {
	adGroupH, _ := capturedMutate(1, videoAdGroupResource)
	srv := demandGenTLSServer(t, videoCascade(t, adGroupH,
		failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate")))
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), demandGenInput()) // no VideoCreative
	if err != nil {
		t.Fatalf("a Video campaign with no creative must still create: %v", err)
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

// TestCreateVideoCampaign_BudgetFailureReturnsNoResult pins the half of the partial-
// result contract that is easy to get backwards: BEFORE the campaign exists there is
// nothing to reconcile, so a DEFINITE budget failure returns (nil, err) and the
// orchestrator releases its claim.
func TestCreateVideoCampaign_BudgetFailureReturnsNoResult(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"invalid budget"}}`))
			return
		}
		t.Errorf("nothing may be sent after a definite budget failure, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
	if err == nil {
		t.Fatal("want an error")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil: nothing was created, so the claim must be released", res)
	}
}

// TestCreateVideoCampaign_AmbiguousBudgetKeepsTheClaim is the same step with an
// AMBIGUOUS outcome: the budget may exist, so the error arrives with a partial and
// says UNCONFIRMED. Reporting it as a clean failure would invite a retry into a
// second paid budget.
func TestCreateVideoCampaign_AmbiguousBudgetKeepsTheClaim(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			serverError(w, r)
			return
		}
		t.Errorf("nothing may be sent after an unconfirmed budget, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
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

// TestCreateVideoCampaign_AdGroupFailureKeepsThePartial is the other side of the
// contract: the campaign exists and spends, so the error must arrive ALONGSIDE a
// result carrying the campaign id and the deterministic ad group NAME a reconciler
// needs in order to find what may exist.
func TestCreateVideoCampaign_AdGroupFailureKeepsThePartial(t *testing.T) {
	srv := demandGenTLSServer(t, videoCascade(t, serverError,
		failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate")))
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
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

// TestCreateVideoCampaign_AdFailureReportsTheAssets: video assets created with nothing
// referencing them are account-level litter, so the ids an operator needs in order to
// find them must survive a failure of the ad half.
func TestCreateVideoCampaign_AdFailureReportsTheAssets(t *testing.T) {
	adGroupH, _ := capturedMutate(1, videoAdGroupResource)
	assetH, _ := capturedMutate(1, videoAssetResource)
	srv := demandGenTLSServer(t, videoCascade(t, adGroupH, assetH, serverError))
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
	if err == nil {
		t.Fatal("want an error")
	}
	if res == nil {
		t.Fatal("the campaign, ad group and assets exist — the partial must not be dropped")
	}
	if len(res.CreativeAssetIDs) != 1 {
		t.Errorf("CreativeAssetIDs = %v, want the created asset: it is the only handle on account-level litter", res.CreativeAssetIDs)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty", res.AdID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("want an UNCONFIRMED classification on a 5xx: %v", err)
	}
}
