// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// demandGenCreativeInput is demandGenInput plus a single-image creative with all three image
// roles resolved to distinct bytes, the shape the dispatcher (G4) will hand the client.
func demandGenCreativeInput() CampaignInput {
	in := demandGenInput()
	in.Creative = &DemandGenCreative{
		MediaFormat:          MediaFormatSingleImage,
		BusinessName:         "Linux Foundation",
		MarketingImage:       CreativeImage{AssetID: "m", Bytes: []byte("LANDSCAPE_BYTES"), MIME: "image/png"},
		SquareMarketingImage: CreativeImage{AssetID: "s", Bytes: []byte("SQUARE_BYTES"), MIME: "image/png"},
		Logo:                 CreativeImage{AssetID: "l", Bytes: []byte("LOGO_BYTES"), MIME: "image/png"},
	}
	return in
}

// demandGenAdServer answers the full creative cascade: three asset creates (distinct ids 401,
// 402, 403 in call order, so a test can pin role→resource mapping), then budget/campaign/ad
// group, then the adGroupAd composite. It records every path and decoded body.
func demandGenAdServer(t *testing.T, bodies *[]map[string]any, paths *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	var assetSeq int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		*paths = append(*paths, r.URL.Path)
		*bodies = append(*bodies, decoded)
		var resource string
		switch {
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetSeq++
			resource = "customers/1234567890/assets/" + map[int]string{1: "401", 2: "402", 3: "403"}[assetSeq]
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			resource = "customers/1234567890/campaignBudgets/111"
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			resource = "customers/1234567890/campaigns/222"
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			resource = "customers/1234567890/adGroups/333"
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			resource = "customers/1234567890/adGroupAds/333~444"
		default:
			resource = "customers/1234567890/unknown/999"
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"resourceName":"` + resource + `"}]}`))
	}))
}

// firstBodyFor returns operations[0].create of the first captured mutate whose path ends with
// suffix, failing if none was seen.
func firstBodyFor(t *testing.T, bodies []map[string]any, paths []string, suffix string) map[string]any {
	t.Helper()
	for i, p := range paths {
		if !strings.HasSuffix(p, suffix) {
			continue
		}
		ops, _ := bodies[i]["operations"].([]any)
		if len(ops) == 0 {
			t.Fatalf("%s carried no operations", suffix)
		}
		op, _ := ops[0].(map[string]any)
		create, _ := op["create"].(map[string]any)
		if create == nil {
			t.Fatalf("%s operation carried no create", suffix)
		}
		return create
	}
	t.Fatalf("no %s seen, paths = %v", suffix, paths)
	return nil
}

func countSuffix(paths []string, suffix string) int {
	n := 0
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

// The whole point of G3: a creative turns the paused shell into a real ad. Assert the three
// assets upload FIRST (before the budget — the fail-before-spending ordering), then that the
// ad carries the Demand Gen ad type with the business name, the three image roles referencing
// the uploaded asset resource names in role order, finalUrls on the AD (not the ad-type), and
// NO responsiveSearchAd. res.AdID is the returned ad id.
func TestCreateDemandGenCampaignWithCreativeBuildsTheAd(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := demandGenAdServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	res, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), demandGenCreativeInput())
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want 444", res.AdID)
	}

	mu.Lock()
	defer mu.Unlock()
	if n := countSuffix(paths, "assets:mutate"); n != 3 {
		t.Errorf("assets:mutate count = %d, want 3 (one per image role)", n)
	}
	// Assets upload before the budget: the first spending mutate must come after all 3 assets.
	firstBudget, lastAsset := -1, -1
	for i, p := range paths {
		if strings.HasSuffix(p, "assets:mutate") {
			lastAsset = i
		}
		if firstBudget == -1 && strings.HasSuffix(p, "campaignBudgets:mutate") {
			firstBudget = i
		}
	}
	if !(lastAsset >= 0 && firstBudget > lastAsset) {
		t.Errorf("assets must upload before the budget (fail-before-spending); lastAsset=%d firstBudget=%d, paths=%v", lastAsset, firstBudget, paths)
	}

	create := firstBodyFor(t, bodies, paths, "adGroupAds:mutate")
	ad, _ := create["ad"].(map[string]any)
	if ad == nil {
		t.Fatalf("adGroupAd create carried no ad, got %v", create)
	}
	if _, ok := ad["responsiveSearchAd"]; ok {
		t.Error("a Demand Gen ad must NOT carry responsiveSearchAd — that is the Search ad type")
	}
	urls, _ := ad["finalUrls"].([]any)
	if len(urls) != 1 || urls[0] == "" {
		t.Errorf("finalUrls must be set on the ad, got %v", ad["finalUrls"])
	}
	dg, _ := ad["demandGenMultiAssetResponsiveDisplayAd"].(map[string]any)
	if dg == nil {
		t.Fatalf("ad carried no demandGenMultiAssetResponsiveDisplayAd, got %v", ad)
	}
	if dg["businessName"] != "Linux Foundation" {
		t.Errorf("businessName = %v, want Linux Foundation", dg["businessName"])
	}
	// Role → uploaded-resource mapping, in upload order (marketing=401, square=402, logo=403).
	for _, role := range []struct {
		field string
		want  string
	}{
		{"marketingImages", "customers/1234567890/assets/401"},
		{"squareMarketingImages", "customers/1234567890/assets/402"},
		{"logoImages", "customers/1234567890/assets/403"},
	} {
		arr, _ := dg[role.field].([]any)
		if len(arr) != 1 {
			t.Errorf("%s must have exactly one asset, got %v", role.field, dg[role.field])
			continue
		}
		ref, _ := arr[0].(map[string]any)
		if ref["asset"] != role.want {
			t.Errorf("%s[0].asset = %v, want %q", role.field, ref["asset"], role.want)
		}
	}
}

// composeAdCopy is reused, but Demand Gen caps headlines at 5 (RSA allows 15). Supply 6 usable
// caller headlines and assert only 5 reach the wire — a 6th is rejected by the API on this ad
// type.
func TestCreateDemandGenAdCapsHeadlinesAtFive(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := demandGenAdServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	in := demandGenCreativeInput()
	in.Headlines = []string{"Headline One", "Headline Two", "Headline Three", "Headline Four", "Headline Five", "Headline Six"}
	if _, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	create := firstBodyFor(t, bodies, paths, "adGroupAds:mutate")
	ad, _ := create["ad"].(map[string]any)
	dg, _ := ad["demandGenMultiAssetResponsiveDisplayAd"].(map[string]any)
	headlines, _ := dg["headlines"].([]any)
	if len(headlines) != maxDemandGenHeadlines {
		t.Errorf("headline count = %d, want %d (Demand Gen cap)", len(headlines), maxDemandGenHeadlines)
	}
}

// An unrecognised media format is rejected BEFORE any request — a typo must not silently build
// the wrong ad, and it certainly must not upload assets or create a paid campaign first.
func TestCreateDemandGenAdRejectsUnknownMediaFormat(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := demandGenAdServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	in := demandGenCreativeInput()
	in.Creative.MediaFormat = "hologram"
	res, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for an unknown media format")
	}
	if res != nil {
		t.Errorf("expected nil result (nothing created), got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 0 {
		t.Errorf("no request must be sent when the format is invalid, got %v", paths)
	}
}

// A business name over the 25-char limit is a pre-create rejection: caught before any upload or
// spend, not after the ad create fails on the far side of a committed budget.
func TestCreateDemandGenAdRejectsOverlongBusinessName(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := demandGenAdServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	in := demandGenCreativeInput()
	in.Creative.BusinessName = strings.Repeat("A", maxBusinessNameRunes+1)
	res, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for an over-length business name")
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 0 {
		t.Errorf("no request must be sent, got %v", paths)
	}
}

// A missing image role (all three are required) is a pre-create rejection with no request sent
// — uploading an empty asset would 400 late or create a useless asset.
func TestCreateDemandGenAdRejectsMissingImageRole(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := demandGenAdServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	in := demandGenCreativeInput()
	in.Creative.Logo = CreativeImage{AssetID: "l"} // no bytes
	res, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error when an image role has no bytes")
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 0 {
		t.Errorf("no request must be sent, got %v", paths)
	}
}

// An asset upload failure happens BEFORE the budget, so nothing spending is created: the call
// returns (nil, err) and no budget/campaign mutate is ever sent.
func TestCreateDemandGenAdAssetFailureCreatesNoSpend(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		// The asset upload fails (a 5xx is ambiguous but still a failure of the upload).
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateDemandGenCampaign(context.Background(), demandGenCreativeInput())
	if err == nil {
		t.Fatal("expected an error when the asset upload fails")
	}
	if res != nil {
		t.Errorf("expected nil result — no spending resource was created, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := countSuffix(paths, "campaignBudgets:mutate"); n != 0 {
		t.Errorf("no budget must be created after an asset-upload failure, got %d budget calls; paths=%v", n, paths)
	}
}

// A 5xx on the ad create is AMBIGUOUS: the budget/campaign/ad group already committed, so the
// call returns a NON-NIL partial carrying those ids with AdID empty, and the error says
// UNCONFIRMED so the caller does not retry into a duplicate ad.
func TestCreateDemandGenAdFailureReturnsReconcilablePartial(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/assets/401"}]}`))
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`))
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`))
		default:
			// A 5xx on the ad: AMBIGUOUS — the ad may or may not exist.
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateDemandGenCampaign(context.Background(), demandGenCreativeInput())
	if err == nil {
		t.Fatal("expected an error when the ad mutate fails")
	}
	if res == nil {
		t.Fatal("expected a NON-NIL partial: the budget, campaign and ad group committed")
	}
	if res.AdGroupID != "333" {
		t.Errorf("AdGroupID = %q, want 333 — the partial must name the committed ad group", res.AdGroupID)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty — the ad was not confirmed", res.AdID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx on the ad is AMBIGUOUS and must be reported UNCONFIRMED, got: %v", err)
	}
}
