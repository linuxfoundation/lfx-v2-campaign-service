// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
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
		EmployerExclusions: []string{"urn:li:company:1111"},
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

// testScheduleMs computes the canonical start/end epoch millis for the standard
// future test dates (2099-01-01 .. 2099-02-01) using the client's own clock, so
// helper tests that now take precomputed millis (instead of date strings) derive
// them the same way CreateCampaign does via validateSchedule.
func testScheduleMs(t *testing.T, c *Client) (startMs, endMs int64) {
	t.Helper()
	startMs, endMs, err := c.validateSchedule("2099-01-01", "2099-02-01")
	if err != nil {
		t.Fatalf("validateSchedule: %v", err)
	}
	return startMs, endMs
}

// ---------------------------------------------------------------------------
// Geo resolution
// ---------------------------------------------------------------------------

func TestResolveGeoTargets_KnownAndUnknown(t *testing.T) {
	got, unresolved := ResolveGeoTargets([]string{"  Japan ", "United States", "Atlantis", "GERMANY"})

	if len(got) != 3 {
		t.Fatalf("expected 3 resolved geos (unknown reported separately), got %d: %+v", len(got), got)
	}

	want := map[string]string{
		"Japan":         "urn:li:geo:101355337",
		"United States": "urn:li:geo:103644278",
		"Germany":       "urn:li:geo:101282230",
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

	// The unknown name is REPORTED (not silently dropped), preserving its original
	// input spelling.
	if len(unresolved) != 1 || unresolved[0] != "Atlantis" {
		t.Errorf("expected unresolved=[\"Atlantis\"] (original spelling), got %+v", unresolved)
	}
}

func TestResolveGeoTargets_UsaAlias(t *testing.T) {
	got, unresolved := ResolveGeoTargets([]string{"usa"})
	if len(got) != 1 || got[0].URN != "urn:li:geo:103644278" {
		t.Fatalf("usa alias should resolve to United States URN, got %+v", got)
	}
	if len(unresolved) != 0 {
		t.Errorf("expected no unresolved names, got %+v", unresolved)
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
	employers, ok := exclude["urn:li:adTargetingFacet:employers"].([]any)
	if !ok {
		t.Errorf("exclude block missing employers facet")
	} else if len(employers) != 1 || employers[0] != "urn:li:company:1111" {
		// The documented urn:li:company:<id> exclusion (docs/api-catalog.md) must
		// pass validation and flow verbatim into the employers targeting criteria.
		t.Errorf("employers exclusion = %+v, want [urn:li:company:1111]", employers)
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
	_, endMs := testScheduleMs(t, c)
	id, err := c.findOrCreateCampaignGroup(context.Background(), "123456789", "Events | KubeCon | CNCF", "2099-01-01", endMs)
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
	_, endMs := testScheduleMs(t, c)
	_, err := c.findOrCreateCampaignGroup(context.Background(), "123456789", "Events | KubeCon | CNCF", "2099-01-01", endMs)
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
// idempotency), so no duplicate is created. Each page advertises a
// metadata.nextPageToken so the cursor walk keeps going.
func TestFindByName_MatchOnLaterPage(t *testing.T) {
	// Place the match on page index 7, well past the old 5-page cap.
	const matchPage = 7

	var mu sync.Mutex
	var getCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		n := getCount
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if n-1 == matchPage {
			// The page carrying the live match; empty nextPageToken ends the walk.
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Late | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:777"}],"metadata":{"nextPageToken":""}}`)
			return
		}
		// A page of non-matching elements, advertising a further cursor page.
		_, _ = io.WriteString(w, `{"elements":[{"name":"Other","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:1"}],"metadata":{"nextPageToken":"cursor-`+strconv.Itoa(n)+`"}}`)
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
		t.Errorf("expected cursor pagination past the old 5-page cap, only made %d GETs", getCount)
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
			// A dark post must return a full post URN (used verbatim as the creative's
			// content.reference); a bare id like "300" is rejected by createDarkPost.
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"400"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		Project:          "tlf",
		RegistrationURL:  "https://x.org",
		BudgetUSD:        5000,
		LifetimeBudget:   true,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
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

func TestToMs_PastStartDateReturnsNowPlusBuffer(t *testing.T) {
	now := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(func() time.Time { return now }))
	got, err := c.toMs("2099-01-01", false)
	if err != nil {
		t.Fatalf("toMs: %v", err)
	}
	// The buffer must exceed doRequest's worst-case lookup+retry budget (~5 min),
	// so it was raised to startTimeBuffer (10 min); see Issue F.
	want := now.UnixMilli() + startTimeBuffer.Milliseconds()
	if got != want {
		t.Errorf("past start date should return now+startTimeBuffer: want %d, got %d", want, got)
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

// TestBuildUTMURL_StripsOneTrailingSlash asserts that a single trailing slash on
// the path is stripped before the UTM query is appended, mirroring the TS source
// (".../reg/" -> ".../reg?utm_..."). On slash-sensitive sites this preserves the
// intended destination.
func TestBuildUTMURL_StripsOneTrailingSlash(t *testing.T) {
	got := BuildUTMURL("https://example.com/reg/", "hs-1", "Events | KubeCon", 1)
	u, err := neturl.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %q (%v)", got, err)
	}
	if u.Path != "/reg" {
		t.Errorf("path should have one trailing slash stripped, want %q, got %q (full: %s)", "/reg", u.Path, got)
	}
	// The '/reg' must be immediately followed by '?', not by '/?'.
	if !strings.Contains(got, "/reg?") {
		t.Errorf("expected '/reg?' before the query, got: %s", got)
	}
	if strings.Contains(got, "/reg/?") {
		t.Errorf("trailing slash was NOT stripped: %s", got)
	}
}

// TestBuildUTMURL_PreservesEncodedSlash asserts that an ENCODED trailing slash
// (%2F), which is not a real path separator, is NOT corrupted or stripped when
// appending UTM params.
func TestBuildUTMURL_PreservesEncodedSlash(t *testing.T) {
	got := BuildUTMURL("https://example.com/reg%2F", "hs-1", "Events | KubeCon", 1)
	if !strings.Contains(got, "reg%2F") {
		t.Errorf("encoded slash %%2F must be preserved, got: %s", got)
	}
	if strings.Contains(got, "reg/") {
		t.Errorf("encoded slash must not be decoded to a literal '/', got: %s", got)
	}
	u, err := neturl.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %q (%v)", got, err)
	}
	if u.Query().Get("utm_source") != "linkedin" {
		t.Errorf("utm_source not appended: %s", got)
	}
}

// TestBuildUTMURL_TrailingSlashPreservesQueryAndFragment asserts that stripping
// the trailing path slash keeps any existing query params and fragment intact.
func TestBuildUTMURL_TrailingSlashPreservesQueryAndFragment(t *testing.T) {
	got := BuildUTMURL("https://example.com/reg/?ref=abc#tickets", "", "Events | KubeCon", 1)
	u, err := neturl.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %q (%v)", got, err)
	}
	if u.Path != "/reg" {
		t.Errorf("trailing slash should be stripped, want path %q, got %q (full: %s)", "/reg", u.Path, got)
	}
	q := u.Query()
	if q.Get("ref") != "abc" {
		t.Errorf("existing query param dropped: %s", got)
	}
	if q.Get("utm_source") != "linkedin" {
		t.Errorf("utm_source not merged: %s", got)
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

// TestResolveOrgID_FailsClosedOnContradictoryDuplicateAccounts verifies that two
// Accounts entries with the SAME account id but DIFFERENT orgId are rejected
// rather than silently resolving to the first, honoring the fail-closed contract.
func TestResolveOrgID_FailsClosedOnContradictoryDuplicateAccounts(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultAccountID = ""
	cfg.DefaultOrgID = ""
	cfg.Accounts = []Account{
		{AccountID: "555", OrgID: "111"},
		{AccountID: "555", OrgID: "222"},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	if _, err := c.resolveOrgID("555"); err == nil {
		t.Error("expected error for contradictory duplicate account mappings, got nil")
	}
}

// TestResolveOrgID_FailsClosedWhenAccountOrgConflictsWithDefaultOrg verifies that
// when the resolved (default) account's orgId disagrees with a configured
// DefaultOrgID, resolution fails closed rather than picking one.
func TestResolveOrgID_FailsClosedWhenAccountOrgConflictsWithDefaultOrg(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultAccountID = "555"
	cfg.DefaultOrgID = "999"
	cfg.Accounts = []Account{
		{AccountID: "555", OrgID: "111"},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	if _, err := c.resolveOrgID("555"); err == nil {
		t.Error("expected error when account orgId conflicts with defaultOrgId, got nil")
	}
}

// TestResolveOrgID_ConsistentMappingResolves verifies that a consistent mapping
// (duplicate entries that agree, and a default account whose orgId matches
// DefaultOrgID) resolves without error.
func TestResolveOrgID_ConsistentMappingResolves(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultAccountID = "555"
	cfg.DefaultOrgID = "111"
	cfg.Accounts = []Account{
		{AccountID: "555", OrgID: "111"},
		{AccountID: "555", OrgID: "111"}, // duplicate but consistent
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	org, err := c.resolveOrgID("555")
	if err != nil {
		t.Fatalf("consistent mapping should resolve, got error: %v", err)
	}
	if org != "111" {
		t.Errorf("resolved orgId = %q, want %q", org, "111")
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
	if _, err := c.doRequest(ctx, http.MethodGet, "adAccounts/1", nil, nil, nil); err == nil {
		t.Error("expected error from cancelled context")
	}
}

// ---------------------------------------------------------------------------
// 429 rate-limit retry/backoff
// ---------------------------------------------------------------------------

// TestDoRequestRetriesOn429 verifies that a 429 followed by a 200 on a SAFE
// (idempotent) method — GET — is retried and ultimately succeeds. Non-idempotent
// methods (POST) are deliberately NOT retried; see TestDoRequest_POST429NotRetried.
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
		// A GET is a search: include `elements` so the response is a valid search
		// envelope (doRequest rejects a GET whose elements field is absent).
		_, _ = io.WriteString(w, `{"id":"urn:li:x:99","elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
		WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	out, err := c.doRequest(context.Background(), http.MethodGet, "adCampaigns/1", nil, nil, nil)
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
	_, err := c.doRequest(context.Background(), http.MethodGet, "adCampaigns/1", nil, nil, nil)
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
		{"positive overflow -> capped", "99999999999999999999999999", maxRetryWait},
		{"negative overflow -> none", "-99999999999999999999999999", 0},
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
	if _, err := c.doRequest(ctx, http.MethodGet, "adCampaigns/1", nil, nil, nil); err == nil {
		t.Fatalf("expected a context error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("doRequest blocked %v; should have aborted on cancel", elapsed)
	}
}

func TestUpdateCampaignStatus_PartialUpdate(t *testing.T) {
	// Capture the request over a channel (race-safe) to assert LinkedIn's RestLi
	// PARTIAL_UPDATE shape: POST /adAccounts/{acct}/adCampaigns/{id},
	// X-Restli-Method: PARTIAL_UPDATE, body {"patch":{"$set":{"status":...}}}.
	type req struct{ method, path, restliMethod, status string }
	gotCh := make(chan req, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Patch struct {
				Set struct {
					Status string `json:"status"`
				} `json:"$set"`
			} `json:"patch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		gotCh <- req{r.Method, r.URL.Path, r.Header.Get("X-Restli-Method"), body.Patch.Set.Status}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignStatus(context.Background(), "555", StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignStatus: %v", err)
	}
	got := <-gotCh
	if got.method != http.MethodPost || got.path != "/adAccounts/123456789/adCampaigns/555" {
		t.Errorf("request = %s %s, want POST /adAccounts/123456789/adCampaigns/555", got.method, got.path)
	}
	if got.restliMethod != "PARTIAL_UPDATE" {
		t.Errorf("X-Restli-Method = %q, want PARTIAL_UPDATE", got.restliMethod)
	}
	if got.status != StatusPaused {
		t.Errorf("patch.$set.status = %q, want %q", got.status, StatusPaused)
	}
}

// TestUpdateCampaignAndCreativesStatus_CascadesAndTolerates verifies the campaign is updated,
// the creatives are discovered via the FINDER and each PARTIAL_UPDATEd to intendedStatus, and
// (on a PAUSE) a definite 400 on an in-review creative is tolerated rather than failing the toggle.
func TestUpdateCampaignAndCreativesStatus_CascadesAndTolerates(t *testing.T) {
	type req struct{ method, path, restli, status string }
	gotCh := make(chan req, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			gotCh <- req{r.Method, r.URL.Path, r.Header.Get("X-Restli-Method"), ""}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"},{"id":"urn:li:sponsoredCreative:901"}],"metadata":{}}`)
			return
		}
		var body struct {
			Patch struct {
				Set struct {
					Status         string `json:"status"`
					IntendedStatus string `json:"intendedStatus"`
				} `json:"$set"`
			} `json:"patch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st := body.Patch.Set.Status
		if st == "" {
			st = body.Patch.Set.IntendedStatus
		}
		// Creative 901 is "in review": a PAUSE on it returns 400 (LinkedIn's documented rule).
		if strings.HasSuffix(r.URL.Path, "sponsoredCreative:901") && st == StatusPaused {
			w.WriteHeader(http.StatusBadRequest)
			gotCh <- req{r.Method, r.URL.Path, r.Header.Get("X-Restli-Method"), st}
			return
		}
		w.WriteHeader(http.StatusOK)
		gotCh <- req{r.Method, r.URL.Path, r.Header.Get("X-Restli-Method"), st}
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	// Pause: the in-review 400 on creative 901 is tolerated → overall success.
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignAndCreativesStatus (pause) should tolerate an in-review 400: %v", err)
	}
	close(gotCh)
	var campaign, finder, creatives int
	for r := range gotCh {
		switch {
		case r.method == http.MethodGet:
			finder++
		case strings.Contains(r.path, "/adCampaigns/555"):
			campaign++
		case strings.Contains(r.path, "/creatives/"):
			creatives++
		}
	}
	if campaign != 1 || finder != 1 || creatives != 2 {
		t.Errorf("requests: campaign=%d finder=%d creatives=%d, want 1/1/2", campaign, finder, creatives)
	}
}

func TestUpdateCampaignStatus_ValidatesInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no API call should happen for invalid input: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	cases := map[string]struct{ id, status string }{
		"empty id":    {"", StatusPaused},
		"non-numeric": {"urn:li:x", StatusActive},
		"bad status":  {"555", "DRAFT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.UpdateCampaignStatus(context.Background(), tc.id, tc.status); err == nil {
				t.Errorf("%s: expected a validation error", name)
			}
		})
	}
}

// TestUpdateCampaignStatus_2xxOversizedBodyIsSuccess verifies a status update (which decodes
// no body) treats a 2xx with an oversized/unreadable body as SUCCESS, not a false-unconfirmed
// transportError.
func TestUpdateCampaignStatus_2xxOversizedBodyIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("A", maxResponseBytes+100) + `"}`))
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignStatus(context.Background(), "555", StatusPaused); err != nil {
		t.Errorf("a 2xx status update with an oversized body must succeed: %v", err)
	}
}

func TestUpdateCampaignStatus_RejectsEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no API call should happen with an empty token: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	for name, tok := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			c := NewClient(Credentials{AccessToken: tok}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			if err := c.UpdateCampaignStatus(context.Background(), "555", StatusPaused); err == nil {
				t.Errorf("%s token must be rejected before sending an invalid Bearer header", name)
			}
		})
	}
}

// TestUpdateCampaignAndCreativesStatus_EncodesCreativeURNInPath verifies the creative update
// path percent-encodes the URN's colons (LinkedIn's required single-entity key form) AND that
// the encoded path passes the client's path validation (which allows %).
func TestUpdateCampaignAndCreativesStatus_EncodesCreativeURNInPath(t *testing.T) {
	// Capture the creative path over a buffered channel so the handler-goroutine write
	// happens-before the test-goroutine read (race-safe under `go test -race`).
	pathCh := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{}}`)
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "creatives/urn") {
			pathCh <- r.URL.EscapedPath()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndCreativesStatus: %v", err)
	}
	close(pathCh)
	creativePath := <-pathCh
	if !strings.Contains(creativePath, "creatives/urn%3Ali%3AsponsoredCreative%3A900") {
		t.Errorf("creative path = %q, want the %%3A-encoded URN key", creativePath)
	}
}

// TestUpdateCampaignAndCreativesStatus_TruncatedDiscoveryFails verifies a stuck/looping finder
// cursor fails the toggle (INCOMPLETE discovery) rather than silently succeeding — otherwise
// the service would persist ACTIVE while undiscovered creatives stay DRAFT.
func TestUpdateCampaignAndCreativesStatus_TruncatedDiscoveryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			// Always return the SAME non-empty cursor → a stuck cursor.
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{"nextPageToken":"STUCK"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	// PAUSE: the campaign gate is flipped before discovery, so an incomplete discovery is a
	// partial application (Unconfirmed). Either way, truncation must FAIL rather than silently
	// succeed with a partial creative set.
	err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusPaused)
	if err == nil {
		t.Fatal("expected an error when creative discovery does not terminate")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("incomplete discovery after the campaign was paused must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestUpdateCampaignAndCreativesStatus_NumericCreativeIDReconstructed verifies a finder element
// whose id is a bare NUMBER (flexibleID numeric form) is reconstructed into a sponsoredCreative
// URN and updated — not silently dropped (which would leave that creative DRAFT).
func TestUpdateCampaignAndCreativesStatus_NumericCreativeIDReconstructed(t *testing.T) {
	pathCh := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":119962155}],"metadata":{}}`) // NUMERIC id
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "creatives/urn") {
			pathCh <- r.URL.EscapedPath()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndCreativesStatus: %v", err)
	}
	close(pathCh)
	creativePath := <-pathCh
	if !strings.Contains(creativePath, "creatives/urn%3Ali%3AsponsoredCreative%3A119962155") {
		t.Errorf("numeric creative id was not reconstructed into a URN + updated; path = %q", creativePath)
	}
}

// TestUpdateCampaignAndCreativesStatus_UnusableElementIDFails verifies a finder element with no
// usable sponsoredCreative id FAILS the toggle (fail-closed) rather than being silently dropped
// (which would persist ACTIVE while that creative stays DRAFT).
func TestUpdateCampaignAndCreativesStatus_UnusableElementIDFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			// An element with an id that is neither a sponsoredCreative URN nor numeric.
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCampaign:5"}],"metadata":{}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	// Use PAUSE so the campaign gate is flipped BEFORE discovery — a discovery failure then is
	// a partial application (Unconfirmed). (The activate path's pre-mutation discovery-failure
	// is a clean failure, covered by TestLinkedIn_ToggleStatus_ActivateDiscoveryFailureIsClean.)
	err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusPaused)
	if err == nil {
		t.Fatal("expected an error when a finder element has no usable creative id")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a no-usable-id element after the campaign was paused must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestUpdateCampaignAndCreativesStatus_Non400OnPauseAborts verifies that on a PAUSE, only a 400
// (in-review) is tolerated: a 403 (or other non-400 4xx) on a creative ABORTS the toggle.
func TestUpdateCampaignAndCreativesStatus_Non400OnPauseAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{}}`)
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "creatives/urn") {
			w.WriteHeader(http.StatusForbidden) // 403 on the creative — NOT the tolerated 400
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusPaused); err == nil {
		t.Fatal("a 403 on a creative during a pause must abort, not be tolerated")
	}
}

// TestUpdateCampaignAndCreativesStatus_ActivateOrdersCreativesBeforeCampaign verifies that on
// ACTIVATE the creatives are lifted BEFORE the campaign is flipped ACTIVE — so a creative
// failure can't leave paid delivery running (the campaign, still paused, gates everything).
func TestUpdateCampaignAndCreativesStatus_ActivateOrdersCreativesBeforeCampaign(t *testing.T) {
	orderCh := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives"):
			orderCh <- "finder"
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{}}`)
			return
		case strings.Contains(p, "creatives/urn"):
			orderCh <- "creative"
		case strings.Contains(p, "adCampaigns/555"):
			orderCh <- "campaign"
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndCreativesStatus: %v", err)
	}
	close(orderCh)
	var seq []string
	for s := range orderCh {
		seq = append(seq, s)
	}
	// Expect: finder, creative(s)..., then campaign LAST.
	if len(seq) < 2 || seq[len(seq)-1] != "campaign" {
		t.Fatalf("campaign must be updated LAST on activate; sequence = %v", seq)
	}
	for _, s := range seq[:len(seq)-1] {
		if s == "campaign" {
			t.Errorf("campaign was updated before creatives on activate; sequence = %v", seq)
		}
	}
}

// TestCreativeURN_RejectsMalformedSuffix verifies a full-URN value with a path-altering or
// non-numeric suffix is rejected (returns ""), so it can't reach encodeURNForPath and alter
// the request URL.
func TestCreativeURN_RejectsMalformedSuffix(t *testing.T) {
	bad := []string{
		"urn:li:sponsoredCreative:900?x=1",
		"urn:li:sponsoredCreative:900/evil",
		"urn:li:sponsoredCreative:../../secret",
		"urn:li:sponsoredCreative:9%2f0",
		"urn:li:sponsoredCreative:abc",
	}
	for _, s := range bad {
		if got := creativeURN(responseElement{URN: s}); got != "" {
			t.Errorf("creativeURN(%q) = %q, want \"\" (malformed suffix must be rejected)", s, got)
		}
	}
	// A clean numeric id (URN or bare) is accepted and canonicalized.
	if got := creativeURN(responseElement{URN: "urn:li:sponsoredCreative:900"}); got != "urn:li:sponsoredCreative:900" {
		t.Errorf("clean URN rejected: got %q", got)
	}
	if got := creativeURN(responseElement{ID: flexibleID("900")}); got != "urn:li:sponsoredCreative:900" {
		t.Errorf("bare numeric id not canonicalized: got %q", got)
	}
}

// TestUpdateCampaignAndCreativesStatus_ActivateZeroCreativesRejected verifies activating a
// campaign with zero creatives is refused before the campaign flip (it can't serve).
func TestUpdateCampaignAndCreativesStatus_ActivateZeroCreativesRejected(t *testing.T) {
	campaignFlipped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`) // zero creatives
			return
		}
		if strings.Contains(r.URL.Path, "/adCampaigns/") {
			campaignFlipped = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusActive); err == nil {
		t.Fatal("expected an error activating a campaign with zero creatives")
	}
	if campaignFlipped {
		t.Error("campaign was flipped ACTIVE despite having no creatives")
	}
}

// TestUpdateCampaignAndCreativesStatus_FinderSendsRequiredParams asserts the creatives FINDER
// request carries q=criteria, the campaign filter (campaigns=List(...) with the campaign URN),
// and the X-RestLi-Method: FINDER header — so a malformed finder can't silently discover
// creatives outside the target campaign.
func TestUpdateCampaignAndCreativesStatus_FinderSendsRequiredParams(t *testing.T) {
	type finderReq struct{ query, restli string }
	ch := make(chan finderReq, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			ch <- finderReq{r.URL.RawQuery, r.Header.Get("X-Restli-Method")}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "555", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndCreativesStatus: %v", err)
	}
	close(ch)
	got := <-ch
	if got.restli != "FINDER" {
		t.Errorf("X-Restli-Method = %q, want FINDER", got.restli)
	}
	if !strings.Contains(got.query, "q=criteria") {
		t.Errorf("finder query %q missing q=criteria", got.query)
	}
	// The campaign filter must scope to THIS campaign's URN (percent-encoded in the query).
	if !strings.Contains(got.query, "campaigns=") || !strings.Contains(got.query, "555") {
		t.Errorf("finder query %q missing campaigns=List(...campaign 555...) filter", got.query)
	}
}
