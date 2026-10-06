// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newAssetClient wires the full Search cascade plus the two mutates the extension
// path uses. Both handlers run on httptest's own goroutines, so anything they
// record for the test to read is handed back through a locked reader — never a
// bare variable, and never t.Fatalf from inside a handler.
func newAssetClient(t *testing.T, assetsH, campaignAssetsH http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignAssets:mutate"):
			campaignAssetsH(w, r)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetsH(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

func assetName(i int) string {
	return "customers/1234567890/assets/" + strconv.Itoa(700+i)
}

// capturedAssetMutate records the request body and replies with one result per
// operation, naming each result with `name`. Same locked-handoff shape as
// capturedMutate; `name` sees the operation index so a campaignAsset reply can
// mirror the asset it was asked to link.
func capturedAssetMutate(name func(i int) string) (http.HandlerFunc, func() string) {
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
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		var req mutateRequest
		_ = json.Unmarshal(b, &req)
		parts := make([]string, 0, len(req.Operations))
		for i := range req.Operations {
			parts = append(parts, `{"resourceName":"`+name(i)+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, read
}

// okAssets is the plain happy-path asset handler for tests that
// care about the result rather than the request body.
func okAssets(w http.ResponseWriter, r *http.Request) {
	h, _ := capturedAssetMutate(assetName)
	h(w, r)
}

func sampleSitelink() Sitelink {
	return Sitelink{
		Text:         "Register now",
		Description1: "Early bird pricing",
		Description2: "Ends 1 December",
		FinalURL:     "https://events.example.org/register",
	}
}

// ---------------------------------------------------------------------------
// Sitelinks
// ---------------------------------------------------------------------------

// A sitelink click is an ad click and must attribute to google/cpc. An untagged
// sitelink destination lands in the attribution data as organic traffic while
// the campaign paid for the click, which is a reporting defect no dashboard can
// recover from after the fact.
func TestValidateSitelinks_TagsTheDestinationLikeTheAdsOwnURL(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}

	got, err := validateSitelinks(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || len(got[0].FinalURLs) != 1 {
		t.Fatalf("got %+v, want one sitelink with one final URL", got)
	}
	url := got[0].FinalURLs[0]
	for _, want := range []string{"utm_source=google", "utm_medium=cpc", "events.example.org/register"} {
		if !strings.Contains(url, want) {
			t.Errorf("sitelink destination %q is missing %q", url, want)
		}
	}
	want := sitelinkAsset{LinkText: "Register now", Description1: "Early bird pricing", Description2: "Ends 1 December"}
	if got[0].SitelinkAsset == nil || *got[0].SitelinkAsset != want {
		t.Errorf("got %+v, want %+v", got[0].SitelinkAsset, want)
	}
	if got[0].CalloutAsset != nil || got[0].StructuredSnippetAsset != nil {
		t.Errorf("a sitelink asset must set exactly one arm of the oneof, got %+v", got[0])
	}
}

// Descriptions are optional, and an absent one is not an empty one: Google
// rejects `"description1": ""` as blank text, where omitting the key is its own
// documented default. A typed decode cannot tell the two apart, so this asserts
// key PRESENCE on the marshalled operation.
func TestValidateSitelinks_OmitsTheDescriptionKeysWhenUnset(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{{Text: "Register now", FinalURL: "https://events.example.org/register"}}

	got, err := validateSitelinks(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		SitelinkAsset map[string]any `json:"sitelinkAsset"`
	}
	if uErr := json.Unmarshal(raw, &decoded); uErr != nil {
		t.Fatalf("decode: %v", uErr)
	}
	if _, present := decoded.SitelinkAsset["linkText"]; !present {
		t.Errorf("linkText must always be sent, got %s", raw)
	}
	for _, key := range []string{"description1", "description2"} {
		if _, present := decoded.SitelinkAsset[key]; present {
			t.Errorf("an unset %s must be omitted, not sent as an empty string: %s", key, raw)
		}
	}
}

func TestValidateSitelinks_RejectsBadInput(t *testing.T) {
	bad := map[string][]Sitelink{
		"no link text":        {{Text: "   ", FinalURL: "https://e.example.org/r"}},
		"link text too long":  {{Text: strings.Repeat("x", maxSitelinkTextRunes+1), FinalURL: "https://e.example.org/r"}},
		"no destination":      {{Text: "Register"}},
		"non-http scheme":     {{Text: "Register", FinalURL: "ftp://e.example.org/r"}},
		"embedded credential": {{Text: "Register", FinalURL: "https://user:pw@e.example.org/r"}},
		"one description":     {{Text: "Register", Description1: "Only one", FinalURL: "https://e.example.org/r"}},
		"description too long": {{
			Text:         "Register",
			Description1: strings.Repeat("x", maxSitelinkDescriptionRunes+1),
			Description2: "ok",
			FinalURL:     "https://e.example.org/r",
		}},
		// Google refuses two sitelinks with the same link text on one campaign, and
		// a caller who wrote it twice meant it once.
		"duplicate link text": {
			{Text: "Register", FinalURL: "https://e.example.org/a"},
			{Text: "register", FinalURL: "https://e.example.org/b"},
		},
	}
	for name, sitelinks := range bad {
		in := sampleInput()
		in.Sitelinks = sitelinks
		if _, err := validateSitelinks(in); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// The error must not echo the raw URL: a sitelink destination can carry a token
// in its query, and this message is persisted in a result step.
func TestValidateSitelinks_ErrorDoesNotEchoTheRawURL(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{{Text: "Register", FinalURL: "ftp://e.example.org/r?token=s3cret"}}

	_, err := validateSitelinks(in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the URL query: %v", err)
	}
}

func TestValidateSitelinks_CapsTheList(t *testing.T) {
	in := sampleInput()
	for i := 0; i < maxSitelinks+1; i++ {
		in.Sitelinks = append(in.Sitelinks, Sitelink{Text: "Link " + strconv.Itoa(i), FinalURL: "https://e.example.org/r"})
	}
	if _, err := validateSitelinks(in); err == nil {
		t.Fatalf("expected %d sitelinks to exceed the cap of %d", len(in.Sitelinks), maxSitelinks)
	}
	in.Sitelinks = in.Sitelinks[:maxSitelinks]
	if _, err := validateSitelinks(in); err != nil {
		t.Fatalf("exactly %d sitelinks must be accepted, got %v", maxSitelinks, err)
	}
}

// ---------------------------------------------------------------------------
// Callouts
// ---------------------------------------------------------------------------

func TestValidateCallouts_TrimsAndBuildsTheAsset(t *testing.T) {
	got, err := validateCallouts([]string{"  Free to attend  ", "500+ sessions"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d callouts, want 2", len(got))
	}
	if got[0].CalloutAsset == nil || got[0].CalloutAsset.CalloutText != "Free to attend" {
		t.Errorf("got %+v, want the trimmed text", got[0].CalloutAsset)
	}
	// A callout has no destination — it is a claim appended to the ad, not a link.
	if got[0].FinalURLs != nil {
		t.Errorf("a callout asset must carry no final URL, got %v", got[0].FinalURLs)
	}
}

func TestValidateCallouts_RejectsBadInput(t *testing.T) {
	bad := map[string][]string{
		"empty":     {"  "},
		"too long":  {strings.Repeat("x", maxCalloutTextRunes+1)},
		"duplicate": {"Free to attend", "free to attend"},
	}
	for name, callouts := range bad {
		if _, err := validateCallouts(callouts); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	over := make([]string, 0, maxCallouts+1)
	for i := 0; i < maxCallouts+1; i++ {
		over = append(over, "Callout "+strconv.Itoa(i))
	}
	if _, err := validateCallouts(over); err == nil {
		t.Errorf("expected %d callouts to exceed the cap of %d", len(over), maxCallouts)
	}
	if _, err := validateCallouts(over[:maxCallouts]); err != nil {
		t.Errorf("exactly %d callouts must be accepted, got %v", maxCallouts, err)
	}
}

// ---------------------------------------------------------------------------
// Structured snippets
// ---------------------------------------------------------------------------

// The header is checked for SHAPE, not membership: Google's predefined header
// list is per-language, so refusing anything not in an English list would refuse
// every correct Spanish or Japanese header.
func TestValidateStructuredSnippets_AcceptsANonEnglishHeader(t *testing.T) {
	got, err := validateStructuredSnippets([]StructuredSnippet{
		{Header: "Marcas", Values: []string{"Kubernetes", "Prometheus", "Envoy"}},
	})
	if err != nil {
		t.Fatalf("a localized header must be accepted, got %v", err)
	}
	if got[0].StructuredSnippetAsset == nil || got[0].StructuredSnippetAsset.Header != "Marcas" {
		t.Fatalf("got %+v, want header Marcas", got[0].StructuredSnippetAsset)
	}
	if len(got[0].StructuredSnippetAsset.Values) != 3 {
		t.Errorf("got %v, want 3 values", got[0].StructuredSnippetAsset.Values)
	}
}

// Three values of which two are the same renders as two, which is below what
// Google accepts — so the dedupe has to happen BEFORE the minimum is counted,
// not after the campaign exists.
func TestValidateStructuredSnippets_DedupesBeforeCountingTheMinimum(t *testing.T) {
	_, err := validateStructuredSnippets([]StructuredSnippet{
		{Header: "Brands", Values: []string{"Kubernetes", "kubernetes", "Envoy"}},
	})
	if err == nil {
		t.Fatal("a snippet whose distinct values fall below the minimum must be rejected")
	}
}

func TestValidateStructuredSnippets_RejectsBadInput(t *testing.T) {
	bad := map[string][]StructuredSnippet{
		"no header": {{Header: " ", Values: []string{"a", "b", "c"}}},
		"header too long": {{
			Header: strings.Repeat("x", maxSnippetHeaderRunes+1),
			Values: []string{"a", "b", "c"},
		}},
		"too few values": {{Header: "Brands", Values: []string{"a", "b"}}},
		"value too long": {{
			Header: "Brands",
			Values: []string{strings.Repeat("x", maxSnippetValueRunes+1), "b", "c"},
		}},
		"duplicate header": {
			{Header: "Brands", Values: []string{"a", "b", "c"}},
			{Header: "brands", Values: []string{"d", "e", "f"}},
		},
	}
	for name, snippets := range bad {
		if _, err := validateStructuredSnippets(snippets); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}

	tooMany := StructuredSnippet{Header: "Brands"}
	for i := 0; i < maxSnippetValues+1; i++ {
		tooMany.Values = append(tooMany.Values, "Value "+strconv.Itoa(i))
	}
	if _, err := validateStructuredSnippets([]StructuredSnippet{tooMany}); err == nil {
		t.Errorf("expected %d values to exceed the cap of %d", len(tooMany.Values), maxSnippetValues)
	}
	tooMany.Values = tooMany.Values[:maxSnippetValues]
	if _, err := validateStructuredSnippets([]StructuredSnippet{tooMany}); err != nil {
		t.Errorf("exactly %d values must be accepted, got %v", maxSnippetValues, err)
	}
}

// ---------------------------------------------------------------------------
// The plan
// ---------------------------------------------------------------------------

// The field type cannot be read back off a created asset, so it is carried
// positionally. A plan whose two slices drift apart links every asset under the
// wrong extension slot.
func TestValidateAssetPlan_PairsEveryAssetWithItsFieldType(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend", "500+ sessions"}
	in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"a", "b", "c"}}}

	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.assets) != len(plan.fieldTypes) {
		t.Fatalf("assets (%d) and fieldTypes (%d) must stay the same length", len(plan.assets), len(plan.fieldTypes))
	}
	want := []string{assetFieldSitelink, assetFieldCallout, assetFieldCallout, assetFieldStructuredSnippet}
	for i := range want {
		if plan.fieldTypes[i] != want[i] {
			t.Errorf("fieldTypes[%d] = %q, want %q", i, plan.fieldTypes[i], want[i])
		}
	}
	if plan.sitelinks != 1 || plan.callouts != 2 || plan.snippets != 1 {
		t.Errorf("counts = %d/%d/%d, want 1/2/1", plan.sitelinks, plan.callouts, plan.snippets)
	}
	if step := assetStep(plan); step != "1 sitelinks, 2 callouts, 1 structured snippets" {
		t.Errorf("assetStep = %q", step)
	}
}

// A step sentence must name only what was asked for: a callout clause on a
// campaign with no callout assets is a lie about a paid resource.
func TestAssetStep_NamesOnlyWhatWasRequested(t *testing.T) {
	in := sampleInput()
	in.Callouts = []string{"Free to attend"}
	plan, err := validateAssetPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := assetStep(plan)
	if step != "1 callouts" {
		t.Errorf("assetStep = %q, want only the callout clause", step)
	}
}

func TestValidateAssetPlan_RefusesEveryExtensionOnDemandGen(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"sitelinks": func(in *CampaignInput) { in.Sitelinks = []Sitelink{sampleSitelink()} },
		"callouts":  func(in *CampaignInput) { in.Callouts = []string{"Free to attend"} },
		"structured snippets": func(in *CampaignInput) {
			in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"a", "b", "c"}}}
		},
		"call extensions": func(in *CampaignInput) { in.CallExtensions = []CallExtension{sampleCallExtension()} },
		"promotions":      func(in *CampaignInput) { in.Promotions = []PromotionExtension{samplePromotion()} },
		"prices":          func(in *CampaignInput) { in.Prices = []PriceExtension{samplePrice()} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			mutate(&in)
			if _, err := validateAssetPlan(campaignKindDemandGen, in); err == nil {
				t.Fatalf("%s must be refused on Demand Gen", name)
			}
			if _, err := validateAssetPlan(campaignKindSearch, in); err != nil {
				t.Fatalf("the same input must be accepted on Search, got %v", err)
			}
		})
	}
}

func TestValidateAssetPlan_EmptyInputIsAnEmptyPlan(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen} {
		plan, err := validateAssetPlan(kind, sampleInput())
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", kind, err)
		}
		if !plan.empty() || plan.count() != 0 {
			t.Errorf("%s: plan = %+v, want empty", kind, plan)
		}
	}
}

// ---------------------------------------------------------------------------
// Resource-name parsing
// ---------------------------------------------------------------------------

// A resource name is the only proof of what a record IS. Each rejection here is
// a 2xx that would otherwise be persisted as a confirmed extension.
func TestAssetID_RejectsAnythingButThisAccountsAsset(t *testing.T) {
	c := NewClient(testCreds(), testAccount())
	if got := c.assetID("customers/1234567890/assets/700"); got != "700" {
		t.Errorf("got %q, want 700", got)
	}
	for _, bad := range []string{
		"customers/9999999999/assets/700", // another account
		"customers/1234567890/campaigns/700",
		"customers/1234567890/assets/",
		"customers/1234567890/assets/abc",
		"customers/1234567890/assets/700/extra",
		"garbage/4242",
		"",
	} {
		if got := c.assetID(bad); got != "" {
			t.Errorf("assetID(%q) = %q, want \"\"", bad, got)
		}
	}
}

// CampaignAsset names are a THREE-part composite. The two-part parser used for
// criteria would reject every valid one, which is why this has its own.
func TestCampaignAssetID_ParsesTheThreePartComposite(t *testing.T) {
	c := NewClient(testCreds(), testAccount())
	campaignID, assetID, fieldType := c.campaignAssetID("customers/1234567890/campaignAssets/222~700~SITELINK")
	if campaignID != "222" || assetID != "700" || fieldType != "SITELINK" {
		t.Errorf("got (%q, %q, %q), want (222, 700, SITELINK)", campaignID, assetID, fieldType)
	}
	for _, bad := range []string{
		"customers/1234567890/campaignAssets/222~700",           // the two-part shape
		"customers/1234567890/campaignAssets/222~700~SITELINK~", // empty field type
		"customers/1234567890/campaignAssets/abc~700~SITELINK",
		"customers/1234567890/campaignAssets/222~abc~SITELINK",
		"customers/9999999999/campaignAssets/222~700~SITELINK",
		"customers/1234567890/campaignCriteria/222~700~SITELINK",
		"",
	} {
		if gotCampaign, gotAsset, gotField := c.campaignAssetID(bad); gotCampaign != "" || gotAsset != "" || gotField != "" {
			t.Errorf("campaignAssetID(%q) = (%q, %q, %q), want empty", bad, gotCampaign, gotAsset, gotField)
		}
	}
}

// ---------------------------------------------------------------------------
// The wire
// ---------------------------------------------------------------------------

// Two mutates, and the second must reference the resource names the FIRST
// returned. Rebuilding those names from the parsed ids would paper over exactly
// the account/kind mismatch assetID exists to catch.
func TestCreateCampaign_CreatesAssetsThenLinksThemByReturnedResourceName(t *testing.T) {
	assetsH, readAssets := capturedAssetMutate(assetName)
	// The fake echoes the field type the operation at that index actually asked for —
	// sitelinks, then callouts, then structured snippets, the order createExtensionAssets
	// plans them in. A fake that stamped SITELINK on all three would be describing links
	// the client never requested, which the link-result check now (correctly) refuses.
	linkFieldTypes := []string{assetFieldSitelink, assetFieldCallout, assetFieldStructuredSnippet}
	linksH, readLinks := capturedAssetMutate(func(i int) string {
		return "customers/1234567890/campaignAssets/222~" + strconv.Itoa(700+i) + "~" + linkFieldTypes[i]
	})
	c := newAssetClient(t, assetsH, linksH)

	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend"}
	in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"Kubernetes", "Prometheus", "Envoy"}}}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var assetReq struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	assetBody := readAssets()
	if uErr := json.Unmarshal([]byte(assetBody), &assetReq); uErr != nil {
		t.Fatalf("decode assets body: %v (body=%s)", uErr, assetBody)
	}
	if len(assetReq.Operations) != 3 {
		t.Fatalf("got %d asset operations, want 3 (body=%s)", len(assetReq.Operations), assetBody)
	}
	// Exactly one arm of the oneof per operation, in plan order.
	for i, arm := range []string{"sitelinkAsset", "calloutAsset", "structuredSnippetAsset"} {
		if _, present := assetReq.Operations[i].Create[arm]; !present {
			t.Errorf("asset operation %d must carry %s, got %v", i, arm, assetReq.Operations[i].Create)
		}
		for _, other := range []string{"sitelinkAsset", "calloutAsset", "structuredSnippetAsset"} {
			if other == arm {
				continue
			}
			if _, present := assetReq.Operations[i].Create[other]; present {
				t.Errorf("asset operation %d sets both %s and %s", i, arm, other)
			}
		}
	}
	// Only the sitelink carries a destination.
	if _, present := assetReq.Operations[0].Create["finalUrls"]; !present {
		t.Errorf("the sitelink asset must carry finalUrls, got %v", assetReq.Operations[0].Create)
	}
	if _, present := assetReq.Operations[1].Create["finalUrls"]; present {
		t.Errorf("a callout asset must carry no finalUrls, got %v", assetReq.Operations[1].Create)
	}

	var linkReq struct {
		Operations []struct {
			Create campaignAssetCreate `json:"create"`
		} `json:"operations"`
	}
	linkBody := readLinks()
	if uErr := json.Unmarshal([]byte(linkBody), &linkReq); uErr != nil {
		t.Fatalf("decode campaignAssets body: %v (body=%s)", uErr, linkBody)
	}
	if len(linkReq.Operations) != 3 {
		t.Fatalf("got %d link operations, want 3 (body=%s)", len(linkReq.Operations), linkBody)
	}
	wantFields := []string{assetFieldSitelink, assetFieldCallout, assetFieldStructuredSnippet}
	for i, op := range linkReq.Operations {
		if op.Create.Asset != assetName(i) {
			t.Errorf("link operation %d references %q, want the resource name the asset mutate returned (%q)", i, op.Create.Asset, assetName(i))
		}
		if op.Create.Campaign != "customers/1234567890/campaigns/222" {
			t.Errorf("link operation %d campaign = %q", i, op.Create.Campaign)
		}
		if op.Create.FieldType != wantFields[i] {
			t.Errorf("link operation %d fieldType = %q, want %q", i, op.Create.FieldType, wantFields[i])
		}
	}

	if len(res.ExtensionAssetIDs) != 3 || len(res.ExtensionLinkIDs) != 3 {
		t.Errorf("got %v / %v, want 3 asset ids and 3 link ids", res.ExtensionAssetIDs, res.ExtensionLinkIDs)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Ad extensions applied: 3 assets (1 sitelinks, 1 callouts, 1 structured snippets)") {
		t.Errorf("steps should report the extensions, got:\n%s", strings.Join(res.Steps, "\n"))
	}
}

// The assets exist account-wide the moment the first mutate succeeds. If the
// link mutate then fails, the ids must still come back — they are what makes the
// litter findable, and a retry creates a second set rather than adopting them.
func TestCreateCampaign_LinkFailureStillReportsTheCreatedAssets(t *testing.T) {
	c := newAssetClient(t, okAssets, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
	})

	in := sampleInput()
	in.Callouts = []string{"Free to attend"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil {
		t.Fatal("expected the campaign alongside the error, got nil")
	}
	if len(res.ExtensionAssetIDs) != 1 {
		t.Errorf("the created assets must be reported so they can be found, got %v", res.ExtensionAssetIDs)
	}
	if len(res.ExtensionLinkIDs) != 0 {
		t.Errorf("nothing was linked, got %v", res.ExtensionLinkIDs)
	}
	if !strings.Contains(err.Error(), "unlinked") {
		t.Errorf("the error must say the assets are unlinked, got %v", err)
	}
}

func TestCreateCampaign_AssetMutateFailureKeepsCampaignPartial(t *testing.T) {
	c := newAssetClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		},
		failHandler(t, "campaignAssets:mutate"))

	in := sampleInput()
	in.Callouts = []string{"Free to attend"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil || res.CampaignID == "" {
		t.Fatalf("the campaign exists and must be returned alongside the error, got %+v", res)
	}
	if len(res.ExtensionAssetIDs) != 0 || len(res.ExtensionLinkIDs) != 0 {
		t.Errorf("no ids may be recorded from a failed mutate, got %v / %v", res.ExtensionAssetIDs, res.ExtensionLinkIDs)
	}
}

// A 2xx with fewer results than operations is UNCONFIRMED: assets may exist with
// ids this run could not read, so nothing may be persisted from it.
func TestCreateCampaign_ShortAssetResponseIsUnconfirmed(t *testing.T) {
	c := newAssetClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+assetName(0)+`"}]}`)
		},
		failHandler(t, "campaignAssets:mutate"))

	in := sampleInput()
	in.Callouts = []string{"Free to attend", "500+ sessions"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for a short mutate response")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should say UNCONFIRMED, got %v", err)
	}
	if res == nil || len(res.ExtensionAssetIDs) != 0 {
		t.Errorf("no ids may be persisted from an unconfirmed mutate, got %+v", res)
	}
}

// A campaignAsset naming a DIFFERENT campaign is not this campaign's extension,
// however healthy the 2xx looks.
func TestCreateCampaign_CampaignAssetForAnotherCampaignIsUnconfirmed(t *testing.T) {
	c := newAssetClient(t, okAssets, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignAssets/999~700~CALLOUT"}]}`)
	})

	in := sampleInput()
	in.Callouts = []string{"Free to attend"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "different campaign id") {
		t.Errorf("error should name the campaign mismatch, got %v", err)
	}
	if res == nil || len(res.ExtensionLinkIDs) != 0 {
		t.Errorf("no link ids may be persisted, got %+v", res)
	}
}

// Every extension input is validated inside preflightCampaignKind, BEFORE the
// budget mutate. A server that errors on ANY request is the only way to prove a
// bad local input cannot orphan a paid resource.
func TestCreateCampaign_BadExtensionsFailBeforeAnyMutate(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"sitelink without a destination": func(in *CampaignInput) {
			in.Sitelinks = []Sitelink{{Text: "Register"}}
		},
		"sitelink text too long": func(in *CampaignInput) {
			in.Sitelinks = []Sitelink{{Text: strings.Repeat("x", maxSitelinkTextRunes+1), FinalURL: "https://e.example.org/r"}}
		},
		"half-described sitelink": func(in *CampaignInput) {
			in.Sitelinks = []Sitelink{{Text: "Register", Description1: "one", FinalURL: "https://e.example.org/r"}}
		},
		"empty callout":    func(in *CampaignInput) { in.Callouts = []string{" "} },
		"callout too long": func(in *CampaignInput) { in.Callouts = []string{strings.Repeat("x", maxCalloutTextRunes+1)} },
		"snippet with too few values": func(in *CampaignInput) {
			in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"a", "b"}}}
		},
		"snippet without a header": func(in *CampaignInput) {
			in.StructuredSnippets = []StructuredSnippet{{Values: []string{"a", "b", "c"}}}
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
			t.Cleanup(tokenSrv.Close)
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no upstream call may be made for an invalid ad extension, got %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(apiSrv.Close)
			c := NewClient(testCreds(), testAccount(),
				WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
				withRetryBaseDelay(time.Millisecond))

			in := sampleInput()
			mutate(&in)

			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected an error")
			}
			if res != nil {
				t.Errorf("expected a nil result (nothing was created), got %+v", res)
			}
			// The adoption path must refuse the identical input, or the same brief is
			// accepted or rejected depending on whether a same-name campaign exists.
			if vErr := c.ValidateCampaignInput(in); vErr == nil {
				t.Error("ValidateCampaignInput must refuse the same input")
			}
		})
	}
}

// A campaignAsset resource name that reports a field type the operation at that index did
// not ask for is not proof the link exists. Mutate results come back in operation order —
// the asset mutate already depends on that to pair ids with plan.fieldTypes — so a 2xx
// describing `222~700~SITELINK` for the operation that linked asset 700 as a CALLOUT means
// the response and the request disagree about what was created. Checking only the campaign
// id accepted exactly that, which is the gap this closes.
func TestCreateCampaign_RefusesALinkResultReportingADifferentFieldType(t *testing.T) {
	assetsH, _ := capturedAssetMutate(assetName)
	linksH, _ := capturedAssetMutate(func(i int) string {
		// Every link claims SITELINK, including the callout at index 1.
		return "customers/1234567890/campaignAssets/222~" + strconv.Itoa(700+i) + "~" + assetFieldSitelink
	})
	c := newAssetClient(t, assetsH, linksH)

	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected a link result reporting the wrong field type to be refused")
	}
	// Past the campaign create, the partial-result contract holds: the error arrives
	// ALONGSIDE a result, never instead of it, so the claim is not released for work that
	// may well exist upstream.
	if res == nil {
		t.Fatal("the campaign was already created — the error must carry a non-nil result")
	}
	// The create cascade reports an unconfirmed outcome by RETURNING A RESULT with the
	// error, which is what keeps Dispatch from releasing the claim; the wording is for the
	// operator. (IsOutcomeUnconfirmed is the toggle path's signal, carried by an error type
	// — a different mechanism, deliberately.)
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("the error must tell the operator the link may exist: %v", err)
	}
	if !strings.Contains(err.Error(), "field type") {
		t.Errorf("the error must say what disagreed, got: %v", err)
	}
}

// The field type is Google's own enum name echoed back, so a casing difference is a change
// in how the API spells a value, not the wrong link. Failing a real, correct create over
// spelling is the over-refusal this guard must not commit — and under-refusal is always the
// safe side here, because a genuinely different field type still fails the comparison.
func TestCreateCampaign_AcceptsALinkResultWhoseFieldTypeDiffersOnlyInCase(t *testing.T) {
	assetsH, _ := capturedAssetMutate(assetName)
	linksH, _ := capturedAssetMutate(func(i int) string {
		return "customers/1234567890/campaignAssets/222~" + strconv.Itoa(700+i) + "~" + strings.ToLower(assetFieldSitelink)
	})
	c := newAssetClient(t, assetsH, linksH)

	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}

	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("a field type differing only in case must be accepted: %v", err)
	}
}

// The asset component is checked the same way and for the same reason: a result naming an
// asset the operation at that index did not link describes a link the client never asked
// for.
func TestCreateCampaign_RefusesALinkResultReportingADifferentAsset(t *testing.T) {
	assetsH, _ := capturedAssetMutate(assetName)
	linksH, _ := capturedAssetMutate(func(_ int) string {
		return "customers/1234567890/campaignAssets/222~999~" + assetFieldSitelink
	})
	c := newAssetClient(t, assetsH, linksH)

	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected a link result naming another asset to be refused")
	}
	if res == nil {
		t.Fatal("the campaign was already created — the error must carry a non-nil result")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("the error must tell the operator the link may exist: %v", err)
	}
}
