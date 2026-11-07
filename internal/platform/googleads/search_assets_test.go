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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// #477: Search campaign image, business-logo and business-name assets, and shared negative
// keyword lists. The fakes answer in Google's real mutate-response shapes; the request shapes
// they accept are the ones the 2026-11-03 validateOnly probes verified, except where the
// production code marks a shape NOT LIVE-VERIFIED.

const (
	searchImageRef1 = ResolvedImageRefPrefix + "11111111-1111-4111-8111-111111111111"
	searchImageRef2 = ResolvedImageRefPrefix + "22222222-2222-4222-8222-222222222222"
	searchLogoRef   = ResolvedImageRefPrefix + "33333333-3333-4333-8333-333333333333"
	// searchLandscapeRef is a landscapeAdImages entry (#538).
	searchLandscapeRef = ResolvedImageRefPrefix + "44444444-4444-4444-8444-444444444444"
	sharedSetA         = "customers/1234567890/sharedSets/501"
	sharedSetB         = "customers/1234567890/sharedSets/502"
	testCampaign       = "customers/1234567890/campaigns/222"
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
	// searches are the googleAds:search bodies; searchReply overrides the shared set type
	// read's answer, which by default is an ENABLED NEGATIVE_KEYWORDS row per named set (#548).
	searches    []string
	searchReply func(body []byte) (int, string)
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
		case "googleAds:search":
			s.mu.Lock()
			s.searches = append(s.searches, string(body))
			override := s.searchReply
			s.mu.Unlock()
			reply(w, body, override, func(b []byte) string {
				return sharedSetSearchReply(t, b, "NEGATIVE_KEYWORDS", "ENABLED")
			})
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
	return noExistingCampaign(NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond)))
}

// sharedSetQueryRE pulls the quoted resource names out of the shared set type read's IN list.
var sharedSetQueryRE = regexp.MustCompile(`'(customers/\d+/sharedSets/\d+)'`)

// sharedSetSearchReply answers the shared set type read (#548) with one row per resource name
// the query names, each of the given type and status, in Google's REST search shape.
func sharedSetSearchReply(t *testing.T, body []byte, typ, status string) string {
	t.Helper()
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("decode googleAds:search body: %v", err)
	}
	rows := make([]string, 0, 2)
	for _, m := range sharedSetQueryRE.FindAllStringSubmatch(req.Query, -1) {
		rows = append(rows, sharedSetRowJSON(m[1], typ, status))
	}
	return `{"results":[` + strings.Join(rows, ",") + `]}`
}

// sharedSetRowJSON is one shared_set search row.
func sharedSetRowJSON(name, typ, status string) string {
	return fmt.Sprintf(`{"sharedSet":{"resourceName":%q,"type":%q,"status":%q}}`,
		name, typ, status)
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
		{"21 images refused", assets{AdImages: many(21)},
			"at most 20 ad images (square and landscape together), got 21"},
		// One cap over both shapes, as Google counts them (#538): 10 + 10 accepted,
		// 19 + 2 and 0 + 21 refused, and 20 landscape alone accepted.
		{"10 square + 10 landscape accepted",
			assets{AdImages: many(10), LandscapeAdImages: many(20)[10:]}, ""},
		{"20 landscape accepted", assets{LandscapeAdImages: many(20)}, ""},
		{"19 square + 2 landscape refused",
			assets{AdImages: many(19), LandscapeAdImages: many(21)[19:]},
			"at most 20 ad images (square and landscape together), got 21"},
		{"21 landscape refused", assets{LandscapeAdImages: many(21)},
			"at most 20 ad images (square and landscape together), got 21"},
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

// The slot table holds Google's boundaries as probed on #538 (see searchImageSlots): each
// image one step past a boundary is refused at the fetch phase, before the budget mutate, so
// no request at all is sent. A 1.91:1 image is refused in adImages, the square slot, and has
// its own landscapeAdImages slot. The logo's 128x128 is the documented specification, kept
// above the API's 16x16 floor on purpose.
func TestCreateCampaign_RefusesAMisShapedSearchImageBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
		landscape       bool
		w, h            int
	}{
		{"landscape in the square slot", searchImageRef1, "Search ad image", false, 600, 314},
		{"small ad image", searchImageRef1, "below the 300x300 minimum", false, 299, 299},
		{"short ad image", searchImageRef1, "below the 300x300 minimum", false, 300, 299},
		{"square ad image past 1%", searchImageRef1, "an aspect ratio of 1.0133", false, 304, 300},
		{"square in the landscape slot", searchLandscapeRef, "Search landscape ad image", true, 600, 600},
		{"narrow landscape", searchLandscapeRef, "below the 600x314 minimum", true, 599, 314},
		{"short landscape", searchLandscapeRef, "below the 600x314 minimum", true, 600, 313},
		{"landscape past 1%", searchLandscapeRef, "an aspect ratio of 1.9395", true, 1218, 628},
		{"small logo", searchLogoRef, "below the 128x128 minimum", false, 127, 127},
		{"logo past 1%", searchLogoRef, "an aspect ratio of 1.0156", false, 130, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{}
			in := searchAssetInput(t)
			if tc.landscape {
				in.SearchAssets.LandscapeAdImages = []string{tc.ref}
			}
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

// Every image Google accepted at a boundary on #538 is accepted here too and sent: a square
// exactly 1% off (303x300, which the float comparison used to refuse), landscape ad images at
// 600x314 and at -0.96% (1188x628), and a logo at +0.78% (129x128). Both ad image shapes link
// under AD_IMAGE, square entries first, in one request.
func TestCreateCampaign_SendsSearchImagesAtGooglesAcceptedBoundaries(t *testing.T) {
	s := &searchAssetServer{}
	in := searchAssetInput(t)
	landscape2 := ResolvedImageRefPrefix + "45454545-4545-4545-8545-454545454545"
	in.SearchAssets.LandscapeAdImages = []string{searchLandscapeRef, landscape2}
	in.ResolvedImages[searchImageRef1] = pngOf(t, 303, 300)
	in.ResolvedImages[searchLandscapeRef] = pngOf(t, 600, 314)
	in.ResolvedImages[landscape2] = pngOf(t, 1188, 628)
	in.ResolvedImages[searchLogoRef] = pngOf(t, 129, 128)
	res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, googleAds, _ := s.snapshot()
	if len(googleAds) != 1 {
		t.Fatalf("got %d googleAds:mutate calls, want 1", len(googleAds))
	}
	ops := decodeSearchAssetOps(t, googleAds[0])
	const assets = 6 // two square, two landscape, the logo, the name
	if len(ops) != 2*assets {
		t.Fatalf("got %d operations, want %d assets + %d links", len(ops), assets, assets)
	}
	wantData := []string{
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchImageRef1]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchImageRef2]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchLandscapeRef]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[landscape2]),
		base64.StdEncoding.EncodeToString(in.ResolvedImages[searchLogoRef]),
	}
	wantFields := []string{"AD_IMAGE", "AD_IMAGE", "AD_IMAGE", "AD_IMAGE", "BUSINESS_LOGO", "BUSINESS_NAME"}
	for i := 0; i < assets; i++ {
		a := ops[i].AssetOperation
		if a == nil {
			t.Fatalf("op %d = %+v, want an asset operation", i, ops[i])
		}
		if i < len(wantData) && (a.Create.ImageAsset == nil || a.Create.ImageAsset.Data != wantData[i]) {
			t.Errorf("asset %d is not the expected image, in slot order", i)
		}
		l := ops[assets+i].CampaignAssetOperation
		if l == nil || l.Create.FieldType != wantFields[i] || l.Create.Asset != a.Create.ResourceName {
			t.Errorf("link %d = %+v, want a %s link of %s", i, l, wantFields[i], a.Create.ResourceName)
		}
	}
	if !containsStep(res.Steps, "Search campaign assets linked: 6 (4 ad image(s), a business logo, a business name)") {
		t.Errorf("steps = %v, want the four ad images counted together", res.Steps)
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
	// 21 entries, 20 distinct once trimmed: the cap counts what is attached.
	atCapWithRepeat := append(append([]string(nil), overCap[:20]...), " "+overCap[0]+" ")
	// 30 distinct: the walk refuses at the 21st, whatever follows.
	farOverCap := make([]string, 30)
	for i := range farOverCap {
		farOverCap[i] = fmt.Sprintf("customers/1234567890/sharedSets/%d", 601+i)
	}
	// 21 distinct, then a malformed 22nd: the cap refusal wins, so the walk stopped at 21.
	overCapThenMalformed := append(append([]string(nil), overCap...), "not-a-resource-name")
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
			err: "with a positive canonical id"},
		{name: "another account", lists: []string{"customers/9999999999/sharedSets/501"},
			err: "belongs to customer \"9999999999\""},
		{name: "id past int64",
			lists: []string{"customers/1234567890/sharedSets/9223372036854775808"},
			err:   "with a positive canonical id"},
		{name: "id at int64 max",
			lists: []string{"customers/1234567890/sharedSets/9223372036854775807"},
			want:  []string{"customers/1234567890/sharedSets/9223372036854775807"}},
		{name: "id zero", lists: []string{"customers/1234567890/sharedSets/0"},
			err: "with a positive canonical id"},
		{name: "leading-zero customer", lists: []string{"customers/01234567890/sharedSets/501"},
			err: "belongs to customer \"01234567890\""},
		{name: "21 entries, 20 distinct", lists: atCapWithRepeat, want: overCap[:20]},
		{name: "over the cap", lists: overCap,
			err: "more than 20 distinct shared negative keyword lists; the maximum is 20"},
		{name: "far over the cap", lists: farOverCap,
			err: "more than 20 distinct shared negative keyword lists; the maximum is 20"},
		{name: "over the cap, then malformed", lists: overCapThenMalformed,
			err: "more than 20 distinct shared negative keyword lists; the maximum is 20"},
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
		"googleAds:search",
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
	// Every image asset carries the name built from its bytes (#491); the business name
	// text asset and the four campaign links carry none.
	assertImageAssetsNamed(t, googleAds[0])
	var raw struct {
		MutateOperations []map[string]struct {
			Create map[string]any `json:"create"`
		} `json:"mutateOperations"`
	}
	if err := json.Unmarshal([]byte(googleAds[0]), &raw); err != nil {
		t.Fatalf("decode googleAds:mutate body: %v", err)
	}
	for i := 3; i < 8; i++ {
		for kind, op := range raw.MutateOperations[i] {
			if name, ok := op.Create["name"]; ok {
				t.Errorf("op %d (%s) sends name %v, want none", i, kind, name)
			}
		}
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

// ---------------------------------------------------------------------------
// The shared set type read (#548)
// ---------------------------------------------------------------------------

// Every named list is read in ONE query, on the connection's account, for its type and status,
// before the budget. The query names exactly the trimmed, de-duplicated lists.
func TestCreateCampaign_SharedListTypesAreReadInOneQuery(t *testing.T) {
	s := &searchAssetServer{}
	in := sampleInput()
	in.SharedNegativeKeywordLists = []string{" " + sharedSetA, sharedSetB, sharedSetA}
	if _, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	paths, _, _ := s.snapshot()
	if len(paths) == 0 || paths[0] != "googleAds:search" {
		t.Fatalf("paths = %v, want the shared set read first", paths)
	}
	s.mu.Lock()
	searches := append([]string(nil), s.searches...)
	s.mu.Unlock()
	if len(searches) != 1 {
		t.Fatalf("got %d searches, want exactly one: %v", len(searches), searches)
	}
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(searches[0]), &req); err != nil {
		t.Fatalf("decode search body: %v", err)
	}
	want := "SELECT shared_set.resource_name, shared_set.type, shared_set.status FROM shared_set " +
		"WHERE shared_set.resource_name IN ('" + sharedSetA + "', '" + sharedSetB + "')"
	if req.Query != want {
		t.Errorf("query = %q\nwant    %q", req.Query, want)
	}
}

// No list, no read: a Search create that names none sends no googleAds:search.
func TestCreateCampaign_NoSharedListsNoTypeRead(t *testing.T) {
	s := &searchAssetServer{}
	if _, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	paths, _, _ := s.snapshot()
	for _, p := range paths {
		if p == "googleAds:search" {
			t.Fatalf("paths = %v, want no shared set read", paths)
		}
	}
}

// A list that is not an ENABLED NEGATIVE_KEYWORDS set of this account is refused before any
// write, as a pre-flight refusal naming the list, its id and what it is. List A is always a
// good one, so the refusal must be of list 2.
func TestCreateCampaign_RefusesSharedListsThatAreNotNegativeKeywordLists(t *testing.T) {
	good := sharedSetRowJSON(sharedSetA, "NEGATIVE_KEYWORDS", "ENABLED")
	rowB := func(typ, status string) string {
		return `{"results":[` + good + `,` + sharedSetRowJSON(sharedSetB, typ, status) + `]}`
	}
	cases := []struct {
		name    string
		body    string
		want    string
		notWant string
	}{
		{name: "negative placements", body: rowB("NEGATIVE_PLACEMENTS", "ENABLED"),
			want: `is a shared set of type NEGATIVE_PLACEMENTS, not NEGATIVE_KEYWORDS`},
		{name: "brands", body: rowB("BRANDS", "ENABLED"),
			want: `is a shared set of type BRANDS, not NEGATIVE_KEYWORDS`},
		{name: "account-level negative keywords",
			body: rowB("ACCOUNT_LEVEL_NEGATIVE_KEYWORDS", "ENABLED"),
			want: `is a shared set of type ACCOUNT_LEVEL_NEGATIVE_KEYWORDS, not NEGATIVE_KEYWORDS`},
		{name: "webpages", body: rowB("WEBPAGES", "ENABLED"),
			want: `is a shared set of type WEBPAGES, not NEGATIVE_KEYWORDS`},
		{name: "unrecognised type is not echoed", body: rowB("SOME_FUTURE_TYPE", "ENABLED"),
			want: "of a type this client does not recognise", notWant: "SOME_FUTURE_TYPE"},
		{name: "missing type", body: rowB("", "ENABLED"),
			want: "of a type this client does not recognise"},
		{name: "removed", body: rowB("NEGATIVE_KEYWORDS", "REMOVED"),
			want: `is REMOVED in account "1234567890"`},
		{name: "unknown status", body: rowB("NEGATIVE_KEYWORDS", "UNKNOWN"),
			want: `is not ENABLED in account "1234567890"`, notWant: "UNKNOWN"},
		{name: "not in the account", body: `{"results":[` + good + `]}`,
			want: `was not found in account "1234567890"`},
		{name: "empty answer", body: `{}`,
			// the first list is the first missing one
			want: `list "customers/1234567890/sharedSets/501" (shared set "501") was not found`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{searchReply: func([]byte) (int, string) {
				return http.StatusOK, tc.body
			}}
			in := sampleInput()
			in.SharedNegativeKeywordLists = []string{sharedSetA, sharedSetB}
			res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("res = %+v, err = %v; want a pre-create refusal", res, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("err = %v, want it not to echo %q", err, tc.notWant)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			if !ok {
				t.Fatalf("err = %v, want a domain.PreflightRefusal", err)
			}
			wantID := `(shared set "502")`
			if tc.name == "empty answer" {
				wantID = `(shared set "501")`
			}
			if !strings.Contains(msg, wantID) {
				t.Errorf("operator message = %q, want it to name %s", msg, wantID)
			}
			paths, _, _ := s.snapshot()
			if !reflect.DeepEqual(paths, []string{"googleAds:search"}) {
				t.Errorf("requests = %v, want only the shared set read", paths)
			}
		})
	}
}

// A read that fails, or answers with rows that cannot be trusted, stops the create before any
// write. It is NOT a pre-flight refusal (the caller's input was not judged), and an upstream
// status stays on the chain for the dispatch layer's "answered HTTP n" mapping.
func TestCreateCampaign_SharedListTypeReadFailureFailsClosed(t *testing.T) {
	good := sharedSetRowJSON(sharedSetA, "NEGATIVE_KEYWORDS", "ENABLED")
	goodB := sharedSetRowJSON(sharedSetB, "NEGATIVE_KEYWORDS", "ENABLED")
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		want       string
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable,
			body: `{"error":{"code":14}}`, wantStatus: http.StatusServiceUnavailable},
		{name: "refused read", status: http.StatusBadRequest,
			body: `{"error":{"code":3}}`, wantStatus: http.StatusBadRequest},
		{name: "bare null", status: http.StatusOK, body: `null`},
		{name: "undecodable row", status: http.StatusOK,
			body: `{"results":[` + good + `,{"sharedSet":"x"}]}`, want: "did not decode"},
		{name: "a set not asked for", status: http.StatusOK,
			body: `{"results":[` + good + `,` + goodB + `,` +
				sharedSetRowJSON("customers/1234567890/sharedSets/999", "NEGATIVE_KEYWORDS",
					"ENABLED") + `]}`,
			want: "not asked for"},
		{name: "another account's set", status: http.StatusOK,
			body: `{"results":[` + good + `,` +
				sharedSetRowJSON("customers/9999999999/sharedSets/502", "NEGATIVE_KEYWORDS",
					"ENABLED") + `]}`,
			want: "not asked for"},
		{name: "a set twice", status: http.StatusOK,
			body: `{"results":[` + good + `,` + goodB + `,` + goodB + `]}`,
			want: `shared set "502" twice`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchAssetServer{searchReply: func([]byte) (int, string) {
				return tc.status, tc.body
			}}
			in := sampleInput()
			in.SharedNegativeKeywordLists = []string{sharedSetA, sharedSetB}
			res, err := newSearchAssetClient(t, s).CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("res = %+v, err = %v; want the create stopped before any write", res, err)
			}
			if _, ok := domain.AsPreflightRefusal(err); ok {
				t.Errorf("err = %v, want a read failure, not a pre-flight refusal", err)
			}
			if !strings.Contains(err.Error(), "could not be checked") {
				t.Errorf("err = %v, want the could-not-be-checked failure", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if got, _ := domain.UpstreamStatus(err); got != tc.wantStatus {
				t.Errorf("upstream status = %d, want %d", got, tc.wantStatus)
			}
			paths, _, _ := s.snapshot()
			for _, p := range paths {
				if p != "googleAds:search" {
					t.Errorf("%s sent after the failed read: %v", p, paths)
				}
			}
		})
	}
}
