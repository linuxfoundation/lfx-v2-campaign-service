// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Performance Max asset group signals and campaign-level extensions (#461)
// ---------------------------------------------------------------------------

// pmaxSignalInput is a Performance Max input whose asset group will be created, so the
// signal tests vary only the signals.
func pmaxSignalInput() CampaignInput {
	return pmaxInput(fullPMaxCreative())
}

func TestValidatePerformanceMaxSignals_AcceptsAndNormalizes(t *testing.T) {
	in := pmaxSignalInput()
	in.PerformanceMaxSearchThemes = []string{"  kubecon  ", "kubecon agenda", strings.Repeat("k", maxSearchThemeRunes)}
	in.PerformanceMaxAudienceSignal = " 4242 "
	plan, err := validatePerformanceMaxSignals(campaignKindPerformanceMax, "1234567890", in, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(plan.searchThemes, "|"); got != "kubecon|kubecon agenda|"+strings.Repeat("k", maxSearchThemeRunes) {
		t.Errorf("searchThemes = %q, want trimmed themes in input order", got)
	}
	if plan.audience != "customers/1234567890/audiences/4242" {
		t.Errorf("audience = %q, want the bare id resolved in this account", plan.audience)
	}

	in.PerformanceMaxAudienceSignal = "customers/1234567890/audiences/77"
	plan, err = validatePerformanceMaxSignals(campaignKindPerformanceMax, "1234567890", in, true)
	if err != nil || plan.audience != "customers/1234567890/audiences/77" {
		t.Errorf("audience = %q, err = %v, want the resource name kept", plan.audience, err)
	}
}

func TestValidatePerformanceMaxSignals_AbsentIsAnEmptyPlan(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindPerformanceMax} {
		plan, err := validatePerformanceMaxSignals(kind, "1234567890", sampleInput(), false)
		if err != nil || !plan.empty() {
			t.Errorf("%s: plan = %+v, err = %v, want an empty plan", kind, plan, err)
		}
	}
}

// notAnAudience is the refusal for an audience signal that is neither spelling.
const notAnAudience = "neither a positive canonical audience id nor a " +
	"customers/<id>/audiences/<id> resource name"

func TestValidatePerformanceMaxSignals_RefusesBadInput(t *testing.T) {
	const (
		thisAccount   = "customers/1234567890/audiences/"
		blankAudience = "PerformanceMaxAudienceSignal must not be blank"
	)
	// maxSearchThemes+1 = 51: the probe's own refusal point.
	tooMany := make([]string, maxSearchThemes+1)
	for i := range tooMany {
		tooMany[i] = "theme " + strconv.Itoa(i)
	}
	for name, tc := range map[string]struct {
		themes   []string
		audience string
		want     string
	}{
		"empty theme":       {themes: []string{"kubecon", "  "}, want: "search theme 1 is empty"},
		"theme over 80":     {themes: []string{strings.Repeat("k", maxSearchThemeRunes+1)}, want: "81 characters, exceeding the 80 limit"},
		"duplicate theme":   {themes: []string{"KubeCon", "kubecon "}, want: `"kubecon" is listed more than once`},
		"too many themes":   {themes: tooMany, want: "at most 50 search themes, got 51"},
		"foreign audience":  {audience: "customers/9999999999/audiences/5", want: "belongs to customer \"9999999999\", not the campaign's account \"1234567890\""},
		"user list":         {audience: "customers/1234567890/userLists/5", want: notAnAudience},
		"non-numeric id":    {audience: "customers/1234567890/audiences/abc", want: notAnAudience},
		"two audiences":     {audience: "5,6", want: notAnAudience},
		"trailing segment":  {audience: "customers/1234567890/audiences/5/x", want: notAnAudience},
		"theme and foreign": {themes: []string{"kubecon"}, audience: "customers/1/audiences/5", want: "belongs to customer \"1\""},
		// #459's canonical-id rule, as validateDemandGenAudienceField applies it.
		"zero id":               {audience: "0", want: notAnAudience},
		"leading-zero id":       {audience: "007", want: notAnAudience},
		"id past int64":         {audience: "99999999999999999999", want: notAnAudience},
		"zero resource id":      {audience: thisAccount + "0", want: notAnAudience},
		"leading-zero resource": {audience: thisAccount + "012", want: notAnAudience},
		// Set but blank is refused, not read as absent, as a blank DemandGenAudience is.
		"blank audience":         {audience: "   ", want: blankAudience},
		"blank audience, themes": {themes: []string{"kubecon"}, audience: " ", want: blankAudience},
	} {
		t.Run(name, func(t *testing.T) {
			in := pmaxSignalInput()
			in.PerformanceMaxSearchThemes = tc.themes
			in.PerformanceMaxAudienceSignal = tc.audience
			_, err := validatePerformanceMaxSignals(campaignKindPerformanceMax, "1234567890", in, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Signals hang off an asset group, so a Performance Max request that creates none has
// nowhere to put them: refused before any request, through the same entry point the
// adoption path uses.
func TestValidateCampaignInputKind_RefusesSignalsWithoutAnAssetGroup(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	for name, set := range map[string]func(*CampaignInput){
		"themes":   func(in *CampaignInput) { in.PerformanceMaxSearchThemes = []string{"kubecon"} },
		"audience": func(in *CampaignInput) { in.PerformanceMaxAudienceSignal = "4242" },
	} {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			set(&in)
			err := c.ValidateCampaignInputKind(campaignKindPerformanceMax, in)
			if err == nil || !strings.Contains(err.Error(), "this request creates none (no PerformanceMaxCreative)") {
				t.Errorf("err = %v, want the no-asset-group refusal", err)
			}
			in.PerformanceMaxCreative = fullPMaxCreative()
			if err := c.ValidateCampaignInputKind(campaignKindPerformanceMax, in); err != nil {
				t.Errorf("with an asset group the same signals must validate: %v", err)
			}
		})
	}
}

// Every other channel refuses the signals rather than dropping them.
func TestValidateCampaignInputKind_RefusesSignalsOffPerformanceMax(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	for _, tc := range []struct {
		kind string
		in   CampaignInput
	}{
		{campaignKindSearch, sampleInput()},
		{campaignKindDemandGen, creativeInput(fullCreative())},
		{campaignKindDisplay, displayInput("https://cdn.example.com")},
		{campaignKindVideo, videoInput()},
	} {
		for name, set := range map[string]func(*CampaignInput){
			"themes":   func(in *CampaignInput) { in.PerformanceMaxSearchThemes = []string{"kubecon"} },
			"audience": func(in *CampaignInput) { in.PerformanceMaxAudienceSignal = "4242" },
		} {
			t.Run(tc.kind+"/"+name, func(t *testing.T) {
				in := tc.in
				if err := c.ValidateCampaignInputKind(tc.kind, in); err != nil {
					t.Fatalf("input must validate before the signal is set: %v", err)
				}
				set(&in)
				err := c.ValidateCampaignInputKind(tc.kind, in)
				if err == nil || !strings.Contains(err.Error(), "search themes and audience signals are not supported on "+tc.kind) {
					t.Errorf("err = %v, want the signals refused on %s", err, tc.kind)
				}
			})
		}
	}
}

// Sitelinks, callouts and structured snippets are accepted on Performance Max and still
// refused on the channels that do not take all three; the other four extension types stay
// Search only.
func TestValidateAssetPlan_PerformanceMaxTakesTheThreeVerifiedTypes(t *testing.T) {
	in := sampleInput()
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend"}
	in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"a", "b", "c"}}}
	plan, err := validateAssetPlan(campaignKindPerformanceMax, in)
	if err != nil {
		t.Fatalf("Performance Max refused the three verified extension types: %v", err)
	}
	if got := strings.Join(plan.fieldTypes, ","); got != "SITELINK,CALLOUT,STRUCTURED_SNIPPET" {
		t.Errorf("field types = %s, want the three in plan order", got)
	}
	// Demand Gen takes sitelinks alone (#474), so the callout and snippet refuse it with
	// its own sentence; Video and Display refuse the whole set.
	for kind, want := range map[string]string{
		campaignKindDemandGen: "other than sitelinks (callouts, structured snippets, call " +
			"extensions, promotions, prices, lead forms) are not supported on " +
			campaignKindDemandGen,
		campaignKindVideo:   "not on " + campaignKindVideo,
		campaignKindDisplay: "not on " + campaignKindDisplay,
	} {
		_, err := validateAssetPlan(kind, in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want the extensions refused", kind, err)
		}
	}

	for name, set := range map[string]func(*CampaignInput){
		"call extensions": func(in *CampaignInput) { in.CallExtensions = []CallExtension{sampleCallExtension()} },
		"promotions":      func(in *CampaignInput) { in.Promotions = []PromotionExtension{samplePromotion()} },
		"prices":          func(in *CampaignInput) { in.Prices = []PriceExtension{samplePrice()} },
		"lead forms":      func(in *CampaignInput) { in.LeadForms = []LeadFormExtension{sampleLeadForm()} },
	} {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			set(&in)
			_, err := validateAssetPlan(campaignKindPerformanceMax, in)
			if err == nil || !strings.Contains(err.Error(), "supported on Search campaigns only, not on "+campaignKindPerformanceMax) {
				t.Errorf("err = %v, want %s refused on Performance Max", name, err)
			}
			if _, err := validateAssetPlan(campaignKindSearch, in); err != nil {
				t.Errorf("Search must still accept %s: %v", name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// recorded is a request body captured by a fake handler, behind a lock because the
// handler runs on the server's goroutine and the test reads afterwards.
type recorded struct {
	mu    sync.Mutex
	paths []string
	body  map[string]string
}

func (r *recorded) put(key, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.body == nil {
		r.body = map[string]string{}
	}
	r.paths = append(r.paths, key)
	r.body[key] = body
}

func (r *recorded) get(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body[key]
}

func (r *recorded) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// okSignals answers a signals googleAds:mutate the way Google does: one
// assetGroupSignalResult per operation, on asset group 555, criterion ids from 7000.
func okSignals(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		MutateOperations []json.RawMessage `json:"mutateOperations"`
	}
	_ = json.Unmarshal(raw, &req)
	parts := make([]string, 0, len(req.MutateOperations))
	for i := range req.MutateOperations {
		parts = append(parts, `{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/555~`+strconv.Itoa(7000+i)+`"}}`)
	}
	_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+strings.Join(parts, ",")+`]}`)
}

// okExtensionLinks answers campaignAssets:mutate naming, at each index, the campaign,
// the asset (800+i) and the field type that operation asked for.
func okExtensionLinks(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Operations []struct {
			Create campaignAssetCreate `json:"create"`
		} `json:"operations"`
	}
	_ = json.Unmarshal(raw, &req)
	parts := make([]string, 0, len(req.Operations))
	for _, op := range req.Operations {
		parts = append(parts, `{"resourceName":"customers/1234567890/campaignAssets/222~`+resourceID(op.Create.Asset)+`~`+op.Create.FieldType+`"}`)
	}
	_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
}

// pmaxSignalServer is the Performance Max cascade with three more routes, told apart by
// path and body: the extension assets:mutate (a body carrying an extension arm), the
// campaignAssets:mutate links, and the signals googleAds:mutate (a body carrying
// assetGroupSignalOperation). Everything else is pmaxCascade's, with the happy asset group.
// Every body is recorded under a key naming the route.
func pmaxSignalServer(t *testing.T, rec *recorded, signalH, extAssetH, extLinkH http.HandlerFunc) (*Client, string) {
	t.Helper()
	assetH, _ := okPMaxAssets()
	groupH, _ := okPMaxGroup()
	var cascade http.HandlerFunc
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		body := string(raw)
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate") && strings.Contains(body, "assetGroupSignalOperation"):
			rec.put("signals", body)
			signalH(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			rec.put("group", body)
			groupH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignAssets:mutate"):
			rec.put("extensionLinks", body)
			extLinkH(w, r)
		case strings.HasSuffix(r.URL.Path, "/assets:mutate") && (strings.Contains(body, "sitelinkAsset") || strings.Contains(body, "calloutAsset") || strings.Contains(body, "structuredSnippetAsset")):
			rec.put("extensionAssets", body)
			extAssetH(w, r)
		case strings.HasSuffix(r.URL.Path, "/assets:mutate"):
			rec.put("groupAssets", body)
			assetH(w, r)
		default:
			cascade(w, r)
		}
	})
	// Built on the test goroutine, as TestCreatePerformanceMaxCampaign_CampaignShape
	// explains: pmaxImages can t.Fatalf.
	cascade = pmaxCascade(t, pmaxImages(t), assetH, groupH)
	return demandGenClient(t, srv), srv.URL
}

func failSignals(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the signals mutate must not be sent")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestCreatePerformanceMaxCampaign_CreatesSignalsOnTheConfirmedGroup(t *testing.T) {
	rec := &recorded{}
	c, base := pmaxSignalServer(t, rec, okSignals, failHandler(t, "extension assets"), failHandler(t, "extension links"))
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(base)
	in.PerformanceMaxSearchThemes = []string{"kubecon", " kubecon agenda "}
	in.PerformanceMaxAudienceSignal = "4242"

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The exact request: one googleAds:mutate, themes in input order then the audience,
	// each naming the asset group by the resource name Google RETURNED.
	const want = `{"mutateOperations":[` +
		`{"assetGroupSignalOperation":{"create":{"assetGroup":"customers/1234567890/assetGroups/555","searchTheme":{"text":"kubecon"}}}},` +
		`{"assetGroupSignalOperation":{"create":{"assetGroup":"customers/1234567890/assetGroups/555","searchTheme":{"text":"kubecon agenda"}}}},` +
		`{"assetGroupSignalOperation":{"create":{"assetGroup":"customers/1234567890/assetGroups/555","audience":{"audience":"customers/1234567890/audiences/4242"}}}}]}`
	if got := rec.get("signals"); got != want {
		t.Errorf("signals body =\n%s\nwant\n%s", got, want)
	}
	// After the asset group, never before it.
	order := rec.order()
	if len(order) < 2 || order[len(order)-1] != "signals" || order[len(order)-2] != "group" {
		t.Errorf("request order = %v, want the signals right after the asset group", order)
	}
	if got := strings.Join(res.AssetGroupSignalIDs, ","); got != "7000,7001,7002" {
		t.Errorf("AssetGroupSignalIDs = %s, want 7000,7001,7002", got)
	}
	wantStep := "Asset group signals applied: 3 signal(s) on asset group 555 (2 search theme(s), 1 audience signal)"
	if !hasExactStep(res.Steps, wantStep) {
		t.Errorf("Steps = %v, want %q", res.Steps, wantStep)
	}
}

// Themes alone send no audience operation and the step names no audience.
func TestCreatePerformanceMaxCampaign_ThemesAloneSendNoAudience(t *testing.T) {
	rec := &recorded{}
	c, base := pmaxSignalServer(t, rec, okSignals, failHandler(t, "extension assets"), failHandler(t, "extension links"))
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(base)
	in.PerformanceMaxSearchThemes = []string{"kubecon"}

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body := rec.get("signals"); strings.Contains(body, "audience") || strings.Count(body, "assetGroupSignalOperation") != 1 {
		t.Errorf("signals body = %s, want one theme and no audience", body)
	}
	wantSignals := "Asset group signals applied: 1 signal(s) on asset group 555 (1 search theme(s))"
	if !hasExactStep(res.Steps, wantSignals) {
		t.Errorf("Steps = %v, want the themes-only signal step", res.Steps)
	}
}

// A campaign with no signals sends no signals request at all.
func TestCreatePerformanceMaxCampaign_NoSignalsSendsNoSignalRequest(t *testing.T) {
	rec := &recorded{}
	c, base := pmaxSignalServer(t, rec, failSignals(t), failHandler(t, "extension assets"), failHandler(t, "extension links"))
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(base)
	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.AssetGroupSignalIDs != nil {
		t.Errorf("AssetGroupSignalIDs = %v, want none", res.AssetGroupSignalIDs)
	}
}

// The partial-result contract on each signal outcome: the campaign, the group and its
// links exist whatever happens to the signals, so the result always carries them.
func TestCreatePerformanceMaxCampaign_SignalFailuresArePartialResults(t *testing.T) {
	for name, tc := range map[string]struct {
		handler     http.HandlerFunc
		unconfirmed bool
		want        string
	}{
		"definite refusal": {
			handler: gaqlError(http.StatusBadRequest, "assetGroupSignalError", "SEARCH_THEME_POLICY_VIOLATION"),
			want:    "so none of them exists",
		},
		"ambiguous 5xx": {
			handler:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			unconfirmed: true,
			want:        "its 2 signal(s) may exist",
		},
		"short response": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/555~7000"}}]}`)
			},
			unconfirmed: true,
			want:        "mis-sized response for 2 signal(s)",
		},
		"another group": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+
					`{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/555~7000"}},`+
					`{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/556~7001"}}]}`)
			},
			unconfirmed: true,
			want:        "result 1 names",
		},
		"another account": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+
					`{"assetGroupSignalResult":{"resourceName":"customers/9999999999/assetGroupSignals/555~7000"}},`+
					`{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/555~7001"}}]}`)
			},
			unconfirmed: true,
			want:        "result 0 names",
		},
		"wrong result arm": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"mutateOperationResponses":[`+
					`{"assetGroupResult":{"resourceName":"customers/1234567890/assetGroups/555"}},`+
					`{"assetGroupSignalResult":{"resourceName":"customers/1234567890/assetGroupSignals/555~7001"}}]}`)
			},
			unconfirmed: true,
			want:        "result 0 names",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorded{}
			c, base := pmaxSignalServer(t, rec, tc.handler, failHandler(t, "extension assets"), failHandler(t, "extension links"))
			in := demandGenInput()
			in.PerformanceMaxCreative = pmaxCreativeAt(base)
			in.PerformanceMaxSearchThemes = []string{"kubecon"}
			in.PerformanceMaxAudienceSignal = "4242"

			res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("a signal failure must be reported")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.unconfirmed {
				t.Errorf("UNCONFIRMED in %q = %v, want %v", err, got, tc.unconfirmed)
			}
			if res == nil {
				t.Fatal("past the campaign create the error must come ALONGSIDE a result, never (nil, err)")
			}
			if res.CampaignID != "222" || res.AssetGroupID != "555" {
				t.Errorf("CampaignID = %q, AssetGroupID = %q — both exist and must be carried", res.CampaignID, res.AssetGroupID)
			}
			if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != pmaxAssetCount {
				t.Errorf("AssetGroupAssetLinks = %v, want %d — the links were confirmed before the signals ran", res.AssetGroupAssetLinks, pmaxAssetCount)
			}
			if len(res.AssetGroupSignalIDs) != 0 {
				t.Errorf("AssetGroupSignalIDs = %v, want none recorded from an unconfirmed or failed request", res.AssetGroupSignalIDs)
			}
			for _, s := range res.Steps {
				if strings.HasPrefix(s, "Asset group signals applied") {
					t.Errorf("Steps claim the signals were applied: %v", res.Steps)
				}
			}
		})
	}
}

// A group that could not be confirmed gets no signals: the request would name a group
// this client cannot vouch for.
func TestCreatePerformanceMaxCampaign_UnconfirmedGroupSendsNoSignals(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	cascade := pmaxCascade(t, pmaxImages(t), assetH, groupH)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "assetGroupSignalOperation") {
			t.Error("signals were sent for an unconfirmed asset group")
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		cascade(w, r)
	})
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)
	in.PerformanceMaxSearchThemes = []string{"kubecon"}

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "asset group") {
		t.Fatalf("err = %v, want the asset group failure", err)
	}
	if res == nil || res.AssetGroupSignalIDs != nil {
		t.Errorf("result = %+v, want a partial with no signal ids", res)
	}
}

// Campaign-level extensions on Performance Max: the same two mutates as Search, linked to
// this campaign under the three field types, BEFORE the asset group.
func TestCreatePerformanceMaxCampaign_LinksExtensionsAtTheCampaignLevel(t *testing.T) {
	rec := &recorded{}
	extAssetH, _ := capturedAssetMutate(func(i int) string { return "customers/1234567890/assets/" + strconv.Itoa(800+i) })
	c, base := pmaxSignalServer(t, rec, failSignals(t), extAssetH, okExtensionLinks)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(base)
	in.Sitelinks = []Sitelink{sampleSitelink()}
	in.Callouts = []string{"Free to attend"}
	in.StructuredSnippets = []StructuredSnippet{{Header: "Brands", Values: []string{"Kubernetes", "Prometheus", "Envoy"}}}

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const wantLinks = `{"operations":[` +
		`{"create":{"campaign":"customers/1234567890/campaigns/222","asset":"customers/1234567890/assets/800","fieldType":"SITELINK"}},` +
		`{"create":{"campaign":"customers/1234567890/campaigns/222","asset":"customers/1234567890/assets/801","fieldType":"CALLOUT"}},` +
		`{"create":{"campaign":"customers/1234567890/campaigns/222","asset":"customers/1234567890/assets/802","fieldType":"STRUCTURED_SNIPPET"}}]}`
	if got := rec.get("extensionLinks"); got != wantLinks {
		t.Errorf("campaignAssets body =\n%s\nwant\n%s", got, wantLinks)
	}
	assets := rec.get("extensionAssets")
	for _, arm := range []string{`"sitelinkAsset"`, `"calloutAsset"`, `"structuredSnippetAsset"`} {
		if strings.Count(assets, arm) != 1 {
			t.Errorf("extension assets body = %s, want exactly one %s", assets, arm)
		}
	}
	order := strings.Join(rec.order(), ",")
	if order != "extensionAssets,extensionLinks,groupAssets,group" {
		t.Errorf("request order = %s, want the extensions before the asset group", order)
	}
	if got := strings.Join(res.ExtensionLinkIDs, ","); got != "800,801,802" {
		t.Errorf("ExtensionLinkIDs = %s, want 800,801,802", got)
	}
	wantExtensions := "Ad extensions applied: 3 assets " +
		"(1 sitelinks, 1 callouts, 1 structured snippets)"
	if !hasExactStep(res.Steps, wantExtensions) {
		t.Errorf("Steps = %v, want the Search extension step", res.Steps)
	}
	if res.AssetGroupID != "555" {
		t.Errorf("AssetGroupID = %q, want the group created after the extensions", res.AssetGroupID)
	}
}

// An extension failure on Performance Max is a partial result that strands no asset
// group: the cascade stops before the group is attempted.
func TestCreatePerformanceMaxCampaign_ExtensionFailureStopsBeforeTheGroup(t *testing.T) {
	for name, tc := range map[string]struct {
		linkH       http.HandlerFunc
		unconfirmed bool
	}{
		// Only the HTTP status decides the outcome; the error code is a placeholder, not a
		// verified Google refusal.
		"definite": {linkH: gaqlError(http.StatusBadRequest, "campaignAssetError", "FIELD_TYPE_INCOMPATIBLE_WITH_CAMPAIGN_TYPE")},
		"ambiguous": {
			linkH:       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			unconfirmed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorded{}
			extAssetH, _ := capturedAssetMutate(func(i int) string { return "customers/1234567890/assets/" + strconv.Itoa(800+i) })
			c, base := pmaxSignalServer(t, rec, failSignals(t), extAssetH, tc.linkH)
			in := demandGenInput()
			in.PerformanceMaxCreative = pmaxCreativeAt(base)
			in.Callouts = []string{"Free to attend"}
			in.PerformanceMaxSearchThemes = []string{"kubecon"}

			res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("a link failure must be reported")
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.unconfirmed {
				t.Errorf("UNCONFIRMED in %q = %v, want %v", err, got, tc.unconfirmed)
			}
			// The wrap that adds "not attempted" must keep the chain: the ambiguous outcome
			// still classifies as unconfirmed through errors.As, not only by its text.
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed(%q) = %v, want %v", err, got, tc.unconfirmed)
			}
			if !strings.Contains(err.Error(), "asset group was NOT attempted") {
				t.Errorf("err = %v, want it to say the asset group was not attempted", err)
			}
			if res == nil || res.CampaignID != "222" {
				t.Fatalf("result = %+v, want a partial carrying the campaign", res)
			}
			if got := strings.Join(res.ExtensionAssetIDs, ","); got != "800" {
				t.Errorf("ExtensionAssetIDs = %s, want the created asset reported", got)
			}
			if res.AssetGroupID != "" {
				t.Errorf("AssetGroupID = %q, want none — the group must not be attempted", res.AssetGroupID)
			}
			for _, p := range rec.order() {
				if p == "groupAssets" || p == "group" {
					t.Errorf("request order = %v, want nothing past the failed link", rec.order())
				}
			}
		})
	}
}

// A context already done when the signals step is reached sends nothing and says so,
// rather than reaching the request layer and reading as an ambiguous write.
func TestCreatePerformanceMaxSignals_DoneContextSendsNothing(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be sent on a done context")
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := performanceMaxSignalPlan{searchThemes: []string{"kubecon"}}
	ids, err := c.createPerformanceMaxSignals(ctx, "customers/1234567890/assetGroups/555", "555", plan)
	if err == nil || !strings.Contains(err.Error(), "aborted before their request") {
		t.Errorf("err = %v, want the aborted-before-request error", err)
	}
	if ids != nil {
		t.Errorf("ids = %v, want none", ids)
	}
}

// The cap is the probed one: 50 themes validate.
func TestValidatePerformanceMaxSignals_AcceptsFiftyThemes(t *testing.T) {
	in := pmaxSignalInput()
	in.PerformanceMaxSearchThemes = make([]string, 50)
	for i := range in.PerformanceMaxSearchThemes {
		in.PerformanceMaxSearchThemes[i] = "theme " + strconv.Itoa(i)
	}
	plan, err := validatePerformanceMaxSignals(campaignKindPerformanceMax, "1234567890", in, true)
	if err != nil || len(plan.searchThemes) != 50 {
		t.Errorf("themes = %d, err = %v, want all 50 accepted", len(plan.searchThemes), err)
	}
}

// Google answered 2xx WITH the group's resource name but not every link could be
// confirmed: the group id is kept and zero links recorded, and no signal is sent to a
// group this client cannot vouch for.
func TestCreatePerformanceMaxCampaign_UnconfirmedLinksSendNoSignals(t *testing.T) {
	assetH, _ := okPMaxAssets()
	groupH, _ := groupMutate(0) // the group result, no link results
	cascade := pmaxCascade(t, pmaxImages(t), assetH, groupH)
	var (
		mu         sync.Mutex
		signalSent bool
	)
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "assetGroupSignalOperation") {
			mu.Lock()
			signalSent = true
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		cascade(w, r)
	})
	c := demandGenClient(t, srv)
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(srv.URL)
	in.PerformanceMaxSearchThemes = []string{"kubecon"}
	in.PerformanceMaxAudienceSignal = "4242"

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "link creation UNCONFIRMED") {
		t.Fatalf("err = %v, want the unconfirmed-links error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if signalSent {
		t.Error("an assetGroupSignalOperation was sent for a group whose links are unconfirmed")
	}
	if res == nil || res.AssetGroupID != "555" {
		t.Fatalf("result = %+v, want the group id kept", res)
	}
	if res.AssetGroupAssetLinks == nil || *res.AssetGroupAssetLinks != 0 {
		t.Errorf("AssetGroupAssetLinks = %v, want a recorded 0", res.AssetGroupAssetLinks)
	}
	if res.AssetGroupSignalIDs != nil {
		t.Errorf("AssetGroupSignalIDs = %v, want nil", res.AssetGroupSignalIDs)
	}
}

// The defensive guard: with no group resource name or id, nothing is sent.
func TestCreatePerformanceMaxSignals_NoGroupSendsNothing(t *testing.T) {
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be sent without a confirmed group")
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := demandGenClient(t, srv)
	plan := performanceMaxSignalPlan{searchThemes: []string{"kubecon"}}
	for name, tc := range map[string][2]string{
		"no resource": {"", "555"},
		"no id":       {"customers/1234567890/assetGroups/555", ""},
	} {
		ids, err := c.createPerformanceMaxSignals(context.Background(), tc[0], tc[1], plan)
		if err == nil || !strings.Contains(err.Error(), "no confirmed asset group") {
			t.Errorf("%s: err = %v, want the no-group refusal", name, err)
		}
		if ids != nil {
			t.Errorf("%s: ids = %v, want none", name, ids)
		}
	}
}
