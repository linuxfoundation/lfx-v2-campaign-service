// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig() RuntimeConfig {
	return RuntimeConfig{
		DefaultAccountID: "123456789",
		DefaultOrgID:     "987654321",
		Accounts: []Account{
			{AccountID: "123456789", Label: "LF Events", OrgID: "987654321", Status: "ACTIVE"},
		},
		EmployerExclusions: []string{"urn:li:organization:1111"},
		TargetingProfiles: []TargetingProfileConfig{
			{
				ID:     "cloud-native",
				Label:  "Cloud Native",
				Skills: []string{"urn:li:skill:1", "urn:li:skill:2"},
				Groups: []string{"urn:li:group:100"},
			},
		},
	}
}

// fixedClock returns a clock far in the past so future-dated campaigns pass.
func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
}

// ---------------------------------------------------------------------------
// Geo resolution
// ---------------------------------------------------------------------------

func TestResolveGeoTargets_KnownAndUnknown(t *testing.T) {
	got := ResolveGeoTargets([]string{"  Japan ", "United States", "Atlantis", "GERMANY"})

	if len(got) != 3 {
		t.Fatalf("expected 3 resolved geos (unknown dropped), got %d: %+v", len(got), got)
	}

	want := map[string]string{
		"Japan":         "urn:li:geo:101355337",
		"United States": "urn:li:geo:103644278",
		"Germany":       "urn:li:geo:101165590",
	}
	for _, g := range got {
		wantURN, ok := want[g.Label]
		if !ok {
			t.Errorf("unexpected geo in result: %+v", g)
			continue
		}
		if g.URN != wantURN {
			t.Errorf("geo %s: want URN %s, got %s", g.Label, wantURN, g.URN)
		}
	}
}

func TestResolveGeoTargets_UsaAlias(t *testing.T) {
	got := ResolveGeoTargets([]string{"usa"})
	if len(got) != 1 || got[0].URN != "urn:li:geo:103644278" {
		t.Fatalf("usa alias should resolve to United States URN, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Targeting criteria building
// ---------------------------------------------------------------------------

func TestBuildTargetingCriteria_SkillsAndGroupsInOneOrBlock(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig())
	crit, err := c.buildTargetingCriteria("cloud-native", []string{"urn:li:geo:1"})
	if err != nil {
		t.Fatalf("buildTargetingCriteria: %v", err)
	}

	// Round-trip through JSON to inspect structure the way LinkedIn receives it.
	b, _ := json.Marshal(crit)
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}

	tc := decoded["targetingCriteria"].(map[string]any)
	include := tc["include"].(map[string]any)
	and := include["and"].([]any)
	if len(and) != 2 {
		t.Fatalf("expected 2 entries in include.and, got %d", len(and))
	}

	// First and-entry: geo locations.
	first := and[0].(map[string]any)["or"].(map[string]any)
	if _, ok := first["urn:li:adTargetingFacet:locations"]; !ok {
		t.Errorf("first and-entry should carry locations facet: %+v", first)
	}

	// Second and-entry: skills + groups + jobFunctions all in ONE or block.
	second := and[1].(map[string]any)["or"].(map[string]any)
	for _, facet := range []string{
		"urn:li:adTargetingFacet:skills",
		"urn:li:adTargetingFacet:groups",
		"urn:li:adTargetingFacet:jobFunctions",
	} {
		if _, ok := second[facet]; !ok {
			t.Errorf("skills/groups OR block missing facet %s: %+v", facet, second)
		}
	}

	skills := second["urn:li:adTargetingFacet:skills"].([]any)
	if len(skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(skills))
	}

	// Exclusions: employers + seniorities.
	exclude := tc["exclude"].(map[string]any)["or"].(map[string]any)
	if _, ok := exclude["urn:li:adTargetingFacet:employers"]; !ok {
		t.Errorf("exclude block missing employers facet")
	}
	if _, ok := exclude["urn:li:adTargetingFacet:seniorities"]; !ok {
		t.Errorf("exclude block missing seniorities facet")
	}
}

func TestBuildTargetingCriteria_CustomAliasesCloudNative(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig())
	crit, err := c.buildTargetingCriteria("custom", []string{"urn:li:geo:1"})
	if err != nil {
		t.Fatalf("custom should not error: %v", err)
	}
	b, _ := json.Marshal(crit)
	if !strings.Contains(string(b), "urn:li:skill:1") {
		t.Errorf("custom should fall back to cloud-native skills, got %s", b)
	}
}

func TestBuildTargetingCriteria_UnknownProfileErrors(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig())
	if _, err := c.buildTargetingCriteria("nonexistent", nil); err == nil {
		t.Fatal("expected error for unknown targeting profile")
	}
}

// ---------------------------------------------------------------------------
// Idempotency: search-by-name returns existing ID -> no duplicate create
// ---------------------------------------------------------------------------

func TestFindOrCreateCampaignGroup_Idempotent(t *testing.T) {
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:555"}]}`)
			return
		}
		if r.Method == http.MethodPost {
			postCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"999"}`)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findOrCreateCampaignGroup(context.Background(), "123456789", "Events | KubeCon | CNCF", "2099-01-01", "2099-02-01")
	if err != nil {
		t.Fatalf("FindOrCreateCampaignGroup: %v", err)
	}
	if id != "555" {
		t.Errorf("expected existing group id 555, got %s", id)
	}
	if postCount != 0 {
		t.Errorf("expected no POST (idempotent hit), got %d POSTs", postCount)
	}
}

// TestFindOrCreateCampaignGroup_TransientSearchErrorNoCreate verifies that a
// transient 500 during the name search propagates as an error and does NOT lead
// to a create POST (which would risk a duplicate campaign group).
func TestFindOrCreateCampaignGroup_TransientSearchErrorNoCreate(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			// Transient server error during search.
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPost {
			postCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"999"}`)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.findOrCreateCampaignGroup(context.Background(), "123456789", "Events | KubeCon | CNCF", "2099-01-01", "2099-02-01")
	if err == nil {
		t.Fatal("expected error from transient 500 during search, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("no create POST should be issued when search fails transiently, got %d POSTs", postCount)
	}
}

// TestFindByName_MatchOnLaterPage verifies that a same-name resource that only
// appears beyond the old fixed 5-page cap is still found (name-based
// idempotency), so no duplicate is created. Each full page advertises a "next"
// link so the client keeps paginating.
func TestFindByName_MatchOnLaterPage(t *testing.T) {
	const pageSize = 50
	// Place the match on page index 7 (start=350), well past the old 5-page cap.
	const matchStart = 7 * pageSize

	var mu sync.Mutex
	var getCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		start, _ := strconv.Atoi(r.URL.Query().Get("start"))

		if start == matchStart {
			// The page carrying the live match.
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Late | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:777"}],"paging":{"links":[]}}`)
			return
		}
		// A full page of non-matching elements, advertising a next page.
		var sb strings.Builder
		sb.WriteString(`{"elements":[`)
		for i := 0; i < pageSize; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"name":"Other","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:1"}`)
		}
		sb.WriteString(`],"paging":{"links":[{"rel":"next","href":"?start="}]}}`)
		_, _ = io.WriteString(w, sb.String())
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Late | CNCF")
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if id != "777" {
		t.Errorf("expected match on later page (id 777), got %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCount <= 5 {
		t.Errorf("expected pagination past the old 5-page cap, only made %d GETs", getCount)
	}
}

// ---------------------------------------------------------------------------
// Campaign-creation happy path (full hierarchy) + dark-post assertion
// ---------------------------------------------------------------------------

func TestCreateCampaign_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var darkPostBody map[string]any
	var campaignBody map[string]any
	var creativePosts int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		// All search GETs return empty -> forces create path.
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)

		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			// Verify group is ACTIVE.
			if body["status"] != "ACTIVE" {
				t.Errorf("campaign group must be ACTIVE, got %v", body["status"])
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			campaignBody = body
			// x-restli-id header path (no body id).
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			darkPostBody = body
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			creativePosts++
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	in := CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		HSToken:          "hs-123",
		BudgetUSD:        100,
		LifetimeBudget:   false,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants: []CreativeVariant{
			{IntroText: "Join us — it's great", Headline: "KubeCon 2099"},
		},
		Project: "CNCF",
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// Parsed IDs (trailing segment of URN).
	if res.CampaignGroupID != "100" {
		t.Errorf("group id: want 100, got %s", res.CampaignGroupID)
	}
	if res.CampaignID != "200" {
		t.Errorf("campaign id (from x-restli-id): want 200, got %s", res.CampaignID)
	}
	if res.CreativeCount != 1 {
		t.Errorf("creative count: want 1, got %d", res.CreativeCount)
	}
	if creativePosts != 1 {
		t.Errorf("expected 1 creative POST, got %d", creativePosts)
	}
	if res.Platform != "linkedin-ads" {
		t.Errorf("platform: want linkedin-ads, got %s", res.Platform)
	}
	if !strings.Contains(res.LinkedInURL, "/campaigns/200") {
		t.Errorf("linkedin url missing campaign id: %s", res.LinkedInURL)
	}

	// Dark-post assertion: feedDistribution NONE.
	if darkPostBody == nil {
		t.Fatal("dark post was never created")
	}
	dist, ok := darkPostBody["distribution"].(map[string]any)
	if !ok {
		t.Fatalf("dark post missing distribution block: %+v", darkPostBody)
	}
	if dist["feedDistribution"] != "NONE" {
		t.Errorf("dark post feedDistribution: want NONE, got %v", dist["feedDistribution"])
	}
	// callToAction must NOT be present (article ads).
	if _, present := darkPostBody["callToAction"]; present {
		t.Errorf("dark post must not send callToAction for article ads")
	}
	// content.article present.
	content, _ := darkPostBody["content"].(map[string]any)
	if content == nil || content["article"] == nil {
		t.Errorf("dark post should carry content.article: %+v", darkPostBody)
	}
	// em-dash in intro should be normalized to a comma.
	if strings.Contains(darkPostBody["commentary"].(string), "—") {
		t.Errorf("commentary should have dashes stripped, got %q", darkPostBody["commentary"])
	}

	// Campaign: budget as decimal string, status PAUSED, dailyBudget field.
	if campaignBody["status"] != "PAUSED" {
		t.Errorf("campaign status: want PAUSED, got %v", campaignBody["status"])
	}
	daily, ok := campaignBody["dailyBudget"].(map[string]any)
	if !ok {
		t.Fatalf("campaign missing dailyBudget: %+v", campaignBody)
	}
	if daily["amount"] != "100.00" {
		t.Errorf("budget amount should be decimal string 100.00, got %v", daily["amount"])
	}
	if _, hasTotal := campaignBody["totalBudget"]; hasTotal {
		t.Errorf("daily-budget campaign must not send totalBudget")
	}
}

func TestCreateCampaign_LifetimeBudgetUsesTotalBudget(t *testing.T) {
	var mu sync.Mutex
	var campaignBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			campaignBody = body
			_, _ = io.WriteString(w, `{"id":"200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"400"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		RegistrationURL:  "https://x.org",
		BudgetUSD:        5000,
		LifetimeBudget:   true,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	total, ok := campaignBody["totalBudget"].(map[string]any)
	if !ok {
		t.Fatalf("lifetime campaign should send totalBudget: %+v", campaignBody)
	}
	if total["amount"] != "5000.00" {
		t.Errorf("totalBudget amount: want 5000.00, got %v", total["amount"])
	}
}

// ---------------------------------------------------------------------------
// Unit helpers
// ---------------------------------------------------------------------------

func TestToMs_PastEndDateErrors(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(func() time.Time {
		return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	}))
	if _, err := c.toMs("2099-01-01", true); err == nil {
		t.Error("expected error for past end date")
	}
}

func TestToMs_PastStartDateReturnsNowPlus5Min(t *testing.T) {
	now := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(func() time.Time { return now }))
	got, err := c.toMs("2099-01-01", false)
	if err != nil {
		t.Fatalf("toMs: %v", err)
	}
	want := now.UnixMilli() + 5*60*1000
	if got != want {
		t.Errorf("past start date should return now+5min: want %d, got %d", want, got)
	}
}

func TestBuildUTMURL(t *testing.T) {
	got := BuildUTMURL("https://x.org/reg", "hs-1", "Events | KubeCon | LinkedIn", 2)
	if !strings.Contains(got, "utm_source=linkedin") {
		t.Errorf("missing utm_source: %s", got)
	}
	if !strings.Contains(got, "utm_campaign=hs-1") {
		t.Errorf("missing utm_campaign: %s", got)
	}
	if !strings.Contains(got, "utm_content=variant-2") {
		t.Errorf("missing utm_content: %s", got)
	}
	if !strings.Contains(got, "utm_term=events_kubecon_linkedin") {
		t.Errorf("term normalization wrong: %s", got)
	}
}

// TestBuildUTMURL_PreservesFragment asserts UTM params land in the query BEFORE
// the "#fragment", not concatenated onto the end of the fragment (where browsers
// would drop them).
func TestBuildUTMURL_PreservesFragment(t *testing.T) {
	got := BuildUTMURL("https://x.org/reg#tickets", "hs-1", "Events | KubeCon", 1)

	u, err := neturl.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %q (%v)", got, err)
	}
	if u.Fragment != "tickets" {
		t.Errorf("fragment should be preserved as %q, got %q (full: %s)", "tickets", u.Fragment, got)
	}
	q := u.Query()
	if q.Get("utm_source") != "linkedin" {
		t.Errorf("utm_source not in query: %s", got)
	}
	if q.Get("utm_content") != "variant-1" {
		t.Errorf("utm_content not in query: %s", got)
	}
	// The utm params must appear before the '#', not inside the fragment.
	hashIdx := strings.Index(got, "#")
	utmIdx := strings.Index(got, "utm_source")
	if hashIdx < 0 || utmIdx < 0 || utmIdx > hashIdx {
		t.Errorf("utm params must precede the fragment: %s", got)
	}
}

// TestBuildUTMURL_PreservesExistingQuery asserts existing query params survive
// alongside the appended UTM params, with the fragment kept at the end.
func TestBuildUTMURL_PreservesExistingQuery(t *testing.T) {
	got := BuildUTMURL("https://x.org/reg?ref=abc#tickets", "", "Events | KubeCon", 1)
	u, err := neturl.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %q (%v)", got, err)
	}
	q := u.Query()
	if q.Get("ref") != "abc" {
		t.Errorf("existing query param dropped: %s", got)
	}
	if q.Get("utm_source") != "linkedin" {
		t.Errorf("utm_source not merged into query: %s", got)
	}
	if u.Fragment != "tickets" {
		t.Errorf("fragment should be preserved, got %q (full: %s)", u.Fragment, got)
	}
}

func TestResolveOrgID_CrossTenantRefusal(t *testing.T) {
	cfg := testConfig()
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	// Unknown account that is not the default -> must refuse.
	if _, err := c.resolveOrgID("000000"); err == nil {
		t.Error("expected refusal for unknown non-default account")
	}
}

func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.doRequest(ctx, http.MethodGet, "adAccounts/1", nil, nil); err == nil {
		t.Error("expected error from cancelled context")
	}
}

// ---------------------------------------------------------------------------
// 429 rate-limit retry/backoff
// ---------------------------------------------------------------------------

// TestDoRequestRetriesOn429 verifies that a 429 followed by a 200 is retried and
// ultimately succeeds.
func TestDoRequestRetriesOn429(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0") // 0 -> falls back to base backoff
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"urn:li:x:99"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
		WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	out, err := c.doRequest(context.Background(), http.MethodPost, "adCampaigns", map[string]any{"k": "v"}, nil)
	if err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if out.ID != "urn:li:x:99" {
		t.Errorf("id = %q, want urn:li:x:99", out.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (one 429 + one success)", calls)
	}
}

// TestDoRequestExhaustsRetries verifies that persistent 429s return an error
// after retryMax attempts rather than looping forever.
func TestDoRequestExhaustsRetries(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
		WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	_, err := c.doRequest(context.Background(), http.MethodGet, "adCampaigns/1", nil, nil)
	if err == nil {
		t.Fatalf("expected an error after exhausting retries")
	}
	// The final attempt (attempt == retryMax) no longer retries, so it returns
	// the standard non-2xx error for the persistent 429 rather than looping.
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %q, want it to report the 429 status", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 1 initial + retryMax retries = retryMax+1 total server hits.
	if calls != retryMax+1 {
		t.Errorf("server calls = %d, want %d", calls, retryMax+1)
	}
}

// TestParseRetryAfter covers the header parsing paths: delay-seconds, HTTP-date,
// and absent/invalid headers.
func TestParseRetryAfter(t *testing.T) {
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
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

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := c.doRequest(ctx, http.MethodGet, "adCampaigns/1", nil, nil); err == nil {
		t.Fatalf("expected a context error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("doRequest blocked %v; should have aborted on cancel", elapsed)
	}
}
