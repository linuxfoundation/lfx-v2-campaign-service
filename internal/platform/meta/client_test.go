// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL))
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL))
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL(srv.URL))
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

// ---------------------------------------------------------------------------
// Input validation errors
// ---------------------------------------------------------------------------

func TestCreateCampaignValidation(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "p"}, WithBaseURL("http://unused.invalid"))
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
		{"bad start date", func(in *CampaignInput) { in.StartDate = "2026/08/01" }, "invalid start date"},
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
