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
	"sync/atomic"
	"testing"
)

// twoGroupInput is a Demand Gen input naming two ad groups — Retargeting with an image
// ad and Prospecting with a video ad — each with its own Audience, plus geo and a
// language that apply to both.
func twoGroupInput(base string) CampaignInput {
	in := demandGenInput()
	in.GeoTargets = []string{"US"}
	in.Languages = []string{"EN"}
	in.DemandGenAdGroups = []DemandGenAdGroup{
		{Name: "Retargeting", Audience: "customers/1234567890/audiences/41", Creative: creativeAt(base)},
		{Name: "Prospecting", Audience: "customers/1234567890/audiences/42", VideoCreative: fullVideoCreative(base)},
	}
	return in
}

func twoGroupImages(t *testing.T) map[string][]byte {
	t.Helper()
	images := twoImages(t)
	images["/vlogo.png"] = pngOf(t, 900, 900)
	return images
}

// ---------------------------------------------------------------------------
// validateDemandGenAdGroups
// ---------------------------------------------------------------------------

func TestValidateDemandGenAdGroups_Refusals(t *testing.T) {
	const base = "https://cdn.example.org"
	group := func(name string) DemandGenAdGroup {
		return DemandGenAdGroup{Name: name, Creative: fullCreative()}
	}
	cases := map[string]struct {
		kind    string
		mutate  func(in *CampaignInput)
		wantSub string
	}{
		"off demand gen": {campaignKindSearch, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
		}, "Demand Gen ad groups are not supported on Search Campaign"},
		// Each channel is pointed at its OWN ad-group shape (adGroupShapeHint), not at
		// Search's list: Display at DisplayAdGroups (#475).
		"off demand gen/search": {campaignKindSearch, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
		}, "omit DemandGenAdGroups (a Search campaign takes its own ad-group list, AdGroups)"},
		"off demand gen/display": {campaignKindDisplay, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
		}, "omit DemandGenAdGroups (a Display campaign takes its own ad-group list, " +
			"DisplayAdGroups)"},
		"off demand gen/video": {campaignKindVideo, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
		}, "omit DemandGenAdGroups (a Video campaign creates one ad group, from VideoCreative)"},
		"off demand gen/performance max": {campaignKindPerformanceMax, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
		}, "omit DemandGenAdGroups (a Performance Max campaign has no ad groups; its asset " +
			"group comes from PerformanceMaxCreative)"},
		"campaign-level image creative too": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
			in.DemandGenCreative = fullCreative()
		}, "must be empty"},
		"campaign-level video creative too": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
			in.DemandGenVideoCreative = fullVideoCreative(base)
		}, "must be empty"},
		"campaign-level audience too": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("A")}
			in.DemandGenAudience = "customers/1234567890/audiences/41"
		}, "must be empty"},
		"over the cap": {campaignKindDemandGen, func(in *CampaignInput) {
			for _, n := range []string{"A", "B", "C", "D", "E", "F"} {
				in.DemandGenAdGroups = append(in.DemandGenAdGroups, group(n))
			}
		}, "6 Demand Gen ad groups requested, limit 5"},
		"blank name": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group(" | ")}
		}, "ad group 1 has no name"},
		"duplicate name ignoring case and spacing": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("Retargeting"), group("  RETARGETING ")}
		}, "is used more than once"},
		// Simple case folding (adGroupNameKey), the key the Search and Display lists use:
		// LONG S folds with s and FINAL SIGMA with capital sigma, which strings.ToLower keeps
		// apart.
		"duplicate name under long s": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group("\u017fale"), group("SALE")}
		}, "is used more than once"},
		"duplicate name under final sigma": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{
				group("Kubecon\u03c2"), group("KUBECON\u03a3"),
			}
		}, "is used more than once"},
		// namedAdGroupNameTooLong, shared with Search and Display: the group by position,
		// and the advice names EventName — the only caller field in the Demand Gen base
		// (demandGenAdGroupName) — and the label, not validateEntityName's Project.
		"over-long name": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{group(strings.Repeat("a", 250))}
		}, "google-ads: Demand Gen ad group 1 name \"KubeCon Europe 2026 - Display | " +
			strings.Repeat("a", 250) + "\" is 282 characters, over Google's limit of 255; " +
			"shorten EventName or this group's name"},
		"no creative": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A"}}
		}, "has no creative"},
		"both creatives": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", Creative: fullCreative(), VideoCreative: fullVideoCreative(base)}}
		}, "both a Demand Gen image creative and a Demand Gen video creative"},
		"bad group creative": {campaignKindDemandGen, func(in *CampaignInput) {
			c := fullCreative()
			c.BusinessName = ""
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", Creative: c}}
		}, "requires a business name"},
		"bad group video": {campaignKindDemandGen, func(in *CampaignInput) {
			v := fullVideoCreative(base)
			v.YouTubeVideoIDs = nil
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", VideoCreative: v}}
		}, "needs at least 1 YouTube video"},
		"foreign audience": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", Audience: "customers/9999999999/audiences/41", Creative: fullCreative()}}
		}, "belongs to customer 9999999999"},
		"malformed audience": {campaignKindDemandGen, func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", Audience: "customers/1234567890/userLists/41", Creative: fullCreative()}}
		}, "not a customers/<id>/audiences/<id>"},
		// The Search list on Demand Gen names the Demand Gen one.
		"search ad-group list": {campaignKindDemandGen, func(in *CampaignInput) {
			in.AdGroups = []AdGroupSpec{{Name: "A"}}
		}, "a Demand Gen campaign takes DemandGenAdGroups"},
	}
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			tc.mutate(&in)
			_, err := c.preflightCampaignKind(tc.kind, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// A refusal inside a group names the group, and a bad Audience is called that group's
// Audience rather than the campaign-level DemandGenAudience the caller did not set.
func TestValidateDemandGenAdGroups_RefusalsNameTheGroup(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	const group = `Demand Gen ad group "KubeCon Europe 2026 - Display | Prospecting"`
	badImage := fullCreative()
	badImage.BusinessName = ""
	badVideo := fullVideoCreative("https://cdn.example.org")
	badVideo.CallToAction = "REGISTER"
	for name, g := range map[string]DemandGenAdGroup{
		"image":    {Creative: badImage},
		"video":    {VideoCreative: badVideo},
		"audience": {Creative: fullCreative(), Audience: "customers/9999999999/audiences/1"},
	} {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			g.Name = "Prospecting"
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "Retargeting", Creative: fullCreative()}, g}
			_, err := c.preflightCampaignKind(campaignKindDemandGen, in)
			if err == nil || !strings.Contains(err.Error(), group) {
				t.Fatalf("err = %v, want it to name %s", err, group)
			}
			if name == "audience" && (!strings.Contains(err.Error(), "the Audience of "+group) || strings.Contains(err.Error(), "DemandGenAudience")) {
				t.Errorf("err = %v, want it to call this the group's Audience, not DemandGenAudience", err)
			}
		})
	}
}

// Without the list the preflight resolves ONE group from the campaign-level fields,
// under the name the single ad group always had; with it, one group per entry, named
// "<that name> | <label>", each with its own Audience and creative.
func TestValidateDemandGenAdGroups_Plans(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	single := demandGenInput()
	single.DemandGenAudience = "customers/1234567890/audiences/41"
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, single)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(pf.demandGenGroups) != 1 || pf.demandGenGroups[0].name != "KubeCon Europe 2026 - Display" ||
		pf.demandGenGroups[0].audience != "customers/1234567890/audiences/41" || len(pf.demandGenGroups[0].ads) != 0 {
		t.Errorf("single group = %+v", pf.demandGenGroups)
	}

	pf, err = c.preflightCampaignKind(campaignKindDemandGen, twoGroupInput("https://cdn.example.org"))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(pf.demandGenGroups) != 2 {
		t.Fatalf("got %d groups, want 2", len(pf.demandGenGroups))
	}
	g0, g1 := pf.demandGenGroups[0], pf.demandGenGroups[1]
	if g0.name != "KubeCon Europe 2026 - Display | Retargeting" || g0.audience != "customers/1234567890/audiences/41" || len(g0.ads) != 1 || !g0.ads[0].creative.present || g0.ads[0].video.present {
		t.Errorf("group 0 = %+v", g0)
	}
	if g1.name != "KubeCon Europe 2026 - Display | Prospecting" || g1.audience != "customers/1234567890/audiences/42" || len(g1.ads) != 1 || g1.ads[0].creative.present || !g1.ads[0].video.present ||
		g1.ads[0].adName != "KubeCon Europe 2026 - Display | Prospecting - Video ad" {
		t.Errorf("group 1 = %+v", g1)
	}

	// Off Demand Gen there are no Demand Gen groups at all.
	if pf, err = c.preflightCampaignKind(campaignKindSearch, demandGenInput()); err != nil || pf.demandGenGroups != nil {
		t.Errorf("search: groups = %+v err = %v, want none", pf.demandGenGroups, err)
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// Two groups, built in order: each its own ad group, the campaign's geo and languages on
// BOTH, each its own Audience, each its own ad. The result has both in AdGroups, the
// scalars are the first group's, and every group step line names its group.
func TestCreateDemandGenCampaign_TwoAdGroups(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), nil)
	res, err := c.CreateDemandGenCampaign(context.Background(), twoGroupInput(base))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "campaignBudgets:mutate,campaigns:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate,assets:mutate,adGroupAds:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate,assets:mutate,googleAds:mutate"
	if got := strings.Join(rig.mutatePaths(), ","); got != want {
		t.Errorf("mutates =\n  %s\nwant\n  %s", got, want)
	}
	groups := rig.sent("adGroups:mutate")
	for i, name := range []string{"KubeCon Europe 2026 - Display | Retargeting", "KubeCon Europe 2026 - Display | Prospecting"} {
		jsonEqual(t, groups[i], `{"operations":[{"create":{"name":"`+name+`","campaign":"customers/1234567890/campaigns/222","status":"ENABLED"}}]}`)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	// Geo then audience+language, per group, each on its own ad group.
	for i, adGroup := range []string{"333", "333", "334", "334"} {
		if !strings.Contains(string(criteria[i]), `"adGroup":"customers/1234567890/adGroups/`+adGroup+`"`) {
			t.Errorf("criteria mutate %d is not on ad group %s: %s", i, adGroup, criteria[i])
		}
	}
	jsonEqual(t, criteria[1], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","audience":{"audience":"customers/1234567890/audiences/41"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","language":{"languageConstant":"languageConstants/1000"}}}]}`)
	jsonEqual(t, criteria[3], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/334","audience":{"audience":"customers/1234567890/audiences/42"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/334","language":{"languageConstant":"languageConstants/1000"}}}]}`)
	// The combined fetch re-indexes each image into its own creative's slot: group 1's
	// marketing image and logo land in their own arrays, group 2's logo in the video ad.
	ad := string(rig.sent("adGroupAds:mutate")[0])
	if !strings.Contains(ad, `"marketingImages":[{"asset":"customers/1234567890/assets/900"}]`) ||
		!strings.Contains(ad, `"logoImages":[{"asset":"customers/1234567890/assets/901"}]`) {
		t.Errorf("group 1's image ad does not reference its assets by slot: %s", ad)
	}
	if video := string(rig.sent("googleAds:mutate")[0]); !strings.Contains(video, `"adGroup":"customers/1234567890/adGroups/334"`) ||
		!strings.Contains(video, `"logoImages":[{"asset":"customers/1234567890/assets/902"}]`) ||
		!strings.Contains(video, `"name":"KubeCon Europe 2026 - Display | Prospecting - Video ad"`) {
		t.Errorf("group 2's video ad is not built on its own ad group and logo: %s", video)
	}
	// Click tracking (#476) covers every group: both ads carry Demand Gen's utm_medium,
	// and the one campaign create carries the utmcampaign parameter they inherit.
	video := string(rig.sent("googleAds:mutate")[0])
	for name, body := range map[string]string{"image ad": ad, "video ad": video} {
		if !strings.Contains(body, "utm_medium=display") || strings.Contains(body, "utm_medium=cpc") {
			t.Errorf("%s final URL lacks utm_medium=display: %s", name, body)
		}
	}
	const utmParam = `"urlCustomParameters":[{"key":"utmcampaign","value":"`
	if camp := string(rig.sent("campaigns:mutate")[0]); !strings.Contains(camp, utmParam) {
		t.Errorf("the campaign create lacks the utmcampaign custom parameter: %s", camp)
	}

	if res.AdGroupID != "333" || res.AdGroupName != "KubeCon Europe 2026 - Display | Retargeting" || res.AdID != "555" {
		t.Errorf("scalars = %s/%q/%s, want the first group's", res.AdGroupID, res.AdGroupName, res.AdID)
	}
	if len(res.AdGroups) != 2 {
		t.Fatalf("AdGroups = %+v, want 2 entries", res.AdGroups)
	}
	g0, g1 := res.AdGroups[0], res.AdGroups[1]
	if g0.ID != "333" || strings.Join(g0.AdIDs, ",") != "555" || strings.Join(g0.AudienceCriteriaIDs, ",") != "800" ||
		strings.Join(g0.TargetingCriterionIDs, ",") != "801" || len(g0.GeoCriterionIDs) != 1 || strings.Join(g0.CreativeAssetIDs, ",") != "900,901" {
		t.Errorf("group 0 = %+v", g0)
	}
	if g1.ID != "334" || strings.Join(g1.AdIDs, ",") != "444" || strings.Join(g1.AudienceCriteriaIDs, ",") != "800" ||
		len(g1.GeoCriterionIDs) != 1 || strings.Join(g1.CreativeAssetIDs, ",") != "902,950,951" {
		t.Errorf("group 1 = %+v", g1)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, line := range []string{
		`Ad group created: 333 [ad group "KubeCon Europe 2026 - Display | Retargeting"]`,
		`Ad group created: 334 [ad group "KubeCon Europe 2026 - Display | Prospecting"]`,
		`Demand Gen video ad created: 444 (PAUSED, 1 YouTube video(s), 1 logo(s), 1 headline(s), 1 long headline(s), 1 description(s), call to action SIGN_UP) [ad group "KubeCon Europe 2026 - Display | Prospecting"]`,
	} {
		if !strings.Contains(steps, line) {
			t.Errorf("steps lack %q:\n%s", line, steps)
		}
	}
	if last := res.Steps[len(res.Steps)-1]; last != "Demand Gen campaign created — review the creative and launch in the Google Ads UI" {
		t.Errorf("closing step = %q", last)
	}
}

// A failure on group k returns groups 1..k-1 complete and what exists of k, with the
// scalars still the first group's.
func TestCreateDemandGenCampaign_SecondGroupFailuresArePartial(t *testing.T) {
	cases := map[string]struct {
		overrides map[string]func(int, []byte, http.ResponseWriter)
		wantSub   string
		check     func(t *testing.T, g AdGroupResult)
	}{
		"second ad group refused": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"adGroups:mutate": func(call int, _ []byte, w http.ResponseWriter) {
				if call == 0 {
					_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`)
			}},
			wantSub: "ad group creation failed",
			check: func(t *testing.T, g AdGroupResult) {
				if g.Name != "KubeCon Europe 2026 - Display | Prospecting" || g.ID != "" || len(g.AdIDs) != 0 {
					t.Errorf("group 2 = %+v, want its name and nothing else", g)
				}
			},
		},
		"second group's audience unconfirmed": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": func(call int, body []byte, w http.ResponseWriter) {
				if call == 3 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var req struct {
					Operations []json.RawMessage `json:"operations"`
				}
				_ = json.Unmarshal(body, &req)
				g := "333"
				if call >= 2 {
					g = "334"
				}
				parts := make([]string, len(req.Operations))
				for i := range parts {
					parts[i] = `{"resourceName":"customers/1234567890/adGroupCriteria/` + g + `~80` + string(rune('0'+i)) + `"}`
				}
				_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
			}},
			wantSub: "audience/language targeting UNCONFIRMED",
			check: func(t *testing.T, g AdGroupResult) {
				if g.ID != "334" || len(g.GeoCriterionIDs) != 1 || len(g.AudienceCriteriaIDs) != 0 || len(g.AdIDs) != 0 {
					t.Errorf("group 2 = %+v, want its id and geo, and no audience or ad", g)
				}
			},
		},
		"second group's video ad unconfirmed": {
			overrides: map[string]func(int, []byte, http.ResponseWriter){"googleAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}},
			wantSub: "video ad \"KubeCon Europe 2026 - Display | Prospecting - Video ad\" creation UNCONFIRMED",
			check: func(t *testing.T, g AdGroupResult) {
				if g.ID != "334" || len(g.AudienceCriteriaIDs) != 1 || strings.Join(g.CreativeAssetIDs, ",") != "902" || len(g.AdIDs) != 0 {
					t.Errorf("group 2 = %+v, want its id, targeting and logo asset, and no ad", g)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, c, base := newDGRig(t, twoGroupImages(t), tc.overrides)
			res, err := c.CreateDemandGenCampaign(context.Background(), twoGroupInput(base))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			if res == nil || res.CampaignID != "222" || res.AdGroupID != "333" || res.AdID != "555" {
				t.Fatalf("res = %+v, want the campaign and the first group complete", res)
			}
			if len(res.AdGroups) != 2 || res.AdGroups[0].ID != "333" || strings.Join(res.AdGroups[0].AdIDs, ",") != "555" {
				t.Fatalf("AdGroups = %+v, want group 1 complete and an entry for group 2", res.AdGroups)
			}
			tc.check(t, res.AdGroups[1])
			if strings.Contains(strings.Join(res.Steps, "\n"), "Demand Gen campaign created") {
				t.Error("a failed create must not carry the closing step")
			}
		})
	}
}

// The 64 MiB total spans every group's images, not each group's: groups whose images
// each fit under it but together exceed it are refused before ANY mutate.
func TestCreateDemandGenCampaign_TotalImageBytesSpanEveryGroup(t *testing.T) {
	// Real PNGs padded past their IEND chunk: image.DecodeConfig reads the header only, so
	// they still decode as the slot geometry while weighing what the test needs.
	pad := func(img []byte, to int) []byte { return append(append([]byte{}, img...), make([]byte, to-len(img))...) }
	const big = MaxImageBytes - 1024
	ref := func(n int) string { return ResolvedImageRefPrefix + fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	resolved := map[string][]byte{}
	group := func(n int) DemandGenAdGroup {
		c := fullCreative()
		c.MarketingImages, c.SquareMarketingImages, c.LogoImages = nil, nil, nil
		for i := 0; i < 6; i++ {
			r := ref(n*100 + i)
			resolved[r] = pad(taggedImage(pngOf(t, 600, 600), n*100+i), big)
			c.SquareMarketingImages = append(c.SquareMarketingImages, r)
		}
		r := ref(n*100 + 99)
		resolved[r] = pngOf(t, 128, 128)
		c.LogoImages = []string{r}
		return DemandGenAdGroup{Name: fmt.Sprintf("G%d", n), Creative: c}
	}
	// Each group is about 30 MiB, well under the 64 MiB total; the three together are
	// about 90 MiB, over it.
	in := demandGenInput()
	in.DemandGenAdGroups = []DemandGenAdGroup{group(1), group(2), group(3)}
	in.ResolvedImages = resolved
	perGroup := 6*big + len(resolved[ref(199)])
	if perGroup >= maxCreativeTotalImageBytes || 3*perGroup <= maxCreativeTotalImageBytes {
		t.Fatalf("fixture: per group %d, total %d against cap %d", perGroup, 3*perGroup, maxCreativeTotalImageBytes)
	}
	rig, c, _ := newDGRig(t, nil, nil)
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "creative images total more than") {
		t.Fatalf("err = %v, want the total-bytes refusal", err)
	}
	if res != nil || len(rig.mutatePaths()) != 0 {
		t.Errorf("res = %+v, mutates = %v; nothing may be sent", res, rig.mutatePaths())
	}
}

// The same 64 MiB total spans every AD of a group too (#474): the walk covers up to
// maxDemandGenAdGroups × maxDemandGenAdsPerGroup creatives in one fetchSlotImages call, so
// three image ads in the single group, each under the total, are refused together before
// ANY mutate.
func TestCreateDemandGenCampaign_TotalImageBytesSpanEveryAd(t *testing.T) {
	pad := func(img []byte, to int) []byte {
		return append(append([]byte{}, img...), make([]byte, to-len(img))...)
	}
	const big = MaxImageBytes - 1024
	ref := func(n int) string {
		return ResolvedImageRefPrefix + fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	}
	resolved := map[string][]byte{}
	ad := func(n int) DemandGenAd {
		c := fullCreative()
		c.MarketingImages, c.SquareMarketingImages, c.LogoImages = nil, nil, nil
		for i := 0; i < 6; i++ {
			r := ref(n*100 + i)
			resolved[r] = pad(taggedImage(pngOf(t, 600, 600), n*100+i), big)
			c.SquareMarketingImages = append(c.SquareMarketingImages, r)
		}
		r := ref(n*100 + 99)
		resolved[r] = pngOf(t, 128, 128)
		c.LogoImages = []string{r}
		return DemandGenAd{Name: fmt.Sprintf("Ad %d", n), Creative: c}
	}
	in := demandGenInput()
	in.DemandGenCreative = DemandGenCreative{}
	in.DemandGenAds = []DemandGenAd{ad(1), ad(2), ad(3)}
	in.ResolvedImages = resolved
	perAd := 6*big + len(resolved[ref(199)])
	if perAd >= maxCreativeTotalImageBytes || 3*perAd <= maxCreativeTotalImageBytes {
		t.Fatalf("fixture: per ad %d, total %d against cap %d",
			perAd, 3*perAd, maxCreativeTotalImageBytes)
	}
	rig, c, _ := newDGRig(t, nil, nil)
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "creative images total more than") {
		t.Fatalf("err = %v, want the total-bytes refusal", err)
	}
	if res != nil || len(rig.mutatePaths()) != 0 {
		t.Errorf("res = %+v, mutates = %v; nothing may be sent", res, rig.mutatePaths())
	}
}

// errAfterFlagCtx reports Canceled from Err once its flag is set, while its Done channel
// never fires — so a request already in flight completes, and the cascade's own
// ctx.Err() check between groups is what sees it.
type errAfterFlagCtx struct {
	context.Context
	flag *atomic.Bool
}

func (c errAfterFlagCtx) Err() error {
	if c.flag.Load() {
		return context.Canceled
	}
	return nil
}

// A context that dies between groups stops the cascade before the next group's mutate,
// with the earlier groups reported.
func TestCreateDemandGenCampaign_ContextDoneBetweenGroups(t *testing.T) {
	var flag atomic.Bool
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"adGroupAds:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			flag.Store(true)
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~555"}]}`)
		},
	})
	res, err := c.CreateDemandGenCampaign(errAfterFlagCtx{Context: context.Background(), flag: &flag}, twoGroupInput(base))
	if err == nil || !strings.Contains(err.Error(), "aborted after 1 of 2 ad group(s)") {
		t.Fatalf("err = %v, want the between-groups abort", err)
	}
	if res == nil || len(res.AdGroups) != 1 || res.AdID != "555" {
		t.Errorf("res = %+v, want the first group only", res)
	}
	if n := len(rig.sent("adGroups:mutate")); n != 1 {
		t.Errorf("sent %d ad group mutates, want only the first group's", n)
	}
}

// The combined fetch hands each group back its OWN images, re-indexed to the slot within
// its own creative: a second image group's logo is slot logoSlotIndex of that group, not
// slot 9 of the walk. Fed from resolved bytes, so no network is involved.
func TestFetchDemandGenGroupImages_ReindexesPerGroup(t *testing.T) {
	ref := func(n int) string {
		return ResolvedImageRefPrefix + fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	}
	resolved := map[string][]byte{
		ref(1): pngOf(t, 1200, 628), ref(2): pngOf(t, 256, 256),
		ref(3): pngOf(t, 900, 900),
		ref(4): pngOf(t, 600, 600), ref(5): pngOf(t, 128, 128),
	}
	image := func(marketing, square, logo []string) demandGenCreativePlan {
		c := fullCreative()
		c.MarketingImages, c.SquareMarketingImages, c.LogoImages = marketing, square, logo
		plan, err := validateDemandGenCreativeSpec(c, resolved)
		if err != nil {
			t.Fatalf("creative: %v", err)
		}
		return plan
	}
	video := fullVideoCreative("")
	video.LogoImages = []string{ref(3)}
	videoPlan, err := validateDemandGenVideoSpec(video, resolved)
	if err != nil {
		t.Fatalf("video: %v", err)
	}
	// The second group carries two ads (#474), so the re-indexing is per AD, not per group.
	groups := []demandGenGroupPlan{
		{ads: []demandGenAdPlan{{creative: image([]string{ref(1)}, nil, []string{ref(2)})}}},
		{ads: []demandGenAdPlan{{video: videoPlan}, {creative: image(nil, []string{ref(4)}, []string{ref(5)})}}},
	}
	out, err := imageFetchTestClient(t).fetchDemandGenGroupImages(context.Background(), groups)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := func(imgs []fetchedImage) string {
		parts := make([]string, 0, len(imgs))
		for _, img := range imgs {
			parts = append(parts, fmt.Sprintf("%d:%s", img.slot, img.url[len(img.url)-1:]))
		}
		return strings.Join(parts, ",")
	}
	if g := got(out[0][0].images); g != "0:1,4:2" || len(out[0][0].logos) != 0 {
		t.Errorf("group 0 images = %s logos = %d, want 0:1,4:2 and none", g, len(out[0][0].logos))
	}
	if g := got(out[1][0].logos); g != "0:3" || len(out[1][0].images) != 0 {
		t.Errorf("group 1 ad 1 logos = %s images = %d, want 0:3 and none", g, len(out[1][0].images))
	}
	if g := got(out[1][1].images); g != "1:4,4:5" || len(out[1][1].logos) != 0 {
		t.Errorf("group 1 ad 2 images = %s, want its square image in slot 1 and its logo in slot 4: 1:4,4:5", g)
	}
	// A video-only campaign has no image plan to lend its map: the video plan's own
	// resolved bytes must reach the fetch, or a stored-asset logo would be fetched as a URL.
	out, err = imageFetchTestClient(t).fetchDemandGenGroupImages(context.Background(), []demandGenGroupPlan{{ads: []demandGenAdPlan{{video: videoPlan}}}})
	if err != nil {
		t.Fatalf("video-only fetch: %v", err)
	}
	if g := got(out[0][0].logos); g != "0:3" {
		t.Errorf("video-only logos = %s, want 0:3", g)
	}
}
