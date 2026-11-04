// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Google returns the EXISTING asset for a duplicate asset create (result_ids.go), so one image
// used in two slots comes back with one asset id at two indexes. Each creative-asset loop must
// accept that repeat at every slot it fills — the ad or asset group is created and references
// the asset in each slot — and report the id once. A repeat whose operations sent DIFFERENT
// content stays UNCONFIRMED: result_ids_test.go pins that for every creative loop, and the
// criteria and link loops, which never accept a repeat.

// contentAssets answers assets:mutate the way Google answers duplicate content: the first
// distinct create gets asset 900, the next distinct one 901, and a create whose JSON matches
// an earlier one gets that earlier asset's id. It records the request body.
func contentAssets() (http.HandlerFunc, func() string) {
	var (
		mu   sync.Mutex
		body string
	)
	read := func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(raw)
		mu.Unlock()
		var req struct {
			Operations []struct {
				Create json.RawMessage `json:"create"`
			} `json:"operations"`
		}
		_ = json.Unmarshal(raw, &req)
		ids := map[string]int{}
		parts := make([]string, 0, len(req.Operations))
		for _, op := range req.Operations {
			id, ok := ids[string(op.Create)]
			if !ok {
				id = 900 + len(ids)
				ids[string(op.Create)] = id
			}
			parts = append(parts,
				fmt.Sprintf(`{"resourceName":"customers/1234567890/assets/%d"}`, id))
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, read
}

// adAssetRefs decodes the one adGroupAds:mutate create and returns, for each named field of
// the ad info under infoKey, the asset resource names it references.
func adAssetRefs(t *testing.T, body, infoKey string, fields ...string) map[string][]string {
	t.Helper()
	var req struct {
		Operations []struct {
			Create struct {
				Ad map[string]json.RawMessage `json:"ad"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Operations) != 1 {
		t.Fatalf("ad body = %s (%v), want one create", body, err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(req.Operations[0].Create.Ad[infoKey], &info); err != nil {
		t.Fatalf("ad.%s: %v", infoKey, err)
	}
	out := map[string][]string{}
	for _, f := range fields {
		var refs []struct {
			Asset string `json:"asset"`
		}
		if err := json.Unmarshal(info[f], &refs); err != nil {
			t.Fatalf("%s.%s = %s: %v", infoKey, f, info[f], err)
		}
		for _, r := range refs {
			out[f] = append(out[f], r.Asset)
		}
	}
	return out
}

const reusedAsset = "customers/1234567890/assets/901"

// Display: one square image as the square marketing image AND the square logo.
func TestCreateDisplayCampaign_ReusedImageFillsBothSlots(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := contentAssets()
	adH, readAd := capturedMutate(1, displayAdResource)
	images := displayImages(t)
	images["/sqlogo.png"] = images["/sq.png"]
	srv := demandGenTLSServer(t, displayCascade(t, images, adGroupH, assetH, adH))
	in := displayInput(srv.URL)
	in.DisplayCreative.SquareLogoImages = []string{srv.URL + "/sqlogo.png"}

	res, err := demandGenClient(t, srv).CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("a reused image must not stop the ad: %v", err)
	}
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want the ad created", res.AdID)
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900,901,902" {
		t.Errorf("CreativeAssetIDs = %q, want each id once: 900,901,902", got)
	}
	refs := adAssetRefs(t, readAd(), "responsiveDisplayAd",
		"squareMarketingImages", "squareLogoImages")
	for f, got := range refs {
		if !reflect.DeepEqual(got, []string{reusedAsset}) {
			t.Errorf("%s = %v, want the reused asset %s", f, got, reusedAsset)
		}
	}
}

// Demand Gen image ad: one square image as the square marketing image AND the logo.
func TestCreateDemandGenCampaign_ReusedImageFillsBothSlots(t *testing.T) {
	assetH, _ := contentAssets()
	adH, readAd := capturedMutate(1, func(int) string {
		return "customers/1234567890/adGroupAds/333~444"
	})
	square := pngOf(t, 400, 400)
	images := map[string][]byte{
		"/m.png": pngOf(t, 1200, 628), "/sq.png": square, "/logo.png": square,
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, images, assetH, adH))
	creative := creativeAt(srv.URL)
	creative.SquareMarketingImages = []string{srv.URL + "/sq.png"}

	res, err := demandGenClient(t, srv).CreateDemandGenCampaign(context.Background(),
		creativeInput(creative))
	if err != nil {
		t.Fatalf("a reused image must not stop the ad: %v", err)
	}
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want the ad created", res.AdID)
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900,901" {
		t.Errorf("CreativeAssetIDs = %q, want each id once: 900,901", got)
	}
	refs := adAssetRefs(t, readAd(), "demandGenMultiAssetAd", "squareMarketingImages", "logoImages")
	for f, got := range refs {
		if !reflect.DeepEqual(got, []string{reusedAsset}) {
			t.Errorf("%s = %v, want the reused asset %s", f, got, reusedAsset)
		}
	}
}

// Performance Max: one square image as the square marketing image AND the logo. The group is
// created, both links name the one asset under their own field types, and the link count is
// every link sent.
func TestCreatePerformanceMaxCampaign_ReusedImageFillsBothSlots(t *testing.T) {
	assetH, _ := contentAssets()
	groupH, readGroup := okPMaxGroup()
	images := pmaxImages(t)
	images["/logo.png"] = images["/sq.png"]
	srv := demandGenTLSServer(t, pmaxCascade(t, images, assetH, groupH))
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := demandGenClient(t, srv).CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("a reused image must not stop the asset group: %v", err)
	}
	if res.AssetGroupID != "555" {
		t.Errorf("AssetGroupID = %q, want the group created", res.AssetGroupID)
	}
	// Seven text assets (900-906), the marketing image (907), then the square image (908)
	// named again for the logo.
	var want []string
	for i := 900; i <= 908; i++ {
		want = append(want, fmt.Sprint(i))
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != strings.Join(want, ",") {
		t.Errorf("CreativeAssetIDs = %q, want each id once: %s", got, strings.Join(want, ","))
	}
	var body groupMutateBody
	if err := json.Unmarshal([]byte(readGroup()), &body); err != nil {
		t.Fatal(err)
	}
	fields := map[string]bool{}
	links := 0
	for _, op := range body.MutateOperations {
		if op.AssetGroupAssetOperation == nil {
			continue
		}
		links++
		if op.AssetGroupAssetOperation.Create["asset"] == "customers/1234567890/assets/908" {
			fields[fmt.Sprint(op.AssetGroupAssetOperation.Create["fieldType"])] = true
		}
	}
	if links != pmaxAssetCount || !fields[assetFieldSquareMarketingImage] ||
		!fields[assetFieldLogo] {
		t.Errorf("links = %d, fields linking asset 908 = %v; want %d links, the asset as "+
			"both the square marketing image and the logo", links, fields, pmaxAssetCount)
	}
	if want := fmt.Sprintf("%d asset(s) linked", pmaxAssetCount); !strings.Contains(
		strings.Join(res.Steps, "\n"), want) {
		t.Errorf("steps = %v, want %q: every link counted", res.Steps, want)
	}
}

// Video: the same YouTube video named twice is one asset, referenced at both positions. The
// preflight already refuses a YouTube id listed twice, so the loop is driven directly: this
// pins its contract, not a path a validated input reaches.
func TestCreateVideoAd_ReusedVideoFillsBothPositions(t *testing.T) {
	assetH, _ := contentAssets()
	adH, readAd := capturedMutate(1, func(int) string {
		return "customers/1234567890/adGroupAds/333~444"
	})
	c := createVideoAdClient(t, assetH, adH)
	plan := twoVideoPlan(t)
	plan.videos = []string{plan.videos[0], plan.videos[0]}
	assetIDs, adID, err := c.createVideoAd(context.Background(), repeatAdGroup, "333",
		"https://example.org/register", plan)
	if err != nil {
		t.Fatalf("a reused video must not stop the ad: %v", err)
	}
	if adID != "444" || !reflect.DeepEqual(assetIDs, []string{"900"}) {
		t.Errorf("assetIDs = %v, adID = %q; want 900 once and the ad", assetIDs, adID)
	}
	refs := adAssetRefs(t, readAd(), "videoResponsiveAd", "videos")
	want := []string{"customers/1234567890/assets/900", "customers/1234567890/assets/900"}
	if !reflect.DeepEqual(refs["videos"], want) {
		t.Errorf("videos = %v, want the asset at both positions", refs["videos"])
	}
}

// The Demand Gen video ad's atomic request: the same YouTube video twice comes back as one
// asset at both indexes, and the ad, created in the same request, is confirmed. As for Video,
// the preflight refuses a YouTube id listed twice, so the loop is driven directly.
func TestCreateDemandGenVideoAd_ReusedVideoIsConfirmed(t *testing.T) {
	const (
		video950 = `{"assetResult":{"resourceName":"customers/1234567890/assets/950"}}`
		cta952   = `{"assetResult":{"resourceName":"customers/1234567890/assets/952"}}`
		ad       = `{"adGroupAdResult":{"resourceName":"customers/1234567890/adGroupAds/333~444"}}`
	)
	_, c, base := newDGRig(t, videoLogo(t), atomicAnswer(
		`{"mutateOperationResponses":[`+video950+`,`+video950+`,`+cta952+`,`+ad+`]}`))
	plan, err := validateDemandGenVideoCreative(campaignKindDemandGen, dgVideoInput(base))
	if err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	plan.videos = []string{plan.videos[0], plan.videos[0]}
	logos := []fetchedImage{{data: videoLogo(t)["/vlogo.png"]}}
	assetIDs, adID, err := c.createDemandGenVideoAd(context.Background(), repeatAdGroup, "333",
		"KubeCon - Video", "https://example.org/register", plan, logos)
	if err != nil {
		t.Fatalf("a reused video must not leave the ad unconfirmed: %v", err)
	}
	if adID != "444" || strings.Join(assetIDs, ",") != "900,950,952" {
		t.Errorf("assetIDs = %v, adID = %q; want 900,950,952 and the ad", assetIDs, adID)
	}
}

// contentSearchReply answers the Search assets' googleAds:mutate the way Google answers
// duplicate content: each distinct image or text gets the next id from 800, a repeat gets the
// earlier id, and each link names the id its temporary asset resolved to.
func contentSearchReply(body []byte) (int, string) {
	var req struct {
		MutateOperations []searchAssetOp `json:"mutateOperations"`
	}
	_ = json.Unmarshal(body, &req)
	byContent, byTemp := map[string]int{}, map[string]int{}
	parts := make([]string, 0, len(req.MutateOperations))
	for _, op := range req.MutateOperations {
		switch {
		case op.AssetOperation != nil:
			a := op.AssetOperation.Create
			key := ""
			if a.ImageAsset != nil {
				key = "image:" + a.ImageAsset.Data
			}
			if a.TextAsset != nil {
				key = "text:" + a.TextAsset.Text
			}
			id, ok := byContent[key]
			if !ok {
				id = 800 + len(byContent)
				byContent[key] = id
			}
			byTemp[a.ResourceName] = id
			parts = append(parts, assetResultJSON(fmt.Sprintf("customers/1234567890/assets/%d", id)))
		case op.CampaignAssetOperation != nil:
			l := op.CampaignAssetOperation.Create
			parts = append(parts, campaignAssetResultJSON(fmt.Sprintf(
				"customers/1234567890/campaignAssets/222~%d~%s", byTemp[l.Asset], l.FieldType)))
		}
	}
	return http.StatusOK, searchAssetMutateResponse(parts...)
}

// Search campaign assets: one square image as an ad image AND the business logo.
func TestCreateCampaign_SearchReusedImageFillsBothFields(t *testing.T) {
	s := &searchAssetServer{googleReply: contentSearchReply}
	in := searchAssetInput(t)
	in.ResolvedImages[searchLogoRef] = in.ResolvedImages[searchImageRef1]
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("a reused image must not stop the Search assets: %v", err)
	}
	if got := res.SearchAssetIDs; !reflect.DeepEqual(got, []string{"800", "801", "802"}) {
		t.Errorf("SearchAssetIDs = %v, want each id once: [800 801 802]", got)
	}
	if res.AdGroupID == "" {
		t.Error("the ad group must follow the confirmed Search assets")
	}
	_, googleAds, _ := s.snapshot()
	ops := decodeSearchAssetOps(t, googleAds[0])
	if len(ops) != 8 {
		t.Fatalf("got %d operations, want 4 assets + 4 links", len(ops))
	}
	// The ad image at index 0 and the logo at index 2 are linked under their own field types.
	for i, field := range map[int]string{4: "AD_IMAGE", 6: "BUSINESS_LOGO"} {
		if l := ops[i].CampaignAssetOperation; l == nil || l.Create.FieldType != field {
			t.Errorf("op %d = %+v, want a %s link", i, ops[i], field)
		}
	}
}

// A Search asset result repeating an id for DIFFERENT content is still UNCONFIRMED: no
// operation sent that content twice, so one is unaccounted for.
func TestCreateCampaign_SearchRepeatedAssetOfOtherContentIsUnconfirmed(t *testing.T) {
	img := assetResultJSON("customers/1234567890/assets/800")
	link := func(field string) string {
		return campaignAssetResultJSON("customers/1234567890/campaignAssets/222~800~" + field)
	}
	s := &searchAssetServer{googleReply: func([]byte) (int, string) {
		return http.StatusOK, searchAssetMutateResponse(img, img, link("AD_IMAGE"),
			link("BUSINESS_NAME"))
	}}
	in := sampleInput()
	in.SearchAssets = SearchCampaignAssets{
		AdImages: []string{searchImageRef1}, BusinessName: "Linux Foundation",
	}
	in.ResolvedImages = map[string][]byte{searchImageRef1: pngOf(t, 300, 300)}
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	wantRepeat(t, err)
	if res == nil || !reflect.DeepEqual(res.SearchAssetIDs, []string{"800"}) ||
		res.AdGroupID != "" {
		t.Errorf("res = %+v, want 800 once and no ad group", res)
	}
}

// ---------------------------------------------------------------------------
// The same bytes twice in ONE slot (#487 review)
// ---------------------------------------------------------------------------

// Two different entries carrying identical bytes in the SAME slot resolve to one asset, so the
// slot would carry it twice: a repeated campaign link on Search, a repeated asset-group link on
// Performance Max, a repeated asset in one field of the Display or Demand Gen ad. Google refuses
// each after the budget and campaign exist, so fetchSlotImages refuses it before any mutate.
// Reuse ACROSS slots stays accepted: the ReusedImageFillsBothSlots tests above.

const sameSlotRefusal = "are the same image (identical bytes)"

// mutateRecorder wraps a cascade handler and records every API mutate it is asked for, so a
// test can assert the refusal came before the first one.
func mutateRecorder(h http.HandlerFunc) (http.HandlerFunc, func() []string) {
	var (
		mu    sync.Mutex
		paths []string
	)
	read := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":mutate") {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
		}
		h(w, r)
	}, read
}

// wantSameSlotRefusal checks the refusal names the slot and both entry positions, and that
// no mutate was sent.
func wantSameSlotRefusal(t *testing.T, err error, label string, mutates []string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sameSlotRefusal) ||
		!strings.Contains(err.Error(), label+" 0 (") ||
		!strings.Contains(err.Error(), ") and 1 (") {
		t.Fatalf("err = %v, want the %s 0 and 1 same-image refusal", err, label)
	}
	if len(mutates) != 0 {
		t.Errorf("mutates sent = %v, want none before the refusal", mutates)
	}
}

func TestCreateCampaign_SearchSameBytesInOneSlotRefusedBeforeAnyRequest(t *testing.T) {
	s := &searchAssetServer{}
	in := searchAssetInput(t)
	in.ResolvedImages[searchImageRef2] = in.ResolvedImages[searchImageRef1]
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if res != nil {
		t.Errorf("res = %+v, want none", res)
	}
	paths, _, _ := s.snapshot()
	wantSameSlotRefusal(t, err, "Search ad image", paths)
}

func TestCreateDisplayCampaign_SameBytesInOneSlotRefusedBeforeAnyRequest(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := contentAssets()
	adH, _ := capturedMutate(1, displayAdResource)
	images := displayImages(t)
	images["/m2.png"] = images["/m.png"]
	h, mutates := mutateRecorder(displayCascade(t, images, adGroupH, assetH, adH))
	srv := demandGenTLSServer(t, h)
	in := displayInput(srv.URL)
	in.DisplayCreative.MarketingImages = []string{srv.URL + "/m.png", srv.URL + "/m2.png"}

	res, err := demandGenClient(t, srv).CreateDisplayCampaign(context.Background(), in)
	if res != nil {
		t.Errorf("res = %+v, want none", res)
	}
	wantSameSlotRefusal(t, err, "Display marketing image", mutates())
}

func TestCreateDemandGenCampaign_SameBytesInOneSlotRefusedBeforeAnyRequest(t *testing.T) {
	assetH, _ := contentAssets()
	adH, _ := capturedMutate(1, func(int) string {
		return "customers/1234567890/adGroupAds/333~444"
	})
	images := twoImages(t)
	images["/m2.png"] = images["/m.png"]
	h, mutates := mutateRecorder(demandGenCascade(t, images, assetH, adH))
	srv := demandGenTLSServer(t, h)
	creative := creativeAt(srv.URL)
	creative.MarketingImages = []string{srv.URL + "/m.png", srv.URL + "/m2.png"}

	res, err := demandGenClient(t, srv).CreateDemandGenCampaign(context.Background(),
		creativeInput(creative))
	if res != nil {
		t.Errorf("res = %+v, want none", res)
	}
	wantSameSlotRefusal(t, err, "Demand Gen marketing image", mutates())
}

func TestCreatePerformanceMaxCampaign_SameBytesInOneSlotRefusedBeforeAnyRequest(t *testing.T) {
	assetH, _ := contentAssets()
	groupH, _ := okPMaxGroup()
	images := pmaxImages(t)
	images["/m2.png"] = images["/m.png"]
	h, mutates := mutateRecorder(pmaxCascade(t, images, assetH, groupH))
	srv := demandGenTLSServer(t, h)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)
	in.PerformanceMaxCreative.MarketingImages = []string{srv.URL + "/m.png", srv.URL + "/m2.png"}

	res, err := demandGenClient(t, srv).CreatePerformanceMaxCampaign(context.Background(), in)
	if res != nil {
		t.Errorf("res = %+v, want none", res)
	}
	wantSameSlotRefusal(t, err, "Performance Max marketing image", mutates())
}

// The guard itself: a repeat inside one slot is refused naming both positions, whether the
// entries are stored assets, URLs or one of each; the same bytes in two slots are accepted,
// each slot keeping its own entry.
func TestFetchSlotImages_RefusesSameBytesInOneSlotOnly(t *testing.T) {
	square := pngOf(t, 400, 400)
	other := pngOf(t, 300, 300)
	resolved := map[string][]byte{
		searchImageRef1: square, searchImageRef2: square, searchLogoRef: square,
	}
	c := NewClient(testCreds(), testAccount())

	_, err := c.fetchSlotImages(context.Background(), searchImageSlots,
		[][]string{{searchImageRef1, searchImageRef2}, nil}, resolved)
	wantSameSlotRefusal(t, err, "Search ad image", nil)

	got, err := c.fetchSlotImages(context.Background(), searchImageSlots,
		[][]string{{searchImageRef1}, {searchLogoRef}}, resolved)
	if err != nil || len(got) != 2 || got[0].slot != 0 || got[1].slot != 1 {
		t.Fatalf("got %+v, err = %v; want the one image accepted in both slots", got, err)
	}

	// Two copies of one slot in the walk are two groups' or ads' creatives (Display and Demand
	// Gen fetch every group in one walk), so the same image in each is accepted.
	adImage := searchImageSlots[searchAdImageSlotIndex]
	got, err = c.fetchSlotImages(context.Background(), []imageSlot{adImage, adImage},
		[][]string{{searchImageRef1}, {searchImageRef2}}, resolved)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d image(s), err = %v; want one image in each group accepted", len(got), err)
	}

	resolved[searchImageRef2] = other
	got, err = c.fetchSlotImages(context.Background(), searchImageSlots,
		[][]string{{searchImageRef1, searchImageRef2}, nil}, resolved)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d image(s), err = %v; want two different images accepted", len(got), err)
	}
}

// One fetched URL and one stored asset with the same bytes are the same image too.
func TestFetchSlotImages_RefusesAURLAndAStoredAssetWithTheSameBytes(t *testing.T) {
	square := pngOf(t, 400, 400)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(square)
	})
	c := demandGenClient(t, srv)
	_, err := c.fetchSlotImages(context.Background(), searchImageSlots,
		[][]string{{searchImageRef1, srv.URL + "/a.png"}, nil},
		map[string][]byte{searchImageRef1: square})
	wantSameSlotRefusal(t, err, "Search ad image", nil)
}
