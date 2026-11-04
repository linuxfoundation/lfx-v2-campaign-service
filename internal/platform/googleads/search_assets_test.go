// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// #477: Search campaign image, business-logo and business-name assets, and shared negative
// keyword lists. The fakes answer in Google's real mutate-response shapes; the request shapes
// they accept are the ones the 2026-11-03 validateOnly probes verified, except where the
// production code marks a shape NOT LIVE-VERIFIED.

const (
	searchImageRef1 = ResolvedImageRefPrefix + "11111111-1111-4111-8111-111111111111"
	searchImageRef2 = ResolvedImageRefPrefix + "22222222-2222-4222-8222-222222222222"
	searchLogoRef   = ResolvedImageRefPrefix + "33333333-3333-4333-8333-333333333333"
	sharedSetA      = "customers/1234567890/sharedSets/501"
	sharedSetB      = "customers/1234567890/sharedSets/502"
	testCampaign    = "customers/1234567890/campaigns/222"
)

// searchAssetServer serves the Search cascade, records every mutate path in order and the
// googleAds:mutate and campaignSharedSets:mutate bodies, and lets a test override those two.
type searchAssetServer struct {
	mu          sync.Mutex
	paths       []string
	googleAds   []string
	sharedSets  []string
	googleReply func(body []byte) (int, string)
	sharedReply func(body []byte) (int, string)
	criteriaSeq int
}

// reply writes an overridden answer, or the echo when there is no override.
func reply(w http.ResponseWriter, body []byte, override func([]byte) (int, string),
	echo func([]byte) string) {
	if override == nil {
		_, _ = io.WriteString(w, echo(body))
		return
	}
	status, out := override(body)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, out)
}

func (s *searchAssetServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		s.mu.Lock()
		s.paths = append(s.paths, path)
		s.mu.Unlock()
		switch path {
		case "campaignBudgets:mutate":
			okBudget(w, r)
		case "campaigns:mutate":
			okCampaign(w, r)
		case "campaignCriteria:mutate":
			s.mu.Lock()
			base := 600 + 100*s.criteriaSeq
			s.criteriaSeq++
			s.mu.Unlock()
			_, _ = io.WriteString(w, perGroupResults(body, "campaignCriteria", "222", base))
		case "campaignSharedSets:mutate":
			s.mu.Lock()
			s.sharedSets = append(s.sharedSets, string(body))
			override := s.sharedReply
			s.mu.Unlock()
			reply(w, body, override, sharedSetEcho)
		case "googleAds:mutate":
			s.mu.Lock()
			s.googleAds = append(s.googleAds, string(body))
			override := s.googleReply
			s.mu.Unlock()
			reply(w, body, override, searchAssetEcho)
		case "assets:mutate":
			_, _ = io.WriteString(w, extensionAssetEcho(body))
		case "campaignAssets:mutate":
			_, _ = io.WriteString(w, campaignAssetEcho(body))
		case "adGroups:mutate":
			_, _ = io.WriteString(w,
				`{"results":[{"resourceName":"customers/1234567890/adGroups/330"}]}`)
		case "adGroupAds:mutate":
			_, _ = io.WriteString(w, perGroupResults(body, "adGroupAds", "330", 440))
		case "adGroupCriteria:mutate":
			_, _ = io.WriteString(w, criteriaEcho(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (s *searchAssetServer) snapshot() (paths, googleAds, sharedSets []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths = append([]string(nil), s.paths...)
	googleAds = append([]string(nil), s.googleAds...)
	sharedSets = append([]string(nil), s.sharedSets...)
	return paths, googleAds, sharedSets
}

func newSearchAssetClient(t *testing.T, s *searchAssetServer) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(s.handler(t))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

// extensionAssetEcho answers an extension assets:mutate with asset 700, 701, ….
func extensionAssetEcho(body []byte) string {
	var req struct {
		Operations []json.RawMessage `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		parts = append(parts,
			fmt.Sprintf(`{"resourceName":"customers/1234567890/assets/%d"}`, 700+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

// campaignAssetEcho answers an extension campaignAssets:mutate with the link each operation
// asked for.
func campaignAssetEcho(body []byte) string {
	var req struct {
		Operations []struct {
			Create campaignAssetCreate `json:"create"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for _, op := range req.Operations {
		parts = append(parts, fmt.Sprintf(
			`{"resourceName":"customers/1234567890/campaignAssets/222~%s~%s"}`,
			resourceID(op.Create.Asset), op.Create.FieldType))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

// searchAssetOp is one decoded googleAds:mutate operation.
type searchAssetOp struct {
	AssetOperation *struct {
		Create struct {
			ResourceName string `json:"resourceName"`
			ImageAsset   *struct {
				Data string `json:"data"`
			} `json:"imageAsset"`
			TextAsset *struct {
				Text string `json:"text"`
			} `json:"textAsset"`
		} `json:"create"`
	} `json:"assetOperation"`
	CampaignAssetOperation *struct {
		Create campaignAssetCreate `json:"create"`
	} `json:"campaignAssetOperation"`
}

func decodeSearchAssetOps(t *testing.T, body string) []searchAssetOp {
	t.Helper()
	var req struct {
		MutateOperations []searchAssetOp `json:"mutateOperations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode googleAds:mutate body: %v (%s)", err, body)
	}
	return req.MutateOperations
}

// realAssetID maps a temporary asset name (…/assets/-n) to the id the fakes assign: 800+n-1.
func realAssetID(temp string) string {
	var n int
	_, _ = fmt.Sscanf(resourceID(temp), "-%d", &n)
	return fmt.Sprintf("%d", 800+n-1)
}

func assetResultJSON(name string) string {
	return `{"assetResult":{"resourceName":"` + name + `"}}`
}

func campaignAssetResultJSON(name string) string {
	return `{"campaignAssetResult":{"resourceName":"` + name + `"}}`
}

func searchAssetMutateResponse(results ...string) string {
	return `{"mutateOperationResponses":[` + strings.Join(results, ",") + `]}`
}

// searchAssetEcho answers a googleAds:mutate as Google does: one assetResult per asset
// operation and one campaignAssetResult per link, naming the real asset id and the field type
// the operation asked for.
func searchAssetEcho(body []byte) string {
	var req struct {
		MutateOperations []searchAssetOp `json:"mutateOperations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.MutateOperations))
	for _, op := range req.MutateOperations {
		switch {
		case op.AssetOperation != nil:
			parts = append(parts, assetResultJSON("customers/1234567890/assets/"+
				realAssetID(op.AssetOperation.Create.ResourceName)))
		case op.CampaignAssetOperation != nil:
			c := op.CampaignAssetOperation.Create
			parts = append(parts, campaignAssetResultJSON(
				"customers/1234567890/campaignAssets/222~"+realAssetID(c.Asset)+"~"+c.FieldType))
		}
	}
	return searchAssetMutateResponse(parts...)
}

// sharedSetEcho answers campaignSharedSets:mutate with one link per operation, naming the
// campaign and shared set that operation asked for.
func sharedSetEcho(body []byte) string {
	var req struct {
		Operations []struct {
			Create campaignSharedSetCreate `json:"create"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for _, op := range req.Operations {
		parts = append(parts, `{"resourceName":"customers/1234567890/campaignSharedSets/`+
			resourceID(op.Create.Campaign)+`~`+resourceID(op.Create.SharedSet)+`"}`)
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

// searchAssetInput is a Search input carrying two ad images, a logo and a business name, all
// pre-resolved so no image server is needed.
func searchAssetInput(t *testing.T) CampaignInput {
	t.Helper()
	in := sampleInput()
	in.SearchAssets = SearchCampaignAssets{
		AdImages:           []string{searchImageRef1, searchImageRef2},
		BusinessLogoImages: []string{searchLogoRef},
		BusinessName:       " Linux Foundation ",
	}
	in.ResolvedImages = map[string][]byte{
		searchImageRef1: pngOf(t, 300, 300),
		searchImageRef2: pngOf(t, 1200, 1200),
		searchLogoRef:   pngOf(t, 128, 128),
	}
	return in
}

var nonSearchKinds = []string{
	campaignKindDemandGen, campaignKindPerformanceMax, campaignKindVideo, campaignKindDisplay,
}

// ---------------------------------------------------------------------------
// validateSearchAssets — the pure half
// ---------------------------------------------------------------------------

func TestValidateSearchAssets_AbsentIsAccepted(t *testing.T) {
	plan, err := validateSearchAssets(campaignKindSearch, sampleInput())
	if err != nil || plan.present {
		t.Fatalf("plan = %+v, err = %v; an absent input must be accepted, not present", plan, err)
	}
	// A whitespace-only name alone is not an asset.
	in := sampleInput()
	in.SearchAssets.BusinessName = "   "
	if plan, err := validateSearchAssets(campaignKindSearch, in); err != nil || plan.present {
		t.Fatalf("plan = %+v, err = %v; a blank business name is not an asset", plan, err)
	}
}

func TestValidateSearchAssets_RefusedOffSearch(t *testing.T) {
	for _, kind := range nonSearchKinds {
		for name, a := range map[string]SearchCampaignAssets{
			"images": {AdImages: []string{"https://cdn.example.org/a.png"}},
			"logo":   {BusinessLogoImages: []string{"https://cdn.example.org/l.png"}},
			"name":   {BusinessName: "Linux Foundation"},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				in := demandGenInput()
				in.SearchAssets = a
				err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(kind, in)
				const want = "supported on Search campaigns only"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want the Search-only refusal", err)
				}
			})
		}
	}
}

func TestValidateSearchAssets_Bounds(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("https://cdn.example.org/%d.png", i)
		}
		return out
	}
	type assets = SearchCampaignAssets
	cases := []struct {
		name string
		a    assets
		want string
	}{
		{"20 images accepted", assets{AdImages: many(20)}, ""},
		{"21 images refused", assets{AdImages: many(21)}, "at most 20 ad images, got 21"},
		{"two logos refused", assets{BusinessLogoImages: many(2)},
			"at most 1 business logo, got 2"},
		{"name of 25 accepted", assets{BusinessName: strings.Repeat("a", 25)}, ""},
		{"name of 26 refused", assets{BusinessName: strings.Repeat("a", 26)},
			"display width of 26"},
		// Double-width: 13 CJK runes weigh 26.
		{"wide name refused", assets{BusinessName: strings.Repeat("会", 13)},
			"display width of 26"},
		{"http refused", assets{AdImages: []string{"http://cdn.example.org/a.png"}},
			"must be an https URL"},
		{"duplicate refused",
			assets{AdImages: []string{"https://x.org/a.png", "https://x.org/a.png"}},
			"listed more than once"},
		{"unresolved stored asset refused", assets{BusinessLogoImages: []string{searchLogoRef}},
			"no resolved image bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sampleInput()
			in.SearchAssets = tc.a
			_, err := validateSearchAssets(campaignKindSearch, in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The slot table is square on both slots at the documented minimums: a 1.91:1 image is not
// admitted as an AD_IMAGE, and a logo under 128x128 is refused — both at the fetch phase,
// before the budget mutate, so no request at all is sent.
func TestCreateCampaign_RefusesAMisShapedSearchImageBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
		w, h            int
	}{
		{"landscape ad image", searchImageRef1, "Search ad image", 600, 314},
		{"small ad image", searchImageRef1, "below the 300x300 minimum", 299, 299},
		{"small logo", searchLogoRef, "below the 128x128 minimum", 127, 127},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{}
			in := searchAssetInput(t)
			in.ResolvedImages[tc.ref] = pngOf(t, tc.w, tc.h)
			res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if res != nil {
				t.Errorf("res = %+v, want nil: nothing was created", res)
			}
			if paths, _, _ := s.snapshot(); len(paths) != 0 {
				t.Errorf("requests sent = %v, want none before the refusal", paths)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateSharedNegativeKeywordLists — the pure half
// ---------------------------------------------------------------------------

func TestValidateSharedNegativeKeywordLists(t *testing.T) {
	overCap := make([]string, 21)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("customers/1234567890/sharedSets/%d", 501+i)
	}
	cases := []struct {
		name  string
		lists []string
		want  []string
		err   string
	}{
		{name: "none", lists: nil},
		{name: "trimmed and deduped", lists: []string{" " + sharedSetA, sharedSetB, sharedSetA},
			want: []string{sharedSetA, sharedSetB}},
		{name: "blank", lists: []string{" "}, err: "list 1 is empty"},
		{name: "bare id", lists: []string{"501"},
			err: "is not a customers/<id>/sharedSets/<id> resource name"},
		{name: "wrong kind", lists: []string{"customers/1234567890/userLists/501"},
			err: "resource name"},
		{name: "leading zero", lists: []string{"customers/1234567890/sharedSets/0501"},
			err: "resource name"},
		{name: "another account", lists: []string{"customers/9999999999/sharedSets/501"},
			err: "belongs to customer 9999999999"},
		{name: "over the cap", lists: overCap,
			err: "21 shared negative keyword lists exceeds the maximum 20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateSharedNegativeKeywordLists(
				campaignKindSearch, "1234567890", tc.lists)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("lists = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateSharedNegativeKeywordLists_RefusedOffSearch(t *testing.T) {
	const want = "shared negative keyword lists are not supported on"
	for _, kind := range nonSearchKinds {
		t.Run(kind, func(t *testing.T) {
			in := demandGenInput()
			in.SharedNegativeKeywordLists = []string{sharedSetA}
			err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(kind, in)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want the Search-only refusal", err)
			}
		})
	}
}

// Another account's shared set is refused before ANY request: Google refuses it only at the
// attach, after the budget and campaign exist (the 2026-11-03 probe).
func TestCreateCampaign_RefusesAnotherAccountsSharedSetBeforeAnyRequest(t *testing.T) {
	s := &searchAssetServer{}
	in := sampleInput()
	in.SharedNegativeKeywordLists = []string{"customers/9999999999/sharedSets/501"}
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err == nil || res != nil {
		t.Fatalf("res = %+v, err = %v; want a pre-create refusal", res, err)
	}
	if paths, _, _ := s.snapshot(); len(paths) != 0 {
		t.Errorf("requests sent = %v, want none", paths)
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// The whole Search cascade with every #477 input, plus campaign negatives and a callout so
// the order among the campaign-level steps is pinned: negatives, shared lists, extensions,
// then the Search assets, all before the ad group.
func TestCreateCampaign_SearchAssetsAndSharedListsPayloadsAndOrder(t *testing.T) {
	s := &searchAssetServer{}
	in := searchAssetInput(t)
	in.NegativeKeywords = []Keyword{{Text: "free", MatchType: MatchTypeExact}}
	in.SharedNegativeKeywordLists = []string{sharedSetA, sharedSetB}
	in.Callouts = []string{"Hands-on labs"}
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	paths, googleAds, sharedSets := s.snapshot()
	wantPaths := []string{
		"campaignBudgets:mutate", "campaigns:mutate", "campaignCriteria:mutate",
		"campaignSharedSets:mutate", "assets:mutate", "campaignAssets:mutate",
		"googleAds:mutate", "adGroups:mutate", "adGroupAds:mutate",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("mutate order = %v\nwant           %v", paths, wantPaths)
	}

	// Shared lists: one operation per list, naming the campaign and the set, in order.
	var shared struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(sharedSets[0]), &shared); err != nil {
		t.Fatalf("decode campaignSharedSets body: %v", err)
	}
	wantShared := []map[string]any{
		{"campaign": testCampaign, "sharedSet": sharedSetA},
		{"campaign": testCampaign, "sharedSet": sharedSetB},
	}
	if len(shared.Operations) != 2 {
		t.Fatalf("got %d shared set operations, want 2", len(shared.Operations))
	}
	for i, op := range shared.Operations {
		if !reflect.DeepEqual(op.Create, wantShared[i]) {
			t.Errorf("shared set op %d = %v, want %v", i, op.Create, wantShared[i])
		}
	}

	// Search assets: four asset creates by temporary id (two ad images, the logo, then the
	// name), then four campaign links naming those temporary ids, in one request.
	ops := decodeSearchAssetOps(t, googleAds[0])
	if len(ops) != 8 {
		t.Fatalf("got %d googleAds:mutate operations, want 4 assets + 4 links", len(ops))
	}
	wantData := []string{
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchImageRef1]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchImageRef2]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchLogoRef]),
	}
	wantFields := []string{"AD_IMAGE", "AD_IMAGE", "BUSINESS_LOGO", "BUSINESS_NAME"}
	for i := 0; i < 4; i++ {
		a := ops[i].AssetOperation
		if a == nil || ops[i].CampaignAssetOperation != nil {
			t.Fatalf("op %d = %+v, want exactly an asset operation", i, ops[i])
		}
		wantTemp := fmt.Sprintf("customers/1234567890/assets/-%d", i+1)
		if a.Create.ResourceName != wantTemp {
			t.Errorf("asset %d temporary name = %q, want %q", i, a.Create.ResourceName, wantTemp)
		}
		img, text := a.Create.ImageAsset, a.Create.TextAsset
		if i < 3 && (img == nil || text != nil || img.Data != wantData[i]) {
			t.Errorf("asset %d is not the expected image asset: %+v", i, a.Create)
		}
		if i == 3 && (text == nil || img != nil || text.Text != "Linux Foundation") {
			t.Errorf("asset %d is not the trimmed business name text asset: %+v", i, a.Create)
		}
		l := ops[4+i].CampaignAssetOperation
		if l == nil || ops[4+i].AssetOperation != nil {
			t.Fatalf("op %d = %+v, want exactly a campaign asset operation", 4+i, ops[4+i])
		}
		want := campaignAssetCreate{
			Campaign: testCampaign, Asset: wantTemp, FieldType: wantFields[i],
		}
		if l.Create != want {
			t.Errorf("link %d = %+v, want %+v", i, l.Create, want)
		}
	}
	// No asset carries a name: the obvious label would export the caller's URL.
	if strings.Contains(googleAds[0], `"name"`) {
		t.Error("a Search asset carries a name")
	}

	if got := res.SharedNegativeKeywordListIDs; !reflect.DeepEqual(got, []string{"501", "502"}) {
		t.Errorf("SharedNegativeKeywordListIDs = %v, want [501 502]", got)
	}
	if got := res.SearchAssetIDs; !reflect.DeepEqual(got, []string{"800", "801", "802", "803"}) {
		t.Errorf("SearchAssetIDs = %v, want [800 801 802 803]", got)
	}
	for _, want := range []string{
		"Shared negative keyword lists attached: 2",
		"Search campaign assets linked: 4 (2 ad image(s), a business logo, a business name)",
	} {
		if !containsStep(res.Steps, want) {
			t.Errorf("steps = %v, want %q", res.Steps, want)
		}
	}
}

// A name alone is one text asset and one link; no image is fetched or sent.
func TestCreateCampaign_BusinessNameAloneIsOneTextAsset(t *testing.T) {
	s := &searchAssetServer{}
	in := sampleInput()
	in.SearchAssets.BusinessName = "Linux Foundation"
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, googleAds, _ := s.snapshot()
	ops := decodeSearchAssetOps(t, googleAds[0])
	if len(ops) != 2 || ops[0].AssetOperation == nil ||
		ops[0].AssetOperation.Create.TextAsset == nil || ops[1].CampaignAssetOperation == nil ||
		ops[1].CampaignAssetOperation.Create.FieldType != "BUSINESS_NAME" {
		t.Fatalf("ops = %+v, want one text asset and its BUSINESS_NAME link", ops)
	}
	if !reflect.DeepEqual(res.SearchAssetIDs, []string{"800"}) {
		t.Errorf("SearchAssetIDs = %v, want [800]", res.SearchAssetIDs)
	}
}

// Without any #477 input, neither endpoint is called: the pre-#477 request sequence.
func TestCreateCampaign_NoSearchAssetsOrSharedListsSendsNeither(t *testing.T) {
	s := &searchAssetServer{}
	_, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	paths, _, _ := s.snapshot()
	for _, p := range paths {
		if p == "googleAds:mutate" || p == "campaignSharedSets:mutate" {
			t.Errorf("sent %s with no #477 input: %v", p, paths)
		}
	}
}

// Every failure of the one atomic request is a partial result: the campaign is kept, the ad
// group is never reached, and the ids recorded are exactly the same-account assets a 2xx
// described.
func TestCreateCampaign_SearchAssetFailuresArePartialResults(t *testing.T) {
	img := assetResultJSON("customers/1234567890/assets/800")
	name := assetResultJSON("customers/1234567890/assets/801")
	link := func(campaign, asset, field string) string {
		return campaignAssetResultJSON(
			"customers/1234567890/campaignAssets/" + campaign + "~" + asset + "~" + field)
	}
	imgLink, nameLink := link("222", "800", "AD_IMAGE"), link("222", "801", "BUSINESS_NAME")
	cases := []struct {
		name            string
		status          int
		body            string
		wantUnconfirmed bool
		wantIDs         []string
	}{
		{name: "definite 4xx records nothing", status: http.StatusBadRequest,
			body: `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`},
		{name: "ambiguous 5xx records nothing", status: http.StatusInternalServerError,
			body: `{"error":{"code":13}}`, wantUnconfirmed: true},
		{name: "malformed 2xx", status: http.StatusOK, body: `not json`, wantUnconfirmed: true},
		{name: "short 2xx keeps the described assets", status: http.StatusOK,
			body:            searchAssetMutateResponse(img, name),
			wantUnconfirmed: true, wantIDs: []string{"800", "801"}},
		{name: "wrong-account asset is not recorded", status: http.StatusOK,
			body: searchAssetMutateResponse(img,
				assetResultJSON("customers/9999999999/assets/801"), imgLink, nameLink),
			wantUnconfirmed: true, wantIDs: []string{"800"}},
		{name: "link with the wrong field type", status: http.StatusOK,
			body: searchAssetMutateResponse(img, name, imgLink,
				link("222", "801", "AD_IMAGE")),
			wantUnconfirmed: true, wantIDs: []string{"800", "801"}},
		{name: "link naming another campaign", status: http.StatusOK,
			body: searchAssetMutateResponse(img, name,
				link("999", "800", "AD_IMAGE"), nameLink),
			wantUnconfirmed: true, wantIDs: []string{"800", "801"}},
		{name: "link naming another asset", status: http.StatusOK,
			body: searchAssetMutateResponse(img, name,
				link("222", "801", "AD_IMAGE"), nameLink),
			wantUnconfirmed: true, wantIDs: []string{"800", "801"}},
		{name: "missing link result", status: http.StatusOK,
			body:            searchAssetMutateResponse(img, name, `{}`, nameLink),
			wantUnconfirmed: true, wantIDs: []string{"800", "801"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{googleReply: func([]byte) (int, string) {
				return tc.status, tc.body
			}}
			in := sampleInput()
			in.SearchAssets = SearchCampaignAssets{
				AdImages:     []string{searchImageRef1},
				BusinessName: "Linux Foundation",
			}
			in.ResolvedImages = map[string][]byte{searchImageRef1: pngOf(t, 300, 300)}
			res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected the Search asset failure to surface")
			}
			if res == nil || res.CampaignID != "222" {
				t.Fatalf("res = %+v, want the campaign kept on the partial result", res)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("err = %v, UNCONFIRMED = %v, want %v", err, got, tc.wantUnconfirmed)
			}
			if !reflect.DeepEqual(res.SearchAssetIDs, tc.wantIDs) {
				t.Errorf("SearchAssetIDs = %v, want %v", res.SearchAssetIDs, tc.wantIDs)
			}
			if res.AdGroupID != "" {
				t.Errorf("AdGroupID = %q: no ad group may follow a failed step", res.AdGroupID)
			}
			paths, _, _ := s.snapshot()
			for _, p := range paths {
				if p == "adGroups:mutate" {
					t.Errorf("adGroups:mutate sent after the failed asset step: %v", paths)
				}
			}
		})
	}
}

func TestCreateCampaign_SharedListFailuresArePartialResults(t *testing.T) {
	link := func(customer, campaign, set string) string {
		return `{"resourceName":"customers/` + customer + `/campaignSharedSets/` +
			campaign + `~` + set + `"}`
	}
	results := func(links ...string) string {
		return `{"results":[` + strings.Join(links, ",") + `]}`
	}
	linkA := link("1234567890", "222", "501")
	cases := []struct {
		name            string
		status          int
		body            string
		wantUnconfirmed bool
		wantIDs         []string
	}{
		{name: "definite 4xx", status: http.StatusBadRequest, body: `{"error":{"code":3}}`},
		{name: "ambiguous 5xx", status: http.StatusServiceUnavailable,
			body: `{"error":{"code":14}}`, wantUnconfirmed: true},
		{name: "short 2xx", status: http.StatusOK, body: results(linkA), wantUnconfirmed: true},
		{name: "second link names another set", status: http.StatusOK,
			body:            results(linkA, link("1234567890", "222", "999")),
			wantUnconfirmed: true, wantIDs: []string{"501"}},
		{name: "link in another account", status: http.StatusOK,
			body:            results(link("9999999999", "222", "501"), linkA),
			wantUnconfirmed: true, wantIDs: []string{}},
		{name: "link naming another campaign", status: http.StatusOK,
			body:            results(link("1234567890", "999", "501"), linkA),
			wantUnconfirmed: true, wantIDs: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{sharedReply: func([]byte) (int, string) {
				return tc.status, tc.body
			}}
			in := sampleInput()
			in.SharedNegativeKeywordLists = []string{sharedSetA, sharedSetB}
			res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected the shared list failure to surface")
			}
			if res == nil || res.CampaignID != "222" {
				t.Fatalf("res = %+v, want the campaign kept on the partial result", res)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("err = %v, UNCONFIRMED = %v, want %v", err, got, tc.wantUnconfirmed)
			}
			if got := res.SharedNegativeKeywordListIDs; !reflect.DeepEqual(got, tc.wantIDs) {
				t.Errorf("SharedNegativeKeywordListIDs = %#v, want %#v", got, tc.wantIDs)
			}
			paths, _, _ := s.snapshot()
			for _, p := range paths {
				if p == "adGroups:mutate" || p == "googleAds:mutate" {
					t.Errorf("%s sent after the failed attach: %v", p, paths)
				}
			}
		})
	}
}

// The two new googleAds:mutate arms are spelled as v23 MutateOperation's oneof fields
// (asset_operation, campaign_asset_operation) and their results as MutateOperationResponse's
// (asset_result, campaign_asset_result). A misspelled key is refused after the campaign exists.
func TestSearchAssetMutateWireKeys(t *testing.T) {
	for want, op := range map[string]googleAdsMutateOperation{
		"assetOperation":         {AssetOperation: &mutateOperation{}},
		"campaignAssetOperation": {CampaignAssetOperation: &mutateOperation{}},
	} {
		raw, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := keys[want]; !ok || len(keys) != 1 {
			t.Errorf("operation %s carries keys %v, want exactly %q", raw, keys, want)
		}
	}
	var resp googleAdsMutateResponse
	body := searchAssetMutateResponse(assetResultJSON("customers/1/assets/2"),
		campaignAssetResultJSON("customers/1/campaignAssets/3~2~AD_IMAGE"))
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := resp.MutateOperationResponses
	if got[0].AssetResult == nil || got[1].CampaignAssetResult == nil {
		t.Errorf("result arms did not decode: %+v", resp)
	}
}

// A link result that repeats an earlier one is UNCONFIRMED and named as a repeat, as in every
// other link loop (result_ids.go); the first link's id is kept.
func TestCreateCampaign_SharedListRepeatedLinkIsUnconfirmed(t *testing.T) {
	linkA := `{"resourceName":"customers/1234567890/campaignSharedSets/222~501"}`
	s := &searchAssetServer{sharedReply: func([]byte) (int, string) {
		return http.StatusOK, `{"results":[` + linkA + `,` + linkA + `]}`
	}}
	in := sampleInput()
	in.SharedNegativeKeywordLists = []string{sharedSetA, sharedSetB}
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") ||
		!strings.Contains(err.Error(), "at index 0 and again at index 1") {
		t.Fatalf("err = %v, want UNCONFIRMED naming the repeat", err)
	}
	if res == nil || !reflect.DeepEqual(res.SharedNegativeKeywordListIDs, []string{"501"}) {
		t.Errorf("res = %+v, want the first link's id kept", res)
	}
}
