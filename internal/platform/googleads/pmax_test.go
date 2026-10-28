// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
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

	// Final-URL expansion now goes out opted-out in the create payload, and the Steps say
	// so. Asserting the exact const rather than a substring is the point: the two
	// sentences this file can produce differ by whether an operator has to DO anything,
	// and a substring match would pass on the hedged "turn it OFF if it is on" being
	// printed for a campaign this request created with the opt-out — telling the operator
	// to go fix something that is already correct.
	var sawExpansionStep bool
	for _, s := range res.Steps {
		if s == expansionOptedOutStep {
			sawExpansionStep = true
		}
		if s == expansionActionRequiredIfExists {
			t.Errorf("a campaign THIS request created carries the hedged step for an earlier attempt's campaign: %v", res.Steps)
		}
	}
	if !sawExpansionStep {
		t.Errorf("Steps never records that final-URL expansion was opted out at create: %v", res.Steps)
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
		// The final-URL-expansion opt-out, asserted as the literal entry rather than as
		// the field merely being present, for the same reason: an omitted or empty
		// `assetAutomationSettings` leaves the outcome to Google's default instead of to
		// this service, which is the bug.
		//
		// The entry is pinned as ONE braced object — both halves in serialization order —
		// rather than as two independent `strings.Contains` calls over the whole body.
		// The type names WHICH automation is being turned off and the status is what
		// turns it off, so the PAIRING is the assertion: once this slice carries more
		// than one entry (the struct's own doc anticipates Google adding types), a body
		// holding `{FINAL_URL_EXPANSION…, OPTED_IN}` alongside `{TEXT_ASSET_AUTOMATION,
		// OPTED_OUT}` satisfies two separate substrings while expansion is back ON. The
		// braced form fails on that body, which is the point. The key is asserted
		// alongside rather than spliced onto the front of the entry, so that adding a
		// SECOND legitimate opt-out entry — at either end of the slice — does not fail a
		// test about the first one.
		`"assetAutomationSettings":[`,
		`{"assetAutomationType":"` + assetAutomationFinalURLExpansion +
			`","assetAutomationStatus":"` + assetAutomationOptedOut + `"}`,
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
// that budget may well have got past Step 2, leaving a campaign billing — and THAT campaign
// is not covered by the opt-out this request would have sent, because an earlier attempt
// created it and may predate the service sending the setting at all (the two prod dispatches
// of 2026-10-16 are exactly that). So this return carries the hedged ACTION REQUIRED
// sentence rather than the informational one or nothing. It is also
// the proof that campaigns:mutate stays untouched: the handler fails the test if it is hit.
//
// The arm is now reached only after the reuse lookup DECLINES the existing budget — here
// because the handler reports no budget of that name at all. That is the pairing this test
// pins: a declined reuse must leave the pre-reuse behaviour exactly as it was, hedge
// included. The accepted-reuse path is a different arm and does not carry the hedge; see
// TestCreatePerformanceMaxCampaign_DuplicateBudgetReusesOrphan.
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
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			// The reuse lookup. Answering with no rows makes it refuse (Google said the
			// name is taken and the account says no budget holds it — a contradiction),
			// which is what keeps this test on the refusal arm it is about.
			_, _ = w.Write([]byte(`{"results":[]}`))
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

// TestCreatePerformanceMaxCampaign_ExpansionStepPerCampaignCreateArm covers EVERY return
// between `campaigns:mutate` and the campaign-created step — the five arms that decide
// which final-URL-expansion sentence, if any, the operator reads. Deliberately named for
// the arms rather than for "warns": two of the five assert `expansionOptedOutStep`, which
// this package's own doc calls a plain step and not an ACTION REQUIRED, and one asserts
// neither sentence.
//
// What splits them is WHOSE campaign the arm is talking about:
//
//   - A 2xx whose results carry no usable resource name — empty results, or a resource
//     name of the wrong kind — created the campaign in THIS request; only the name came
//     back unusable. It went out with the opt-out, so the informational step is the
//     truthful one, and since the result carries no CampaignID these Steps are the single
//     thing saying a billable campaign exists.
//   - DUPLICATE_CAMPAIGN_NAME and an AMBIGUOUS outcome (5xx/429, where the mutate may have
//     committed before the answer was lost) are about a campaign SOME OTHER attempt
//     created, possibly before this service sent the setting at all — so both get the
//     hedged ACTION REQUIRED.
//   - A definite non-ambiguous error — a 4xx that is not the duplicate — is the one arm
//     that stays silent, because no campaign was created and there is nothing whose
//     expansion setting could be wrong. `want: ""` asserts that silence: a sentence
//     appearing here would send an operator to hunt for a campaign that does not exist.
func TestCreatePerformanceMaxCampaign_ExpansionStepPerCampaignCreateArm(t *testing.T) {
	cases := []struct {
		name     string
		campaign http.HandlerFunc
		want     string // "" means NEITHER sentence may appear
		// wantErr pins the case to the ARM it is about. Several arms hand back the
		// same sentence, so without this a handler that drifted into a neighbouring
		// arm would still satisfy `want` and the arm it claims to cover would go
		// untested — which is the gap this table was widened to close.
		wantErr string
	}{
		{
			name:     "2xx with no resource name",
			campaign: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"results":[]}`)) },
			want:     expansionOptedOutStep,
			wantErr:  "2xx with no/malformed resource name",
		},
		{
			// The other half of "2xx with an unusable name": the body parses and
			// carries an id segment, so firstResourceName is satisfied and it is
			// validateCampaignResource that refuses it — a different arm, same
			// sentence. The customer id matches testAccount so it is the KIND that
			// is wrong, which is the case the guard exists for.
			name: "2xx with a wrong-kind resource name",
			campaign: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/adGroups/222"}]}`))
			},
			want:    expansionOptedOutStep,
			wantErr: "malformed campaign resource name",
		},
		{
			name:     "duplicate campaign name",
			campaign: gaqlError(http.StatusBadRequest, "campaignError", "DUPLICATE_CAMPAIGN_NAME"),
			want:     expansionActionRequiredIfExists,
			wantErr:  "already exists (DUPLICATE_CAMPAIGN_NAME",
		},
		{
			// 5xx: the mutate may have committed and only the answer was lost, so
			// createOutcomeAmbiguous treats it as a campaign that may exist.
			name:     "ambiguous outcome",
			campaign: gaqlError(http.StatusInternalServerError, "internalError", "INTERNAL_ERROR"),
			want:     expansionActionRequiredIfExists,
			wantErr:  "may exist — verify in Google Ads before retrying",
		},
		{
			// A definite 4xx that is not the duplicate: no campaign exists, so the
			// Steps must say nothing about expansion at all.
			name:     "definite non-duplicate error",
			campaign: gaqlError(http.StatusBadRequest, "campaignError", "CANNOT_TARGET_CONTENT_NETWORK"),
			want:     "",
			wantErr:  "campaign creation failed (budget",
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
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one naming %q — this case must land on that arm", err, tc.wantErr)
			}
			if res.CampaignID != "" {
				t.Errorf("CampaignID = %q, want empty — no usable resource name came back", res.CampaignID)
			}
			// Both directions, because the two sentences differ by whether the operator
			// has to go and change something: printing the wrong one either sends them
			// to fix a campaign this request already opted out, or stays quiet about one
			// that an earlier attempt may have left expanding. On the silent arm `want`
			// is empty, so BOTH are forbidden and none is required.
			var saw bool
			for _, s := range res.Steps {
				switch s {
				case expansionOptedOutStep, expansionActionRequiredIfExists:
					if s == tc.want {
						saw = true
						continue
					}
					t.Errorf("Steps carry the wrong final-URL-expansion sentence (want %q): %v", tc.want, res.Steps)
				}
			}
			if tc.want != "" && !saw {
				t.Errorf("a campaign may already be billing and its Steps never say %q: %v", tc.want, res.Steps)
			}
		})
	}
}

// TestCreatePerformanceMaxCampaign_PartialFailureStillReportsExpansion pins the
// step to the paths that need it MOST. Every return past the campaign create hands back a
// campaign that already exists and is already billable, and internal/dispatch/googleads.go
// retains and persists the claim on a non-nil result — so this operator reads these Steps,
// finishes the asset-group work by hand in the Google Ads UI and launches from there, more
// surely than the happy-path one does. What that operator needs to know here is that the
// expansion opt-out DID go out with the create, so the campaign they are about to finish is
// not quietly stripping the UTM attribution: without the step they would have no way to
// tell, and the Google Ads UI is the only other place that answers it. Appended at the end
// of the cascade the sentence reached only the success path; this test fails if it moves
// back there.
func TestCreatePerformanceMaxCampaign_PartialFailureStillReportsExpansion(t *testing.T) {
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
	var sawExpansionStep bool
	for _, s := range res.Steps {
		if s == expansionOptedOutStep {
			sawExpansionStep = true
		}
		if s == expansionActionRequiredIfExists {
			t.Errorf("this request created the campaign and opted it out; the hedged step is for an earlier attempt's: %v", res.Steps)
		}
	}
	if !sawExpansionStep {
		t.Errorf("a billable campaign exists and its Steps never record that final-URL expansion was opted out at create: %v", res.Steps)
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

// TestCreatePerformanceMaxCampaign_DuplicateBudgetReusesOrphan is the other half of the
// pairing: the same DUPLICATE_NAME refusal, but the account shows the budget holding the
// name is an adoptable orphan, so the cascade continues against it instead of dead-ending.
//
// This is the case the 2026-10-16 and 2026-10-20 dispatches produced and could not get
// past: each one died at campaigns:mutate having already created a budget, and because a
// non-shared budget with no campaign on it has no Google Ads UI surface to delete it from,
// the name stayed taken forever. Adoption is the only way a brief in that state is ever
// dispatched again.
//
// The hedge is deliberately asserted ABSENT here. expansionActionRequiredIfExists warns
// that an earlier attempt may have left a campaign billing outside this request's opt-out;
// a successful reuse has just proved from the campaign side that NO campaign references
// this budget, so there is nothing left to warn about and the sentence would be false.
func TestCreatePerformanceMaxCampaign_DuplicateBudgetReusesOrphan(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	linkH, _ := okPMaxLinks()
	images := pmaxImages(t)
	in := demandGenInput()
	budgetName := ComposeName(budgetKindFor(campaignKindPerformanceMax), in)

	// mu guards the captures below: httptest serves each request on its own goroutine,
	// matching newBudgetLookupServer's own pattern.
	var (
		mu           sync.Mutex
		searches     int
		campaignBody map[string]any
	)
	cascade := pmaxCascade(t, images, assetH, groupH, linkH)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			// The create still fails — nothing about reuse changes what Google says.
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			// First the budget lookup, then the attachment check. The orphan already
			// carries this brief's amount, so no alignment mutate is expected — and the
			// budget handler above would fail the test if one were attempted.
			mu.Lock()
			searches++
			n := searches
			mu.Unlock()
			if n == 1 {
				_, _ = w.Write([]byte(`{"results":[` +
					string(budgetRow(budgetRowOpts{name: budgetName, amountMicros: "500000000"})) + `]}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			body := decode(t, r)
			mu.Lock()
			campaignBody = body
			mu.Unlock()
			okCampaign(w, r)
		default:
			cascade(w, r)
		}
	})
	c := demandGenClient(t, srv)
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("an adoptable orphan budget must not fail the dispatch: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the reused 777", res.CampaignBudgetID)
	}
	if res.CampaignID == "" || res.AssetGroupID != "555" {
		t.Errorf("the whole cascade must run: campaign %q, asset group %q", res.CampaignID, res.AssetGroupID)
	}
	// The campaign has to attach to the budget that was actually adopted.
	mu.Lock()
	defer mu.Unlock()
	ops, _ := campaignBody["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("campaign body = %v", campaignBody)
	}
	op, _ := ops[0].(map[string]any)
	create, _ := op["create"].(map[string]any)
	if got, _ := create["campaignBudget"].(string); got != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("campaignBudget = %q, want the reused budget's resource name", got)
	}
	var sawReuse, sawHedge bool
	for _, s := range res.Steps {
		if strings.Contains(s, "Campaign budget reused: 777") {
			sawReuse = true
		}
		if s == expansionActionRequiredIfExists {
			sawHedge = true
		}
	}
	if !sawReuse {
		t.Errorf("the result must say the budget was reused rather than created: %v", res.Steps)
	}
	if sawHedge {
		t.Errorf("a proven-unattached budget leaves nothing to hedge about, but the Steps warn anyway: %v", res.Steps)
	}
}

// TestCreatePerformanceMaxCampaign_AmbiguousBudgetResolvesTheOrphan is issue #385 on the
// channel that produced it. campaignBudgets:mutate answers 5xx — which says nothing about
// whether the write landed — and the budget DID land. Before the account was read back,
// this returned an id-less partial every time, so the committed budget became an orphan
// nobody was told about: invisible (a non-shared budget is in no Shared Library page) and
// blocking the next retry of the same brief by name.
func TestCreatePerformanceMaxCampaign_AmbiguousBudgetResolvesTheOrphan(t *testing.T) {
	in := demandGenInput()
	budgetName := ComposeName(budgetKindFor(campaignKindPerformanceMax), in)
	images := pmaxImages(t)

	// mu guards searches: httptest serves each request on its own goroutine.
	var (
		mu       sync.Mutex
		searches int
	)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			serverError(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			// The budget lookup, then the attachment check — a budget created seconds ago
			// and never attached is referenced by nothing.
			mu.Lock()
			searches++
			n := searches
			mu.Unlock()
			if n == 1 {
				_, _ = w.Write([]byte(`{"results":[` +
					string(budgetRow(budgetRowOpts{name: budgetName, amountMicros: "500000000"})) + `]}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			t.Errorf("no further mutate may be sent after an unconfirmed budget, got %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	c := demandGenClient(t, srv)
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a budget create that was never acknowledged must still fail the dispatch")
	}
	if res == nil || res.CampaignBudgetID != "777" {
		t.Fatalf("the partial must carry the id the account resolved, got %+v", res)
	}
	if !strings.Contains(err.Error(), "WAS created") {
		t.Errorf("error = %q, want it to state the budget exists rather than that it may", err)
	}
	// The id is what CreateOutcome reads, and it is the whole operator-visible difference:
	// a budget they can name instead of one they must hunt for in change history.
	if outcome, ok := CreateOutcome(res, err); !ok || outcome != domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed {
		t.Errorf("outcome = %q (ok=%v), want %q", outcome, ok, domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed)
	}
	mu.Lock()
	defer mu.Unlock()
	if searches != 2 {
		t.Errorf("want the lookup and its attachment check, got %d searches", searches)
	}
}

// TestCreatePerformanceMaxCampaign_AmbiguousBudgetNotVisibleKeepsTheClaim is the other
// direction: the account was read, repeatedly, and holds nothing under this name. The
// sentence says so, which is what the operator needs, but the partial and the claim stay —
// an absence is a property of the read, not proof that the mutate never landed.
func TestCreatePerformanceMaxCampaign_AmbiguousBudgetNotVisibleKeepsTheClaim(t *testing.T) {
	in := demandGenInput()
	images := pmaxImages(t)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			serverError(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			// A honoured query that matched nothing. Never `{"results":null}`.
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			t.Errorf("no further mutate may be sent after an unconfirmed budget, got %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	c := demandGenClient(t, srv)
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("want an error")
	}
	if res == nil {
		t.Fatal("expected a partial: an ambiguous mutating create must retain the claim")
	}
	if res.CampaignBudgetID != "" {
		t.Errorf("CampaignBudgetID = %q, want empty — no budget was found", res.CampaignBudgetID)
	}
	if !strings.Contains(err.Error(), "no budget of that name is live in the account") {
		t.Errorf("error = %q, want it to report the absence", err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("an absence does not settle an ambiguous mutate: %v", err)
	}
	if outcome, ok := CreateOutcome(res, err); !ok || outcome != domain.CreateOutcomeUnconfirmed {
		t.Errorf("outcome = %q (ok=%v), want %q", outcome, ok, domain.CreateOutcomeUnconfirmed)
	}
}
