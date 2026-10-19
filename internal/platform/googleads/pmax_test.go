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
	// The count the activation gate reads, on the path where everything worked. It is
	// what Google CONFIRMED, not what was sent, so it is asserted against the number of
	// link results the server returned.
	if res.AssetGroupAssetLinks == nil {
		t.Fatal("AssetGroupAssetLinks is nil after a successful link mutate; the activation gate needs a recorded count")
	}
	if *res.AssetGroupAssetLinks != pmaxAssetCount {
		t.Errorf("AssetGroupAssetLinks = %d, want %d — one per confirmed link", *res.AssetGroupAssetLinks, pmaxAssetCount)
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

	// The final-URL-expansion opt-out is the one post-create action v23 left with no
	// API at all, and forgetting it silently spends the event's budget on URLs with no
	// UTM attribution. It reaches the operator ONLY through Steps, so assert it is
	// there: a refactor that drops it would otherwise be invisible until a campaign
	// had already overspent.
	var sawExpansionWarning bool
	for _, s := range res.Steps {
		if strings.Contains(s, "final-URL expansion") {
			sawExpansionWarning = true
		}
	}
	if !sawExpansionWarning {
		t.Errorf("Steps never tells the operator to turn off final-URL expansion: %v", res.Steps)
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
		// Asserted as the literal `false` rather than merely present: the account has
		// Brand Guidelines on, and anything that made this field serialize away — an
		// `omitempty` added later, say — would take the account's setting instead and
		// put the create back on the REQUIRED_BUSINESS_NAME_ASSET_NOT_LINKED 400 that
		// orphaned a budget on 2026-10-16.
		`"brandGuidelinesEnabled":false`,
		`"maximizeConversions"`,
		"geoTargetTypeSetting",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("campaigns:mutate body is missing %q: %s", want, body)
		}
	}
	// Shapes that must never appear on this channel: the first two are Search-only, and
	// `urlExpansionOptOut` is a field v23 removed outright. Google rejects an unknown
	// field AFTER the budget mutate has committed, so re-adding it does not fail safe —
	// it orphans a paid budget, which is exactly what it did on 2026-10-16.
	for _, forbidden := range []string{"networkSettings", "manualCpc", "urlExpansionOptOut"} {
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
	// And the count that tells the ACTIVATION GATE those two facts apart. The group id above
	// is kept deliberately, so the gate cannot read it as "ready to serve" — a RECORDED zero
	// is what distinguishes "the group exists and is empty" from "no group was created".
	// Nil here would be read as a row predating the field and the campaign would activate
	// with nothing attached.
	if res.AssetGroupAssetLinks == nil {
		t.Fatal("AssetGroupAssetLinks is nil on a failed link mutate; nil means 'not recorded' and the activation gate would let this campaign activate empty")
	}
	if *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %d, want 0 — no link was confirmed", *res.AssetGroupAssetLinks)
	}
}

// TestCreatePerformanceMaxCampaign_DuplicateBudgetWarnsAboutExpansion covers the arm that
// fails one mutate EARLIER than any of the campaign arms. The budget name is this client's
// idempotency key, so a re-dispatch of the same brief on this channel is rejected at
// campaignBudgets:mutate and campaigns:mutate is never attempted. The attempt that created
// that budget may well have got past Step 2 — leaving a campaign billing with final-URL
// expansion on — so this return carries the hedged sentence rather than nothing. It is also
// the proof that campaigns:mutate stays untouched: the handler fails the test if it is hit.
func TestCreatePerformanceMaxCampaign_DuplicateBudgetWarnsAboutExpansion(t *testing.T) {
	images := pmaxImages(t)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, r)
			return
		}
		t.Errorf("nothing may be mutated once the budget name is taken: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a duplicate budget name must be reported")
	}
	if res == nil {
		t.Fatal("a duplicate name means a prior attempt got this far; the partial result must say so")
	}
	if res.CampaignBudgetID != "" {
		t.Errorf("CampaignBudgetID = %q, want empty — this attempt created no budget", res.CampaignBudgetID)
	}
	var saw bool
	for _, s := range res.Steps {
		if s == expansionActionRequiredIfExists {
			saw = true
		}
	}
	if !saw {
		t.Errorf("the prior attempt may have left a billing campaign and these Steps never mention final-URL expansion: %v", res.Steps)
	}
}

// TestCreatePerformanceMaxCampaign_UnconfirmedCampaignStillWarnsAboutExpansion covers the
// two returns that sit ABOVE the campaign-created step and still hand back a campaign.
// A 2xx from campaigns:mutate whose results carry no usable resource name created the
// campaign — only the name came back unusable — so it is billable with expansion on while
// the result carries no CampaignID at all, leaving these Steps the single thing that says
// so. DUPLICATE_CAMPAIGN_NAME is the weaker case and is worded hedged: the name is taken,
// so a campaign by that name exists, created by an attempt that may itself have died
// before it printed the step. Only a definite non-ambiguous error stays silent, because
// then no campaign was created.
func TestCreatePerformanceMaxCampaign_UnconfirmedCampaignStillWarnsAboutExpansion(t *testing.T) {
	cases := []struct {
		name     string
		campaign http.HandlerFunc
		want     string
	}{
		{
			name:     "2xx with no resource name",
			campaign: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"results":[]}`)) },
			want:     expansionActionRequired,
		},
		{
			name:     "duplicate campaign name",
			campaign: gaqlError(http.StatusBadRequest, "campaignError", "DUPLICATE_CAMPAIGN_NAME"),
			want:     expansionActionRequiredIfExists,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			images := pmaxImages(t)
			srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				if body, ok := images[r.URL.Path]; ok {
					_, _ = w.Write(body)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
					okBudget(w, r)
				case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
					tc.campaign(w, r)
				default:
					t.Errorf("nothing may be mutated once the campaign create is unconfirmed: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			c := demandGenClient(t, srv)
			in := demandGenInput()
			in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

			res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("an unconfirmed campaign create must be reported")
			}
			if res == nil {
				t.Fatal("the budget was created; the partial result must carry it")
			}
			if res.CampaignID != "" {
				t.Errorf("CampaignID = %q, want empty — no usable resource name came back", res.CampaignID)
			}
			var saw bool
			for _, s := range res.Steps {
				if s == tc.want {
					saw = true
				}
			}
			if !saw {
				t.Errorf("a campaign may already be billing and its Steps never mention final-URL expansion: %v", res.Steps)
			}
		})
	}
}

// TestCreatePerformanceMaxCampaign_PartialFailureStillWarnsAboutExpansion pins the
// warning to the paths that need it MOST. Every return past the campaign create hands
// back a campaign that already exists and is already billable with final-URL expansion
// on, and internal/dispatch/googleads.go retains and persists the claim on a non-nil
// result — so this operator reads these Steps, finishes the asset-group work by hand in
// the Google Ads UI and launches from there, more surely than the happy-path one does.
// Appended at the end of the cascade the warning reached only the success path; this test
// fails if it moves back there.
func TestCreatePerformanceMaxCampaign_PartialFailureStillWarnsAboutExpansion(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	linkH := gaqlError(http.StatusBadRequest, "assetGroupAssetError", "DUPLICATE_RESOURCE")
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH, linkH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a failed link mutate must be reported")
	}
	if res == nil || res.CampaignID == "" {
		t.Fatalf("a campaign was created; the partial result must carry it: %+v", res)
	}
	var sawExpansionWarning bool
	for _, s := range res.Steps {
		if strings.Contains(s, "final-URL expansion") {
			sawExpansionWarning = true
		}
	}
	if !sawExpansionWarning {
		t.Errorf("a billable campaign exists and its Steps never tell the operator to turn off final-URL expansion: %v", res.Steps)
	}
}

// TestCreatePerformanceMaxCampaign_ShortLinkResponseRecordsZero covers the AMBIGUOUS
// half of the link outcome: a 2xx that describes fewer links than were sent. Some links
// may well exist, but this client cannot say which, and "cannot say" must record zero
// rather than nothing — recording nothing would be indistinguishable from a row written
// before the field existed, and the activation gate would let the campaign serve on the
// strength of a group whose contents nobody has confirmed.
func TestCreatePerformanceMaxCampaign_ShortLinkResponseRecordsZero(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	shortLinkH := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{{"resourceName": "customers/1234567890/assetGroupAssets/555~900~HEADLINE"}},
		})
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH, shortLinkH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a short link mutate response must be reported, not treated as success")
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come ALONGSIDE a result, never (nil, err)")
	}
	if res.AssetGroupAssetLinks == nil {
		t.Fatal("AssetGroupAssetLinks is nil on an unconfirmed link mutate; the ambiguous case must record zero, not nothing")
	}
	if *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %d, want 0 — nothing was confirmed", *res.AssetGroupAssetLinks)
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

	// wantSub, not a bare "it errored": this preflight refuses for a dozen reasons and
	// the fixture it mutates is a whole campaign input, so an unbound assertion stays
	// green if the narrowing under test is deleted and some unrelated clause refuses
	// instead — which is exactly the regression this test exists to catch.
	refused := map[string]struct {
		mutate  func(*CampaignInput)
		wantSub string
	}{
		"device bid modifiers": {func(in *CampaignInput) {
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "MOBILE", BidModifier: 1.2}}
		}, "device bid modifiers are not supported on PerformanceMax Campaign"},
		"excluded age ranges": {func(in *CampaignInput) { in.ExcludedAgeRanges = []string{"AGE_RANGE_18_24"} },
			"campaign-level demographic exclusions are not supported on PerformanceMax Campaign"},
		"excluded genders": {func(in *CampaignInput) { in.ExcludedGenders = []string{"MALE"} },
			"campaign-level demographic exclusions are not supported on PerformanceMax Campaign"},
		"ad groups": {func(in *CampaignInput) { in.AdGroups = []AdGroupSpec{{Name: "extra"}} },
			"multiple ad groups are a SEARCH capability"},
	}
	for name, tc := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			in := demandGenInput()
			in.PerformanceMaxCreative = fullPMaxCreative()
			tc.mutate(&in)
			_, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
			if err == nil {
				t.Fatalf("Performance Max accepted %s; it must refuse rather than drop", name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("refused by the wrong rule: got %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}
