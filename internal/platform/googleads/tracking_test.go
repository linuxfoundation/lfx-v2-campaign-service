// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// #476: the utmcampaign custom parameter and the per-channel utm_medium
// ---------------------------------------------------------------------------

// url.QueryEscape is pinned against the shape the hand-built campaigns already carry in
// their literal utm_campaign: space → `+`, `|` → `%7C`, `+` → `%2B`. If Go's encoder ever
// spelled one of these differently, LFX and hand-built campaigns would stop reporting
// under the same encoding, so the test names the live shape rather than re-deriving it.
func TestUTMCampaignCustomParameters_MatchesTheLiveEncoding(t *testing.T) {
	const liveShape = "Events+%7C+KubeCon+%2B+CloudNativeCon+North+America+2026+%7C+Search"
	if got := url.QueryEscape("Events | KubeCon + CloudNativeCon North America 2026 | Search"); got != liveShape {
		t.Fatalf("url.QueryEscape = %q, want the live campaigns' encoding %q", got, liveShape)
	}

	params, step := utmCampaignCustomParameters("LFX | Search Campaign | CNCF | KubeCon + CloudNativeCon North America 2026 | brief-1")
	want := []urlCustomParameter{{
		Key:   "utmcampaign",
		Value: "LFX+%7C+Search+Campaign+%7C+CNCF+%7C+KubeCon+%2B+CloudNativeCon+North+America+2026+%7C+brief-1",
	}}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("params = %+v, want %+v", params, want)
	}
	if step != "" {
		t.Errorf("step = %q, want none when the parameter is sent", step)
	}
}

// Characters that could end the query value or open a ValueTrack reference are escaped, so
// the substituted parameter cannot add a utm_* pair, start a fragment or inject {lpurl}.
// url.PathEscape would leave `&` and `=` raw, which is why the query escaper is the one used.
func TestUTMCampaignCustomParameters_EscapesInjectionCharacters(t *testing.T) {
	params, _ := utmCampaignCustomParameters("A&utm_source=x#{lpurl}")
	want := []urlCustomParameter{{Key: "utmcampaign", Value: "A%26utm_source%3Dx%23%7Blpurl%7D"}}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("params = %+v, want %+v", params, want)
	}
}

// 250 is sent and 251 is not: a validateOnly probe on 2026-11-03 saw 251 refused with
// TOO_LONG. Both sides of the boundary, so an off-by-one in either direction fails.
func TestUTMCampaignCustomParameters_OmitsAValueOverTheLimit(t *testing.T) {
	at := strings.Repeat("a", maxCustomParamValueChars)
	if params, step := utmCampaignCustomParameters(at); len(params) != 1 || params[0].Value != at || step != "" {
		t.Errorf("a %d-character value must be sent as is, got params=%+v step=%q", len(at), params, step)
	}

	over := at + "a"
	params, step := utmCampaignCustomParameters(over)
	if params != nil {
		t.Errorf("a %d-character value must be omitted, got %+v", len(over), params)
	}
	want := "tracking template's utm_campaign left empty: the query-escaped campaign name is 251 characters, over Google's 250-character limit for a URL custom parameter, so no utmcampaign custom parameter was set"
	if step != want {
		t.Errorf("step = %q, want %q", step, want)
	}

	// The limit is on the ESCAPED value: 84 characters that each escape to three are 252.
	if params, _ := utmCampaignCustomParameters(strings.Repeat("|", 84)); params != nil {
		t.Errorf("an escaped value of 252 characters must be omitted even though the raw name is 84, got %+v", params)
	}
}

func TestUTMMediumFor_EachChannel(t *testing.T) {
	for kind, want := range map[string]string{
		CampaignKindSearch:         "paid-search",
		CampaignKindDemandGen:      "display",
		CampaignKindDisplay:        "display",
		CampaignKindVideo:          "display",
		CampaignKindPerformanceMax: "cpc",
	} {
		if got := utmMediumFor(kind); got != want {
			t.Errorf("utmMediumFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

// trackingRequest is one request the tracking cascade saw.
type trackingRequest struct {
	path string
	body map[string]any
}

// trackingCascade answers every mutate any of the five channels sends and records each
// request in order. campaignAssets:mutate echoes the asset and field type each operation
// asked to link, because the client checks the link it gets back against the one it sent.
// An unknown path is a failure rather than a default.
func trackingCascade(t *testing.T, images map[string][]byte) (http.HandlerFunc, func() []trackingRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []trackingRequest
	)
	read := func() []trackingRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]trackingRequest(nil), reqs...)
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		reqs = append(reqs, trackingRequest{path: r.URL.Path, body: decoded})
		mu.Unlock()
		// The handlers below re-read the body, so hand them a fresh reader.
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			groupMutateOK(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignAssets:mutate"):
			var req struct {
				Operations []struct {
					Create struct {
						Asset     string `json:"asset"`
						FieldType string `json:"fieldType"`
					} `json:"create"`
				} `json:"operations"`
			}
			_ = json.Unmarshal(raw, &req)
			parts := make([]string, 0, len(req.Operations))
			for _, op := range req.Operations {
				parts = append(parts, `{"resourceName":"customers/1234567890/campaignAssets/222~`+resourceID(op.Create.Asset)+`~`+op.Create.FieldType+`"}`)
			}
			_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			perOperationResults(func(i int) string {
				return "customers/1234567890/assets/" + strconv.Itoa(900+i)
			})(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	return h, read
}

// campaignCreateBody is the create of the one campaigns:mutate a cascade sent.
func campaignCreateBody(t *testing.T, reqs []trackingRequest) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, r := range reqs {
		if !strings.HasSuffix(r.path, "/campaigns:mutate") {
			continue
		}
		ops, _ := r.body["operations"].([]any)
		if len(ops) != 1 {
			t.Fatalf("campaigns:mutate carried %d operations, want 1", len(ops))
		}
		op, _ := ops[0].(map[string]any)
		create, _ := op["create"].(map[string]any)
		found = append(found, create)
	}
	if len(found) != 1 {
		t.Fatalf("saw %d campaigns:mutate requests, want 1", len(found))
	}
	return found[0]
}

// collectFinalURLs walks a decoded body and returns every string in every "finalUrls"
// array, wherever it is nested: an ad, an asset group, a sitelink, a promotion or a
// price offering.
func collectFinalURLs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "finalUrls" {
				if list, ok := child.([]any); ok {
					for _, u := range list {
						if s, ok := u.(string); ok {
							*out = append(*out, s)
						}
					}
				}
				continue
			}
			collectFinalURLs(child, out)
		}
	case []any:
		for _, child := range x {
			collectFinalURLs(child, out)
		}
	}
}

type trackingChannel struct {
	name   string
	kind   string
	images func(t *testing.T) map[string][]byte
	input  func(base string) CampaignInput
	create func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error)
	// finalURLs is how many tagged final URLs the cascade sends, so a test that found
	// none cannot pass by finding nothing to check.
	finalURLs int
}

// trackingChannels is every channel this client has a create cascade for. Search carries
// a sitelink, a promotion and a three-row price extension so the extension destinations
// are checked on the one channel that takes them; Video is the retained cascade, called
// directly because CreateVideoCampaign refuses before step 1.
func trackingChannels() []trackingChannel {
	return []trackingChannel{
		{
			name:   "search",
			kind:   CampaignKindSearch,
			images: func(*testing.T) map[string][]byte { return nil },
			input: func(string) CampaignInput {
				in := demandGenInput()
				in.Sitelinks = []Sitelink{sampleSitelink()}
				in.Promotions = []PromotionExtension{samplePromotion()}
				in.Prices = []PriceExtension{samplePrice()}
				return in
			},
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreateCampaign(ctx, in)
			},
			// The ad, one sitelink, one promotion and three price offerings.
			finalURLs: 6,
		},
		{
			name:   "demand gen",
			kind:   CampaignKindDemandGen,
			images: twoImages,
			input:  func(base string) CampaignInput { return creativeInput(creativeAt(base)) },
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreateDemandGenCampaign(ctx, in)
			},
			finalURLs: 1,
		},
		{
			name:   "display",
			kind:   CampaignKindDisplay,
			images: displayImages,
			input:  displayInput,
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreateDisplayCampaign(ctx, in)
			},
			finalURLs: 1,
		},
		{
			name:   "performance max",
			kind:   CampaignKindPerformanceMax,
			images: pmaxImages,
			input: func(base string) CampaignInput {
				in := demandGenInput()
				in.PerformanceMaxCreative = pmaxCreativeAt(base)
				return in
			},
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreatePerformanceMaxCampaign(ctx, in)
			},
			finalURLs: 1,
		},
		{
			name:   "video",
			kind:   CampaignKindVideo,
			images: func(*testing.T) map[string][]byte { return nil },
			input:  func(string) CampaignInput { return videoInput() },
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.createVideoCampaignCascade(ctx, in)
			},
			finalURLs: 1,
		},
	}
}

// Every channel's campaign create carries exactly the utmcampaign custom parameter, its
// value the composed campaign name query-escaped, and every final URL the cascade sends —
// the ad's, and on Search each extension's — carries that channel's utm_medium with the
// event-level utm_campaign unchanged.
func TestCampaignCreate_UTMCampaignAndMediumOnEveryChannel(t *testing.T) {
	wantMedium := map[string]string{
		CampaignKindSearch:         "paid-search",
		CampaignKindDemandGen:      "display",
		CampaignKindDisplay:        "display",
		CampaignKindPerformanceMax: "cpc",
		CampaignKindVideo:          "display",
	}
	for _, ch := range trackingChannels() {
		t.Run(ch.name, func(t *testing.T) {
			h, read := trackingCascade(t, ch.images(t))
			srv := demandGenTLSServer(t, h)
			c := demandGenClient(t, srv)

			in := ch.input(srv.URL)
			res, err := ch.create(c, context.Background(), in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			reqs := read()

			create := campaignCreateBody(t, reqs)
			// The literal, not QueryEscape(res.CampaignName): a test that recomputed the
			// value the way the code does would agree with any encoding the code chose.
			wantValue := "LFX+%7C+" + strings.ReplaceAll(ch.kind, " ", "+") + "+%7C+tlf+%7C+KubeCon+Europe+2026"
			want := []any{map[string]any{"key": "utmcampaign", "value": wantValue}}
			if got := create["urlCustomParameters"]; !reflect.DeepEqual(got, want) {
				t.Errorf("urlCustomParameters = %#v, want %#v", got, want)
			}
			if res.CampaignName != "LFX | "+ch.kind+" | tlf | KubeCon Europe 2026" {
				t.Errorf("CampaignName = %q — the expected value above assumes a different name", res.CampaignName)
			}
			for _, s := range res.Steps {
				if strings.HasPrefix(s, "tracking template's utm_campaign left empty") {
					t.Errorf("a sent parameter must not record the omission step: %q", s)
				}
			}

			var urls []string
			for _, r := range reqs {
				collectFinalURLs(r.body, &urls)
			}
			if len(urls) != ch.finalURLs {
				t.Fatalf("found %d final URLs, want %d: %v", len(urls), ch.finalURLs, urls)
			}
			for _, raw := range urls {
				u, perr := url.Parse(raw)
				if perr != nil {
					t.Fatalf("final URL %q does not parse: %v", raw, perr)
				}
				q := u.Query()
				if got := q.Get("utm_medium"); got != wantMedium[ch.kind] {
					t.Errorf("final URL %q: utm_medium = %q, want %q", raw, got, wantMedium[ch.kind])
				}
				if got := q.Get("utm_source"); got != "google" {
					t.Errorf("final URL %q: utm_source = %q, want google", raw, got)
				}
				if got := q.Get("utm_campaign"); got != "kubecon-eu-2026" {
					t.Errorf("final URL %q: utm_campaign = %q, want the event slug unchanged", raw, got)
				}
			}
		})
	}
}

// An escaped name over the limit omits the parameter on every channel — the create still
// succeeds, the payload carries no urlCustomParameters key at all, and the result records
// why utm_campaign will be empty, directly after the campaign step.
func TestCampaignCreate_OmitsUTMCampaignOverTheLimitOnEveryChannel(t *testing.T) {
	for _, ch := range trackingChannels() {
		t.Run(ch.name, func(t *testing.T) {
			h, read := trackingCascade(t, ch.images(t))
			srv := demandGenTLSServer(t, h)
			c := demandGenClient(t, srv)

			in := ch.input(srv.URL)
			// Each "/" escapes to three characters, so the name stays well inside Google's
			// 256-character campaign-name limit while its escaped form is far over 250.
			in.EventName = "KubeCon " + strings.Repeat("a/", 60)
			name := ComposeName(ch.kind, in)
			escaped := len(url.QueryEscape(name))
			if escaped <= maxCustomParamValueChars || len([]rune(name)) > maxCampaignNameRunes {
				t.Fatalf("fixture: name is %d runes, escaped %d — want a valid name whose escape is over %d", len([]rune(name)), escaped, maxCustomParamValueChars)
			}

			res, err := ch.create(c, context.Background(), in)
			if err != nil {
				t.Fatalf("an over-long escaped name must not fail the create: %v", err)
			}
			create := campaignCreateBody(t, read())
			if got, present := create["urlCustomParameters"]; present {
				t.Errorf("urlCustomParameters = %#v, want the key absent", got)
			}

			wantStep := "tracking template's utm_campaign left empty: the query-escaped campaign name is " + strconv.Itoa(escaped) +
				" characters, over Google's 250-character limit for a URL custom parameter, so no utmcampaign custom parameter was set"
			at := -1
			for i, s := range res.Steps {
				if s == wantStep {
					at = i
				}
			}
			if at < 1 || !strings.HasPrefix(res.Steps[at-1], "Campaign created: ") {
				t.Errorf("want the step %q directly after the campaign step, got:\n%s", wantStep, strings.Join(res.Steps, "\n"))
			}
		})
	}
}

// The omission step is recorded the moment the campaign exists, so a later step's failure
// returns it on the partial result alongside what was created.
func TestCreateCampaign_OmissionStepSurvivesALaterFailure(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		},
		okAdGroupAd)
	in := sampleInput()
	in.EventName = "KubeCon " + strings.Repeat("a/", 60)

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("want the ad group failure")
	}
	if res == nil || res.CampaignID != "222" {
		t.Fatalf("want the partial result carrying the campaign, got %+v", res)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "tracking template's utm_campaign left empty") {
		t.Errorf("the partial result lost the omission step:\n%s", strings.Join(res.Steps, "\n"))
	}
}
