// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fullVideoCreative is a Demand Gen video creative that passes every local rule, its
// logo served at base + "/vlogo.png".
func fullVideoCreative(base string) DemandGenVideoCreative {
	return DemandGenVideoCreative{
		YouTubeVideoIDs: []string{"dQw4w9WgXcQ"},
		LogoImages:      []string{base + "/vlogo.png"},
		Headlines:       []string{"KubeCon NA 2026"},
		LongHeadlines:   []string{"Join the cloud native community in Salt Lake City"},
		Descriptions:    []string{"Four days of talks and workshops."},
		BusinessName:    "Linux Foundation",
		CallToAction:    "SIGN_UP",
	}
}

// dgRig serves the whole Demand Gen cascade over TLS and records every mutate. Ad
// groups are numbered 333, 334, … in creation order; criteria echo their own ad group;
// assets:mutate answers one asset per operation (ids 900 + running count); the
// googleAds:mutate answer is the test's, as is any override by path suffix.
type dgRig struct {
	mu        sync.Mutex
	paths     []string
	bodies    [][]byte
	adGroups  int
	assets    int
	overrides map[string]func(call int, body []byte, w http.ResponseWriter)
	calls     map[string]int
}

func (r *dgRig) sent(suffix string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for i, p := range r.paths {
		if strings.HasSuffix(p, "/"+suffix) {
			out = append(out, r.bodies[i])
		}
	}
	return out
}

func (r *dgRig) mutatePaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.paths {
		if i := strings.LastIndex(p, "/"); i >= 0 && strings.HasSuffix(p, ":mutate") {
			out = append(out, p[i+1:])
		}
	}
	return out
}

// okGoogleAdsMutate echoes a googleAds:mutate: an assetResult per assetOperation (ids
// 950, 951, …) and an adGroupAdResult naming the ad group the ad was created under.
func okGoogleAdsMutate(body []byte, w http.ResponseWriter) {
	var req struct {
		MutateOperations []struct {
			AssetOperation     json.RawMessage `json:"assetOperation"`
			AdGroupAdOperation *struct {
				Create struct {
					AdGroup string `json:"adGroup"`
				} `json:"create"`
			} `json:"adGroupAdOperation"`
		} `json:"mutateOperations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.MutateOperations))
	for i, op := range req.MutateOperations {
		if op.AdGroupAdOperation != nil {
			adGroupID := op.AdGroupAdOperation.Create.AdGroup[strings.LastIndex(op.AdGroupAdOperation.Create.AdGroup, "/")+1:]
			parts = append(parts, fmt.Sprintf(`{"adGroupAdResult":{"resourceName":"customers/1234567890/adGroupAds/%s~444"}}`, adGroupID))
			continue
		}
		parts = append(parts, fmt.Sprintf(`{"assetResult":{"resourceName":"customers/1234567890/assets/%d"}}`, 950+i))
	}
	_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
}

func newDGRig(t *testing.T, images map[string][]byte, overrides map[string]func(call int, body []byte, w http.ResponseWriter)) (*dgRig, *Client, string) {
	t.Helper()
	rig := &dgRig{overrides: overrides, calls: map[string]int{}}
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if img, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(img)
			return
		}
		body, _ := io.ReadAll(r.Body)
		suffix := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		rig.mu.Lock()
		rig.paths = append(rig.paths, r.URL.Path)
		rig.bodies = append(rig.bodies, body)
		call := rig.calls[suffix]
		rig.calls[suffix]++
		rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if h, ok := overrides[suffix]; ok {
			h(call, body, w)
			return
		}
		switch suffix {
		case "campaignBudgets:mutate":
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case "campaigns:mutate":
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case "adGroups:mutate":
			rig.mu.Lock()
			id := 333 + rig.adGroups
			rig.adGroups++
			rig.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/%d"}]}`, id)
		case "adGroupCriteria:mutate":
			var req struct {
				Operations []struct {
					Create struct {
						AdGroup string `json:"adGroup"`
					} `json:"create"`
				} `json:"operations"`
			}
			_ = json.Unmarshal(body, &req)
			parts := make([]string, 0, len(req.Operations))
			for i, op := range req.Operations {
				g := op.Create.AdGroup[strings.LastIndex(op.Create.AdGroup, "/")+1:]
				parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/adGroupCriteria/%s~%d"}`, g, 800+i))
			}
			_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
		case "assets:mutate":
			var req struct {
				Operations []json.RawMessage `json:"operations"`
			}
			_ = json.Unmarshal(body, &req)
			parts := make([]string, 0, len(req.Operations))
			rig.mu.Lock()
			for range req.Operations {
				parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/assets/%d"}`, 900+rig.assets))
				rig.assets++
			}
			rig.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
		case "googleAds:mutate":
			okGoogleAdsMutate(body, w)
		case "adGroupAds:mutate":
			var req struct {
				Operations []struct {
					Create struct {
						AdGroup string `json:"adGroup"`
					} `json:"create"`
				} `json:"operations"`
			}
			_ = json.Unmarshal(body, &req)
			g := "0"
			if len(req.Operations) == 1 {
				g = req.Operations[0].Create.AdGroup[strings.LastIndex(req.Operations[0].Create.AdGroup, "/")+1:]
			}
			_, _ = fmt.Fprintf(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/%s~555"}]}`, g)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
	return rig, demandGenClient(t, srv), srv.URL
}

func videoLogo(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{"/vlogo.png": pngOf(t, 900, 900)}
}

func dgVideoInput(base string) CampaignInput {
	in := demandGenInput()
	in.DemandGenVideoCreative = fullVideoCreative(base)
	return in
}

// ---------------------------------------------------------------------------
// validateDemandGenVideoCreative
// ---------------------------------------------------------------------------

func TestValidateDemandGenVideoCreative_AbsentIsAccepted(t *testing.T) {
	plan, err := validateDemandGenVideoCreative(campaignKindDemandGen, demandGenInput())
	if err != nil || plan.present {
		t.Fatalf("absent video creative: plan.present=%v err=%v, want false/nil", plan.present, err)
	}
}

// Off Demand Gen it is refused, never dropped, on every other kind — driven through the
// whole preflight so the refusal is proven reachable.
func TestPreflightRefusesDemandGenVideoCreativeOffDemandGen(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for _, kind := range []string{campaignKindSearch, campaignKindPerformanceMax, campaignKindVideo, campaignKindDisplay} {
		t.Run(kind, func(t *testing.T) {
			in := dgVideoInput("https://cdn.example.org")
			if kind == campaignKindPerformanceMax {
				in.PerformanceMaxCreative = fullPMaxCreative()
			}
			_, err := c.preflightCampaignKind(kind, in)
			if err == nil || !strings.Contains(err.Error(), "supported on Demand Gen campaigns only, not on "+kind) {
				t.Errorf("preflight(%s) = %v, want the Demand Gen video refusal naming the channel", kind, err)
			}
		})
	}
}

// An image AND a video creative on the one ad group is refused, naming the way out.
func TestValidateDemandGenVideoCreative_RefusedBesideTheImageCreative(t *testing.T) {
	in := dgVideoInput("https://cdn.example.org")
	in.DemandGenCreative = fullCreative()
	_, err := validateDemandGenVideoCreative(campaignKindDemandGen, in)
	if err == nil || !strings.Contains(err.Error(), "both a Demand Gen image creative and a Demand Gen video creative") {
		t.Fatalf("err = %v, want the two-creatives refusal", err)
	}
}

func TestValidateDemandGenVideoCreative_Bounds(t *testing.T) {
	const base = "https://cdn.example.org"
	cases := map[string]struct {
		mutate  func(v *DemandGenVideoCreative)
		wantSub string
	}{
		"no video":              {func(v *DemandGenVideoCreative) { v.YouTubeVideoIDs = nil }, "needs at least 1 YouTube video"},
		"video URL":             {func(v *DemandGenVideoCreative) { v.YouTubeVideoIDs = []string{"https://youtu.be/dQw4w9WgXcQ"} }, "looks like a URL"},
		"bad video char":        {func(v *DemandGenVideoCreative) { v.YouTubeVideoIDs = []string{"dQw4w9WgX!Q"} }, "not a YouTube id character"},
		"six videos":            {func(v *DemandGenVideoCreative) { v.YouTubeVideoIDs = videoIDs(6) }, "at most 5 YouTube videos"},
		"no logo":               {func(v *DemandGenVideoCreative) { v.LogoImages = nil }, "needs exactly 1 logo image, got 0"},
		"two logos":             {func(v *DemandGenVideoCreative) { v.LogoImages = []string{base + "/a.png", base + "/b.png"} }, "needs exactly 1 logo image, got 2"},
		"http logo":             {func(v *DemandGenVideoCreative) { v.LogoImages = []string{"http://cdn.example.org/a.png"} }, "Demand Gen video logo image 0 must be an https URL"},
		"no headline":           {func(v *DemandGenVideoCreative) { v.Headlines = nil }, "needs at least 1 headline"},
		"wide headline":         {func(v *DemandGenVideoCreative) { v.Headlines = []string{strings.Repeat("a", 31)} }, "exceeding the 30 limit"},
		"no long headline":      {func(v *DemandGenVideoCreative) { v.LongHeadlines = nil }, "needs at least 1 long headline"},
		"wide long headline":    {func(v *DemandGenVideoCreative) { v.LongHeadlines = []string{strings.Repeat("a", 91)} }, "exceeding the 90 limit"},
		"six long headlines":    {func(v *DemandGenVideoCreative) { v.LongHeadlines = repeatText("Long", 6) }, "at most 5 long headlines"},
		"wide description":      {func(v *DemandGenVideoCreative) { v.Descriptions = []string{strings.Repeat("a", 91)} }, "exceeding the 90 limit"},
		"no business name":      {func(v *DemandGenVideoCreative) { v.BusinessName = " " }, "requires a business name"},
		"wide business name":    {func(v *DemandGenVideoCreative) { v.BusinessName = strings.Repeat("a", 26) }, "exceeding the 25 limit"},
		"unlisted CTA":          {func(v *DemandGenVideoCreative) { v.CallToAction = "REGISTER" }, `call to action "REGISTER" is not one this client accepts`},
		"lower-case CTA":        {func(v *DemandGenVideoCreative) { v.CallToAction = "sign_up" }, "is not one this client accepts"},
		"duplicate description": {func(v *DemandGenVideoCreative) { v.Descriptions = []string{"Same", "same"} }, "listed more than once"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := fullVideoCreative(base)
			tc.mutate(&v)
			in := demandGenInput()
			in.DemandGenVideoCreative = v
			_, err := validateDemandGenVideoCreative(campaignKindDemandGen, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateDemandGenVideoCreative_PlanCarriesEveryField(t *testing.T) {
	v := fullVideoCreative("https://cdn.example.org")
	v.CallToAction = " LEARN_MORE "
	v.BusinessName = " Linux Foundation "
	in := demandGenInput()
	in.DemandGenVideoCreative = v
	plan, err := validateDemandGenVideoCreative(campaignKindDemandGen, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.present || len(plan.videos) != 1 || len(plan.logos) != 1 || len(plan.headlines) != 1 ||
		len(plan.longHeadlines) != 1 || len(plan.descriptions) != 1 ||
		plan.businessName != "Linux Foundation" || plan.callToAction != "LEARN_MORE" {
		t.Errorf("plan = %+v", plan)
	}
	// No call to action is not an error: the field is optional.
	in.DemandGenVideoCreative.CallToAction = ""
	if plan, err = validateDemandGenVideoCreative(campaignKindDemandGen, in); err != nil || plan.callToAction != "" {
		t.Errorf("no CTA: plan.callToAction=%q err=%v", plan.callToAction, err)
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// The exact payloads: the logo by assets:mutate, then ONE googleAds:mutate creating the
// YouTube asset and the call-to-action asset by temporary name and the ad that names
// them — the shape the 2026-11-03 validateOnly probe accepted, ad.name included.
func TestCreateDemandGenCampaign_VideoAdPayload(t *testing.T) {
	rig, c, base := newDGRig(t, videoLogo(t), nil)
	res, err := c.CreateDemandGenCampaign(context.Background(), dgVideoInput(base))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(rig.mutatePaths(), ","), "campaignBudgets:mutate,campaigns:mutate,adGroups:mutate,assets:mutate,googleAds:mutate"; got != want {
		t.Errorf("mutates = %s\nwant      %s", got, want)
	}
	logoReq := rig.sent("assets:mutate")[0]
	var logo struct {
		Operations []struct {
			Create struct {
				ImageAsset struct {
					Data string `json:"data"`
				} `json:"imageAsset"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(logoReq, &logo); err != nil || len(logo.Operations) != 1 || logo.Operations[0].Create.ImageAsset.Data != base64Image(pngOf(t, 900, 900)) {
		t.Errorf("logo asset mutate does not carry the logo's bytes: %.200s", logoReq)
	}
	jsonEqual(t, rig.sent("googleAds:mutate")[0], `{"mutateOperations":[
		{"assetOperation":{"create":{"resourceName":"customers/1234567890/assets/-1","youtubeVideoAsset":{"youtubeVideoId":"dQw4w9WgXcQ"}}}},
		{"assetOperation":{"create":{"resourceName":"customers/1234567890/assets/-2","callToActionAsset":{"callToAction":"SIGN_UP"}}}},
		{"adGroupAdOperation":{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"PAUSED","ad":{
			"name":"KubeCon Europe 2026 - Display - Video ad",
			"finalUrls":["https://events.linuxfoundation.org/kubecon-eu-2026/?utm_campaign=kubecon-eu-2026&utm_content=tlf&utm_medium=cpc&utm_source=google"],
			"demandGenVideoResponsiveAd":{
				"headlines":[{"text":"KubeCon NA 2026"}],
				"longHeadlines":[{"text":"Join the cloud native community in Salt Lake City"}],
				"descriptions":[{"text":"Four days of talks and workshops."}],
				"videos":[{"asset":"customers/1234567890/assets/-1"}],
				"logoImages":[{"asset":"customers/1234567890/assets/900"}],
				"businessName":{"text":"Linux Foundation"},
				"callToActions":[{"asset":"customers/1234567890/assets/-2"}]}}}}}]}`)
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want 444", res.AdID)
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900,950,951" {
		t.Errorf("CreativeAssetIDs = %s, want the logo, the video, then the CTA: 900,950,951", got)
	}
	if !containsStep(res.Steps, "Demand Gen video ad created: 444 (PAUSED, 1 YouTube video(s), 1 logo(s), 1 headline(s), 1 long headline(s), 1 description(s), call to action SIGN_UP)") {
		t.Errorf("steps do not report the video ad:\n%s", strings.Join(res.Steps, "\n"))
	}
	if last := res.Steps[len(res.Steps)-1]; !strings.Contains(last, "(no geo targeting set) — add geo targeting, then review the creative") {
		t.Errorf("closing step = %q, want the has-ad, no-geo wording", last)
	}
	if res.AdGroups != nil {
		t.Errorf("a single Demand Gen group must not populate AdGroups, got %+v", res.AdGroups)
	}
}

// Without a call to action no CTA asset is created and the ad carries no callToActions.
func TestCreateDemandGenCampaign_VideoAdWithoutCallToAction(t *testing.T) {
	rig, c, base := newDGRig(t, videoLogo(t), nil)
	in := dgVideoInput(base)
	in.DemandGenVideoCreative.CallToAction = ""
	if _, err := c.CreateDemandGenCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := string(rig.sent("googleAds:mutate")[0])
	if strings.Contains(body, "callToAction") {
		t.Errorf("no call to action was asked for, but the mutate carries one: %s", body)
	}
	if n := strings.Count(body, `"assetOperation"`); n != 1 {
		t.Errorf("got %d asset operations, want the one YouTube asset", n)
	}
}

// A logo that misses its shape fails BEFORE any mutate: (nil, err).
func TestCreateDemandGenCampaign_BadVideoLogoFailsBeforeAnyMutate(t *testing.T) {
	rig, c, base := newDGRig(t, map[string][]byte{"/vlogo.png": pngOf(t, 900, 600)}, nil)
	res, err := c.CreateDemandGenCampaign(context.Background(), dgVideoInput(base))
	if err == nil || !strings.Contains(err.Error(), "Demand Gen video logo image") {
		t.Fatalf("err = %v, want the logo geometry refusal", err)
	}
	if res != nil || len(rig.mutatePaths()) != 0 {
		t.Errorf("res = %+v, mutates = %v; nothing may be sent for a bad logo", res, rig.mutatePaths())
	}
}

// Every failure of the two video-ad steps is a partial result carrying the campaign, the
// ad group and whatever asset ids are known, and an ambiguous one is UNCONFIRMED.
func TestCreateDemandGenCampaign_VideoAdFailuresArePartialResults(t *testing.T) {
	const wrongGroup = `{"mutateOperationResponses":[{"assetResult":{"resourceName":"customers/1234567890/assets/950"}},{"assetResult":{"resourceName":"customers/1234567890/assets/951"}},{"adGroupAdResult":{"resourceName":"customers/1234567890/adGroupAds/999~444"}}]}`
	cases := map[string]struct {
		overrides  map[string]func(int, []byte, http.ResponseWriter)
		wantSub    string
		wantAssets string
		noAtomic   bool
	}{
		"logo upload 5xx": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"assets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			}},
			wantSub: "logo asset creation UNCONFIRMED", noAtomic: true,
		},
		"logo upload 4xx": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"assets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`)
			}},
			wantSub: "logo asset creation failed", noAtomic: true,
		},
		"logo wrong account": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"assets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/9999999999/assets/900"}]}`)
			}},
			wantSub: "logo asset creation UNCONFIRMED", noAtomic: true,
		},
		"atomic 4xx": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`)
			}},
			wantSub: "one atomic request, so none of them exists", wantAssets: "900",
		},
		"atomic 5xx": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			}},
			wantSub: "creation UNCONFIRMED", wantAssets: "900",
		},
		"atomic short": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[{"assetResult":{"resourceName":"customers/1234567890/assets/950"}}]}`)
			}},
			wantSub: "malformed/short mutate response", wantAssets: "900",
		},
		"atomic wrong-account asset": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				_, _ = io.WriteString(w, strings.Replace(wrongGroup, "customers/1234567890/assets/950", "customers/9999999999/assets/950", 1))
			}},
			wantSub: "no usable asset resource name at index 0", wantAssets: "900",
		},
		"atomic ad in another ad group": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				_, _ = io.WriteString(w, wrongGroup)
			}},
			wantSub: "does not name an ad in that ad group", wantAssets: "900,950,951",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig, c, base := newDGRig(t, videoLogo(t), tc.overrides)
			res, err := c.CreateDemandGenCampaign(context.Background(), dgVideoInput(base))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			if res == nil || res.CampaignID != "222" || res.AdGroupID != "333" || res.AdID != "" {
				t.Fatalf("res = %+v, want the campaign and ad group and no ad", res)
			}
			if got := strings.Join(res.CreativeAssetIDs, ","); got != tc.wantAssets {
				t.Errorf("CreativeAssetIDs = %q, want %q", got, tc.wantAssets)
			}
			if n := len(rig.sent("googleAds:mutate")); tc.noAtomic && n != 0 {
				t.Errorf("the atomic mutate was sent after the logo upload failed")
			}
		})
	}
}

// The adoption path refuses exactly what the create path refuses.
func TestValidateCampaignInputKind_RefusesTheSameVideoCreative(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	in := dgVideoInput("https://cdn.example.org")
	in.DemandGenVideoCreative.CallToAction = "REGISTER"
	if err := c.ValidateCampaignInputKind(CampaignKindDemandGen, in); err == nil {
		t.Error("ValidateCampaignInputKind accepted a call to action the create path refuses")
	}
}
