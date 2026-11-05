// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
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
// leaves the two asset-group mutates to the caller — assets:mutate, and the one
// googleAds:mutate that creates the group with its links — so each test varies only
// the part it is about. Any ad-group or ad path is a FAILURE rather than a default:
// Performance Max has neither, and a cascade that grew one would otherwise pass.
//
// A STANDALONE assetGroups:mutate or assetGroupAssets:mutate create is refused the way
// Google refuses it (#437): a non-retail Performance Max asset group cannot be created in
// isolation, only together with its links in one GoogleAdsService.Mutate. This fake
// accepted the standalone shape for as long as the client sent it, which is how every
// create with creative reached production failing at that step.
func pmaxCascade(t *testing.T, images map[string][]byte, assetH, groupH http.HandlerFunc) http.HandlerFunc {
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
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			groupH(w, r)
		case strings.HasSuffix(r.URL.Path, "assetGroupAssets:mutate"), strings.HasSuffix(r.URL.Path, "assetGroups:mutate"):
			t.Errorf("standalone %s: Google refuses a Performance Max asset group created apart from its links", r.URL.Path)
			gaqlError(http.StatusBadRequest, "assetGroupError", "NOT_ENOUGH_HEADLINE_ASSET")(w, r)
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

// groupMutateBody is the decoded googleAds:mutate request, enough to assert on its shape.
type groupMutateBody struct {
	MutateOperations []groupMutateOperation `json:"mutateOperations"`
}

// groupMutateOperation is one decoded MutateOperation of groupMutateBody.
type groupMutateOperation struct {
	AssetGroupOperation *struct {
		Create map[string]any `json:"create"`
	} `json:"assetGroupOperation"`
	AssetGroupAssetOperation *struct {
		Create map[string]any `json:"create"`
	} `json:"assetGroupAssetOperation"`
}

// groupMutate answers googleAds:mutate the way Google does: one response per operation, in
// order, each carrying the result arm of its own operation. `links` caps how many link
// results are returned (-1 for all of them), so a test can describe fewer than were sent.
// The asset group resolves to id 555. Returns the handler and a reader for the request body.
func groupMutate(links int) (http.HandlerFunc, func() string) {
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
		var req groupMutateBody
		_ = json.Unmarshal(raw, &req)
		parts := make([]string, 0, len(req.MutateOperations))
		sent := 0
		for _, op := range req.MutateOperations {
			switch {
			case op.AssetGroupOperation != nil:
				parts = append(parts, `{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`)
			case op.AssetGroupAssetOperation != nil && (links < 0 || sent < links):
				parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+linkNameFor(op)+`"}}`)
				sent++
			}
		}
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, read
}

// linkNameFor is the assetGroupAssets resource name Google returns for one link operation:
// `<groupId>~<assetId>~<fieldType>`, with the group resolved to 555 (#439).
func linkNameFor(op groupMutateOperation) string {
	asset, _ := op.AssetGroupAssetOperation.Create["asset"].(string)
	fieldType, _ := op.AssetGroupAssetOperation.Create["fieldType"].(string)
	return "customers/1234567890/assetGroupAssets/555~" + resourceID(asset) + "~" + fieldType
}

func okPMaxGroup() (http.HandlerFunc, func() string) { return groupMutate(-1) }

// groupMutateOK is groupMutate(-1) for a caller that needs no capture of the body.
func groupMutateOK(w http.ResponseWriter, r *http.Request) {
	h, _ := groupMutate(-1)
	h(w, r)
}

func TestCreatePerformanceMaxCampaign_HappyPath(t *testing.T) {
	assetH, readAssets := okPMaxAssets()
	groupH, readGroup := okPMaxGroup()
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
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
		t.Fatal("AssetGroupAssetLinks is nil after a successful asset group mutate; the activation gate needs a recorded count")
	}
	if *res.AssetGroupAssetLinks != pmaxAssetCount {
		t.Errorf("AssetGroupAssetLinks = %d, want %d — one per confirmed link", *res.AssetGroupAssetLinks, pmaxAssetCount)
	}
	// Performance Max has no ad group and no ad, so neither id may be invented.
	if res.AdGroupID != "" || res.AdID != "" {
		t.Errorf("AdGroupID = %q, AdID = %q — Performance Max has neither", res.AdGroupID, res.AdID)
	}

	group := readGroup()
	// ONE request carrying the group first and then every link, each link naming the group
	// by the request's temporary resource name — the shape Google requires (#437).
	var req groupMutateBody
	if err := json.Unmarshal([]byte(group), &req); err != nil {
		t.Fatalf("googleAds:mutate body is not JSON: %v", err)
	}
	if n := len(req.MutateOperations); n != 1+pmaxAssetCount {
		t.Fatalf("googleAds:mutate carries %d operation(s), want the group plus %d links", n, pmaxAssetCount)
	}
	head := req.MutateOperations[0].AssetGroupOperation
	if head == nil {
		t.Fatalf("the first operation must create the asset group: %s", group)
	}
	const tempGroup = "customers/1234567890/assetGroups/-1"
	if head.Create["resourceName"] != tempGroup {
		t.Errorf("asset group resourceName = %v, want the temporary %s", head.Create["resourceName"], tempGroup)
	}
	for i, op := range req.MutateOperations[1:] {
		if op.AssetGroupAssetOperation == nil || op.AssetGroupOperation != nil {
			t.Fatalf("operation %d must be exactly one asset-group-asset create: %s", i+1, group)
		}
		if op.AssetGroupAssetOperation.Create["assetGroup"] != tempGroup {
			t.Errorf("link %d names asset group %v, want the temporary %s", i, op.AssetGroupAssetOperation.Create["assetGroup"], tempGroup)
		}
	}
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
	for _, want := range []string{"customers/1234567890/assets/900", assetFieldHeadline, assetFieldBusinessName, assetFieldLogo} {
		if !strings.Contains(group, want) {
			t.Errorf("googleAds:mutate body is missing %q: %s", want, group)
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

	// Built on the TEST goroutine: pmaxImages renders through pngOf, which ends in
	// t.Fatalf, and FailNow from a handler does not stop the test — it can leave the
	// server blocked while a deferred Close runs.
	cascade := pmaxCascade(t, pmaxImages(t), assetH, groupH)
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

// The group and its links are one ATOMIC request (#437), so a definite refusal of it means
// none of them exists: no group id may be reported, or an operator would go looking for a
// group that is not there. The assets were created by the earlier request and must
// survive the error — they are the only handle on that account-level litter.
func TestCreatePerformanceMaxCampaign_GroupRefusalReportsNoGroup(t *testing.T) {
	assetH, _ := okPMaxAssets()
	// A DEFINITE 4xx, not a 5xx: this test is about what a refusal leaves, not the wording
	// of an ambiguous outcome (see the next test).
	groupH := gaqlError(http.StatusBadRequest, "assetGroupError", "DUPLICATE_NAME")
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a refused asset group mutate must be reported")
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come ALONGSIDE a result, never (nil, err)")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %q — a definite refusal of an atomic request applied nothing and must not read as unconfirmed", err)
	}
	if res.AssetGroupID != "" {
		t.Errorf("AssetGroupID = %q, want empty — the request was atomic and refused, so no group exists", res.AssetGroupID)
	}
	if len(res.CreativeAssetIDs) != pmaxAssetCount {
		t.Errorf("CreativeAssetIDs = %d, want %d — the assets exist", len(res.CreativeAssetIDs), pmaxAssetCount)
	}
	if res.CampaignID == "" {
		t.Error("CampaignID must be carried on the partial result")
	}
	// Recorded as a ZERO, not left nil: nil would read as a row predating the field.
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
}

// An AMBIGUOUS outcome of the same request — a 5xx, which may have been applied — reports
// UNCONFIRMED and still records no group id and zero links: this client cannot say the
// group exists, and the activation gate must refuse until an operator has looked.
func TestCreatePerformanceMaxCampaign_AmbiguousGroupMutateIsUnconfirmed(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a 5xx on the asset group mutate must be reported")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %q, want UNCONFIRMED — the group and its links may exist", err)
	}
	if res == nil || res.AssetGroupID != "" {
		t.Fatalf("result = %+v, want a partial with no asset group id", res)
	}
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
}

// A 2xx with no asset group result in first place cannot confirm the group, so no id is
// invented from it.
func TestCreatePerformanceMaxCampaign_MissingGroupResultIsUnconfirmed(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[{"assetGroupAssetResult":{"resourceName":"customers/1234567890/assetGroupAssets/555~900~HEADLINE"}}]}`)
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("error = %v, want UNCONFIRMED", err)
	}
	if res == nil || res.AssetGroupID != "" {
		t.Errorf("result = %+v, want no asset group id", res)
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
	groupH := gaqlError(http.StatusBadRequest, "assetGroupError", "DUPLICATE_NAME")
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a failed asset group mutate must be reported")
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
// half of the link outcome: a 2xx that describes the group and fewer links than were sent.
// The request was atomic and answered 2xx, so the group exists and its id is kept; but
// this client cannot say which links do, and "cannot say" must record zero rather than
// nothing — recording nothing would be indistinguishable from a row written before the
// field existed, and the activation gate would let the campaign serve on the strength of a
// group whose contents nobody has confirmed.
func TestCreatePerformanceMaxCampaign_ShortLinkResponseRecordsZero(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := groupMutate(1)
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a short link response must be reported, not treated as success")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %q, want UNCONFIRMED", err)
	}
	if res == nil {
		t.Fatal("past the campaign create the error must come ALONGSIDE a result, never (nil, err)")
	}
	if res.AssetGroupID != "555" {
		t.Errorf("AssetGroupID = %q, want 555 — a 2xx on an atomic request means the group exists", res.AssetGroupID)
	}
	if res.AssetGroupAssetLinks == nil {
		t.Fatal("AssetGroupAssetLinks is nil on an unconfirmed link outcome; the ambiguous case must record zero, not nothing")
	}
	if *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %d, want 0 — not every link was confirmed", *res.AssetGroupAssetLinks)
	}
}

// A response with the RIGHT NUMBER of entries can still fail to describe a link: an entry
// with no assetGroupAssetResult is not a confirmed link, and counting entries alone would
// read it as one. The group is kept, the count stays zero.
func TestCreatePerformanceMaxCampaign_EmptyLinkResultIsUnconfirmed(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req groupMutateBody
		_ = json.Unmarshal(raw, &req)
		parts := []string{`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`}
		for i := 1; i < len(req.MutateOperations); i++ {
			if i == 1 {
				parts = append(parts, `{}`) // one link entry carries no result
				continue
			}
			parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+linkNameFor(req.MutateOperations[i])+`"}}`)
		}
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("error = %v, want UNCONFIRMED — one link was not described", err)
	}
	if res == nil || res.AssetGroupID != "555" {
		t.Fatalf("result = %+v, want the group kept", res)
	}
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
}

// A link result Google answered with must name exactly the link its operation asked for —
// this account, this group, the operation's asset and field type (#439); anything else does
// not describe a link this request created and is not counted. The first link's result is
// replaced by the case's name, built from what that operation actually sent, and every other
// result is correct, so each case fails on its own name. One bad name keeps the count at zero,
// as an empty result does above.
func TestCreatePerformanceMaxCampaign_ForeignLinkResultIsUnconfirmed(t *testing.T) {
	cases := map[string]func(right, asset, fieldType string) string{
		"another account": func(_, asset, ft string) string {
			return "customers/9999999999/assetGroupAssets/555~" + asset + "~" + ft
		},
		"another group": func(_, asset, ft string) string {
			return "customers/1234567890/assetGroupAssets/777~" + asset + "~" + ft
		},
		"another kind":  func(_, asset, _ string) string { return "customers/1234567890/assets/" + asset },
		"another asset": func(_, _, ft string) string { return "customers/1234567890/assetGroupAssets/555~1~" + ft },
		"another field type": func(_, asset, _ string) string {
			return "customers/1234567890/assetGroupAssets/555~" + asset + "~NOT_THIS_FIELD"
		},
		"group only":         func(_, _, _ string) string { return "customers/1234567890/assetGroupAssets/555~" },
		"no field type":      func(_, asset, _ string) string { return "customers/1234567890/assetGroupAssets/555~" + asset },
		"a trailing segment": func(right, _, _ string) string { return right + "~EXTRA" },
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			assetH, _ := okPMaxAssets()
			groupH := func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var req groupMutateBody
				_ = json.Unmarshal(raw, &req)
				parts := []string{`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`}
				for i := 1; i < len(req.MutateOperations); i++ {
					op := req.MutateOperations[i]
					link := linkNameFor(op)
					if i == 1 {
						asset, _ := op.AssetGroupAssetOperation.Create["asset"].(string)
						fieldType, _ := op.AssetGroupAssetOperation.Create["fieldType"].(string)
						link = bad(link, resourceID(asset), fieldType)
					}
					parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+link+`"}}`)
				}
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
			}
			srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
			c := demandGenClient(t, srv)
			in := demandGenInput()
			in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

			res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Fatalf("error = %v, want UNCONFIRMED — link result for the %s case is not the link its operation asked for", err, name)
			}
			if res == nil || res.AssetGroupID != "555" {
				t.Fatalf("result = %+v, want the group kept", res)
			}
			if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
				t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
			}
		})
	}
}

// The response is index-aligned, so one valid link name repeated for every result describes
// one link, not all of them (#439): each result is matched against its own operation.
func TestCreatePerformanceMaxCampaign_RepeatedLinkResultIsUnconfirmed(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req groupMutateBody
		_ = json.Unmarshal(raw, &req)
		parts := []string{`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`}
		first := linkNameFor(req.MutateOperations[1])
		for i := 1; i < len(req.MutateOperations); i++ {
			parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+first+`"}}`)
		}
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("error = %v, want UNCONFIRMED — one link name repeated describes one link", err)
	}
	if res == nil || res.AssetGroupID != "555" {
		t.Fatalf("result = %+v, want the group kept", res)
	}
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
}

// More link results than link operations is not a description of this request: the extra
// result has no operation to match, and must neither panic the loop nor count (#439).
func TestCreatePerformanceMaxCampaign_ExtraLinkResultIsUnconfirmed(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req groupMutateBody
		_ = json.Unmarshal(raw, &req)
		parts := []string{`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`}
		last := ""
		for i := 1; i < len(req.MutateOperations); i++ {
			last = linkNameFor(req.MutateOperations[i])
			parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+last+`"}}`)
		}
		parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+last+`"}}`)
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Fatalf("error = %v, want UNCONFIRMED — the response describes one link more than was sent", err)
	}
	if res == nil || res.AssetGroupID != "555" {
		t.Fatalf("result = %+v, want the group kept", res)
	}
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
}

// The field type is Google's enum name echoed back, compared case-insensitively as the
// extension links are: a casing change is not the wrong link, so the group still confirms.
func TestCreatePerformanceMaxCampaign_LinkFieldTypeCaseIsNotARefusal(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req groupMutateBody
		_ = json.Unmarshal(raw, &req)
		parts := []string{`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}}`}
		for i := 1; i < len(req.MutateOperations); i++ {
			name := linkNameFor(req.MutateOperations[i])
			cut := strings.LastIndex(name, "~") + 1
			parts = append(parts, `{"assetGroupAssetResult":{"resourceName":"`+name[:cut]+strings.ToLower(name[cut:])+`"}}`)
		}
		_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), assetH, groupH))
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("error = %v, want none — a lower-cased field type is the same link", err)
	}
	if res == nil || res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks == 0 {
		t.Fatalf("AssetGroupAssetLinks = %v, want every link confirmed", res)
	}
}

func TestCreatePerformanceMaxCampaign_ShortAssetResponseIsUnconfirmed(t *testing.T) {
	// A 2xx describing FEWER assets than were sent leaves assets unaccounted for.
	shortH := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{{"resourceName": "customers/1234567890/assets/900"}},
		})
	}
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), shortH, failHandler(t, "googleAds:mutate")))
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
	srv := demandGenTLSServer(t, pmaxCascade(t, pmaxImages(t), wrongH, failHandler(t, "googleAds:mutate")))
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
	// Every same-account id is reported, AFTER the bad one as well as before it (#481
	// review): those assets exist. The error names the bad index.
	var want []string
	for i := 0; i < pmaxAssetCount; i++ {
		if i != 2 {
			want = append(want, "90"+string(rune('0'+i)))
		}
	}
	if got := strings.Join(res.CreativeAssetIDs, ","); got != strings.Join(want, ",") {
		t.Errorf("CreativeAssetIDs = %q, want every id but 902: %q", got, strings.Join(want, ","))
	}
	if !strings.Contains(err.Error(), "at index 2") {
		t.Errorf("error = %q, want it to name index 2", err)
	}
}

func TestCreatePerformanceMaxCampaign_NoAssetGroupSaysSoLoudly(t *testing.T) {
	// No creative at all — legal, because adoption of a hand-built asset group must
	// not be refused — but the campaign cannot serve, and the step list has to say it.
	srv := demandGenTLSServer(t, pmaxCascade(t, nil, failHandler(t, "assets:mutate"), failHandler(t, "googleAds:mutate")))
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
// Gen refuses four of the five criteria kinds (its languages go on its ad group, #459),
// Performance Max refuses only two.
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
	cascade := pmaxCascade(t, images, assetH, groupH)
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
