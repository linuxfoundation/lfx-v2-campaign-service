// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A confirmed mutate result that names the same resource twice is UNCONFIRMED (#470
// review): every loop below gets a response of the right length, every entry well-formed,
// in this account and under the right parent, with one id repeated.

func TestResultIDSet_Repeat(t *testing.T) {
	s := newResultIDSet(3)
	if got := s.repeat("800", 0); got != "" {
		t.Fatalf("first record = %q, want empty", got)
	}
	if got := s.repeat("801", 1); got != "" {
		t.Fatalf("a distinct id = %q, want empty", got)
	}
	want := "2xx names resource id 800 at index 0 and again at index 2, so one operation " +
		"is unaccounted for"
	if got := s.repeat("800", 2); got != want {
		t.Errorf("repeat = %q, want %q", got, want)
	}
}

// repeatClient answers every mutate with body.
func repeatClient(t *testing.T, body string) *Client {
	t.Helper()
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
	return demandGenClient(t, srv)
}

func twoResults(name string) string {
	return fmt.Sprintf(`{"results":[{"resourceName":%q},{"resourceName":%q}]}`, name, name)
}

// wantRepeat fails unless err is UNCONFIRMED and names the repeat at indexes 0 and 1.
func wantRepeat(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") ||
		!strings.Contains(err.Error(), repeatRepeatedIndex) {
		t.Fatalf("err = %v, want UNCONFIRMED naming the repeat", err)
	}
}

var (
	twoLanguages = []languageInfo{
		{LanguageConstant: "languageConstants/1000"}, {LanguageConstant: "languageConstants/1003"},
	}
	twoGeos = geoPlan{included: []string{"geoTargetConstants/2840", "geoTargetConstants/2826"}}
)

const (
	repeatCampaign      = "customers/1234567890/campaigns/222"
	repeatAdGroup       = "customers/1234567890/adGroups/333"
	campCrit            = "customers/1234567890/campaignCriteria/222~800"
	agCrit              = "customers/1234567890/adGroupCriteria/333~800"
	repeatRepeatedIndex = "at index 0 and again at index 1"
	signal              = `{"assetGroupSignalResult":{"resourceName":` +
		`"customers/1234567890/assetGroupSignals/444~800"}}`
)

var twoUserLists = []string{
	"customers/1234567890/userLists/70", "customers/1234567890/userLists/71",
}

// The criteria loops: each returns NO ids when a result repeats, its contract on every
// UNCONFIRMED arm.
func TestCriteriaLoops_RepeatedIDIsUnconfirmed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		body string
		call func(c *Client) (int, error)
	}{
		"campaign criteria": {twoResults(campCrit), func(c *Client) (int, error) {
			ids, err := c.createCampaignTargetingCriteria(ctx, repeatCampaign, "222",
				criteriaPlan{languages: twoLanguages})
			return len(ids), err
		}},
		"campaign geo": {twoResults(campCrit), func(c *Client) (int, error) {
			ids, err := c.createCampaignGeoTargeting(ctx, repeatCampaign, "222", twoGeos)
			return len(ids), err
		}},
		"ad group geo": {twoResults(agCrit), func(c *Client) (int, error) {
			ids, err := c.createAdGroupGeoTargeting(ctx, repeatAdGroup, "333", twoGeos)
			return len(ids), err
		}},
		"campaign negative keywords": {twoResults(campCrit), func(c *Client) (int, error) {
			ids, err := c.createCampaignNegativeKeywords(ctx, repeatCampaign, "222", []Keyword{
				{Text: "free", MatchType: MatchTypeExact},
				{Text: "jobs", MatchType: MatchTypeExact},
			})
			return len(ids), err
		}},
		"ad group criteria": {twoResults(agCrit), func(c *Client) (int, error) {
			ids, err := c.createAdGroupTargeting(ctx, repeatAdGroup, "333", adGroupCriteriaSet{
				keywords:         []Keyword{{Text: "kubecon", MatchType: MatchTypeExact}},
				audienceSegments: []string{"customers/1234567890/userLists/9"},
			})
			return len(ids.keywords) + len(ids.audiences), err
		}},
		"demand gen audience and language": {twoResults(agCrit), func(c *Client) (int, error) {
			a, l, err := c.createDemandGenAdGroupTargeting(ctx, repeatAdGroup, "333",
				"customers/1234567890/audiences/41", twoLanguages[:1])
			return len(a) + len(l), err
		}},
		"display audiences": {twoResults(agCrit), func(c *Client) (int, error) {
			ids, err := c.createDisplayAudienceCriteria(ctx, repeatAdGroup, "333",
				displayTargetingPlan{userLists: twoUserLists})
			return len(ids), err
		}},
		"placement exclusions": {twoResults(campCrit), func(c *Client) (int, error) {
			ids, err := c.createCampaignPlacementExclusions(ctx, repeatCampaign, "222",
				[]string{"a.example", "b.example"})
			return len(ids), err
		}},
		"ad group audience exclusions": {twoResults(agCrit), func(c *Client) (int, error) {
			ids, err := c.createAdGroupAudienceExclusions(ctx, repeatAdGroup, "333", twoUserLists)
			return len(ids), err
		}},
		"campaign audience exclusions": {twoResults(campCrit), func(c *Client) (int, error) {
			ids, err := c.createCampaignAudienceExclusions(ctx, repeatCampaign, "222", twoUserLists)
			return len(ids), err
		}},
		"performance max signals": {`{"mutateOperationResponses":[` + signal + `,` + signal + `]}`,
			func(c *Client) (int, error) {
				themes := []string{"kubernetes", "cloud native"}
				plan := performanceMaxSignalPlan{searchThemes: themes}
				ids, err := c.createPerformanceMaxSignals(ctx,
					"customers/1234567890/assetGroups/444", "444", plan)
				return len(ids), err
			}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n, err := tc.call(repeatClient(t, tc.body))
			wantRepeat(t, err)
			if !IsOutcomeUnconfirmed(err) && name == "campaign negative keywords" {
				t.Errorf("the negative keyword arm must stay typed as unconfirmed: %v", err)
			}
			if n != 0 {
				t.Errorf("got %d ids, want none: a criteria loop returns none when unconfirmed", n)
			}
		})
	}
}

// The extension asset loop returns no ids on an unusable result, so it returns none on a
// repeat, and nothing is linked.
func TestCreateCampaignAssets_RepeatedAssetIsUnconfirmed(t *testing.T) {
	in := CampaignInput{Callouts: []string{"Free WiFi", "Live demos"}}
	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil || plan.count() != 2 {
		t.Fatalf("plan = %+v, %v; want two callouts", plan, err)
	}
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "campaignAssets:mutate") {
			t.Error("nothing may be linked after an unconfirmed asset mutate")
		}
		_, _ = io.WriteString(w, twoResults("customers/1234567890/assets/900"))
	})
	c := demandGenClient(t, srv)
	ids, links, err := c.createCampaignAssets(context.Background(), repeatCampaign, "222", plan)
	wantRepeat(t, err)
	if len(ids) != 0 || len(links) != 0 {
		t.Errorf("ids = %v, links = %v, want none", ids, links)
	}
}

// The Search ads loop: a group's ad ids are not set when a result repeats.
func TestCreateAdGroupAndAd_RepeatedAdIsUnconfirmed(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+repeatAdGroup+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+agCrit+`"},`+
				`{"resourceName":"customers/1234567890/adGroupCriteria/333~801"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, twoResults("customers/1234567890/adGroupAds/333~444"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	plan := samplePlanBase()
	plan.ads = append(plan.ads, plan.ads[0])
	res := &CampaignResult{}
	err := demandGenClient(t, srv).createAdGroupAndAd(context.Background(), repeatCampaign, "222",
		"https://example.org/register", plan, res)
	wantRepeat(t, err)
	if res.AdID != "" || (len(res.AdGroups) > 0 && len(res.AdGroups[0].AdIDs) != 0) {
		t.Errorf("res = %+v, want no ad ids", res)
	}
}

// The creative asset loops keep every valid id ONCE and name the repeat (#482's contract).

func TestCreateDisplayCampaign_RepeatedAssetIsUnconfirmed(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := capturedMutate(3, func(i int) string {
		return []string{"customers/1234567890/assets/900", "customers/1234567890/assets/900",
			"customers/1234567890/assets/902"}[i]
	})
	srv := demandGenTLSServer(t, displayCascade(t, displayImages(t), adGroupH, assetH,
		failHandler(t, "the ad mutate after a repeated asset")))
	c := demandGenClient(t, srv)
	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	wantRepeat(t, err)
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900,902" {
		t.Errorf("CreativeAssetIDs = %q, want every valid id once: 900,902", got)
	}
}

func TestCreateDemandGenCampaign_RepeatedAssetIsUnconfirmed(t *testing.T) {
	assetH := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, twoResults("customers/1234567890/assets/900"))
	}
	srv := demandGenTLSServer(t, demandGenCascade(t, twoImages(t), assetH,
		failHandler(t, "the ad mutate after a repeated asset")))
	c := demandGenClient(t, srv)
	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	wantRepeat(t, err)
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900" {
		t.Errorf("CreativeAssetIDs = %q, want 900 once", got)
	}
}

func TestCreateVideoAd_RepeatedAssetIsUnconfirmed(t *testing.T) {
	c := createVideoAdClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, twoResults("customers/1234567890/assets/900"))
	}, failHandler(t, "adGroupAds:mutate"))
	assetIDs, adID, err := c.createVideoAd(context.Background(), repeatAdGroup, "333",
		"https://example.org/register", twoVideoPlan(t))
	wantRepeat(t, err)
	if adID != "" || strings.Join(assetIDs, ",") != "900" {
		t.Errorf("assetIDs = %v, adID = %q; want 900 once and no ad", assetIDs, adID)
	}
}

func TestCreatePerformanceMaxCampaign_RepeatedAssetIsUnconfirmed(t *testing.T) {
	assetH, _ := capturedMutate(pmaxAssetCount, func(i int) string {
		if i == 1 {
			i = 0
		}
		return fmt.Sprintf("customers/1234567890/assets/90%d", i)
	})
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH,
		failHandler(t, "googleAds:mutate")))
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)
	res, err := demandGenClient(t, srv).CreatePerformanceMaxCampaign(context.Background(), in)
	wantRepeat(t, err)
	var want []string
	for i := 0; i < pmaxAssetCount; i++ {
		if i != 1 {
			want = append(want, fmt.Sprintf("90%d", i))
		}
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != strings.Join(want, ",") {
		t.Errorf("CreativeAssetIDs = %q, want every valid id once: %q", got,
			strings.Join(want, ","))
	}
}

// The Demand Gen video ad's atomic mutate: the repeated video asset is named, and the ids
// recorded are the logo's and each distinct asset's once.
func TestCreateDemandGenCampaign_VideoAdRepeatedAssetIsUnconfirmed(t *testing.T) {
	const (
		video950 = `{"assetResult":{"resourceName":"customers/1234567890/assets/950"}}`
		cta952   = `{"assetResult":{"resourceName":"customers/1234567890/assets/952"}}`
		ad       = `{"adGroupAdResult":{"resourceName":"customers/1234567890/adGroupAds/333~444"}}`
	)
	_, c, base := newDGRig(t, videoLogo(t), atomicAnswer(
		`{"mutateOperationResponses":[`+video950+`,`+video950+`,`+cta952+`,`+ad+`]}`))
	in := dgVideoInput(base)
	in.DemandGenVideoCreative.YouTubeVideoIDs = []string{"dQw4w9WgXcQ", "abcdefghijk"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	wantRepeat(t, err)
	if res == nil || res.AdID != "" {
		t.Fatalf("res = %+v, want a partial with no ad", res)
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != "900,950,952" {
		t.Errorf("CreativeAssetIDs = %q, want 900,950,952", got)
	}
}
