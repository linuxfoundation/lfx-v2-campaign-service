// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixedMetaClock pins the clock so date-based tests (StartDate/EndDate) stay
// valid regardless of the wall clock. Chosen before the test fixtures' dates.
func fixedMetaClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) }
}

// ---------------------------------------------------------------------------
// Objective -> parameter mapping
// ---------------------------------------------------------------------------

func TestObjectiveParamsMapping(t *testing.T) {
	cases := []struct {
		objective string
		campaign  string
		optGoal   string
		promoted  PromotedObjectType
	}{
		{"awareness", "OUTCOME_AWARENESS", "REACH", PromotedObjectNone},
		{"traffic", "OUTCOME_TRAFFIC", "LINK_CLICKS", PromotedObjectNone},
		{"engagement", "OUTCOME_ENGAGEMENT", "POST_ENGAGEMENT", PromotedObjectPageID},
		{"leads", "OUTCOME_LEADS", "LEAD_GENERATION", PromotedObjectPageID},
		{"conversions", "OUTCOME_SALES", "OFFSITE_CONVERSIONS", PromotedObjectPixelID},
	}
	for _, tc := range cases {
		p, ok := ObjectiveParamsFor(tc.objective)
		if !ok {
			t.Fatalf("objective %q not found", tc.objective)
		}
		if p.CampaignObjective != tc.campaign {
			t.Errorf("%s: campaign objective = %q, want %q", tc.objective, p.CampaignObjective, tc.campaign)
		}
		if p.OptimizationGoal != tc.optGoal {
			t.Errorf("%s: optimization goal = %q, want %q", tc.objective, p.OptimizationGoal, tc.optGoal)
		}
		if p.PromotedObjectType != tc.promoted {
			t.Errorf("%s: promoted type = %q, want %q", tc.objective, p.PromotedObjectType, tc.promoted)
		}
	}

	if _, ok := ObjectiveParamsFor("nonsense"); ok {
		t.Errorf("unknown objective unexpectedly found")
	}
}

func TestBuildPromotedObject(t *testing.T) {
	// page_id objective
	po, err := buildPromotedObject("engagement", "PAGE1", "")
	if err != nil {
		t.Fatalf("engagement: %v", err)
	}
	if po["page_id"] != "PAGE1" {
		t.Errorf("engagement promoted_object = %v", po)
	}

	// pixel_id objective without pixel -> error
	if _, err := buildPromotedObject("conversions", "PAGE1", "  "); err == nil {
		t.Errorf("expected error for conversions without pixelID")
	}

	// pixel_id objective with pixel
	po, err = buildPromotedObject("conversions", "PAGE1", " PIX9 ")
	if err != nil {
		t.Fatalf("conversions: %v", err)
	}
	if po["pixel_id"] != "PIX9" || po["custom_event_type"] != "PURCHASE" {
		t.Errorf("conversions promoted_object = %v", po)
	}

	// none objective
	po, err = buildPromotedObject("traffic", "PAGE1", "")
	if err != nil {
		t.Fatalf("traffic: %v", err)
	}
	if po != nil {
		t.Errorf("traffic promoted_object = %v, want nil", po)
	}
}

// ---------------------------------------------------------------------------
// Placement targeting
// ---------------------------------------------------------------------------

func TestBuildPlacementTargetingDefaults(t *testing.T) {
	// Defaults enable facebook feed + instagram feed only.
	tg, err := buildPlacementTargeting(Placement{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp, _ := tg["publisher_platforms"].([]string)
	if !contains(pp, "facebook") || !contains(pp, "instagram") {
		t.Errorf("publisher_platforms = %v, want facebook+instagram", pp)
	}
	if contains(pp, "audience_network") {
		t.Errorf("audience_network should be off by default")
	}
	fbPos, _ := tg["facebook_positions"].([]string)
	if !contains(fbPos, "feed") {
		t.Errorf("facebook_positions = %v, want feed", fbPos)
	}
}

func TestBuildPlacementTargetingNoneEnabled(t *testing.T) {
	f := false
	_, err := buildPlacementTargeting(Placement{
		FacebookFeed:  &f,
		InstagramFeed: &f,
	})
	if err == nil {
		t.Fatalf("expected error when no placement is enabled")
	}
	if !strings.Contains(err.Error(), "at least one placement") {
		t.Errorf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Geo / region / validation helpers
// ---------------------------------------------------------------------------

func TestValidateGeoTargets(t *testing.T) {
	got := validateGeoTargets([]string{" us ", "gb", "zzz", "1", "de"})
	want := []string{"US", "GB", "DE"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("validateGeoTargets = %v, want %v", got, want)
	}
	if def := validateGeoTargets([]string{"!!!"}); strings.Join(def, ",") != "US" {
		t.Errorf("default = %v, want [US]", def)
	}
}

func TestResolveRegion(t *testing.T) {
	if r := resolveRegion([]string{"DE", "US"}); r != "EMEA" {
		t.Errorf("region = %q, want EMEA", r)
	}
	if r := resolveRegion([]string{"ZZ"}); r != "Global" {
		t.Errorf("region = %q, want Global", r)
	}
	if r := resolveRegion(nil); r != "Global" {
		t.Errorf("region = %q, want Global", r)
	}
}

func TestValidateRegistrationURL(t *testing.T) {
	if err := validateRegistrationURL("https://events.linuxfoundation.org/x"); err != nil {
		t.Errorf("valid https url errored: %v", err)
	}
	if err := validateRegistrationURL("http://insecure.example"); err == nil {
		t.Errorf("expected error for non-https")
	}
	if err := validateRegistrationURL("not a url"); err == nil {
		t.Errorf("expected error for invalid url")
	}
}

func TestBuildCampaignName(t *testing.T) {
	name := buildCampaignName(CampaignInput{
		EventName: "Open|Source Summit",
		Project:   "CNCF",
		Objective: "leads",
	}, []string{"DE"})
	want := "Events | Open-Source Summit | EMEA | Leads | Intent | Social | CNCF | MoFU"
	if name != want {
		t.Errorf("campaign name = %q, want %q", name, want)
	}
}

func TestBuildUTMURL(t *testing.T) {
	u := buildUTMURL(CampaignInput{
		EventName:       "KubeCon EU",
		RegistrationURL: "https://events.example.org/kubecon/",
		HSToken:         "hs-123",
	}, 0)
	if !strings.HasPrefix(u, "https://events.example.org/kubecon?") {
		t.Errorf("utm url = %q (bad base/trailing slash handling)", u)
	}
	for _, want := range []string{"utm_source=meta", "utm_medium=paid-social", "utm_campaign=hs-123", "utm_content=variant-1", "utm_term=kubecon-eu"} {
		if !strings.Contains(u, want) {
			t.Errorf("utm url %q missing %q", u, want)
		}
	}
}

func TestBuildUTMURLPreservesFragment(t *testing.T) {
	u := buildUTMURL(CampaignInput{
		EventName:       "KubeCon EU",
		RegistrationURL: "https://events.example.org/event#register",
		HSToken:         "hs-123",
	}, 0)

	hashIdx := strings.Index(u, "#")
	if hashIdx < 0 {
		t.Fatalf("utm url %q dropped the fragment", u)
	}
	if !strings.HasSuffix(u, "#register") {
		t.Errorf("utm url %q must keep #register at the very end", u)
	}
	// All utm params must land before the fragment, not after it.
	beforeFragment := u[:hashIdx]
	for _, want := range []string{"utm_source=meta", "utm_medium=paid-social", "utm_campaign=hs-123", "utm_content=variant-1", "utm_term=kubecon-eu"} {
		if !strings.Contains(beforeFragment, want) {
			t.Errorf("utm url %q missing %q before the fragment", u, want)
		}
	}
}

func TestBuildUTMURLPreservesExistingQuery(t *testing.T) {
	u := buildUTMURL(CampaignInput{
		EventName:       "KubeCon EU",
		RegistrationURL: "https://events.example.org/e?ref=abc#section",
		HSToken:         "hs-1",
	}, 0)
	if !strings.Contains(u, "ref=abc") {
		t.Errorf("utm url %q dropped the existing query param ref=abc", u)
	}
	if !strings.HasSuffix(u, "#section") {
		t.Errorf("utm url %q must keep #section at the end", u)
	}
}

// TestBuildUTMURLPreservesSlashInQueryValue guards against the whole-string
// TrimRight bug: a query value ending in '/' (e.g. ?redirect=/) must survive.
func TestBuildUTMURLPreservesSlashInQueryValue(t *testing.T) {
	u := buildUTMURL(CampaignInput{
		EventName:       "KubeCon EU",
		RegistrationURL: "https://events.example.org/register?redirect=/",
		HSToken:         "hs-1",
	}, 0)

	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("result url %q did not parse: %v", u, err)
	}
	if got := parsed.Query().Get("redirect"); got != "/" {
		t.Errorf("redirect value = %q, want %q (trailing slash was stripped)", got, "/")
	}
	if !strings.Contains(u, "utm_source=meta") {
		t.Errorf("utm url %q missing utm_source", u)
	}
}

// ---------------------------------------------------------------------------
// CreateCampaign happy path against an httptest server
// ---------------------------------------------------------------------------

func TestCreateCampaignHappyPath(t *testing.T) {
	var gotAuth string
	var campaignBody map[string]any
	var adsetBody map[string]any
	creativeCount := 0
	adCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_TEST") && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			campaignBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeCount++
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(creativeCount)+`"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			adCount++
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(adCount)+`"}`)
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-abc"},
		AccountConfig{AccountID: "act_TEST", PageID: "PAGE99", Label: "LF Core"},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
	)

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US", "DE"},
		BudgetUSD:       500,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants: []AdVariant{
			{PrimaryText: "Join us", Headline: "KubeCon 2026"},
			{PrimaryText: "Register", Headline: "See you there"},
		},
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}

	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if res.CampaignID != "camp_123" {
		t.Errorf("campaign id = %q, want camp_123", res.CampaignID)
	}
	if res.AdSetID != "adset_456" {
		t.Errorf("adset id = %q, want adset_456", res.AdSetID)
	}
	if res.AdCount != 2 {
		t.Errorf("ad count = %d, want 2", res.AdCount)
	}
	if res.Platform != "meta-ads" {
		t.Errorf("platform = %q", res.Platform)
	}
	if !strings.Contains(res.MetaURL, "act=TEST") {
		t.Errorf("meta url = %q, want act=TEST (act_ stripped)", res.MetaURL)
	}

	// Campaign body assertions.
	if campaignBody["objective"] != "OUTCOME_TRAFFIC" {
		t.Errorf("campaign objective = %v", campaignBody["objective"])
	}
	if campaignBody["status"] != "PAUSED" {
		t.Errorf("campaign status = %v", campaignBody["status"])
	}

	// Ad set body assertions: daily budget in cents, geo filtered.
	if adsetBody["daily_budget"] != float64(50000) {
		t.Errorf("daily_budget = %v, want 50000", adsetBody["daily_budget"])
	}
	if adsetBody["optimization_goal"] != "LINK_CLICKS" {
		t.Errorf("optimization_goal = %v", adsetBody["optimization_goal"])
	}
}

func TestCreateCampaignLifetimeBudget(t *testing.T) {
	var adsetBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"adset_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_TEST", PageID: "PAGE99"},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
	)
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US"},
		BudgetUSD:       500,
		LifetimeBudget:  true,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "Join us", Headline: "KubeCon 2026"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if adsetBody["lifetime_budget"] != float64(50000) {
		t.Errorf("lifetime_budget = %v, want 50000", adsetBody["lifetime_budget"])
	}
	if _, ok := adsetBody["daily_budget"]; ok {
		t.Errorf("daily_budget should be absent when LifetimeBudget is set, got %v", adsetBody["daily_budget"])
	}
}

func TestCreateCampaignSkipsRegulatedGeos(t *testing.T) {
	var adsetBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"a1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"cr1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad1"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US", "SG", "KR"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	geo := adsetBody["targeting"].(map[string]any)["geo_locations"].(map[string]any)["countries"].([]any)
	if len(geo) != 1 || geo[0] != "US" {
		t.Errorf("geo countries = %v, want [US]", geo)
	}
	if !anyStepContains(res.Steps, "Geo targets skipped") {
		t.Errorf("expected a skipped-geo step, got %v", res.Steps)
	}
}

func TestCreateCampaignAllGeosRegulated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"x"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"SG", "KR"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "require manual compliance") {
		t.Fatalf("expected regulated-geo error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Graph API error body -> Go error mapping
// ---------------------------------------------------------------------------

func TestGraphAPIErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid parameter","type":"OAuthException","code":100,"fbtrace_id":"XYZ"}}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Message != "Invalid parameter" {
		t.Errorf("message = %q, want 'Invalid parameter'", apiErr.Message)
	}
}

// TestNonGraphErrorBodySurfaces verifies that a non-2xx response whose body is
// NOT a Graph error envelope still surfaces the raw body in the error, rather
// than pointing at nonexistent server logs.
func TestNonGraphErrorBodySurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>502 Bad Gateway from upstream proxy</html>")
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if !strings.Contains(apiErr.Message, "502 Bad Gateway from upstream proxy") {
		t.Errorf("APIError.Message = %q, want the raw body snippet", apiErr.Message)
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway from upstream proxy") {
		t.Errorf("error string = %q, want the raw body snippet", err.Error())
	}
	if strings.Contains(err.Error(), "server logs") {
		t.Errorf("error string %q must not reference server logs", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Non-fatal failure paths (per-variant ad failure; account verification)
// ---------------------------------------------------------------------------

// TestCreateCampaignPerVariantFailureIsNonFatal verifies that when the FIRST
// variant's creative call fails, the campaign still succeeds, a later variant is
// still created, AdCount counts only the successes, and the failure is recorded
// as a step.
func TestCreateCampaignPerVariantFailureIsNonFatal(t *testing.T) {
	var creativeCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			// Fail the first creative; succeed on all subsequent ones.
			if atomic.AddInt32(&creativeCalls, 1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"bad creative"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"creative_ok"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_ok"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants: []AdVariant{
			{PrimaryText: "p1", Headline: "h1"},
			{PrimaryText: "p2", Headline: "h2"},
		},
	})
	if err != nil {
		t.Fatalf("CreateCampaign should not fail when one variant fails: %v", err)
	}
	if res.CampaignID != "camp_1" {
		t.Errorf("campaign id = %q, want camp_1", res.CampaignID)
	}
	if res.AdCount != 1 {
		t.Errorf("ad count = %d, want 1 (only the second variant succeeds)", res.AdCount)
	}
	if !anyStepContains(res.Steps, "Ad 1 failed") {
		t.Errorf("expected an 'Ad 1 failed' step, got %v", res.Steps)
	}
	if !anyStepContains(res.Steps, "Ad 2 created") {
		t.Errorf("expected an 'Ad 2 created' step, got %v", res.Steps)
	}
}

// TestCreateCampaignAccountVerificationFailureIsNonFatal verifies that a failing
// account-verification GET does not abort creation: the campaign is still created
// and a warning step is recorded.
func TestCreateCampaignAccountVerificationFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			// Account verification fails.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"account lookup failed"}}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p", Label: "LF Core"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("account verification failure must be non-fatal: %v", err)
	}
	if res.CampaignID != "camp_1" {
		t.Errorf("campaign id = %q, want camp_1", res.CampaignID)
	}
	if res.AdCount != 1 {
		t.Errorf("ad count = %d, want 1", res.AdCount)
	}
	if !anyStepContains(res.Steps, "Account verification warning") {
		t.Errorf("expected an 'Account verification warning' step, got %v", res.Steps)
	}
}

// ---------------------------------------------------------------------------
// Input validation errors
// ---------------------------------------------------------------------------

func TestCreateCampaignValidation(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL("http://unused.invalid"), WithClock(fixedMetaClock()))
	base := CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	}

	tests := []struct {
		name   string
		mutate func(*CampaignInput)
		want   string
	}{
		{"no variants", func(in *CampaignInput) { in.Variants = nil }, "at least one ad variant"},
		{"empty variants", func(in *CampaignInput) { in.Variants = []AdVariant{{PrimaryText: " ", Headline: ""}} }, "non-empty primary text"},
		{"bad url", func(in *CampaignInput) { in.RegistrationURL = "http://x.example" }, "must use HTTPS"},
		{"bad budget", func(in *CampaignInput) { in.BudgetUSD = 0 }, "positive number"},
		{"sub-cent budget rounds to zero", func(in *CampaignInput) { in.BudgetUSD = 0.001 }, "budget too small"},
		{"bad start date", func(in *CampaignInput) { in.StartDate = "2026/08/01" }, "invalid start date"},
		{"impossible calendar date", func(in *CampaignInput) { in.StartDate = "2026-13-40" }, "invalid start date"},
		{"end before start", func(in *CampaignInput) { in.EndDate = "2026-07-01" }, "must be after start date"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// noPostServer returns an httptest server that fails the test if it ever
// receives a POST (a mutating call). GETs return a benign body so account
// verification succeeds.
func noPostServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST (mutating call) to %s: input validation should have failed first", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x"}`)
	}))
}

func TestCreateCampaignRejectsSubCentBudgetBeforeAnyPost(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       0.001,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "budget too small") {
		t.Fatalf("err = %v, want 'budget too small'", err)
	}
}

func TestCreateCampaignAllDisabledPlacementsMakesZeroPosts(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	f := false
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Placements:      Placement{FacebookFeed: &f, InstagramFeed: &f},
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "at least one placement") {
		t.Fatalf("err = %v, want 'at least one placement'", err)
	}
}

func TestCreateCampaignRequiresPageIDBeforeAnyPost(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	// PageID intentionally left empty.
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "PageID is required") {
		t.Fatalf("err = %v, want 'PageID is required'", err)
	}
}

func TestCreateCampaignRequiresAccountIDBeforeAnyPost(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	// AccountID intentionally left empty; an empty ID would build "//campaigns".
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "AccountID is required") {
		t.Fatalf("err = %v, want 'AccountID is required'", err)
	}
}

func TestCreateCampaignImpossibleDateMakesZeroPosts(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-13-40",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid start date") {
		t.Fatalf("err = %v, want 'invalid start date'", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func anyStepContains(steps []string, sub string) bool {
	for _, s := range steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 429 rate-limit retry/backoff
// ---------------------------------------------------------------------------

// TestDoRequestRetriesOn429 verifies that a 429 followed by a 200 is retried and
// ultimately succeeds. A short Retry-After keeps the test fast.
func TestDoRequestRetriesOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0") // 0 -> falls back to base backoff
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"123"}`)
	}))
	defer srv.Close()

	// Shrink the base backoff so the fallback wait doesn't slow the test.
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	var out createResponse
	if err := c.doRequest(context.Background(), http.MethodPost, "/x", map[string]any{"k": "v"}, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if out.ID != "123" {
		t.Errorf("id = %q, want 123", out.ID)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want 2 (one 429 + one success)", got)
	}
}

// TestDoRequestExhaustsRetries verifies that persistent 429s return an error
// after retryMax attempts rather than looping forever.
func TestDoRequestExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.doRequest(ctx, http.MethodGet, "/x", nil, nil)
	if err == nil {
		t.Fatalf("expected an error after exhausting retries")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
	// 1 initial + retryMax retries = retryMax+1 total server hits.
	if got := atomic.LoadInt32(&calls); got != int32(retryMax+1) {
		t.Errorf("server calls = %d, want %d", got, retryMax+1)
	}
}

// TestParseRetryAfter covers the header parsing paths: delay-seconds, HTTP-date,
// and absent/invalid headers.
func TestParseRetryAfter(t *testing.T) {
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "a"},
		WithClock(func() time.Time { return fixed }))

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"delay seconds", "5", 5 * time.Second},
		{"zero -> none", "0", 0},
		{"negative -> none", "-3", 0},
		{"http-date future", fixed.Add(10 * time.Second).UTC().Format(http.TimeFormat), 10 * time.Second},
		{"http-date past -> none", fixed.Add(-10 * time.Second).UTC().Format(http.TimeFormat), 0},
		{"absent -> none", "", 0},
		{"garbage -> none", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tt.header != "" {
				resp.Header.Set("Retry-After", tt.header)
			}
			if got := c.parseRetryAfter(resp); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

// TestDoRequestRetryHonorsContextCancel verifies that a cancelled context during
// 429 backoff aborts promptly rather than sleeping out the full delay.
func TestDoRequestRetryHonorsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30") // long enough that cancel must win
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "a"}, WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := c.doRequest(ctx, http.MethodGet, "/x", nil, nil)
	if err == nil {
		t.Fatalf("expected a context error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("doRequest blocked %v; should have aborted on cancel", elapsed)
	}
}

// TestCreateCampaignRejectsLeadsObjective verifies the leads objective is
// rejected up front (before any mutating call) since it would create a
// lead-gen-optimized campaign wired to a website-click creative.
func TestCreateCampaignRejectsLeadsObjective(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Objective:       "leads",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "leads") {
		t.Fatalf("err = %v, want the leads-unsupported error", err)
	}
}

// TestValidateGeoTargetsRejectsBogusISO verifies that a well-shaped but
// non-existent code (XX) is dropped and does not reach Meta.
func TestValidateGeoTargetsRejectsBogusISO(t *testing.T) {
	got := validateGeoTargets([]string{"XX", "ZZ"})
	// All inputs invalid -> defaults to US, and the bogus codes are absent.
	if len(got) != 1 || got[0] != "US" {
		t.Errorf("validateGeoTargets(XX,ZZ) = %v, want [US] (bogus codes dropped)", got)
	}
	if contains(got, "XX") || contains(got, "ZZ") {
		t.Errorf("bogus codes leaked into %v", got)
	}
	// A mix keeps the real one, drops the bogus one.
	got2 := validateGeoTargets([]string{"DE", "XX"})
	if len(got2) != 1 || got2[0] != "DE" {
		t.Errorf("validateGeoTargets(DE,XX) = %v, want [DE]", got2)
	}
}

// TestDoRequestRetriesOnGraphThrottleCode verifies a 400 with a Graph rate-limit
// envelope code (e.g. 4) is retried like a 429 and ultimately succeeds.
func TestDoRequestRetriesOnGraphThrottleCode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","code":4}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"123"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	var out createResponse
	if err := c.doRequest(context.Background(), http.MethodPost, "/x", map[string]any{"k": "v"}, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if out.ID != "123" {
		t.Errorf("id = %q, want 123", out.ID)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want 2 (one throttled 400 + one success)", got)
	}
}

// TestCreateCampaignRejectsPastStartDate verifies a start date before today is
// rejected before any mutating call.
func TestCreateCampaignRejectsPastStartDate(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	// Pin the clock so "past" is deterministic.
	now := func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"},
		WithBaseURL(srv.URL), WithClock(now))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01", // before the pinned "today" of 2026-08-15
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "past") {
		t.Fatalf("err = %v, want past-start-date rejection", err)
	}
}

// TestCreateCampaignRejectsHugeBudget verifies an overflow-scale budget is
// rejected before any mutating call.
func TestCreateCampaignRejectsHugeBudget(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		BudgetUSD:       1e18,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "budget too large") {
		t.Fatalf("err = %v, want 'budget too large'", err)
	}
}

// TestDoRequestReadsLargeSuccessBody verifies a success body larger than the
// old 64KiB drain cap is fully read (not truncated) and decoded.
func TestDoRequestReadsLargeSuccessBody(t *testing.T) {
	// Build a >64KiB JSON success body with a padded field plus the id.
	pad := strings.Repeat("x", 100<<10) // 100 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pad":"`+pad+`","id":"123"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"}, WithBaseURL(srv.URL))
	var out createResponse
	if err := c.doRequest(context.Background(), http.MethodGet, "/x", nil, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if out.ID != "123" {
		t.Errorf("id = %q, want 123 (body must not be truncated before the id field)", out.ID)
	}
}

// TestDoRequestPropagatesBodyReadError verifies a truncated response (declared
// Content-Length larger than the body sent) is reported as an error, not a
// false success, even if the partial body would parse.
func TestDoRequestPropagatesBodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Advertise more bytes than we actually write, then hijack-close so the
		// client sees an unexpected EOF mid-body.
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"id":"123"}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	var out createResponse
	err := c.doRequest(context.Background(), http.MethodGet, "/x", nil, &out)
	if err == nil {
		t.Fatal("expected a read error, got nil (a truncated body must not be a success)")
	}
}

// TestWithHTTPClientNilIsIgnored verifies a nil client doesn't clobber the
// default (which would panic on the next request).
func TestWithHTTPClientNilIsIgnored(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"}, WithHTTPClient(nil))
	if c.httpClient == nil {
		t.Fatal("WithHTTPClient(nil) nil-ed the default http client")
	}
}

// TestNewClientTrimsCredentials verifies whitespace in AccountID/PageID/token is
// trimmed at construction so it can't produce a malformed request URL.
func TestNewClientTrimsCredentials(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "  tok  "}, AccountConfig{AccountID: "  act_1  ", PageID: "  p  "})
	if c.creds.AccessToken != "tok" || c.account.AccountID != "act_1" || c.account.PageID != "p" {
		t.Errorf("credentials not trimmed: token=%q account=%q page=%q", c.creds.AccessToken, c.account.AccountID, c.account.PageID)
	}
}

// TestAdSetStartTimeTodayUsesBuffer verifies that a campaign starting today gets
// an ad-set start_time of now+buffer (not 00:00 UTC, which would be in the past),
// while a future start date uses start-of-day.
func TestAdSetStartTimeTodayUsesBuffer(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC)

	// Start today: must be after now (buffered), not 00:00.
	today := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	got := adSetStartTime(today, now)
	parsed, err := time.Parse("2006-01-02T15:04:05-0700", got)
	if err != nil {
		t.Fatalf("unparseable start_time %q: %v", got, err)
	}
	if !parsed.After(now) {
		t.Errorf("today start_time = %q, want after now (%v)", got, now)
	}

	// Future date: start-of-day.
	future := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got := adSetStartTime(future, now); got != "2026-09-01T00:00:00+0000" {
		t.Errorf("future start_time = %q, want 2026-09-01T00:00:00+0000", got)
	}
}

// TestValidateGeoTargetsExcludesSanctioned verifies Meta-ineligible countries
// (comprehensively sanctioned, plus RU per Meta's ads policy) are dropped even
// though they're valid ISO codes.
func TestValidateGeoTargetsExcludesSanctioned(t *testing.T) {
	got := validateGeoTargets([]string{"US", "IR", "KP", "CU", "SY", "RU", "DE"})
	for _, bad := range []string{"IR", "KP", "CU", "SY", "RU"} {
		if contains(got, bad) {
			t.Errorf("ineligible country %s leaked into %v", bad, got)
		}
	}
	if !contains(got, "US") || !contains(got, "DE") {
		t.Errorf("valid countries dropped: %v", got)
	}
}

// TestCreateCampaignRejectsAllSanctionedGeos verifies that when every supplied
// geo is invalid/sanctioned, CreateCampaign errors instead of silently
// falling back to US.
func TestCreateCampaignRejectsAllSanctionedGeos(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"IR", "KP"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable geo targets") {
		t.Fatalf("err = %v, want all-sanctioned-geos rejection (no silent US fallback)", err)
	}
}

// TestCreateCampaignRejectsRussiaOnlyGeo verifies that a Russia-only target is
// rejected at preflight (no mutating HTTP call) rather than passing preflight and
// failing at the ad-set step after the campaign already exists. RU is Meta-
// ineligible per Meta's ads policy, so it must be handled identically to the
// comprehensively-sanctioned geos (no silent fallback to US).
func TestCreateCampaignRejectsRussiaOnlyGeo(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"RU"},
		BudgetUSD:       10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable geo targets") {
		t.Fatalf("err = %v, want Russia-only rejection at preflight (no silent US fallback)", err)
	}
}
