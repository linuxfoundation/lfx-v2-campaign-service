// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// Display ad groups and audience exclusions (#475)
// ---------------------------------------------------------------------------

const displayGroupBase = "KubeCon Europe 2026 - Display Network"

// twoDisplayGroupInput names two Display ad groups — Retargeting and Prospecting — each
// with its own creative and audiences; Prospecting also excludes a list of its own. The
// campaign excludes one list and one site, which cover both groups.
func twoDisplayGroupInput(base string) CampaignInput {
	in := demandGenInput()
	in.ExcludedPlacements = []string{"example.com"}
	in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/70"}
	prospecting := displayCreativeAt(base)
	prospecting.MarketingImages = []string{base + "/m2.png"}
	prospecting.SquareMarketingImages = []string{base + "/sq2.png"}
	prospecting.LogoImages = nil
	prospecting.Headlines = []string{"Discover cloud native"}
	in.DisplayAdGroups = []DisplayAdGroup{
		{Name: "Retargeting", Creative: displayCreativeAt(base), AudienceSegments: []string{"customers/1234567890/userLists/11"}},
		{
			Name: "Prospecting", Creative: prospecting,
			CustomAffinities:         []string{"customers/1234567890/customInterests/33"},
			UserInterests:            []string{"customers/1234567890/userInterests/44"},
			ExcludedAudienceSegments: []string{"customers/1234567890/userLists/11"},
		},
	}
	return in
}

func twoDisplayGroupImages(t *testing.T) map[string][]byte {
	t.Helper()
	images := displayImages(t)
	images["/m2.png"] = pngOf(t, 1200, 628)
	images["/sq2.png"] = pngOf(t, 400, 400)
	return images
}

// rigCall is one mutate the rig received.
type rigCall struct {
	op   string
	body []byte
}

// displayRig is a TLS server that serves the creative images and answers every Display
// mutate with resource names derived from the request, so a multi-group cascade runs end
// to end. overrides replace the default answer for one mutate path; call is that path's
// 0-based call count.
type displayRig struct {
	mu    sync.Mutex
	calls []rigCall
}

var rigAdGroupRe = regexp.MustCompile(`"adGroup":"customers/1234567890/adGroups/(\d+)"`)

func newDisplayRig(t *testing.T, images map[string][]byte, overrides map[string]func(call int, body []byte, w http.ResponseWriter)) (*displayRig, *Client, string) {
	t.Helper()
	rig := &displayRig{}
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		call := 0
		for _, c := range rig.calls {
			if c.op == op {
				call++
			}
		}
		rig.calls = append(rig.calls, rigCall{op: op, body: body})
		rig.mu.Unlock()
		if h, ok := overrides[op]; ok {
			h(call, body, w)
			return
		}
		var req struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(body, &req)
		adGroup := "0"
		if m := rigAdGroupRe.FindSubmatch(body); m != nil {
			adGroup = string(m[1])
		}
		names := make([]string, len(req.Operations))
		for i := range names {
			switch op {
			case "campaignBudgets:mutate":
				names[i] = "customers/1234567890/campaignBudgets/111"
			case "campaigns:mutate":
				names[i] = "customers/1234567890/campaigns/222"
			case "adGroups:mutate":
				names[i] = fmt.Sprintf("customers/1234567890/adGroups/%d", 333+call)
			case "adGroupCriteria:mutate":
				names[i] = fmt.Sprintf("customers/1234567890/adGroupCriteria/%s~%d", adGroup, 800+10*call+i)
			case "campaignCriteria:mutate":
				names[i] = fmt.Sprintf("customers/1234567890/campaignCriteria/222~%d", 700+10*call+i)
			case "assets:mutate":
				names[i] = fmt.Sprintf("customers/1234567890/assets/%d", 900+10*call+i)
			case "adGroupAds:mutate":
				names[i] = fmt.Sprintf("customers/1234567890/adGroupAds/%s~%d", adGroup, 555+call)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = `{"resourceName":"` + n + `"}`
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	})
	return rig, demandGenClient(t, srv), srv.URL
}

func (r *displayRig) ops() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.op
	}
	return strings.Join(out, ",")
}

func (r *displayRig) sent(op string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if c.op == op {
			out = append(out, string(c.body))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The preflight
// ---------------------------------------------------------------------------

func TestValidateDisplayAdGroups_Refusals(t *testing.T) {
	const base = "https://cdn.example.org"
	group := func(name string) DisplayAdGroup {
		return DisplayAdGroup{Name: name, Creative: displayCreativeAt(base)}
	}
	withGroups := func(gs ...DisplayAdGroup) func(*CampaignInput) {
		return func(in *CampaignInput) { in.DisplayAdGroups = gs }
	}
	cases := map[string]struct {
		kind    string
		mutate  func(in *CampaignInput)
		wantSub string
	}{
		"off display": {campaignKindSearch, withGroups(group("A")), "Display ad groups are not supported on Search Campaign"},
		// Each channel is pointed at its OWN ad-group shape, not at Search's list.
		"off display/search": {campaignKindSearch, withGroups(group("A")),
			"(a Search campaign takes its own ad-group list, AdGroups)"},
		"off display/demand gen": {campaignKindDemandGen, withGroups(group("A")),
			"(a Demand Gen campaign takes its own ad-group list, DemandGenAdGroups)"},
		"off display/video": {campaignKindVideo, withGroups(group("A")),
			"(a Video campaign creates one ad group, from VideoCreative)"},
		"off display/performance max": {campaignKindPerformanceMax, withGroups(group("A")),
			"(a Performance Max campaign has no ad groups; its asset group comes from " +
				"PerformanceMaxCreative)"},
		"campaign-level creative too": {campaignKindDisplay, func(in *CampaignInput) {
			withGroups(group("A"))(in)
			in.DisplayCreative = displayCreativeAt(base)
		}, "must be empty"},
		"campaign-level user list too": {campaignKindDisplay, func(in *CampaignInput) {
			withGroups(group("A"))(in)
			in.AudienceSegments = []string{"customers/1234567890/userLists/11"}
		}, "must be empty"},
		"campaign-level custom audience too": {campaignKindDisplay, func(in *CampaignInput) {
			withGroups(group("A"))(in)
			in.CustomAudiences = []string{"customers/1234567890/customAudiences/22"}
		}, "must be empty"},
		"campaign-level custom affinity too": {campaignKindDisplay, func(in *CampaignInput) {
			withGroups(group("A"))(in)
			in.CustomAffinities = []string{"customers/1234567890/customInterests/33"}
		}, "must be empty"},
		"campaign-level user interest too": {campaignKindDisplay, func(in *CampaignInput) {
			withGroups(group("A"))(in)
			in.UserInterests = []string{"customers/1234567890/userInterests/44"}
		}, "must be empty"},
		"over the cap": {campaignKindDisplay, withGroups(group("A"), group("B"), group("C"), group("D"), group("E"), group("F")),
			"6 Display ad groups requested, limit 5"},
		"blank name": {campaignKindDisplay, withGroups(group(" | ")), "Display ad group 1 has no name"},
		"duplicate name ignoring case and spacing": {campaignKindDisplay, withGroups(group("Retargeting"), group("  RETARGETING ")), "is used more than once"},
		// Simple case folding, as strings.EqualFold: LONG S folds with s and FINAL SIGMA
		// with capital sigma, which strings.ToLower keeps apart.
		"duplicate name under long s": {campaignKindDisplay,
			withGroups(group("ſale"), group("SALE")), "is used more than once"},
		"duplicate name under final sigma": {campaignKindDisplay,
			withGroups(group("Kubecon\u03c2"), group("KUBECON\u03a3")), "is used more than once"},
		"over-long name": {campaignKindDisplay, withGroups(group(strings.Repeat("a", 250))),
			"google-ads: Display ad group 1 name \"" + displayGroupBase + " | " +
				strings.Repeat("a", 250) + "\" is 290 characters, over Google's limit of 255; " +
				"shorten EventName or this group's name"},
		"no creative": {campaignKindDisplay, withGroups(DisplayAdGroup{Name: "A"}),
			"has no creative"},
		"bad group creative": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.Creative.BusinessName = ""
			withGroups(g)(in)
		}, `Display ad group "` + displayGroupBase + ` | A": google-ads responsive display ad requires a business name`},
		"foreign group user list": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.AudienceSegments = []string{"customers/9999999999/userLists/11"}
			withGroups(g)(in)
		}, "is on customer \"9999999999\", not this campaign's account \"1234567890\""},
		"foreign group user interest": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.UserInterests = []string{"customers/9999999999/userInterests/44"}
			withGroups(g)(in)
		}, "is on customer \"9999999999\""},
		"foreign group exclusion": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.ExcludedAudienceSegments = []string{"customers/9999999999/userLists/11"}
			withGroups(g)(in)
		}, "is on customer \"9999999999\""},
		"group excludes a custom audience": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.ExcludedAudienceSegments = []string{"customers/1234567890/customAudiences/22"}
			withGroups(g)(in)
		}, "only user lists (customers/{customerId}/userLists/{id}) can be excluded"},
		"group targets and excludes one list": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.AudienceSegments = []string{"customers/1234567890/userLists/11"}
			g.ExcludedAudienceSegments = []string{" customers/1234567890/userLists/11 "}
			withGroups(g)(in)
		}, `is named twice — Display ad group "` + displayGroupBase + ` | A" targets it and Display ad group "` + displayGroupBase + ` | A" excludes it`},
		"campaign excludes a list a group targets": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.AudienceSegments = []string{"customers/1234567890/userLists/11"}
			withGroups(group("B"), g)(in)
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/11"}
		}, "targets it and the campaign excludes it"},
		// A leading zero is refused, not read as a second list: 011 and 11 are one id.
		"non-canonical list id": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.AudienceSegments = []string{"customers/1234567890/userLists/011"}
			withGroups(g)(in)
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/11"}
		}, `AudienceSegments entry "customers/1234567890/userLists/011" has the id "011", ` +
			"which is not the canonical base-10 spelling"},
		"group and campaign exclude one list": {campaignKindDisplay, func(in *CampaignInput) {
			g := group("A")
			g.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/11"}
			withGroups(g)(in)
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/11"}
		}, "excludes it and the campaign excludes it"},
		// The Search list on Display names the Display one.
		"search ad-group list": {campaignKindDisplay, func(in *CampaignInput) {
			in.AdGroups = []AdGroupSpec{{Name: "A"}}
		}, "a Display campaign takes DisplayAdGroups"},
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

// The campaign-level exclusion list is Display only, account-checked, user lists only, and
// may not exclude a list the single ad group targets.
func TestPreflight_CampaignAudienceExclusions(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen, campaignKindPerformanceMax, campaignKindVideo} {
		t.Run("off display/"+kind, func(t *testing.T) {
			in := demandGenInput()
			if kind == campaignKindPerformanceMax {
				in.PerformanceMaxCreative = fullPMaxCreative()
			}
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/70"}
			_, err := c.preflightCampaignKind(kind, in)
			for _, sub := range []string{"audience exclusions are not supported on " + kind, "omit ExcludedAudienceSegments", "create a Display campaign"} {
				if err == nil || !strings.Contains(err.Error(), sub) {
					t.Errorf("err = %v, want it to contain %q", err, sub)
				}
			}
		})
	}
	for name, tc := range map[string]struct {
		mutate  func(*CampaignInput)
		wantSub string
	}{
		"foreign account": {func(in *CampaignInput) {
			in.ExcludedAudienceSegments = []string{"customers/9999999999/userLists/70"}
		}, "ExcludedAudienceSegments entry \"customers/9999999999/userLists/70\" is on customer \"9999999999\""},
		"a user interest": {func(in *CampaignInput) {
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userInterests/44"}
		}, "is not a user list (it names the userInterests collection); only user lists"},
		"an Audience": {func(in *CampaignInput) {
			in.ExcludedAudienceSegments = []string{"customers/1234567890/audiences/5"}
		}, "is not a user list (it names the audiences collection); only user lists"},
		"malformed": {func(in *CampaignInput) {
			in.ExcludedAudienceSegments = []string{"userLists/70"}
		}, "is not a customers/{customerId}/userLists/{id} resource name"},
		"targeted and excluded": {func(in *CampaignInput) {
			in.AudienceSegments = []string{"customers/1234567890/userLists/70"}
			in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/70"}
		}, "the ad group targets it and the campaign excludes it"},
	} {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			tc.mutate(&in)
			_, err := c.preflightCampaignKind(campaignKindDisplay, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// Without the list the preflight resolves ONE group from the campaign-level fields, under
// the name the single ad group always had and with no group-level exclusion; with it, one
// group per entry, named "<that name> | <label>".
func TestValidateDisplayAdGroups_Plans(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	single := displayTargetingInput()
	single.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/70"}
	pf, err := c.preflightCampaignKind(campaignKindDisplay, single)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(pf.displayGroups) != 1 {
		t.Fatalf("groups = %+v, want one", pf.displayGroups)
	}
	g := pf.displayGroups[0]
	if g.name != displayGroupBase || g.targeting.audienceCount() != 4 || len(g.targeting.excludedUserLists) != 0 || len(g.targeting.excludedPlacements) != 0 {
		t.Errorf("single group = %+v", g)
	}
	if strings.Join(pf.displayTargeting.excludedUserLists, ",") != "customers/1234567890/userLists/70" {
		t.Errorf("campaign exclusions = %v", pf.displayTargeting.excludedUserLists)
	}

	pf, err = c.preflightCampaignKind(campaignKindDisplay, twoDisplayGroupInput("https://cdn.example.org"))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(pf.displayGroups) != 2 {
		t.Fatalf("got %d groups, want 2", len(pf.displayGroups))
	}
	g0, g1 := pf.displayGroups[0], pf.displayGroups[1]
	if g0.name != displayGroupBase+" | Retargeting" || len(g0.targeting.userLists) != 1 || !g0.creative.present {
		t.Errorf("group 0 = %+v", g0)
	}
	if g1.name != displayGroupBase+" | Prospecting" || g1.targeting.audienceCount() != 2 ||
		strings.Join(g1.targeting.excludedUserLists, ",") != "customers/1234567890/userLists/11" || !g1.creative.present {
		t.Errorf("group 1 = %+v", g1)
	}

	if pf, err = c.preflightCampaignKind(campaignKindSearch, sampleInput()); err != nil || pf.displayGroups != nil {
		t.Errorf("search: groups = %+v err = %v, want none", pf.displayGroups, err)
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// Two groups, each with its own ad group, targetingSetting, audiences and ad; group 2's own
// exclusion on its ad group; the campaign's placement and user-list exclusions once, after
// every group and before any ad. The result has both groups in AdGroups and the scalars are
// the first group's.
func TestCreateDisplayCampaign_TwoAdGroups(t *testing.T) {
	rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), nil)
	res, err := c.CreateDisplayCampaign(context.Background(), twoDisplayGroupInput(base))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "campaignBudgets:mutate,campaigns:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate," +
		"campaignCriteria:mutate,campaignCriteria:mutate," +
		"assets:mutate,adGroupAds:mutate,assets:mutate,adGroupAds:mutate"
	if got := rig.ops(); got != want {
		t.Errorf("mutates =\n  %s\nwant\n  %s", got, want)
	}

	setting := `"targetingSetting":{"targetRestrictions":[` +
		`{"targetingDimension":"AGE_RANGE","bidOnly":true},{"targetingDimension":"GENDER","bidOnly":true},` +
		`{"targetingDimension":"PARENTAL_STATUS","bidOnly":true},{"targetingDimension":"INCOME_RANGE","bidOnly":true},` +
		`{"targetingDimension":"AUDIENCE","bidOnly":false}]}`
	groups := rig.sent("adGroups:mutate")
	for i, name := range []string{"Retargeting", "Prospecting"} {
		sameJSON(t, groups[i], `{"operations":[{"create":{"name":"`+displayGroupBase+` | `+name+`",`+
			`"campaign":"customers/1234567890/campaigns/222","status":"PAUSED","type":"DISPLAY_STANDARD",`+setting+`}}]}`)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	sameJSON(t, criteria[0], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"ENABLED","userList":{"userList":"customers/1234567890/userLists/11"}}}]}`)
	sameJSON(t, criteria[1], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/334","status":"ENABLED","customAffinity":{"customAffinity":"customers/1234567890/customInterests/33"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/334","status":"ENABLED","userInterest":{"userInterestCategory":"customers/1234567890/userInterests/44"}}}]}`)
	// The negative user list, in the probed shape: explicit negative, no status.
	sameJSON(t, criteria[2], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/334","negative":true,"userList":{"userList":"customers/1234567890/userLists/11"}}}]}`)
	campaignCriteria := rig.sent("campaignCriteria:mutate")
	sameJSON(t, campaignCriteria[0], `{"operations":[
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"placement":{"url":"example.com"}}}]}`)
	sameJSON(t, campaignCriteria[1], `{"operations":[
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"userList":{"userList":"customers/1234567890/userLists/70"}}}]}`)

	// The combined fetch re-indexes each image into its own creative's slot: group 2's
	// landscape and square images land in its own ad's arrays, under its own ad group.
	ads := rig.sent("adGroupAds:mutate")
	if !strings.Contains(ads[0], `"adGroup":"customers/1234567890/adGroups/333"`) ||
		!strings.Contains(ads[0], `"logoImages":[{"asset":"customers/1234567890/assets/902"}]`) {
		t.Errorf("group 1's ad is not built on its own ad group and assets: %s", ads[0])
	}
	if !strings.Contains(ads[1], `"adGroup":"customers/1234567890/adGroups/334"`) ||
		!strings.Contains(ads[1], `"marketingImages":[{"asset":"customers/1234567890/assets/910"}]`) ||
		!strings.Contains(ads[1], `"squareMarketingImages":[{"asset":"customers/1234567890/assets/911"}]`) ||
		strings.Contains(ads[1], `"logoImages"`) ||
		!strings.Contains(ads[1], `"headlines":[{"text":"Discover cloud native"}]`) {
		t.Errorf("group 2's ad is not built on its own ad group and assets: %s", ads[1])
	}

	if res.AdGroupID != "333" || res.AdGroupName != displayGroupBase+" | Retargeting" || res.AdID != "555" {
		t.Errorf("scalars = %s/%q/%s, want the first group's", res.AdGroupID, res.AdGroupName, res.AdID)
	}
	if strings.Join(res.AudienceCriteriaIDs, ",") != "800" || strings.Join(res.CreativeAssetIDs, ",") != "900,901,902" {
		t.Errorf("scalar criteria/assets = %v / %v, want the first group's", res.AudienceCriteriaIDs, res.CreativeAssetIDs)
	}
	if strings.Join(res.ExcludedPlacementCriteriaIDs, ",") != "700" || strings.Join(res.ExcludedAudienceCriteriaIDs, ",") != "710" {
		t.Errorf("campaign exclusions = %v / %v", res.ExcludedPlacementCriteriaIDs, res.ExcludedAudienceCriteriaIDs)
	}
	if len(res.AdGroups) != 2 {
		t.Fatalf("AdGroups = %+v, want 2 entries", res.AdGroups)
	}
	g0, g1 := res.AdGroups[0], res.AdGroups[1]
	if g0.ID != "333" || strings.Join(g0.AdIDs, ",") != "555" || strings.Join(g0.AudienceCriteriaIDs, ",") != "800" ||
		len(g0.ExcludedAudienceCriteriaIDs) != 0 || strings.Join(g0.CreativeAssetIDs, ",") != "900,901,902" {
		t.Errorf("group 0 = %+v", g0)
	}
	if g1.ID != "334" || strings.Join(g1.AdIDs, ",") != "556" || strings.Join(g1.AudienceCriteriaIDs, ",") != "810,811" ||
		strings.Join(g1.ExcludedAudienceCriteriaIDs, ",") != "820" || strings.Join(g1.CreativeAssetIDs, ",") != "910,911" {
		t.Errorf("group 1 = %+v", g1)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, line := range []string{
		`Ad group created: 333 (DISPLAY_STANDARD) [ad group "` + displayGroupBase + ` | Retargeting"]`,
		`Ad group created: 334 (DISPLAY_STANDARD) [ad group "` + displayGroupBase + ` | Prospecting"]`,
		`(1 custom affinity segment(s); 1 user interest(s))`,
		`Audience exclusions applied: 1 ad group negative user list criteria [ad group "` + displayGroupBase + ` | Prospecting"]`,
		"Placement exclusions applied: 1 campaign negative placement criteria",
		"Audience exclusions applied: 1 campaign negative user list criteria",
		`Display ad created: 556 (PAUSED, 2 marketing image(s), 0 logo(s), 1 headline(s), 1 description(s)) [ad group "` + displayGroupBase + ` | Prospecting"]`,
	} {
		if !strings.Contains(steps, line) {
			t.Errorf("steps lack %q:\n%s", line, steps)
		}
	}
	if last := res.Steps[len(res.Steps)-1]; last != "Display campaign created (no geo targeting set) — add geo targeting, then review the ad and launch in the Google Ads UI" {
		t.Errorf("closing step = %q", last)
	}
}

// Every group's ad carries the Display tracking (#479): the final URL each ad is sent with
// is tagged utm_source=google and utm_medium=display, on group 2 as on group 1, because
// every group's ad is built from the one tagged final URL the preflight composes.
func TestCreateDisplayCampaign_EveryGroupAdIsTagged(t *testing.T) {
	rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), nil)
	_, err := c.CreateDisplayCampaign(context.Background(), twoDisplayGroupInput(base))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ads := rig.sent("adGroupAds:mutate")
	if len(ads) != 2 {
		t.Fatalf("ad mutates = %d, want one per group", len(ads))
	}
	for i, body := range ads {
		var req struct {
			Operations []struct {
				Create struct {
					Ad struct {
						FinalURLs []string `json:"finalUrls"`
					} `json:"ad"`
				} `json:"create"`
			} `json:"operations"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Operations) != 1 {
			t.Fatalf("group %d ad body = %s (%v)", i+1, body, err)
		}
		urls := req.Operations[0].Create.Ad.FinalURLs
		if len(urls) != 1 {
			t.Fatalf("group %d finalUrls = %v, want one", i+1, urls)
		}
		u, err := url.Parse(urls[0])
		if err != nil {
			t.Fatalf("group %d finalUrl %q: %v", i+1, urls[0], err)
		}
		q := u.Query()
		if q.Get("utm_medium") != utmMediumDisplay || q.Get("utm_source") != "google" {
			t.Errorf("group %d finalUrl = %q, want utm_source=google and utm_medium=%s",
				i+1, urls[0], utmMediumDisplay)
		}
	}
}

// The income-range and parental-status exclusions (#473) go on EVERY named group, each in
// its own adGroupCriteria mutate after the group and before its audiences, with the ids on
// the group's AdGroups entry and the scalar field the first group's.
func TestCreateDisplayCampaign_DemographicsOnEveryGroup(t *testing.T) {
	rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), nil)
	in := twoDisplayGroupInput(base)
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	in.ExcludedParentalStatuses = []string{"PARENT"}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "campaignBudgets:mutate,campaigns:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate," +
		"adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate," +
		"campaignCriteria:mutate,campaignCriteria:mutate," +
		"assets:mutate,adGroupAds:mutate,assets:mutate,adGroupAds:mutate"
	if got := rig.ops(); got != want {
		t.Errorf("mutates =\n  %s\nwant\n  %s", got, want)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	for _, i := range []int{0, 2} {
		group := map[int]string{0: "333", 2: "334"}[i]
		adGroup := `"adGroup":"customers/1234567890/adGroups/` + group + `","negative":true`
		sameJSON(t, criteria[i], `{"operations":[
			{"create":{`+adGroup+`,"incomeRange":{"type":"INCOME_RANGE_0_50"}}},
			{"create":{`+adGroup+`,"parentalStatus":{"type":"PARENT"}}}]}`)
	}
	if got := strings.Join(res.AdGroupDemographicCriteriaIDs, ","); got != "800,801" {
		t.Errorf("AdGroupDemographicCriteriaIDs = %s, want the first group's", got)
	}
	if len(res.AdGroups) != 2 ||
		strings.Join(res.AdGroups[0].DemographicCriteriaIDs, ",") != "800,801" ||
		strings.Join(res.AdGroups[1].DemographicCriteriaIDs, ",") != "820,821" {
		t.Errorf("AdGroups demographics = %+v", res.AdGroups)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps,
		`Ad group exclusions applied: 2 criteria (1 income range exclusion(s); `+
			`1 parental status exclusion(s)) [ad group "`+
			displayGroupBase+` | Prospecting"]`) {
		t.Errorf("steps lack group 2's exclusions:\n%s", steps)
	}
}

// Only a group with audiences carries the targetingSetting: a named group with no audience
// keeps the pre-#462 ad group payload, exclusions or not.
func TestCreateDisplayCampaign_TargetingSettingPerGroup(t *testing.T) {
	rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), nil)
	in := twoDisplayGroupInput(base)
	in.DisplayAdGroups[1].CustomAffinities, in.DisplayAdGroups[1].UserInterests = nil, nil
	if _, err := c.CreateDisplayCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	groups := rig.sent("adGroups:mutate")
	if !strings.Contains(groups[0], `"targetingSetting"`) {
		t.Errorf("group 1 has audiences and must carry the targetingSetting: %s", groups[0])
	}
	sameJSON(t, groups[1], `{"operations":[{"create":{"name":"`+displayGroupBase+` | Prospecting",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"PAUSED","type":"DISPLAY_STANDARD"}}]}`)
	if got := len(rig.sent("adGroupCriteria:mutate")); got != 2 {
		t.Errorf("adGroupCriteria mutates = %d, want group 1's audiences and group 2's exclusion", got)
	}
}

// The single group with a campaign-level exclusion: the user-list exclusion follows the
// placements, the ad group is unchanged, and no AdGroups entry is written.
func TestCreateDisplayCampaign_SingleGroupCampaignExclusion(t *testing.T) {
	rig, c, base := newDisplayRig(t, displayImages(t), nil)
	in := displayInput(base)
	in.ExcludedPlacements = []string{"example.com"}
	in.ExcludedAudienceSegments = []string{"customers/1234567890/userLists/70", "customers/1234567890/userLists/71"}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := rig.ops(), "campaignBudgets:mutate,campaigns:mutate,adGroups:mutate,campaignCriteria:mutate,campaignCriteria:mutate,assets:mutate,adGroupAds:mutate"; got != want {
		t.Errorf("mutates = %s, want %s", got, want)
	}
	sameJSON(t, rig.sent("campaignCriteria:mutate")[1], `{"operations":[
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"userList":{"userList":"customers/1234567890/userLists/70"}}},
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"userList":{"userList":"customers/1234567890/userLists/71"}}}]}`)
	sameJSON(t, rig.sent("adGroups:mutate")[0], `{"operations":[{"create":{"name":"`+displayGroupBase+`",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"PAUSED","type":"DISPLAY_STANDARD"}}]}`)
	if strings.Join(res.ExcludedAudienceCriteriaIDs, ",") != "710,711" || len(res.AdGroups) != 0 || res.AdID != "555" {
		t.Errorf("res = %+v", res)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Audience exclusions applied: 2 campaign negative user list criteria") {
		t.Errorf("steps = %v", res.Steps)
	}
}

// A failure after the campaign exists returns the partial with the error, UNCONFIRMED
// exactly when the write may have landed, and nothing past the failed step is sent.
func TestCreateDisplayCampaign_AdGroupsPartialResults(t *testing.T) {
	reply := func(names ...string) func(int, []byte, http.ResponseWriter) {
		return func(_ int, _ []byte, w http.ResponseWriter) {
			parts := make([]string, len(names))
			for i, n := range names {
				parts[i] = `{"resourceName":"` + n + `"}`
			}
			_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
		}
	}
	status := func(code int) func(int, []byte, http.ResponseWriter) {
		return func(_ int, _ []byte, w http.ResponseWriter) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		}
	}
	// onCall answers call `n` of a path with h and every other call with the rig default.
	onCall := func(n int, h func(int, []byte, http.ResponseWriter), def func(int, []byte, http.ResponseWriter)) func(int, []byte, http.ResponseWriter) {
		return func(call int, body []byte, w http.ResponseWriter) {
			if call == n {
				h(call, body, w)
				return
			}
			def(call, body, w)
		}
	}
	adGroupOK := func(call int, _ []byte, w http.ResponseWriter) {
		reply(fmt.Sprintf("customers/1234567890/adGroups/%d", 333+call))(call, nil, w)
	}
	criteriaOK := func(call int, body []byte, w http.ResponseWriter) {
		ag := rigAdGroupRe.FindSubmatch(body)[1]
		var req struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(body, &req)
		names := make([]string, len(req.Operations))
		for i := range names {
			names[i] = fmt.Sprintf("customers/1234567890/adGroupCriteria/%s~%d", ag, 800+10*call+i)
		}
		reply(names...)(call, body, w)
	}
	campaignCriteriaOK := reply("customers/1234567890/campaignCriteria/222~700")

	type want struct {
		sub         string
		unconfirmed bool
		groups      int    // AdGroups entries
		ops         string // the last mutate sent
		check       func(t *testing.T, res *CampaignResult)
	}
	cases := map[string]struct {
		overrides map[string]func(int, []byte, http.ResponseWriter)
		want      want
	}{
		"second ad group refused": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroups:mutate": onCall(1, status(http.StatusBadRequest), adGroupOK)},
			want{"display ad group creation failed", false, 2, "adGroups:mutate", func(t *testing.T, res *CampaignResult) {
				if g := res.AdGroups[1]; g.Name != displayGroupBase+" | Prospecting" || g.ID != "" {
					t.Errorf("group 2 = %+v, want its name only", g)
				}
			}},
		},
		"second ad group unconfirmed": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroups:mutate": onCall(1, status(http.StatusInternalServerError), adGroupOK)},
			want{"display ad group creation UNCONFIRMED", true, 2, "adGroups:mutate", nil},
		},
		"group exclusions refused": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": onCall(2, status(http.StatusBadRequest), criteriaOK)},
			want{"display ad group audience exclusions failed (ad group 334 created; it has NO audience exclusions", false, 2, "adGroupCriteria:mutate", func(t *testing.T, res *CampaignResult) {
				if g := res.AdGroups[1]; g.ID != "334" || len(g.AudienceCriteriaIDs) != 2 || len(g.ExcludedAudienceCriteriaIDs) != 0 {
					t.Errorf("group 2 = %+v, want its id and audiences, no exclusions", g)
				}
			}},
		},
		"group exclusions unconfirmed": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": onCall(2, status(http.StatusServiceUnavailable), criteriaOK)},
			want{"display ad group audience exclusions UNCONFIRMED", true, 2, "adGroupCriteria:mutate", nil},
		},
		"group exclusions short 2xx": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": onCall(2, reply(), criteriaOK)},
			want{"audience exclusions UNCONFIRMED (ad group 334; 2xx with a malformed/short mutate response", true, 2, "adGroupCriteria:mutate", nil},
		},
		"group exclusions on another ad group": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": onCall(2, reply("customers/1234567890/adGroupCriteria/333~820"), criteriaOK)},
			want{"reports a different ad group id \"333\"", true, 2, "adGroupCriteria:mutate", nil},
		},
		"group exclusions on another account": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupCriteria:mutate": onCall(2, reply("customers/9999999999/adGroupCriteria/334~820"), criteriaOK)},
			want{"malformed/wrong-kind/wrong-account criterion resource name", true, 2, "adGroupCriteria:mutate", nil},
		},
		"campaign exclusions refused": {
			map[string]func(int, []byte, http.ResponseWriter){"campaignCriteria:mutate": onCall(1, status(http.StatusBadRequest), campaignCriteriaOK)},
			want{"campaign audience exclusions failed (campaign 222 created; it has NO audience exclusions", false, 2, "campaignCriteria:mutate", func(t *testing.T, res *CampaignResult) {
				if len(res.ExcludedPlacementCriteriaIDs) != 1 || len(res.ExcludedAudienceCriteriaIDs) != 0 || res.AdID != "" {
					t.Errorf("res = %+v, want the placements, no audience exclusions and no ad", res)
				}
			}},
		},
		"campaign exclusions unconfirmed": {
			map[string]func(int, []byte, http.ResponseWriter){"campaignCriteria:mutate": onCall(1, status(http.StatusBadGateway), campaignCriteriaOK)},
			want{"campaign audience exclusions UNCONFIRMED", true, 2, "campaignCriteria:mutate", nil},
		},
		"campaign exclusions short 2xx": {
			map[string]func(int, []byte, http.ResponseWriter){"campaignCriteria:mutate": onCall(1, reply(), campaignCriteriaOK)},
			want{"campaign audience exclusions UNCONFIRMED (campaign 222; 2xx with a malformed/short", true, 2, "campaignCriteria:mutate", nil},
		},
		"campaign exclusions on another campaign": {
			map[string]func(int, []byte, http.ResponseWriter){"campaignCriteria:mutate": onCall(1, reply("customers/1234567890/campaignCriteria/999~710"), campaignCriteriaOK)},
			want{"reports a different campaign id \"999\"", true, 2, "campaignCriteria:mutate", nil},
		},
		"campaign exclusions on another account": {
			map[string]func(int, []byte, http.ResponseWriter){"campaignCriteria:mutate": onCall(1, reply("customers/9999999999/campaignCriteria/222~710"), campaignCriteriaOK)},
			want{"campaign audience exclusions UNCONFIRMED (campaign 222; malformed/wrong-kind/wrong-account", true, 2, "campaignCriteria:mutate", nil},
		},
		// Every ad group exists before the ad pass, so a stop on group 1's ad leaves group 2
		// created with no ad: it is listed with its id and no adIds, and the error names it.
		"first ad refused": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupAds:mutate": onCall(0,
				status(http.StatusBadRequest), reply("customers/1234567890/adGroupAds/334~556"))},
			want{`display ad pass stopped at ad group "` + displayGroupBase + ` | Retargeting"; ` +
				`the later ad group(s) "` + displayGroupBase + ` | Prospecting" were created and ` +
				"have NO ad: google-ads display ad creation failed (ad group 333 and 3 image " +
				"asset(s) created)", false, 2, "adGroupAds:mutate",
				func(t *testing.T, res *CampaignResult) {
					if res.AdID != "" || len(res.AdGroups[0].AdIDs) != 0 {
						t.Errorf("res = %+v, want no ad on group 1", res)
					}
					if g := res.AdGroups[1]; g.ID != "334" || len(g.AdIDs) != 0 {
						t.Errorf("group 2 = %+v, want its id and no adIds", g)
					}
				}},
		},
		"second ad unconfirmed": {
			map[string]func(int, []byte, http.ResponseWriter){"adGroupAds:mutate": onCall(1, status(http.StatusServiceUnavailable), reply("customers/1234567890/adGroupAds/333~555"))},
			want{"display ad creation UNCONFIRMED (ad group 334 and 2 image asset(s) created", true, 2, "adGroupAds:mutate", func(t *testing.T, res *CampaignResult) {
				if res.AdID != "555" || strings.Join(res.AdGroups[0].AdIDs, ",") != "555" {
					t.Errorf("res = %+v, want group 1's ad", res)
				}
				if g := res.AdGroups[1]; g.ID != "334" || len(g.AdIDs) != 0 || strings.Join(g.CreativeAssetIDs, ",") != "910,911" {
					t.Errorf("group 2 = %+v, want its id and assets, and no ad", g)
				}
			}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), tc.overrides)
			res, err := c.CreateDisplayCampaign(context.Background(), twoDisplayGroupInput(base))
			if err == nil || !strings.Contains(err.Error(), tc.want.sub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want.sub)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.want.unconfirmed {
				t.Errorf("UNCONFIRMED in error = %v, want %v: %v", got, tc.want.unconfirmed, err)
			}
			if res == nil || res.CampaignID != "222" || res.AdGroupID != "333" {
				t.Fatalf("res = %+v, want the campaign and the first group", res)
			}
			if outcome, _ := CreateOutcome(res, err); outcome != domain.CreateOutcomeCampaignCreatedSetupIncomplete {
				t.Errorf("CreateOutcome = %q, want %q", outcome, domain.CreateOutcomeCampaignCreatedSetupIncomplete)
			}
			if len(res.AdGroups) != tc.want.groups || res.AdGroups[0].ID != "333" {
				t.Errorf("AdGroups = %+v, want %d entries with group 1 created", res.AdGroups, tc.want.groups)
			}
			ops := strings.Split(rig.ops(), ",")
			if last := ops[len(ops)-1]; last != tc.want.ops {
				t.Errorf("last mutate = %s, want %s — nothing past the failed step may be sent (%v)", last, tc.want.ops, ops)
			}
			if strings.Contains(strings.Join(res.Steps, "\n"), "Display campaign created") {
				t.Error("a failed create must not carry the closing step")
			}
			if tc.want.check != nil {
				tc.want.check(t, res)
			}
		})
	}
}

// A context that dies between groups stops the cascade before the next group's mutate —
// in the ad-group pass and in the ad pass — with what exists reported.
func TestCreateDisplayCampaign_ContextDoneBetweenGroups(t *testing.T) {
	for name, tc := range map[string]struct {
		trip, wantSub, wantStop string
	}{
		"ad-group pass": {"adGroupCriteria:mutate", "aborted after 1 of 2 ad group(s) were created", "adGroupCriteria:mutate"},
		"ad pass":       {"adGroupAds:mutate", "aborted after the ads of 1 of 2 ad group(s) were created", "adGroupAds:mutate"},
	} {
		t.Run(name, func(t *testing.T) {
			var flag atomic.Bool
			rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
				tc.trip: func(call int, body []byte, w http.ResponseWriter) {
					flag.Store(true)
					ag := rigAdGroupRe.FindSubmatch(body)[1]
					name := fmt.Sprintf("customers/1234567890/adGroupCriteria/%s~800", ag)
					if tc.trip == "adGroupAds:mutate" {
						name = fmt.Sprintf("customers/1234567890/adGroupAds/%s~555", ag)
					}
					_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+name+`"}]}`)
				},
			})
			in := twoDisplayGroupInput(base)
			in.ExcludedPlacements, in.ExcludedAudienceSegments = nil, nil
			ctx := errAfterFlagCtx{Context: context.Background(), flag: &flag}
			res, err := c.CreateDisplayCampaign(ctx, in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
			if res == nil || res.AdGroupID != "333" {
				t.Fatalf("res = %+v, want the first group", res)
			}
			ops := strings.Split(rig.ops(), ",")
			if last := ops[len(ops)-1]; last != tc.wantStop {
				t.Errorf("last mutate = %s, want %s (%v)", last, tc.wantStop, ops)
			}
		})
	}
}

// The 64 MiB total spans every group's images, not each group's: groups whose images each
// fit under it but together exceed it are refused before ANY mutate.
func TestCreateDisplayCampaign_TotalImageBytesSpanEveryGroup(t *testing.T) {
	// Real PNGs padded past their IEND chunk: image.DecodeConfig reads the header only, so
	// they still decode as the slot geometry while weighing what the test needs.
	pad := func(img []byte, to int) []byte { return append(append([]byte{}, img...), make([]byte, to-len(img))...) }
	const big = MaxImageBytes - 1024
	ref := func(n int) string { return ResolvedImageRefPrefix + fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	resolved := map[string][]byte{}
	group := func(n int) DisplayAdGroup {
		c := displayCreativeAt("")
		c.LogoImages = nil
		r := ref(n * 100)
		resolved[r] = pad(taggedImage(pngOf(t, 1200, 628), n*100), big)
		c.MarketingImages = []string{r}
		c.SquareMarketingImages = nil
		for i := 1; i < 6; i++ {
			r := ref(n*100 + i)
			resolved[r] = pad(taggedImage(pngOf(t, 400, 400), n*100+i), big)
			c.SquareMarketingImages = append(c.SquareMarketingImages, r)
		}
		return DisplayAdGroup{Name: fmt.Sprintf("G%d", n), Creative: c}
	}
	in := demandGenInput()
	in.DisplayAdGroups = []DisplayAdGroup{group(1), group(2), group(3)}
	in.ResolvedImages = resolved
	// Each group is about 30 MiB, well under the 64 MiB total; the three together are about
	// 90 MiB, over it.
	if perGroup := 6 * big; perGroup >= maxCreativeTotalImageBytes || 3*perGroup <= maxCreativeTotalImageBytes {
		t.Fatalf("fixture: per group %d against cap %d", perGroup, maxCreativeTotalImageBytes)
	}
	rig, c, _ := newDisplayRig(t, nil, nil)
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "creative images total more than") {
		t.Fatalf("err = %v, want the total-bytes refusal", err)
	}
	if res != nil || rig.ops() != "" {
		t.Errorf("res = %+v, mutates = %q; nothing may be sent", res, rig.ops())
	}
}

// Every group refusal is decided before the budget mutate: no request at all.
func TestCreateDisplayCampaign_GroupRefusalsSendNothing(t *testing.T) {
	rig, c, base := newDisplayRig(t, nil, nil)
	in := twoDisplayGroupInput(base)
	in.DisplayAdGroups[1].ExcludedAudienceSegments = []string{"customers/9999999999/userLists/11"}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err == nil || res != nil || rig.ops() != "" {
		t.Errorf("res = %+v, err = %v, mutates = %q; want a refusal and nothing sent", res, err, rig.ops())
	}
}
