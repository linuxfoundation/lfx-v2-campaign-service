// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// urlWithUserinfo composes a URL with embedded userinfo at runtime, so the test
// source never contains a literal "user:pass@host" — which secretlint's
// basic-auth rule (MegaLinter) flags as a credential and fails CI on. The
// composed value still exercises the userinfo-rejection paths.
func urlWithUserinfo(scheme, user, pass, hostAndRest string) string {
	cred := user
	if pass != "" {
		cred += ":" + pass
	}
	return scheme + "://" + cred + "@" + hostAndRest
}

// roundTripFunc adapts a function to http.RoundTripper for tests that need to
// inject transport-level errors (e.g. a canceled context) deterministically.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
		{"leads", "OUTCOME_TRAFFIC", "LINK_CLICKS", PromotedObjectNone},
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

	// pixel_id objective with a valid (numeric) pixel — surrounding whitespace is
	// trimmed. Meta Pixel IDs are numeric strings.
	po, err = buildPromotedObject("conversions", "PAGE1", " 1234567890 ")
	if err != nil {
		t.Fatalf("conversions: %v", err)
	}
	if po["pixel_id"] != "1234567890" || po["custom_event_type"] != "PURCHASE" {
		t.Errorf("conversions promoted_object = %v", po)
	}

	// pixel_id objective with a malformed (non-numeric) pixel -> error, so a bad
	// pixel id fails before any campaign is created rather than at ad-set time.
	if _, err := buildPromotedObject("conversions", "PAGE1", "PIX9"); err == nil {
		t.Errorf("expected error for conversions with a non-numeric pixelID")
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
	// Port-only / empty-hostname authorities parse with a non-empty Host but an
	// empty Hostname(); they must be rejected as invalid destinations.
	for _, raw := range []string{"https://:443", "https://:443/register", "https://", "//events.example.org/x"} {
		if err := validateRegistrationURL(raw); err == nil {
			t.Errorf("expected error for port-only/empty-hostname url %q", raw)
		}
	}
}

func TestBuildCampaignName(t *testing.T) {
	// The Project segment carries the canonical LFX slug (docs/api-catalog.md),
	// so the fixture uses the canonical "cncf" rather than a display label. It is
	// padded with whitespace to prove the builder trims segments: validation
	// TrimSpaces its checks, so " cncf " passes validation, but the attribution
	// pipeline joins the Project segment exactly. The name is the 8-segment
	// convention ending in the Funnel (MoFU) segment.
	name := buildCampaignName(CampaignInput{
		EventName: "Open|Source Summit",
		Project:   " cncf ",
		Objective: "leads",
		StartDate: "2026-08-01",
	}, []string{"DE"})
	want := "Events | Open-Source Summit | EMEA | Leads | Intent | Social | cncf | MoFU"
	if name != want {
		t.Errorf("campaign name = %q, want %q", name, want)
	}

	// buildCampaignName no longer substitutes a placeholder for an omitted Project:
	// CreateCampaign rejects an empty Project up front, so this builder is only ever
	// called with a non-empty caller-supplied slug. Verify the slug is placed
	// verbatim (with '|' sanitized) into the Project segment.
	named := buildCampaignName(CampaignInput{EventName: "Summit", Project: "kubernetes", Objective: "leads"}, []string{"DE"})
	if !strings.Contains(named, "| kubernetes |") {
		t.Errorf("project segment = %q, want the caller-supplied slug 'kubernetes'", named)
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
	authCapture := make(chan string, 8)
	campaignCap := newBodyCapture()
	adsetCap := newBodyCapture()
	creativeCap := newBodyCapture()
	adCap := newBodyCapture()
	var creativeCount, adCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCapture <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777") && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			campaignCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeCap.set(decodeBody(t, r))
			n := atomic.AddInt32(&creativeCount, 1)
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(int(n))+`"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			adCap.set(decodeBody(t, r))
			n := atomic.AddInt32(&adCount, 1)
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(int(n))+`"}`)
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-abc"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", Label: "LF Core", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
	)

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US", "DE"},
		Budget:          500,
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

	// Read all captures after CreateCampaign has returned (all requests done).
	gotAuth := ""
	for done := false; !done; {
		select {
		case a := <-authCapture:
			gotAuth = a
		default:
			done = true
		}
	}
	campaignBody := campaignCap.get()
	adsetBody := adsetCap.get()
	creativeBody := creativeCap.get()
	adBody := adCap.get()

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
	if !strings.Contains(res.MetaURL, "act=777") {
		t.Errorf("meta url = %q, want act=777 (act_ stripped)", res.MetaURL)
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

	// Creative body assertions: object_story_spec.page_id and the UTM link/copy
	// must be present so a regression that drops them is caught. creativeBody is
	// the last (second) variant, "Register"/"See you there".
	if creativeBody == nil {
		t.Fatalf("no creative body captured")
	}
	oss, ok := creativeBody["object_story_spec"].(map[string]any)
	if !ok {
		t.Fatalf("creative object_story_spec missing or wrong type: %v", creativeBody["object_story_spec"])
	}
	if oss["page_id"] != "987654321" {
		t.Errorf("creative object_story_spec.page_id = %v, want 987654321", oss["page_id"])
	}
	linkData, ok := oss["link_data"].(map[string]any)
	if !ok {
		t.Fatalf("creative link_data missing or wrong type: %v", oss["link_data"])
	}
	link, _ := linkData["link"].(string)
	if !strings.Contains(link, "utm_source=meta") {
		t.Errorf("creative link = %q, want UTM link with utm_source=meta", link)
	}
	if !strings.HasPrefix(link, "https://events.example.org/kubecon") {
		t.Errorf("creative link = %q, want registration URL base", link)
	}
	if linkData["message"] != "Register" {
		t.Errorf("creative link_data.message = %v, want 'Register'", linkData["message"])
	}
	if linkData["name"] != "See you there" {
		t.Errorf("creative link_data.name = %v, want 'See you there'", linkData["name"])
	}

	// Ad body assertions: adset_id and creative_id must wire the ad to the ad set
	// and creative just created.
	if adBody == nil {
		t.Fatalf("no ad body captured")
	}
	if adBody["adset_id"] != "adset_456" {
		t.Errorf("ad adset_id = %v, want adset_456", adBody["adset_id"])
	}
	creative, ok := adBody["creative"].(map[string]any)
	if !ok {
		t.Fatalf("ad creative field missing or wrong type: %v", adBody["creative"])
	}
	if creative["creative_id"] != "creative_2" {
		t.Errorf("ad creative.creative_id = %v, want creative_2", creative["creative_id"])
	}
}

// TestCreateCampaignNormalizesEventName verifies that a padded EventName is
// trimmed for ALL generated names and the UTM term — not just the campaign name
// (which trims internally). A raw " KubeCon EU " would otherwise leak into the
// ad-set/creative/ad names and produce a malformed utm_term=-kubecon-eu-.
func TestCreateCampaignNormalizesEventName(t *testing.T) {
	adsetCap := newBodyCapture()
	creativeCap := newBodyCapture()
	adCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"adset_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			adCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "  KubeCon EU  ", Project: "tlf", Objective: "traffic",
		// Padded RegistrationURL: validation trims its own copy, but buildUTMURL
		// reads in.RegistrationURL directly — CreateCampaign must trim it in place
		// so the creative click URL is built from the clean base, not " https...".
		RegistrationURL: "  https://x.example.org/e  ", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	}); err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}

	// Drain each capture ONCE (get() consumes the buffered value).
	adsetBody := adsetCap.get()
	creativeBody := creativeCap.get()
	adBody := adCap.get()

	// Ad-set name uses the trimmed event name (no leading/trailing spaces around
	// the " - Traffic" join).
	if got, _ := adsetBody["name"].(string); got != "KubeCon EU - Traffic" {
		t.Errorf("ad set name = %q, want %q", got, "KubeCon EU - Traffic")
	}
	// Creative name uses the trimmed event name.
	if got, _ := creativeBody["name"].(string); got != "KubeCon EU - Variant 1" {
		t.Errorf("creative name = %q, want %q", got, "KubeCon EU - Variant 1")
	}
	// Ad name uses the trimmed event name.
	if got, _ := adBody["name"].(string); got != "KubeCon EU - Ad 1" {
		t.Errorf("ad name = %q, want %q", got, "KubeCon EU - Ad 1")
	}
	// UTM term is derived from the trimmed name: no leading/trailing dash.
	oss, _ := creativeBody["object_story_spec"].(map[string]any)
	linkData, _ := oss["link_data"].(map[string]any)
	link, _ := linkData["link"].(string)
	if !strings.Contains(link, "utm_term=kubecon-eu") {
		t.Errorf("creative link = %q, want clean utm_term=kubecon-eu", link)
	}
	// The padded RegistrationURL was trimmed in place: the creative link must start
	// with the clean base, never a leading space or an unparseable " https".
	if !strings.HasPrefix(link, "https://x.example.org/e") {
		t.Errorf("creative link = %q, want it to start with the trimmed base URL", link)
	}
	if strings.Contains(link, "utm_term=-kubecon") || strings.Contains(link, "kubecon-eu-&") || strings.Contains(link, "kubecon-eu-#") {
		t.Errorf("creative link = %q, want no leading/trailing dash in utm_term", link)
	}
}

// TestCreateCampaignAdSetFailureReturnsPartialResult verifies that when the ad
// set POST fails AFTER the campaign was already created, CreateCampaign returns
// a non-nil partial CampaignResult carrying the orphaned campaign's ID (so a
// caller can reconcile/clean up without parsing the error string) alongside the
// error.
func TestCreateCampaignAdSetFailureReturnsPartialResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777") && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_orphan"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			// Ad set creation fails after the campaign already exists.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad targeting","type":"OAuthException","code":100}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-abc"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
	)

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US"},
		Budget:          500,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "Join us", Headline: "KubeCon 2026"}},
	})
	if err == nil {
		t.Fatal("expected an error when ad set creation fails")
	}
	if res == nil {
		t.Fatal("expected a non-nil partial result carrying the orphaned campaign ID, got nil")
	}
	if res.CampaignID != "camp_orphan" {
		t.Errorf("partial result CampaignID = %q, want camp_orphan", res.CampaignID)
	}
	if res.AdSetID != "" {
		t.Errorf("partial result AdSetID = %q, want empty (ad set was never created)", res.AdSetID)
	}
	if res.AdCount != 0 {
		t.Errorf("partial result AdCount = %d, want 0", res.AdCount)
	}
}

// TestCreateCampaignNoIDReturnsPartial verifies a 2xx campaign create with no id
// returns a partial result carrying the campaign name (reconcilable by name), not
// a bare (nil, err).
func TestCreateCampaignNoIDReturnsPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{}`) // 2xx, no id
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatal("expected an error for a campaign with no id")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a partial result with the campaign name, got %+v", res)
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty", res.CampaignID)
	}
}

// TestCreateCampaignAmbiguousCampaignCreateReturnsPartial verifies that a 5xx on
// the campaign POST (an AMBIGUOUS outcome — Meta may have committed the create)
// yields a NON-NIL partial result carrying the campaign name and an UNCONFIRMED
// step, rather than (nil, err) that would discard the reconcilable name.
func TestCreateCampaignAmbiguousCampaignCreateReturnsPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			// 5xx: an ambiguous failure that may have committed the create.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"internal","type":"OAuthException","code":1}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatal("expected an error for an ambiguous 5xx campaign create")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a non-nil partial result carrying the campaign name, got %+v", res)
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty (id was never read)", res.CampaignID)
	}
	if !anyStepContains(res.Steps, "UNCONFIRMED") {
		t.Errorf("expected an UNCONFIRMED step, got %v", res.Steps)
	}
}

// TestCreateCampaignAmbiguousTransportReturnsPartial verifies that a transport
// timeout on the campaign POST (also AMBIGUOUS) yields a partial result with the
// campaign name and an UNCONFIRMED step.
func TestCreateCampaignAmbiguousTransportReturnsPartial(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/campaigns") {
			// A mid-flight timeout (not a pre-send dial error) is ambiguous.
			return nil, &url.Error{
				Op:  "Post",
				URL: req.URL.String(),
				Err: fmt.Errorf("net/http: request canceled (Client.Timeout exceeded while awaiting headers): %w", context.DeadlineExceeded),
			}
		}
		body := `{"name":"x","currency":"USD"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatal("expected an error for an ambiguous transport timeout on campaign create")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a non-nil partial result carrying the campaign name, got %+v", res)
	}
	if !anyStepContains(res.Steps, "UNCONFIRMED") {
		t.Errorf("expected an UNCONFIRMED step, got %v", res.Steps)
	}
}

// TestCreateCampaign4xxCampaignCreateReturnsNoPartial verifies that a clear 4xx
// on the campaign POST (nothing was created) still returns (nil, err) with no
// partial result — the non-ambiguous path is unchanged.
func TestCreateCampaign4xxCampaignCreateReturnsNoPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad name","type":"OAuthException","code":100}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatal("expected an error for a 4xx campaign create")
	}
	if res != nil {
		t.Errorf("expected NO partial result for a definite 4xx (nothing created), got %+v", res)
	}
}

// TestDisplayMetaUTMURLStripsSecretsAndFragment verifies displayMetaUTMURL drops
// a caller ?token=... query, any fragment, and any userinfo — keeping only the
// generated utm_* params. The secret is composed at RUNTIME so a literal never
// appears in the test source (secretlint/gitleaks).
func TestDisplayMetaUTMURLStripsSecretsAndFragment(t *testing.T) {
	secret := "s3cr" + "et-" + "abc123" // composed at runtime, not a literal
	in := CampaignInput{
		EventName:       "KubeCon",
		RegistrationURL: "https://events.example.org/register?token=" + secret + "&ref=x#section-tickets",
	}
	got := displayMetaUTMURL(in, 0)
	if strings.Contains(got, secret) {
		t.Errorf("display URL %q leaked the secret token", got)
	}
	if strings.Contains(got, "ref=x") {
		t.Errorf("display URL %q kept a caller query param; only utm_* should survive", got)
	}
	if strings.Contains(got, "#") || strings.Contains(got, "section-tickets") {
		t.Errorf("display URL %q kept the fragment", got)
	}
	if !strings.Contains(got, "utm_source=meta") || !strings.Contains(got, "utm_content=variant-1") {
		t.Errorf("display URL %q missing the generated utm_* params", got)
	}
	// The REAL click URL (buildUTMURL) must still carry the caller's full query.
	real := buildUTMURL(in, 0)
	if !strings.Contains(real, secret) {
		t.Errorf("real click URL %q must preserve the caller's token; it is the ad destination", real)
	}

	// Userinfo is stripped even when present (displayMetaUTMURL is defensive; the
	// URL is composed at runtime so no literal user:pass@ appears in source).
	inUser := CampaignInput{EventName: "E", RegistrationURL: urlWithUserinfo("https", "u", "p", "events.example.org/register?token="+secret)}
	gotUser := displayMetaUTMURL(inUser, 0)
	if strings.Contains(gotUser, "u:p@") || strings.Contains(gotUser, secret) {
		t.Errorf("display URL %q leaked userinfo or secret", gotUser)
	}
}

// TestCreateCampaignSuccessStepsHideSecret verifies a fully-successful
// CreateCampaign records the SANITIZED display URL in Steps, so a caller
// ?token=... secret never reaches the (persisted/logged) Steps.
func TestCreateCampaignSuccessStepsHideSecret(t *testing.T) {
	secret := "s3cr" + "et-" + "xyz789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"cr_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "KubeCon", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://events.example.org/register?token=" + secret,
		GeoTargets:      []string{"US"},
		Budget:          10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.AdCount != 1 {
		t.Fatalf("ad count = %d, want 1", res.AdCount)
	}
	for _, s := range res.Steps {
		if strings.Contains(s, secret) {
			t.Errorf("Steps leaked the caller secret: %q", s)
		}
	}
}

// TestCreateCampaignAllGeosInvalidNamesThem verifies the all-invalid-geo error
// names the dropped codes (the discarded "Geo targets dropped" step otherwise
// hides them).
func TestCreateCampaignAllGeosInvalidNamesThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("no POST should happen when all geos are invalid: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"IR", "ZZ"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable geo targets") {
		t.Fatalf("err = %v, want 'no usable geo targets'", err)
	}
	if !strings.Contains(err.Error(), "IR") || !strings.Contains(err.Error(), "ZZ") {
		t.Errorf("error should name the dropped geos IR and ZZ, got: %v", err)
	}
}

// TestCreateCampaignWhitespaceOnlyGeosDefaultToUS verifies that geo targets
// consisting solely of blank entries are treated like no geos (a legitimate US
// default), NOT as an explicit request that errors with an empty "(dropped: )".
func TestCreateCampaignWhitespaceOnlyGeosDefaultToUS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"   ", ""},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("whitespace-only geos should default to US, got err: %v", err)
	}
	if res == nil || res.CampaignID == "" {
		t.Fatalf("expected a created campaign, got %+v", res)
	}
	for _, s := range res.Steps {
		if strings.Contains(s, "dropped") {
			t.Errorf("no 'dropped' step expected for blank-only geos, got: %q", s)
		}
	}
}

// TestCreateCampaignPartialVariantErrorsWithIndex verifies a variant missing
// primary text or headline is rejected by NAME (1-based index) rather than
// silently dropped, which would renumber the surviving variants.
func TestCreateCampaignPartialVariantErrorsWithIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("no POST should happen when a variant is invalid: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{
			{PrimaryText: "p", Headline: "h"},
			{PrimaryText: "p2", Headline: ""}, // second variant missing headline
		},
	})
	if err == nil || !strings.Contains(err.Error(), "variant 2") {
		t.Fatalf("err = %v, want an error naming 'variant 2'", err)
	}
}

func TestCreateCampaignLifetimeBudget(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
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
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
	)
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US"},
		Budget:          500,
		LifetimeBudget:  true,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "Join us", Headline: "KubeCon 2026"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	adsetBody := adsetCap.get()
	if adsetBody["lifetime_budget"] != float64(50000) {
		t.Errorf("lifetime_budget = %v, want 50000", adsetBody["lifetime_budget"])
	}
	if _, ok := adsetBody["daily_budget"]; ok {
		t.Errorf("daily_budget should be absent when LifetimeBudget is set, got %v", adsetBody["daily_budget"])
	}
}

// TestCreateCampaignCurrencyOffset verifies budget conversion honors the ad
// account's minor-unit offset instead of a hardcoded ×100: a zero-decimal
// currency (JPY, offset 1) must NOT be multiplied by 100, and an explicit offset
// of 100 scales an account-currency amount to minor units. An UNSET (zero) offset
// is fetched from the account preflight instead (see
// TestCreateCampaignUsesPreflightCurrencyOffset), and rejected before mutation
// when the preflight cannot supply it (see
// TestCreateCampaignRejectsUnsetOffsetWhenPreflightOmitsIt); a NEGATIVE offset is
// rejected (see TestCreateCampaignRejectsNegativeCurrencyOffset).
func TestCreateCampaignCurrencyOffset(t *testing.T) {
	newSrv := func(cap *bodyCapture) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet:
				_, _ = io.WriteString(w, `{"name":"x"}`)
			case strings.HasSuffix(r.URL.Path, "/campaigns"):
				_, _ = io.WriteString(w, `{"id":"camp_1"}`)
			case strings.HasSuffix(r.URL.Path, "/adsets"):
				cap.set(decodeBody(t, r))
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
	}
	input := func(budget float64) CampaignInput {
		return CampaignInput{
			EventName:       "E",
			Project:         "tlf",
			Objective:       "traffic",
			RegistrationURL: "https://x.example.org/e",
			GeoTargets:      []string{"US"},
			Budget:          budget,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
		}
	}

	// JPY account: the budget is denominated in the ACCOUNT's currency (¥, no FX
	// done by the client). With offset 1, a ¥5000 budget stays 5000 minor units,
	// NOT 500000. A hardcoded ×100 would over-send this account 100×.
	t.Run("jpy offset 1 does not multiply by 100", func(t *testing.T) {
		cap := newBodyCapture()
		srv := newSrv(cap)
		defer srv.Close()
		c := NewClient(
			Credentials{AccessToken: "t"},
			AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 1},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()),
		)
		if _, err := c.CreateCampaign(context.Background(), input(5000)); err != nil {
			t.Fatalf("CreateCampaign error: %v", err)
		}
		if got := cap.get()["daily_budget"]; got != float64(5000) {
			t.Errorf("daily_budget = %v, want 5000 (offset 1, no ×100)", got)
		}
	})

	// Explicit offset 100: a 500 account-currency budget (e.g. $500 for a USD
	// account) becomes 50000 minor units.
	t.Run("explicit offset 100 scales x100 to account-currency amount", func(t *testing.T) {
		cap := newBodyCapture()
		srv := newSrv(cap)
		defer srv.Close()
		c := NewClient(
			Credentials{AccessToken: "t"},
			AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()),
		)
		if _, err := c.CreateCampaign(context.Background(), input(500)); err != nil {
			t.Fatalf("CreateCampaign error: %v", err)
		}
		if got := cap.get()["daily_budget"]; got != float64(50000) {
			t.Errorf("daily_budget = %v, want 50000 (offset 100)", got)
		}
	})
}

// TestCreateCampaignRejectsNegativeCurrencyOffset verifies a NEGATIVE offset is
// rejected before any mutating call (it's malformed). An unset (zero) offset is
// resolved from the account preflight — see
// TestCreateCampaignUsesPreflightCurrencyOffset.
func TestCreateCampaignRejectsNegativeCurrencyOffset(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: -1},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("err = %v, want it to reject a negative CurrencyOffset", err)
	}
}

// TestCreateCampaignRejectsUnsetOffsetWhenPreflightOmitsIt verifies that when
// CurrencyOffset is unset (0) AND the account preflight succeeds but does NOT
// return a usable currency code, CreateCampaign fails BEFORE any mutating call
// rather than guessing 100. Defaulting to 100 would silently encode a zero-decimal
// -currency (JPY/KRW/CLP) budget 100× too high, and a warning step after resource
// creation cannot prevent that budget from being activated. noPostServer returns
// {"name":"x"} — no currency field — so the offset can't be derived.
func TestCreateCampaignRejectsUnsetOffsetWhenPreflightOmitsIt(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()

	// CurrencyOffset omitted (0); preflight body carries no currency code.
	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 5, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported or missing currency code") {
		t.Fatalf("err = %v, want it to reject an undeterminable offset before mutation", err)
	}
}

// TestCreateCampaignRejectsUnsetOffsetWhenPreflightFails verifies that when
// CurrencyOffset is unset (0) AND the account preflight FAILS, CreateCampaign
// fails BEFORE any mutating call (no POST) rather than guessing 100 — the offset
// cannot be determined, so encoding a budget would be unsafe.
func TestCreateCampaignRejectsUnsetOffsetWhenPreflightFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST to %s: offset resolution should fail first", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Preflight GET fails.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"account lookup failed"}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 5, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "could not determine the account currency") {
		t.Fatalf("err = %v, want it to reject an undeterminable offset (preflight failed) before mutation", err)
	}
}

// TestCreateCampaignUsesPreflightCurrencyOffset verifies that when CurrencyOffset
// is unset (0), the offset DERIVED from the ISO currency code returned by the
// account preflight is used to encode the budget — and, crucially, a zero-decimal
// currency (JPY, offset 1) does NOT get multiplied by 100. With JPY, a ¥5000
// budget stays 5000 minor units; with USD it scales ×100.
func TestCreateCampaignUsesPreflightCurrencyOffset(t *testing.T) {
	cases := []struct {
		name      string
		currency  string
		budget    float64
		wantMinor float64
	}{
		{"jpy preflight code derives offset 1, no ×100", "JPY", 5000, 5000},
		{"usd preflight code derives offset 100, scales ×100", "USD", 500, 50000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adsetCap := newBodyCapture()
			currency := tc.currency
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet:
					// Preflight returns the account ISO currency code (NOT a
					// currency_offset field — the AdAccount node does not expose one).
					_, _ = io.WriteString(w, `{"name":"x","account_status":1,"currency":"`+currency+`"}`)
				case strings.HasSuffix(r.URL.Path, "/campaigns"):
					_, _ = io.WriteString(w, `{"id":"camp_1"}`)
				case strings.HasSuffix(r.URL.Path, "/adsets"):
					adsetCap.set(decodeBody(t, r))
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

			// CurrencyOffset intentionally omitted (0): must be derived from the
			// preflight currency code.
			c := NewClient(Credentials{AccessToken: "t"},
				AccountConfig{AccountID: "act_1", PageID: "100"},
				WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
			if _, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName: "E", Project: "tlf", Objective: "traffic",
				RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
				Budget: tc.budget, StartDate: "2026-08-01", EndDate: "2026-08-31",
				Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
			}); err != nil {
				t.Fatalf("CreateCampaign error: %v", err)
			}
			if got := adsetCap.get()["daily_budget"]; got != tc.wantMinor {
				t.Errorf("daily_budget = %v, want %v (preflight currency %s)", got, tc.wantMinor, tc.currency)
			}
		})
	}
}

// TestCreateCampaignExplicitOffsetMustMatchPreflightCurrency verifies the
// override-consistency rule: when the preflight returns a recognized currency, an
// explicit AccountConfig.CurrencyOffset that CONFLICTS with that currency's true
// offset is REJECTED before any mutation (a stale override would mis-scale the
// budget), while an override that AGREES with the preflight currency is accepted.
func TestCreateCampaignExplicitOffsetMustMatchPreflightCurrency(t *testing.T) {
	newSrv := func(currency string, adsetCap *bodyCapture, postCount *int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet:
				_, _ = io.WriteString(w, `{"name":"x","currency":"`+currency+`"}`)
			case strings.HasSuffix(r.URL.Path, "/campaigns"):
				atomic.AddInt32(postCount, 1)
				_, _ = io.WriteString(w, `{"id":"camp_1"}`)
			case strings.HasSuffix(r.URL.Path, "/adsets"):
				atomic.AddInt32(postCount, 1)
				adsetCap.set(decodeBody(t, r))
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
	}
	input := CampaignInput{
		EventName: "E", Project: "tlf", Objective: "traffic",
		RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
		Budget: 5000, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	}

	// Conflicting override (1) vs a USD account (true offset 100): rejected before
	// any POST.
	var postCount int32
	srv := newSrv("USD", newBodyCapture(), &postCount)
	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 1},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "conflicts with the account's currency") {
		t.Errorf("conflicting override: err = %v, want a currency-conflict rejection", err)
	}
	if n := atomic.LoadInt32(&postCount); n != 0 {
		t.Errorf("conflicting override made %d POSTs, want 0 (reject before mutation)", n)
	}
	srv.Close()

	// Agreeing override (100) vs a USD account: accepted, budget scaled ×100.
	adsetCap := newBodyCapture()
	var postCount2 int32
	srv2 := newSrv("USD", adsetCap, &postCount2)
	defer srv2.Close()
	c2 := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv2.URL), WithClock(fixedMetaClock()))
	if _, err := c2.CreateCampaign(context.Background(), input); err != nil {
		t.Fatalf("agreeing override: CreateCampaign error: %v", err)
	}
	if got := adsetCap.get()["daily_budget"]; got != float64(500000) {
		t.Errorf("daily_budget = %v, want 500000 (5000 × offset 100)", got)
	}
}

// TestCreateCampaignRejectsEmptyProjectBeforeAnyPost verifies that an empty or
// whitespace-only Project is rejected during pre-flight validation, before any
// mutating call. The campaign name's Project segment must be the caller-supplied
// canonical LFX slug; silently substituting a placeholder (e.g. "tlf") could
// mis-attribute a non-Linux-Foundation campaign to the wrong project.
func TestCreateCampaignRejectsEmptyProjectBeforeAnyPost(t *testing.T) {
	base := func() CampaignInput {
		return CampaignInput{
			EventName:       "E",
			Project:         "tlf",
			RegistrationURL: "https://x.example.org/e",
			GeoTargets:      []string{"US"},
			Budget:          10,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
		}
	}
	for _, project := range []string{"", "   "} {
		srv := noPostServer(t)
		c := NewClient(Credentials{AccessToken: "t"},
			AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		in := base()
		in.Project = project
		_, err := c.CreateCampaign(context.Background(), in)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "project is required") {
			t.Fatalf("project %q: err = %v, want it to mention Project is required", project, err)
		}
	}
}

// TestCreateCampaignRejectsEmptyEventNameBeforeAnyPost verifies that an empty or
// whitespace-only EventName is rejected during pre-flight validation, before any
// mutating call. EventName is the base-name segment of every generated name and
// feeds attribution; a blank value would otherwise create paid resources with an
// empty base-name segment (e.g. " - Traffic") and break attribution.
func TestCreateCampaignRejectsEmptyEventNameBeforeAnyPost(t *testing.T) {
	base := func() CampaignInput {
		return CampaignInput{
			EventName:       "E",
			Project:         "tlf",
			RegistrationURL: "https://x.example.org/e",
			GeoTargets:      []string{"US"},
			Budget:          10,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
		}
	}
	for _, eventName := range []string{"", "   ", "\t\n"} {
		srv := noPostServer(t)
		c := NewClient(Credentials{AccessToken: "t"},
			AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		in := base()
		in.EventName = eventName
		_, err := c.CreateCampaign(context.Background(), in)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "event name is required") {
			t.Fatalf("event name %q: err = %v, want it to mention event name is required", eventName, err)
		}
	}
}

func TestCreateCampaignSkipsRegulatedGeos(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"a1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"cr1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad1"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US", "SG", "KR"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	adsetBody := adsetCap.get()
	geo := adsetBody["targeting"].(map[string]any)["geo_locations"].(map[string]any)["countries"].([]any)
	if len(geo) != 1 || geo[0] != "US" {
		t.Errorf("geo countries = %v, want [US]", geo)
	}
	if !anyStepContains(res.Steps, "Geo targets skipped") {
		t.Errorf("expected a skipped-geo step, got %v", res.Steps)
	}
}

// TestCreateCampaignReportsDroppedIneligibleGeos verifies that when eligible and
// ineligible/sanctioned geos are mixed (US + IR), the dropped ineligible code is
// surfaced in a step — a caller must not silently believe a sanctioned country is
// being targeted.
func TestCreateCampaignReportsDroppedIneligibleGeos(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"a1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"cr1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad1"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US", "IR"}, // IR is sanctioned/ineligible
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	geo := adsetCap.get()["targeting"].(map[string]any)["geo_locations"].(map[string]any)["countries"].([]any)
	if len(geo) != 1 || geo[0] != "US" {
		t.Errorf("geo countries = %v, want [US] (IR dropped)", geo)
	}
	dropStep := false
	for _, s := range res.Steps {
		if strings.Contains(s, "Geo targets dropped") && strings.Contains(s, "IR") {
			dropStep = true
		}
	}
	if !dropStep {
		t.Errorf("expected a step naming the dropped ineligible geo IR, got %v", res.Steps)
	}
}

func TestCreateCampaignAllGeosRegulated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"x"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"SG", "KR"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "supply at least one eligible") {
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
	// The Graph envelope's diagnostic fields must be preserved so callers can
	// distinguish invalid-params vs auth failures and quote Meta's trace id.
	if apiErr.Type != "OAuthException" {
		t.Errorf("type = %q, want 'OAuthException'", apiErr.Type)
	}
	if apiErr.Code != 100 {
		t.Errorf("code = %d, want 100", apiErr.Code)
	}
	if apiErr.FBTraceID != "XYZ" {
		t.Errorf("fbtrace_id = %q, want 'XYZ'", apiErr.FBTraceID)
	}
	// ...and they must appear in the error string (fbtrace_id is critical for
	// Meta support tickets).
	msg := apiErr.Error()
	for _, want := range []string{"OAuthException", "code: 100", "fbtrace_id: XYZ"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error string %q missing %q", msg, want)
		}
	}
}

// TestNonGraphErrorBodySurfaces verifies that a non-2xx response whose body is
// NOT a Graph error envelope still surfaces the raw body in the error, rather
// than pointing at nonexistent server logs. A 5xx on the campaign create is
// AMBIGUOUS, so CreateCampaign wraps the *APIError in an UNCONFIRMED partial
// result; the underlying *APIError (and its raw body snippet) must still be
// reachable via errors.As.
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want an unwrappable *APIError", err)
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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

// TestCreateCampaignContextCancelDuringAdsIsFatal verifies that a cancelled
// CALLER context observed while creating a variant ad is FATAL: CreateCampaign
// must return an error rather than reporting a "successful" result after its
// context died. The decision keys off the caller ctx (ctx.Err()), not errors.Is
// on the returned error, so this test cancels the CALLER ctx directly.
func TestCreateCampaignContextCancelDuringAdsIsFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// A RoundTripper that succeeds for the account GET, campaign, and ad-set calls
	// but, on the first /adcreatives call, cancels the caller ctx and returns the
	// context.Canceled error that c.httpClient.Do surfaces mid-flight. Deterministic
	// and non-blocking (no server goroutine to drain).
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/adcreatives") {
			cancel()
			return nil, fmt.Errorf("Post %q: %w", req.URL.String(), context.Canceled)
		}
		body := `{"id":"x"}`
		switch {
		case req.Method == http.MethodGet:
			body = `{"name":"x"}`
		case strings.HasSuffix(req.URL.Path, "/campaigns"):
			body = `{"id":"camp_1"}`
		case strings.HasSuffix(req.URL.Path, "/adsets"):
			body = `{"id":"adset_1"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	defer cancel()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(ctx, CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected error after context cancellation, got success: %+v", res)
	}
	// A caller-cancel during ad creation is still fatal (error returned), but the
	// campaign + ad set already exist, so a partial result must carry their IDs for
	// cleanup/reconcile rather than being discarded.
	if res == nil {
		t.Fatal("expected a non-nil partial result carrying the created campaign/ad set IDs, got nil")
	}
	if res.CampaignID != "camp_1" {
		t.Errorf("partial result CampaignID = %q, want camp_1", res.CampaignID)
	}
	if res.AdSetID != "adset_1" {
		t.Errorf("partial result AdSetID = %q, want adset_1", res.AdSetID)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// TestCreateCampaignContextCancelAfterCreativeSurfacesOrphan verifies that when
// the caller ctx is cancelled DURING the /ads call — after the adcreative was
// already created — the fatal error names the orphaned creative id, so the
// known paid-adjacent resource isn't silently lost (the non-fatal path already
// reports it; the fatal ctx-cancel path must too).
func TestCreateCampaignContextCancelAfterCreativeSurfacesOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Succeed through the adcreative (returning a real id), then cancel the caller
	// ctx and fail the /ads call — mirroring http.Client.Do surfacing the cancel.
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/ads") {
			cancel()
			return nil, fmt.Errorf("Post %q: %w", req.URL.String(), context.Canceled)
		}
		body := `{"id":"x"}`
		switch {
		case req.Method == http.MethodGet:
			body = `{"name":"x"}`
		case strings.HasSuffix(req.URL.Path, "/campaigns"):
			body = `{"id":"camp_1"}`
		case strings.HasSuffix(req.URL.Path, "/adsets"):
			body = `{"id":"adset_1"}`
		case strings.HasSuffix(req.URL.Path, "/adcreatives"):
			body = `{"id":"creative_orphan_9"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	defer cancel()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(ctx, CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected error after context cancellation, got success: %+v", res)
	}
	// Fatal (error returned), but campaign + ad set already exist, so a partial
	// result must carry their IDs. The orphaned creative id has no CampaignResult
	// field, so it stays surfaced in the error string (asserted below).
	if res == nil {
		t.Fatal("expected a non-nil partial result carrying the created campaign/ad set IDs, got nil")
	}
	if res.CampaignID != "camp_1" {
		t.Errorf("partial result CampaignID = %q, want camp_1", res.CampaignID)
	}
	if res.AdSetID != "adset_1" {
		t.Errorf("partial result AdSetID = %q, want adset_1", res.AdSetID)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "creative_orphan_9") {
		t.Errorf("err = %v, want it to name the orphaned creative id creative_orphan_9", err)
	}
}

// TestCreateCampaignPerCreativeTimeoutIsNonFatal verifies that a per-creative
// request failing with a DeadlineExceeded-like error while the CALLER context is
// still live stays NON-fatal: the campaign still returns and the failure is
// recorded as a warning step. This is the client's own http.Client.Timeout case —
// it must NOT abort the whole campaign (contrast with the caller-cancel test).
// The timeout is an AMBIGUOUS outcome (the creative MAY have been created), so
// the step is worded UNCONFIRMED rather than a definite "failed / create
// manually" that could duplicate the creative.
func TestCreateCampaignPerCreativeTimeoutIsNonFatal(t *testing.T) {
	// The caller ctx is never cancelled. The first /adcreatives call fails with a
	// url error wrapping context.DeadlineExceeded — exactly what http.Client.Timeout
	// surfaces — but ctx.Err() stays nil, so it must be treated as an ordinary
	// per-creative failure.
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/adcreatives") {
			return nil, &url.Error{
				Op:  "Post",
				URL: req.URL.String(),
				Err: fmt.Errorf("net/http: request canceled (Client.Timeout exceeded while awaiting headers): %w", context.DeadlineExceeded),
			}
		}
		body := `{"id":"x"}`
		switch {
		case req.Method == http.MethodGet:
			body = `{"name":"x"}`
		case strings.HasSuffix(req.URL.Path, "/campaigns"):
			body = `{"id":"camp_1"}`
		case strings.HasSuffix(req.URL.Path, "/adsets"):
			body = `{"id":"adset_1"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("per-creative timeout with a live caller ctx must be non-fatal: %v", err)
	}
	if res == nil {
		t.Fatalf("expected a campaign result, got nil")
	}
	if res.CampaignID != "camp_1" {
		t.Errorf("campaign id = %q, want camp_1", res.CampaignID)
	}
	// The creative failed, so no ad was created, but the campaign still returns.
	if res.AdCount != 0 {
		t.Errorf("ad count = %d, want 0 (the only creative timed out)", res.AdCount)
	}
	if !anyStepContains(res.Steps, "Ad/creative creation outcome UNCONFIRMED for variant 1") {
		t.Errorf("expected an UNCONFIRMED warning step for the timed-out creative, got %v", res.Steps)
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", Label: "LF Core", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
	if !anyStepContains(res.Steps, "Account preflight warning") {
		t.Errorf("expected an 'Account preflight warning' step, got %v", res.Steps)
	}
}

// TestCreateCampaignNormalizesObjective verifies that a padded / mixed-case
// Objective is trimmed and lowercased so it resolves like the canonical value
// instead of failing the objectiveParams lookup as "unknown", and that a
// whitespace-only Objective defaults to traffic.
func TestCreateCampaignNormalizesObjective(t *testing.T) {
	cases := []struct {
		name         string
		objective    string
		wantCampaign string
		wantOptGoal  string
	}{
		{"padded lowercase", "  traffic  ", "OUTCOME_TRAFFIC", "LINK_CLICKS"},
		{"mixed case", "AwArEnEsS", "OUTCOME_AWARENESS", "REACH"},
		{"whitespace only defaults to traffic", "   ", "OUTCOME_TRAFFIC", "LINK_CLICKS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			campaignCap := newBodyCapture()
			adsetCap := newBodyCapture()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet:
					_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
				case strings.HasSuffix(r.URL.Path, "/campaigns"):
					campaignCap.set(decodeBody(t, r))
					_, _ = io.WriteString(w, `{"id":"camp_1"}`)
				case strings.HasSuffix(r.URL.Path, "/adsets"):
					adsetCap.set(decodeBody(t, r))
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

			c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
				WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName: "E", Project: "tlf", Objective: tc.objective,
				RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
				Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
				Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
			})
			if err != nil {
				t.Fatalf("objective %q should normalize and succeed, got err = %v", tc.objective, err)
			}
			if got := campaignCap.get()["objective"]; got != tc.wantCampaign {
				t.Errorf("campaign objective = %v, want %v", got, tc.wantCampaign)
			}
			if got := adsetCap.get()["optimization_goal"]; got != tc.wantOptGoal {
				t.Errorf("optimization_goal = %v, want %v", got, tc.wantOptGoal)
			}
		})
	}
}

// TestCreateCampaignRejectsInactiveAccountBeforeAnyPost verifies that a successful
// preflight reporting a known-inactive account_status (e.g. 2 = disabled) fails
// BEFORE any mutating call, rather than creating a paid campaign that Meta would
// reject at a later step. account_status is fetched during the preflight, so a
// successful GET alone must not be treated as "verified/active".
func TestCreateCampaignRejectsInactiveAccountBeforeAnyPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST to %s: an inactive account must fail before mutation", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","account_status":2,"currency":"USD"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("err = %v, want inactive-account rejection before mutation", err)
	}
}

// TestCreateCampaignAcceptsAnyActiveAccountStatus verifies account_status 201
// (ANY_ACTIVE — a Meta aggregate, not a per-account inactive state) is NOT
// treated as inactive, so the campaign proceeds.
func TestCreateCampaignAcceptsAnyActiveAccountStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x","account_status":201,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"a1"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"cr1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad1"}`)
		}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	}); err != nil {
		t.Errorf("account_status 201 (ANY_ACTIVE) must be accepted, got error: %v", err)
	}
}

// TestCreateCampaignRejectsOverlongCreativeNameBeforeAnyPost verifies an
// EventName long enough to push the composed creative name ("<EventName> -
// Variant N") past Meta's 255-char cap is rejected before any POST — it must not
// create the campaign + ad set and then fail at every creative.
func TestCreateCampaignRejectsOverlongCreativeNameBeforeAnyPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("no POST should happen for an over-long creative name: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","currency":"USD"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: strings.Repeat("E", 260), Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ad-creative name") {
		t.Fatalf("err = %v, want ad-creative-name length rejection before mutation", err)
	}
}

// TestValidateRegistrationURLRejectsMalformedQuery verifies a registration URL
// whose query can't be cleanly parsed (so u.Query() would silently drop a param)
// is rejected up front.
func TestValidateRegistrationURLRejectsMalformedQuery(t *testing.T) {
	if err := validateRegistrationURL("https://x.example.org/e?a=%zz"); err == nil {
		t.Error("malformed-query registration URL should be rejected")
	}
	if err := validateRegistrationURL("https://x.example.org/e?a=b&c=d"); err != nil {
		t.Errorf("well-formed query should pass, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Input validation errors
// ---------------------------------------------------------------------------

func TestCreateCampaignValidation(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL("http://unused.invalid"), WithClock(fixedMetaClock()))
	base := CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
		{"bad budget", func(in *CampaignInput) { in.Budget = 0 }, "positive number"},
		{"sub-cent budget rounds to zero", func(in *CampaignInput) { in.Budget = 0.001 }, "budget too small"},
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          0.001,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "budget too small") {
		t.Fatalf("err = %v, want 'budget too small'", err)
	}
}

// TestCreateCampaignRejectsOverLimitCopyBeforeAnyPost verifies that a variant
// whose copy exceeds Meta's per-field character limits is rejected during
// pre-flight validation, before any mutating call — so over-limit copy fails
// fast rather than after a paid campaign/ad-set already exists (the creative
// call is non-fatal and would otherwise leave an orphaned paid campaign).
func TestCreateCampaignRejectsOverLimitCopyBeforeAnyPost(t *testing.T) {
	base := func() CampaignInput {
		return CampaignInput{
			EventName:       "E",
			Project:         "tlf",
			RegistrationURL: "https://x.example.org/e",
			GeoTargets:      []string{"US"},
			Budget:          10,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
		}
	}
	cases := []struct {
		name    string
		mutate  func(in *CampaignInput)
		wantSub string
	}{
		{"over-limit primary text", func(in *CampaignInput) {
			in.Variants[0].PrimaryText = strings.Repeat("a", maxPrimaryTextChars+1)
		}, "primary text"},
		{"over-limit headline", func(in *CampaignInput) {
			in.Variants[0].Headline = strings.Repeat("h", maxHeadlineChars+1)
		}, "headline"},
		{"over-limit description", func(in *CampaignInput) {
			in.Variants[0].Description = strings.Repeat("d", maxDescriptionChars+1)
		}, "description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := noPostServer(t)
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
			in := base()
			tc.mutate(&in)
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestCreateCampaignAtLimitCopyAllowed verifies that copy exactly at the limit
// (and multi-byte runes counted by rune, not byte) passes validation.
func TestCreateCampaignAtLimitCopyAllowed(t *testing.T) {
	// posts is written by the httptest handler goroutine and read by the test
	// goroutine below, so it must be accessed atomically to stay race-free under
	// `go test -race`.
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			atomic.AddInt32(&posts, 1)
			_, _ = io.WriteString(w, `{"id":"x"}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"x"}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	// Use multi-byte runes to prove the check counts runes, not bytes: a headline
	// of maxHeadlineChars 'é' runes is 2*maxHeadlineChars bytes but still valid.
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants: []AdVariant{{
			PrimaryText: strings.Repeat("a", maxPrimaryTextChars),
			Headline:    strings.Repeat("é", maxHeadlineChars),
			Description: strings.Repeat("d", maxDescriptionChars),
		}},
	})
	if err != nil {
		t.Fatalf("at-limit copy should be accepted, got err = %v", err)
	}
	if atomic.LoadInt32(&posts) == 0 {
		t.Errorf("expected mutating calls to proceed for at-limit copy")
	}
}

func TestCreateCampaignAllDisabledPlacementsMakesZeroPosts(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	f := false
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-13-40",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid start date") {
		t.Fatalf("err = %v, want 'invalid start date'", err)
	}
}

// TestCreateCampaignRejectsPortOnlyURLBeforeAnyPost verifies a port-only /
// empty-hostname destination URL is rejected at preflight, before any mutating
// call — so no campaign or ad set is created for an unreachable destination.
func TestCreateCampaignRejectsPortOnlyURLBeforeAnyPost(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://:443",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("err = %v, want 'not a valid URL'", err)
	}
}

// TestCreateCampaignAdSetFailureReportsOrphanCampaignID verifies that when the
// ad-set call fails AFTER the campaign was created, the returned error carries
// the created (PAUSED) campaign id so the caller can identify the orphan.
func TestCreateCampaignAdSetFailureReportsOrphanCampaignID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_orphan"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad ad set"}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected an error when the ad set fails")
	}
	if !strings.Contains(err.Error(), "camp_orphan") {
		t.Errorf("error = %q, want it to mention the orphaned campaign id camp_orphan", err.Error())
	}
	if !strings.Contains(err.Error(), "PAUSED") {
		t.Errorf("error = %q, want it to note the campaign is PAUSED", err.Error())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeBody decodes a JSON request body inside an httptest handler goroutine.
// It uses t.Errorf (not t.Fatalf): FailNow/Fatalf must only be called from the
// test goroutine, so a malformed payload records a failure and returns an empty
// map rather than trying to abort from the wrong goroutine.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Errorf("decode body: %v", err)
		return map[string]any{}
	}
	return m
}

// bodyCapture provides a race-free handoff of a request body decoded inside an
// httptest handler goroutine to the test goroutine. The handler calls set(); the
// test calls get() after CreateCampaign returns. The buffered channel creates the
// happens-before edge that -race requires (the send in the handler happens-before
// the receive in the test), and buffering keeps the handler from blocking if the
// body is captured more than once.
type bodyCapture struct {
	ch chan map[string]any
}

func newBodyCapture() *bodyCapture {
	// Buffer generously so a handler never blocks even if the endpoint is hit
	// more than once; get() drains to the most recent value.
	return &bodyCapture{ch: make(chan map[string]any, 16)}
}

func (b *bodyCapture) set(m map[string]any) { b.ch <- m }

// get returns the most recently captured body, or nil if nothing was captured.
// It must be called from the test goroutine after the request(s) have completed.
func (b *bodyCapture) get() map[string]any {
	var last map[string]any
	for {
		select {
		case m := <-b.ch:
			last = m
		default:
			return last
		}
	}
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

// TestDoRequestAbortsOnOverCapRetryAfter verifies that a server-declared
// Retry-After exceeding maxRetryWait ABORTS immediately (does not clamp and retry
// early while Meta is still throttling), issuing exactly one request.
func TestDoRequestAbortsOnOverCapRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "600") // 10 min, well over maxRetryWait
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1"},
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	var out createResponse
	err := c.doRequest(context.Background(), http.MethodPost, "/x", map[string]any{"k": "v"}, &out)
	if err == nil {
		t.Fatal("expected a rate-limit abort error on an over-cap Retry-After")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (abort, not clamp+retry)", got)
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
		// A huge delay-seconds value must be CLAMPED to just over maxRetryWait, not
		// multiplied (which overflows time.Duration NEGATIVE and would make the
		// caller retry far too early). 10_000_000_000s is well past the ~9.2e9-second
		// wrap threshold.
		{"huge delay clamps just over max", "10000000000", maxRetryWait + time.Second},
		// Exactly at the ceiling is allowed through as-is (not spuriously clamped).
		{"delay exactly at max wait", strconv.FormatInt(int64(maxRetryWait/time.Second), 10), maxRetryWait},
		// A far-future HTTP-date is clamped the same way rather than returning an
		// enormous positive duration.
		{"far-future http-date clamps just over max", fixed.Add(365 * 24 * time.Hour).UTC().Format(http.TimeFormat), maxRetryWait + time.Second},
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

// TestCreateCampaignSupportsLeadsObjective verifies the leads objective creates an
// interim website-traffic campaign: OUTCOME_TRAFFIC optimizing for LINK_CLICKS to
// the registration URL, with no promoted object and no pixel/lead form required.
func TestCreateCampaignSupportsLeadsObjective(t *testing.T) {
	campaignCap := newBodyCapture()
	adsetCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			campaignCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
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

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		Objective:       "leads",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("leads objective must be supported: %v", err)
	}
	if res.AdCount != 1 {
		t.Errorf("ad count = %d, want 1", res.AdCount)
	}
	campaignBody := campaignCap.get()
	adsetBody := adsetCap.get()
	if campaignBody["objective"] != "OUTCOME_TRAFFIC" {
		t.Errorf("campaign objective = %v, want OUTCOME_TRAFFIC", campaignBody["objective"])
	}
	if adsetBody["optimization_goal"] != "LINK_CLICKS" {
		t.Errorf("optimization_goal = %v, want LINK_CLICKS", adsetBody["optimization_goal"])
	}
	// Website-leads needs no promoted object (no lead form / no pixel).
	if _, ok := adsetBody["promoted_object"]; ok {
		t.Errorf("leads ad set unexpectedly carried a promoted_object: %v", adsetBody["promoted_object"])
	}
}

// TestValidateGeoTargetsExcludesNonTargetableTerritories verifies that ISO codes
// for uninhabited / non-targetable territories (AQ/BV/HM/TF/GS/UM) are dropped even
// though they are assigned ISO 3166-1 codes — they are not Meta ad-geolocation
// countries, so admitting them would create a campaign that then fails at the
// ad-set step.
func TestValidateGeoTargetsExcludesNonTargetableTerritories(t *testing.T) {
	got := validateGeoTargets([]string{"US", "AQ", "BV", "HM", "TF", "GS", "UM", "DE"})
	for _, bad := range []string{"AQ", "BV", "HM", "TF", "GS", "UM"} {
		if contains(got, bad) {
			t.Errorf("non-targetable territory %s leaked into %v", bad, got)
		}
	}
	if !contains(got, "US") || !contains(got, "DE") {
		t.Errorf("valid countries dropped: %v", got)
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
// envelope code is retried like a 429 and ultimately succeeds — covering both a
// classic app-level code (4) and the Marketing API account/business-use-case
// throttling code (80004), which Meta also reports over HTTP 400.
func TestDoRequestRetriesOnGraphThrottleCode(t *testing.T) {
	for _, code := range []int{4, 80004} {
		t.Run(fmt.Sprintf("code %d", code), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"message":"rate limited","code":%d}}`, code))
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
		})
	}
}

// TestCreateCampaignRejectsPastStartDate verifies a start date before today is
// rejected before any mutating call.
func TestCreateCampaignRejectsPastStartDate(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	// Pin the clock so "past" is deterministic.
	now := func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(now))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01", // before the pinned "today" of 2026-08-15
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "past") {
		t.Fatalf("err = %v, want past-start-date rejection", err)
	}
}

// TestCreateCampaignRejectsHugeBudget verifies an overflow-scale budget is
// rejected before any mutating call. There is no fixed major-unit cap anymore, so
// the offset-aware overflow guard is the one that must catch it: at offset 100 a
// 1e18 budget scales to 1e20, well past int64, and must be rejected.
func TestCreateCampaignRejectsHugeBudget(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          1e18,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds the representable minor-unit range") {
		t.Fatalf("err = %v, want offset-aware overflow rejection", err)
	}
}

// TestCreateCampaignAcceptsLargeLowValueCurrencyBudget verifies that removing the
// fixed major-unit cap lets a valid budget in a low-value, zero-decimal currency
// (VND, offset 1) through: 100,000,001 VND is only a few thousand USD-equivalent
// but exceeded the old 100M major-unit cap. With offset 1 it stays 100,000,001
// minor units — well inside int64 — so it must be ACCEPTED.
func TestCreateCampaignAcceptsLargeLowValueCurrencyBudget(t *testing.T) {
	adsetCap := newBodyCapture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			// VND account: zero-decimal, offset 1.
			_, _ = io.WriteString(w, `{"name":"x","currency":"VND"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_1"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			adsetCap.set(decodeBody(t, r))
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

	// CurrencyOffset omitted (0): derived from the VND preflight code (offset 1).
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100"}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		Objective:       "traffic",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          100_000_001,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err != nil {
		t.Fatalf("a large but sane VND budget must be accepted, got err = %v", err)
	}
	if got := adsetCap.get()["daily_budget"]; got != float64(100_000_001) {
		t.Errorf("daily_budget = %v, want 100000001 (VND offset 1, no ×100)", got)
	}
}

// TestCreateCampaignRejectsOffsetOverflowBeforeAnyPost verifies that a bogus
// large currency offset (supplied here as an explicit AccountConfig.CurrencyOffset
// override) that would push the scaled minor-unit value past int64 is rejected
// BEFORE any mutating call, rather than converting an out-of-range float to a
// wrapped int64. Note the DERIVED offset can never trigger this (it is at most
// 100), so an explicit override is the only path that can supply an overflow-scale
// offset — hence the guard is exercised via the explicit field.
func TestCreateCampaignRejectsOffsetOverflowBeforeAnyPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST to %s: overflow guard should fail first", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Preflight returns an UNRECOGNIZED currency so the override-consistency
		// check is skipped and the explicit (absurd) offset is trusted — letting the
		// overflow guard be the thing under test.
		_, _ = io.WriteString(w, `{"name":"x","currency":"ZZZ"}`)
	}))
	defer srv.Close()

	// Explicit absurd offset that overflows int64 when scaled by the budget.
	c := NewClient(Credentials{AccessToken: "t"},
		AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 1000000000000000000},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 1000, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds the representable minor-unit range") {
		t.Fatalf("err = %v, want it to reject an offset-scaled overflow before mutation", err)
	}
}

// TestValidateRegistrationURLUppercaseScheme verifies an uppercase HTTPS scheme is
// accepted: Go's url.Parse normalizes the scheme to lowercase per RFC 3986, so the
// exact "https" comparison already matches "HTTPS://...".
func TestValidateRegistrationURLUppercaseScheme(t *testing.T) {
	for _, raw := range []string{"HTTPS://events.example.org/register", "HttpS://events.example.org/x"} {
		if err := validateRegistrationURL(raw); err != nil {
			t.Errorf("validateRegistrationURL(%q) = %v, want nil (scheme is case-insensitive)", raw, err)
		}
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
// (comprehensively sanctioned CU/IR/KP, plus RU and SY excluded on Meta ads-
// eligibility grounds) are dropped even though they're valid ISO codes.
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"IR", "KP"},
		Budget:          10,
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
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"RU"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable geo targets") {
		t.Fatalf("err = %v, want Russia-only rejection at preflight (no silent US fallback)", err)
	}
}

// TestCreateCampaignAdFailureSurfacesOrphanCreative verifies that when the
// creative is created but the subsequent /ads call fails (non-fatally), the
// created creative's id is surfaced in the failure step rather than discarded,
// so the orphaned creative is visible for cleanup/reuse.
func TestCreateCampaignAdFailureSurfacesOrphanCreative(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// /ads fails with a plain (non-context) error → non-fatal per-variant path.
		if strings.HasSuffix(req.URL.Path, "/ads") {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad ad"}}`)),
				Request:    req,
			}, nil
		}
		body := `{"id":"x"}`
		switch {
		case req.Method == http.MethodGet:
			body = `{"name":"x"}`
		case strings.HasSuffix(req.URL.Path, "/campaigns"):
			body = `{"id":"camp_1"}`
		case strings.HasSuffix(req.URL.Path, "/adsets"):
			body = `{"id":"adset_1"}`
		case strings.HasSuffix(req.URL.Path, "/adcreatives"):
			body = `{"id":"creative_777"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
		WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "E",
		Project:         "tlf",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"},
		Budget:          10,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	// Ad failure is non-fatal: campaign still returns.
	if err != nil {
		t.Fatalf("ad failure should be non-fatal, got err: %v", err)
	}
	if res == nil {
		t.Fatal("expected a non-nil result on non-fatal ad failure")
	}
	var found bool
	for _, s := range res.Steps {
		if strings.Contains(s, "orphaned creative: creative_777") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a step surfacing the orphaned creative id, got steps: %v", res.Steps)
	}
}

// TestTruncate verifies the rune-aware truncate clips at max runes with an
// ellipsis (multi-byte safe) without converting the whole string to []rune.
func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 3, "hel…"},
		{"hello", 5, "hello"},
		{"hello", 10, "hello"},
		{"héllo", 2, "hé…"},
		{"日本語テスト", 3, "日本語…"},
		{"", 3, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.max); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// TestCurrencyOffsetForAuthoritativeMap verifies the supported-currency map is the
// source of truth: known two-decimal and zero-decimal codes resolve to their
// factor, while any code NOT in the map (blank, or a well-formed-but-unknown code
// like "ZZZ"/"XYZ") returns ok=false so the caller fails closed rather than
// guessing 100.
func TestCurrencyOffsetForAuthoritativeMap(t *testing.T) {
	known := map[string]int64{
		"USD": 100, "usd": 100, " EUR ": 100, "GBP": 100, "BRL": 100, "AED": 100,
		// A sampling of the broader Meta-supported two-decimal set.
		"ARS": 100, "NGN": 100, "PKR": 100, "EGP": 100,
		// Zero-decimal (offset 1) per Meta's Marketing API currency table —
		// including IDR/HUF/COP/CRC/TWD, which have minor units in general ISO
		// usage but are billed in whole units by Meta.
		"JPY": 1, "KRW": 1, "CLP": 1, "VND": 1, "XOF": 1,
		"IDR": 1, "HUF": 1, "COP": 1, "CRC": 1, "TWD": 1,
	}
	for code, want := range known {
		got, ok := currencyOffsetFor(code)
		if !ok || got != want {
			t.Errorf("currencyOffsetFor(%q) = (%d,%v), want (%d,true)", code, got, ok, want)
		}
	}
	for _, code := range []string{"", "   ", "ZZZ", "XYZ", "US", "US$", "123"} {
		if got, ok := currencyOffsetFor(code); ok {
			t.Errorf("currencyOffsetFor(%q) = (%d,true), want ok=false (not in supported-currency map)", code, got)
		}
	}
}

// TestCreateCampaignRejectsUnknownCurrencyBeforeAnyPost verifies that when the
// preflight returns a well-formed but UNSUPPORTED currency code (ZZZ) and no
// explicit CurrencyOffset override is set, CreateCampaign fails BEFORE any mutating
// call (0 POSTs) rather than treating the unknown code as a two-decimal default and
// risking a 100× over-encoded budget for a new/zero-decimal currency.
func TestCreateCampaignRejectsUnknownCurrencyBeforeAnyPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST to %s: an unknown currency must fail before mutation", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","currency":"ZZZ"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 5, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported or missing currency code") {
		t.Fatalf("err = %v, want it to reject the unknown ZZZ currency before mutation", err)
	}
}

// TestCreateCampaignRejectsUserinfoURLBeforeAnyPost verifies a RegistrationURL that
// embeds userinfo (basic-auth user[:password]@host) is rejected at preflight,
// before any mutating call — so a password can't be forwarded as the creative click
// URL or echoed in the success step.
func TestCreateCampaignRejectsUserinfoURLBeforeAnyPost(t *testing.T) {
	for _, raw := range []string{
		urlWithUserinfo("https", "user", "pass", "events.example.org/register"),
		"https://user@events.example.org/register",
	} {
		srv := noPostServer(t)
		c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName: "E", Project: "tlf", RegistrationURL: raw,
			GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
			Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
		})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "embedded credentials") {
			t.Fatalf("url %q: err = %v, want embedded-credentials rejection", raw, err)
		}
	}
}

// TestValidateRegistrationURLRejectsUserinfo verifies the helper rejects userinfo
// directly (unit-level).
func TestValidateRegistrationURLRejectsUserinfo(t *testing.T) {
	for _, raw := range []string{
		urlWithUserinfo("https", "user", "pass", "events.example.org/x"),
		"https://user@events.example.org/x",
	} {
		if err := validateRegistrationURL(raw); err == nil || !strings.Contains(err.Error(), "embedded credentials") {
			t.Errorf("validateRegistrationURL(%q) = %v, want embedded-credentials rejection", raw, err)
		}
	}
}

// TestCreateCampaignRejectsMalformedAccountIDBeforeAnyPost verifies a non-empty but
// malformed AccountID (wrong shape, or containing path delimiters / traversal) is
// rejected before any mutating call, so it can't redirect a Graph request.
func TestCreateCampaignRejectsMalformedAccountIDBeforeAnyPost(t *testing.T) {
	for _, id := range []string{"12345", "act_", "act_12/34", "act_..", "act_12?x", "act_12#y", "acct_123"} {
		srv := noPostServer(t)
		c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: id, PageID: "100", CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
			GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
			Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
		})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("AccountID %q: err = %v, want malformed-AccountID rejection", id, err)
		}
	}
}

// TestCreateCampaignRejectsMalformedPageIDBeforeAnyPost verifies a non-empty but
// non-numeric PageID is rejected before any mutating call, so a bad Page id can't
// create a campaign+ad set that then orphans when the creative fails.
func TestCreateCampaignRejectsMalformedPageIDBeforeAnyPost(t *testing.T) {
	for _, id := range []string{"PAGE99", "12a", "12/34", ".."} {
		srv := noPostServer(t)
		c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: id, CurrencyOffset: 100},
			WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
			GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
			Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
		})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "PageID") || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("PageID %q: err = %v, want malformed-PageID rejection", id, err)
		}
	}
}

// TestCreateCampaignRejectsMalformedPixelIDBeforeAnyPost verifies a non-empty but
// non-numeric PixelID (for a conversions objective that requires it) is rejected
// before any mutating call, so a bad pixel id can't create a campaign that then
// orphans when the promoted object is rejected at ad-set time.
func TestCreateCampaignRejectsMalformedPixelIDBeforeAnyPost(t *testing.T) {
	srv := noPostServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "12345", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", Objective: "conversions", PixelID: "PIX9",
		RegistrationURL: "https://x.example.org/e",
		GeoTargets:      []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "pixelID") || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want malformed-pixelID rejection before mutation", err)
	}
}

// TestCreateCampaignPreflightErrorUnwrapsToAPIError verifies that when the account
// preflight fails with a Graph 4xx AND CurrencyOffset is unset (so the failure is
// fatal at offset resolution), the returned error still unwraps to *APIError via
// errors.As — i.e. the preflight error is wrapped with %w, not flattened with %s.
func TestCreateCampaignPreflightErrorUnwrapsToAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST to %s: offset resolution should fail first", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid account","type":"OAuthException","code":100,"fbtrace_id":"ABC"}}`)
	}))
	defer srv.Close()

	// CurrencyOffset unset (0): the preflight failure becomes fatal at offset
	// resolution, and that returned error must carry the *APIError chain.
	c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100"},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName: "E", Project: "tlf", RegistrationURL: "https://x.example.org/e",
		GeoTargets: []string{"US"}, Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
		Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
	})
	if err == nil {
		t.Fatalf("expected an error when the preflight fails")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want it to unwrap to *APIError via errors.As", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("unwrapped APIError.StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.FBTraceID != "ABC" {
		t.Errorf("unwrapped APIError.FBTraceID = %q, want ABC", apiErr.FBTraceID)
	}
}

// TestBuildPlacementTargetingRejectsMessengerInbox verifies that enabling the
// Messenger Inbox placement (removed from Meta Ads in Nov 2025) is rejected,
// rather than producing a v25.0 ad-set request that fails after the campaign
// already exists.
func TestBuildPlacementTargetingRejectsMessengerInbox(t *testing.T) {
	on := true
	_, err := buildPlacementTargeting(Placement{MessengerInbox: &on})
	if err == nil {
		t.Fatal("expected an error enabling MessengerInbox, got nil")
	}
	if !strings.Contains(err.Error(), "messengerInbox") {
		t.Errorf("error = %v, want it to name messengerInbox", err)
	}
}
