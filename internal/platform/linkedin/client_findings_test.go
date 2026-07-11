// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Second-round Copilot findings
// ---------------------------------------------------------------------------

// TestFindByName_NumericID verifies that a search response returning `id` as a
// JSON number (a long, the real LinkedIn shape) is decoded without failure and
// findByName returns its string form. Before flexibleID this failed
// json.Unmarshal outright.
func TestFindByName_NumericID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// id is an unquoted JSON number, as LinkedIn actually returns it.
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Numeric | CNCF","status":"ACTIVE","id":12345}]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Numeric | CNCF")
	if err != nil {
		t.Fatalf("findByName with numeric id: %v", err)
	}
	if id != "12345" {
		t.Errorf("expected numeric id normalized to \"12345\", got %q", id)
	}
}

// TestFindByName_CursorPagination verifies that findByName follows LinkedIn's
// 202602 CURSOR pagination: page 1 returns metadata.nextPageToken and no match,
// the client re-requests carrying that token as the `pageToken` param, and the
// match on page 2 (with an empty nextPageToken) is found.
func TestFindByName_CursorPagination(t *testing.T) {
	const wantToken = "cursor-abc-123"

	var mu sync.Mutex
	var getCount int
	var sawPageToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		n := getCount
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			sawPageToken = tok
		}
		mu.Unlock()

		// Every request must use cursor pagination: page size is carried by
		// `pageSize`, never by the legacy offset param `count`, and no `start`
		// offset is ever sent.
		if r.URL.Query().Get("pageSize") == "" {
			t.Errorf("cursor pagination must send `pageSize`, got none (query=%q)", r.URL.RawQuery)
		}
		if got := r.URL.Query().Get("count"); got != "" {
			t.Errorf("cursor pagination must not send legacy offset `count`, got %q", got)
		}
		if got := r.URL.Query().Get("start"); got != "" {
			t.Errorf("cursor pagination must not send offset `start`, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Page 1: no match, but a nextPageToken advertising a further page.
			_, _ = io.WriteString(w, `{"elements":[{"name":"Other","status":"ACTIVE","id":1}],"metadata":{"nextPageToken":"`+wantToken+`"}}`)
			return
		}
		// Page 2: the match, with an empty nextPageToken (end of results).
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Cursor | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:888"}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaigns", "Events | Cursor | CNCF")
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if id != "888" {
		t.Errorf("expected cursor-page-2 match id 888, got %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCount != 2 {
		t.Errorf("expected exactly 2 GETs (page 1 + cursor page 2), got %d", getCount)
	}
	if sawPageToken != wantToken {
		t.Errorf("second request must carry pageToken=%q, got %q", wantToken, sawPageToken)
	}
}

// TestValidateRegistrationURL covers accept/reject cases for the up-front URL
// validation.
func TestValidateRegistrationURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https ok", "https://events.example.org/reg", false},
		{"http ok", "http://events.example.org/reg", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"relative", "/reg", true},
		{"scheme-relative", "//events.example.org/reg", true},
		{"ftp scheme", "ftp://events.example.org/reg", true},
		{"no host", "https://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRegistrationURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateRegistrationURL(%q): want error, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRegistrationURL(%q): want nil, got %v", tc.url, err)
			}
		})
	}
}

// TestCreateCampaign_RejectsBadRegistrationURLBeforeAnyPOST verifies that an
// empty/relative registration URL is rejected up front — before any POST that
// would create a permanent campaign group or campaign. The test server fails on
// any POST, so a passing test proves no POST was attempted.
func TestCreateCampaign_RejectsBadRegistrationURLBeforeAnyPOST(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST %s — URL should have been rejected first", r.URL.Path)
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	for _, bad := range []string{"", "/relative/only"} {
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        "E",
			RegistrationURL:  bad,
			BudgetUSD:        100,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil {
			t.Errorf("CreateCampaign with registration URL %q: expected error, got nil", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero POSTs for invalid registration URL, got %d", postCount)
	}
}

// TestCreateCampaign_PartialVariantFailureReported verifies that when a later
// variant fails after an earlier one succeeded, CreateCampaign returns an error
// that reports the partial success AND a non-nil *CampaignResult carrying the
// group/campaign IDs and the successful creative count, so the caller does not
// blindly retry.
func TestCreateCampaign_PartialVariantFailureReported(t *testing.T) {
	var mu sync.Mutex
	var darkPostCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			darkPostCount++
			// First dark post succeeds; the second one fails.
			if darkPostCount >= 2 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants: []CreativeVariant{
			{IntroText: "a", Headline: "one"},
			{IntroText: "b", Headline: "two"}, // this variant's dark post fails
		},
	})
	if err == nil {
		t.Fatal("expected an error on mid-variant failure, got nil")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("error should report 1 of 2 variants created, got: %v", err)
	}
	if !strings.Contains(err.Error(), "do NOT blindly retry") {
		t.Errorf("error should warn against blind retry, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected a non-nil partial CampaignResult, got nil")
	}
	if res.CampaignGroupID != "100" || res.CampaignID != "200" {
		t.Errorf("partial result should carry created group/campaign IDs, got group=%q campaign=%q", res.CampaignGroupID, res.CampaignID)
	}
	if res.CreativeCount != 1 {
		t.Errorf("partial result should report 1 successful creative, got %d", res.CreativeCount)
	}
}

// TestCreateCampaign_UnknownAccountFailsClosed verifies that an unknown account
// id supplied through the sole public entry point (CreateCampaign) fails closed
// before any POST — the account-scoped hierarchy helpers are unexported so a
// caller cannot bypass the resolveAccountID cross-tenant check.
func TestCreateCampaign_UnknownAccountFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST — unknown account should fail closed first")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		AdAccountID:      "000000", // not in the runtime config
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("expected fail-closed error for unknown account id")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("no POST should be issued for an unknown account, got %d", postCount)
	}
}

// ---------------------------------------------------------------------------
// Third-round Copilot findings
// ---------------------------------------------------------------------------

// noPOSTServer returns an httptest.Server that answers GETs with an empty
// element set and fails the test on any POST, proving the code under test
// rejected the input before attempting a create.
func noPOSTServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST %s — input should have been rejected first", r.URL.Path)
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
}

// TestCreateCampaign_RejectsBadBudgetBeforeAnyPOST verifies that a zero,
// negative, NaN, or Inf budget is rejected up front — before any POST that would
// create a permanent campaign group. The server fails on any POST.
func TestCreateCampaign_RejectsBadBudgetBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	budgets := []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, b := range budgets {
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        "E",
			RegistrationURL:  "https://events.example.org/reg",
			BudgetUSD:        b,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil {
			t.Errorf("CreateCampaign with budget %v: expected error, got nil", b)
		}
	}
}

// TestCreateCampaign_RejectsEmptyGeoBeforeAnyPOST verifies that an input whose
// geo targets all resolve to nothing (empty URN set) is rejected before any POST
// — creating a campaign with empty geo targeting is refused up front.
func TestCreateCampaign_RejectsEmptyGeoBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	// ResolveGeoTargets drops "Atlantis" (unknown), leaving an empty slice.
	geos := ResolveGeoTargets([]string{"Atlantis"})
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       geos,
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign with only unknown geos: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "geo") {
		t.Errorf("error should mention empty geo targeting, got: %v", err)
	}
}

// TestCreateCampaign_TrimsRegistrationURLForUTM verifies that a registration URL
// with surrounding whitespace passes validation AND produces a well-formed UTM
// URL (no embedded spaces) — the trimmed value must be used downstream in
// BuildUTMURL, not the original untrimmed field.
func TestCreateCampaign_TrimsRegistrationURLForUTM(t *testing.T) {
	var mu sync.Mutex
	var darkPostSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			if content, ok := body["content"].(map[string]any); ok {
				if article, ok := content["article"].(map[string]any); ok {
					if src, ok := article["source"].(string); ok {
						mu.Lock()
						darkPostSource = src
						mu.Unlock()
					}
				}
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "  https://events.example.org/reg  ", // surrounding whitespace
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign with whitespace-padded URL: %v", err)
	}

	mu.Lock()
	src := darkPostSource
	mu.Unlock()
	if src == "" {
		t.Fatal("dark post source URL was never captured")
	}
	if strings.ContainsAny(src, " \t\n") {
		t.Errorf("built UTM URL must not contain whitespace, got %q", src)
	}
	// The built URL must parse cleanly and keep its scheme/host intact.
	u, perr := neturl.Parse(src)
	if perr != nil {
		t.Fatalf("built UTM URL does not parse: %v (url=%q)", perr, src)
	}
	if u.Scheme != "https" || u.Host != "events.example.org" {
		t.Errorf("built UTM URL malformed: scheme=%q host=%q (url=%q)", u.Scheme, u.Host, src)
	}
}

// ---------------------------------------------------------------------------
// Fourth-round Copilot findings
// ---------------------------------------------------------------------------

// TestCreateSponsoredCampaign_SameNameDifferentGroupNotMatched verifies that the
// find-existing-campaign lookup is scoped to the resolved campaign group: a
// same-name campaign returned by the account-wide search but belonging to a
// DIFFERENT group is NOT treated as an idempotent match, so a new campaign is
// created under the correct group.
func TestCreateSponsoredCampaign_SameNameDifferentGroupNotMatched(t *testing.T) {
	const wantGroupID = "100"
	const wrongGroupCampaignID = "999" // a same-name campaign under a different group
	const newCampaignID = "200"

	var mu sync.Mutex
	var createdCampaign bool
	var createdCampaignGroupURN string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// The campaign search is account-wide and returns a same-name campaign
			// that belongs to a DIFFERENT (old) group. The client must ignore it.
			_, _ = io.WriteString(w, `{"elements":[{"name":"Same Name","status":"ACTIVE","id":`+wrongGroupCampaignID+`,"campaignGroup":"urn:li:sponsoredCampaignGroup:777"}],"metadata":{"nextPageToken":""}}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		createdCampaign = true
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if cg, ok := body["campaignGroup"].(string); ok {
			createdCampaignGroupURN = cg
		}
		w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:"+newCampaignID)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	startMs, endMs := testScheduleMs(t, c)
	id, err := c.createSponsoredCampaign(
		context.Background(),
		"123456789", wantGroupID, "Same Name",
		100, []string{"urn:li:geo:103644278"}, "cloud-native",
		startMs, endMs, false,
	)
	if err != nil {
		t.Fatalf("createSponsoredCampaign: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !createdCampaign {
		t.Fatal("expected a new campaign to be created; the same-name campaign under a different group must not match")
	}
	if id != newCampaignID {
		t.Errorf("expected the newly-created campaign id %q, got %q (matched the wrong-group campaign?)", newCampaignID, id)
	}
	if createdCampaignGroupURN != "urn:li:sponsoredCampaignGroup:"+wantGroupID {
		t.Errorf("new campaign must reference the resolved group, got campaignGroup=%q", createdCampaignGroupURN)
	}
}

// TestCreateSponsoredCampaign_SameNameSameGroupMatched is the positive counterpart:
// a same-name campaign whose campaignGroup DOES resolve to the target group is
// treated as an idempotent match, so no new campaign is created.
func TestCreateSponsoredCampaign_SameNameSameGroupMatched(t *testing.T) {
	const groupID = "100"
	const existingCampaignID = "555"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected POST — a same-name campaign in the same group must match idempotently")
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[{"name":"Same Name","status":"ACTIVE","id":`+existingCampaignID+`,"campaignGroup":"urn:li:sponsoredCampaignGroup:`+groupID+`"}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	startMs, endMs := testScheduleMs(t, c)
	id, err := c.createSponsoredCampaign(
		context.Background(),
		"123456789", groupID, "Same Name",
		100, []string{"urn:li:geo:103644278"}, "cloud-native",
		startMs, endMs, false,
	)
	if err != nil {
		t.Fatalf("createSponsoredCampaign: %v", err)
	}
	if id != existingCampaignID {
		t.Errorf("expected idempotent match of existing campaign %q, got %q", existingCampaignID, id)
	}
}

// TestCreateDarkPost_TruncatesIntroTextTo600Runes verifies that intro/primary
// (commentary) text longer than 600 characters is truncated rune-safely to 600
// runes before the dark post is created, matching LinkedIn's single-image ad
// limit and the TS source.
func TestCreateDarkPost_TruncatesIntroTextTo600Runes(t *testing.T) {
	// 700 multi-byte runes (é) — proves both the 600-rune cap and that
	// truncation is rune-safe (no split into invalid UTF-8).
	longIntro := strings.Repeat("é", 700)

	var mu sync.Mutex
	var commentary string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if cm, ok := body["commentary"].(string); ok {
			mu.Lock()
			commentary = cm
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.createDarkPost(context.Background(), "123456789", longIntro, "Headline", "https://events.example.org/reg", ""); err != nil {
		t.Fatalf("createDarkPost: %v", err)
	}

	mu.Lock()
	got := commentary
	mu.Unlock()
	if n := len([]rune(got)); n != 600 {
		t.Errorf("intro text must be truncated to 600 runes, got %d runes", n)
	}
	// Rune-safe truncation must yield exactly 600 "é" runes with no U+FFFD
	// replacement chars from a split multi-byte rune.
	if got != strings.Repeat("é", 600) {
		t.Errorf("truncation corrupted multi-byte runes or wrong length: got %q", got)
	}
}

// TestCreateCampaign_RejectsWhitespaceEventNameBeforeAnyPOST verifies that a
// whitespace-only EventName is rejected up front, before any POST — an empty or
// whitespace-only event name would collapse every campaign to the same
// idempotency key ("Events |  | TLF").
func TestCreateCampaign_RejectsWhitespaceEventNameBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	for _, name := range []string{"", "   ", "\t\n "} {
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        name,
			RegistrationURL:  "https://events.example.org/reg",
			BudgetUSD:        100,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil {
			t.Errorf("CreateCampaign with EventName=%q: expected error, got nil", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Fifth-round Copilot findings
// ---------------------------------------------------------------------------

// captureResourceNamesServer returns an httptest.Server that records the `name`
// field of the created campaign-group and campaign POST bodies into the supplied
// pointers, guarded by mu. GETs return an empty element set (nothing found, so a
// create is always attempted). The returned server drives a full, successful
// CreateCampaign so the resource names built from the input can be asserted.
func captureResourceNamesServer(t *testing.T, mu *sync.Mutex, groupName, campaignName *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		readName := func() string {
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			if n, ok := body["name"].(string); ok {
				return n
			}
			return ""
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			mu.Lock()
			*groupName = readName()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			*campaignName = readName()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
}

// TestCreateCampaign_TrimsEventNameInResourceNames verifies that an EventName
// with surrounding whitespace passes validation AND produces campaign-group and
// campaign names with NO leading/trailing whitespace — the trimmed value must be
// the single source of truth for all resource names and idempotency keys, not
// the original untrimmed field.
func TestCreateCampaign_TrimsEventNameInResourceNames(t *testing.T) {
	var mu sync.Mutex
	var groupName, campaignName string
	srv := captureResourceNamesServer(t, &mu, &groupName, &campaignName)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "  KubeCon  ", // surrounding whitespace
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign with whitespace-padded EventName: %v", err)
	}

	mu.Lock()
	gotGroup, gotCampaign := groupName, campaignName
	mu.Unlock()

	if gotGroup == "" || gotCampaign == "" {
		t.Fatalf("resource names were not captured: group=%q campaign=%q", gotGroup, gotCampaign)
	}
	if gotGroup != strings.TrimSpace(gotGroup) {
		t.Errorf("campaign-group name has surrounding whitespace: %q", gotGroup)
	}
	if gotCampaign != strings.TrimSpace(gotCampaign) {
		t.Errorf("campaign name has surrounding whitespace: %q", gotCampaign)
	}
	// The trimmed event name must be embedded exactly, with no padding around it.
	if gotGroup != "Events | KubeCon | TLF" {
		t.Errorf("group name = %q, want %q", gotGroup, "Events | KubeCon | TLF")
	}
	if !strings.Contains(gotCampaign, "| KubeCon |") {
		t.Errorf("campaign name must embed the trimmed event name, got %q", gotCampaign)
	}
	if strings.Contains(gotCampaign, "KubeCon  ") || strings.Contains(gotCampaign, "  KubeCon") {
		t.Errorf("campaign name embeds untrimmed event name, got %q", gotCampaign)
	}
}

// TestCreateCampaign_TrimsProjectInResourceNames verifies that Project is trimmed
// once and used as the single source of truth: a whitespace-only Project behaves
// exactly like an empty one (defaults to "TLF"), and a padded Project like
// "  cncf  " is embedded trimmed in the resource names.
func TestCreateCampaign_TrimsProjectInResourceNames(t *testing.T) {
	cases := []struct {
		name        string
		project     string
		wantProject string
	}{
		{"whitespace-only defaults to TLF", "   ", "TLF"},
		{"empty defaults to TLF", "", "TLF"},
		{"padded project trimmed", "  cncf  ", "cncf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var groupName, campaignName string
			srv := captureResourceNamesServer(t, &mu, &groupName, &campaignName)
			defer srv.Close()

			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "KubeCon",
				Project:          tc.project,
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        100,
				StartDate:        "2099-01-01",
				EndDate:          "2099-02-01",
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
			})
			if err != nil {
				t.Fatalf("CreateCampaign with Project=%q: %v", tc.project, err)
			}

			mu.Lock()
			gotGroup, gotCampaign := groupName, campaignName
			mu.Unlock()

			wantGroup := "Events | KubeCon | " + tc.wantProject
			if gotGroup != wantGroup {
				t.Errorf("group name = %q, want %q", gotGroup, wantGroup)
			}
			if !strings.HasSuffix(gotCampaign, "| "+tc.wantProject+" | MoFU") {
				t.Errorf("campaign name must use trimmed/defaulted project %q, got %q", tc.wantProject, gotCampaign)
			}
			if gotCampaign != strings.TrimSpace(gotCampaign) {
				t.Errorf("campaign name has surrounding whitespace: %q", gotCampaign)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Sixth-round Copilot findings
// ---------------------------------------------------------------------------

// TestCreateCampaign_RejectsEmptyVariantContentBeforeAnyPOST verifies that a
// variant whose Headline (or IntroText) normalizes to empty — whitespace-only or
// dash-only, which stripDashes collapses away — is rejected up front, before any
// POST. LinkedIn would otherwise reject the ad only after the campaign group and
// campaign already exist, orphaning them. The server fails on any POST.
func TestCreateCampaign_RejectsEmptyVariantContentBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	cases := []struct {
		name    string
		variant CreativeVariant
	}{
		{"whitespace-only headline", CreativeVariant{IntroText: "ok", Headline: "   "}},
		{"dash-only headline (em dash)", CreativeVariant{IntroText: "ok", Headline: "—"}},
		{"dash-only headline (en dash, padded)", CreativeVariant{IntroText: "ok", Headline: " – "}},
		{"empty headline", CreativeVariant{IntroText: "ok", Headline: ""}},
		{"whitespace-only intro", CreativeVariant{IntroText: "  \t ", Headline: "Headline"}},
		{"dash-only intro", CreativeVariant{IntroText: "—", Headline: "Headline"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "KubeCon",
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        100,
				StartDate:        "2099-01-01",
				EndDate:          "2099-02-01",
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{tc.variant},
			})
			if err == nil {
				t.Fatalf("CreateCampaign with %s: expected error, got nil", tc.name)
			}
		})
	}
}

// TestCreateCampaign_RejectsBadScheduleBeforeAnyLookupEvenWhenResourcesExist
// verifies that a reversed or past schedule is rejected up front — before any
// idempotency lookup (GET) or POST — even when the create path would otherwise
// short-circuit on already-existing group and campaign resources. Because the
// schedule is validated before the first lookup, no GET or POST is issued at all.
func TestCreateCampaign_RejectsBadScheduleBeforeAnyLookupEvenWhenResourcesExist(t *testing.T) {
	// This server would return existing group/campaign on GET (so toMs inside the
	// create helpers would be bypassed) and succeed on any POST. If either is ever
	// hit, the up-front schedule check failed to run first.
	var mu sync.Mutex
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		mu.Unlock()
		t.Errorf("unexpected %s %s — schedule should have been rejected before any lookup/POST", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		// Pretend the group and campaign already exist so, absent the up-front
		// check, the flow would short-circuit past toMs entirely.
		if strings.Contains(r.URL.Path, "adCampaignGroups") {
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | TLF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
			return
		}
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	cases := []struct {
		name      string
		startDate string
		endDate   string
	}{
		{"reversed dates", "2099-06-01", "2099-01-01"},
		{"past end date", "2010-01-01", "2010-02-01"},
		{"malformed start", "2099-1-1", "2099-02-01"},
		{"malformed end", "2099-01-01", "not-a-date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "KubeCon",
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        100,
				StartDate:        tc.startDate,
				EndDate:          tc.endDate,
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
			})
			if err == nil {
				t.Fatalf("CreateCampaign with %s: expected error, got nil", tc.name)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if reqCount != 0 {
		t.Errorf("expected zero HTTP requests for an invalid schedule, got %d", reqCount)
	}
}

// TestCreateCampaign_PartialFailureReportsDarkPostWhenCreativeFails verifies that
// when the FIRST variant's creative fails after its dark post already succeeded,
// the partial-failure report surfaces the created dark post shareURN — both in the
// error message and in the returned result's steps — so recovery state is clear
// even though creativeCount is still 0. A blind retry would duplicate that
// orphaned dark post, which has no idempotency lookup.
func TestCreateCampaign_PartialFailureReportsDarkPostWhenCreativeFails(t *testing.T) {
	const shareURN = "urn:li:share:300"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			// The first variant's dark post succeeds.
			_, _ = io.WriteString(w, `{"id":"`+shareURN+`"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			// ...but its creative fails.
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "one"}}, // single variant: creativeCount stays 0
	})
	if err == nil {
		t.Fatal("expected an error when the creative fails after its dark post succeeded, got nil")
	}
	// The error must name the created dark post so the caller knows a retry would
	// duplicate it, even though zero creatives completed.
	if !strings.Contains(err.Error(), shareURN) {
		t.Errorf("partial-failure error must mention the created dark post %q, got: %v", shareURN, err)
	}
	if !strings.Contains(err.Error(), "dark post") {
		t.Errorf("partial-failure error must mention the dark post, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected a non-nil partial CampaignResult, got nil")
	}
	if res.CreativeCount != 0 {
		t.Errorf("no creative completed; creativeCount should be 0, got %d", res.CreativeCount)
	}
	// The returned result's steps must also surface the dark post shareURN.
	var stepMentionsShare bool
	for _, s := range res.Steps {
		if strings.Contains(s, shareURN) {
			stepMentionsShare = true
			break
		}
	}
	if !stepMentionsShare {
		t.Errorf("partial result steps must include the created dark post %q, got steps=%v", shareURN, res.Steps)
	}
}

// ---------------------------------------------------------------------------
// Seventh-round Copilot findings
// ---------------------------------------------------------------------------

// TestCreateCampaign_RejectsSubCentBudgetBeforeAnyPOST verifies that a small
// positive budget that rounds to zero at the 2-decimal API boundary (e.g. 0.001
// formats to "0.00") is rejected up front — before any POST that would create a
// permanent campaign group. It passes the > 0 / NaN / Inf checks but must still
// be refused. The server fails on any POST.
func TestCreateCampaign_RejectsSubCentBudgetBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        0.001,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign with sub-cent budget 0.001: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "round") && !strings.Contains(err.Error(), "minimum") {
		t.Errorf("error should mention rounding-to-zero / minimum, got: %v", err)
	}
}

// TestResolveOrgID_RejectsNonNumericOrgID verifies that a config whose orgId is
// present but not a numeric LinkedIn organization id is rejected, rather than
// being used to build an invalid "urn:li:organization:<id>" URN.
func TestResolveOrgID_RejectsNonNumericOrgID(t *testing.T) {
	cfg := RuntimeConfig{
		DefaultAccountID: "123456789",
		DefaultOrgID:     "987654321",
		Accounts: []Account{
			{AccountID: "123456789", Label: "Bad Org", OrgID: "not-a-number", Status: "ACTIVE"},
		},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)

	if _, err := c.resolveOrgID("123456789"); err == nil {
		t.Fatal("resolveOrgID with non-numeric account orgId: expected error, got nil")
	}

	// The same invariant must hold for the default org fallback.
	cfg2 := RuntimeConfig{
		DefaultAccountID: "555",
		DefaultOrgID:     "urn:li:organization:987654321",
	}
	c2 := NewClient(Credentials{AccessToken: "t"}, cfg2)
	if _, err := c2.resolveOrgID("555"); err == nil {
		t.Fatal("resolveOrgID with malformed defaultOrgId: expected error, got nil")
	}
}

// TestCreateCampaign_RejectsNonNumericOrgBeforeAnyPOST verifies the malformed-org
// invariant is enforced through the public CreateCampaign path (via
// validatePrerequisites -> orgURN -> resolveOrgID) before any POST.
func TestCreateCampaign_RejectsNonNumericOrgBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	cfg := RuntimeConfig{
		DefaultAccountID: "123456789",
		DefaultOrgID:     "987654321",
		Accounts: []Account{
			{AccountID: "123456789", Label: "Bad Org", OrgID: "bad org id", Status: "ACTIVE"},
		},
		TargetingProfiles: []TargetingProfileConfig{
			{ID: "cloud-native", Label: "Cloud Native"},
		},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign with malformed orgId: expected error, got nil")
	}
}

// TestCreateDarkPost_TrimsWhitespaceInCreativeText verifies createDarkPost sends
// trimmed intro/headline (matching up-front normalization), not raw values with
// surrounding whitespace.
func TestCreateDarkPost_TrimsWhitespaceInCreativeText(t *testing.T) {
	var mu sync.Mutex
	var commentary, title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		if cm, ok := body["commentary"].(string); ok {
			commentary = cm
		}
		if content, ok := body["content"].(map[string]any); ok {
			if article, ok := content["article"].(map[string]any); ok {
				if ti, ok := article["title"].(string); ok {
					title = ti
				}
			}
		}
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"urn:li:share:301"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.createDarkPost(context.Background(), "123456789", "  Intro with spaces  ", "  Headline  ", "https://events.example.org/reg", ""); err != nil {
		t.Fatalf("createDarkPost: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if commentary != "Intro with spaces" {
		t.Errorf("commentary = %q, want trimmed 'Intro with spaces'", commentary)
	}
	if title != "Headline" {
		t.Errorf("article title = %q, want trimmed 'Headline'", title)
	}
}

// TestResolveAccountID_RejectsNonNumeric verifies a non-numeric ad-account id
// (default or matched override) is rejected rather than interpolated into a URN.
func TestResolveAccountID_RejectsNonNumeric(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultAccountID = "acct-abc" // non-numeric
	cfg.Accounts = []Account{{AccountID: "acct-abc", OrgID: "987654321", Status: "ACTIVE"}}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	if _, err := c.resolveAccountID(""); err == nil {
		t.Error("expected non-numeric default account id to be rejected")
	}
	if _, err := c.resolveAccountID("acct-abc"); err == nil {
		t.Error("expected non-numeric override account id to be rejected")
	}
}

// TestCreateCampaign_RejectsMalformedGeoURNBeforeAnyPOST verifies a caller-
// supplied malformed geo URN is rejected before any campaign is created.
func TestCreateCampaign_RejectsMalformedGeoURNBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-01-31",
		GeoTargets:       []GeoTarget{{URN: "invalid"}},
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "hi", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid geo target URN") {
		t.Fatalf("err = %v, want malformed geo URN rejection", err)
	}
}

// TestCreateCampaign_RejectsOverlongNameBeforeAnyPOST verifies an event name
// that pushes a resource name past 255 chars is rejected before any create.
func TestCreateCampaign_RejectsOverlongNameBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        strings.Repeat("X", 300),
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-01-31",
		GeoTargets:       []GeoTarget{{URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "hi", Headline: "h"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds the 255-character limit") {
		t.Fatalf("err = %v, want name-length rejection", err)
	}
}

// fullFlowServer returns an httptest.Server that answers search GETs with an
// empty element set (forcing the create path) and returns valid resource IDs for
// every create POST, so a well-formed CreateCampaign call completes end to end.
// It lets a test assert that an input passed the up-front validation gates
// (e.g. the budget minimum) by reaching a successful create rather than a
// validation error.
func fullFlowServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
}

// TestCreateCampaign_RejectsBelowMinimumBudgetBeforeAnyPOST verifies that a
// budget below LinkedIn's per-campaign minimum ($10 daily, $100 lifetime) is
// rejected up front, before any POST that would create a permanent campaign
// group. The server fails on any POST.
func TestCreateCampaign_RejectsBelowMinimumBudgetBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	cases := []struct {
		name     string
		budget   float64
		lifetime bool
	}{
		{"$5 daily", 5, false},
		{"$50 lifetime", 50, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "E",
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        tc.budget,
				LifetimeBudget:   tc.lifetime,
				StartDate:        "2099-01-01",
				EndDate:          "2099-02-01",
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
			})
			if err == nil {
				t.Fatalf("CreateCampaign with %s: expected error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "minimum") {
				t.Errorf("error should mention the LinkedIn budget minimum, got: %v", err)
			}
		})
	}
}

// TestCreateCampaign_AcceptsMinimumBudget verifies that a budget AT LinkedIn's
// minimum ($10 daily, $100 lifetime) passes the up-front budget check and the
// create flow proceeds (completes against a full-flow mock). If either value
// were rejected at the budget gate, CreateCampaign would return an error before
// any POST.
func TestCreateCampaign_AcceptsMinimumBudget(t *testing.T) {
	cases := []struct {
		name     string
		budget   float64
		lifetime bool
	}{
		{"$10 daily", 10, false},
		{"$100 lifetime", 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fullFlowServer(t)
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "E",
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        tc.budget,
				LifetimeBudget:   tc.lifetime,
				StartDate:        "2099-01-01",
				EndDate:          "2099-02-01",
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
			})
			if err != nil {
				t.Fatalf("CreateCampaign with %s should pass the budget check, got error: %v", tc.name, err)
			}
		})
	}
}

// TestCreateCampaign_RejectsMalformedImageURNBeforeAnyPOST verifies that a
// variant carrying a non-empty but malformed ImageURN is rejected up front,
// before any POST — an empty ImageURN stays allowed, but a bad digital-asset URN
// must not reach LinkedIn only after the campaign group and campaign exist.
func TestCreateCampaign_RejectsMalformedImageURNBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-01-31",
		GeoTargets:       []GeoTarget{{URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "hi", Headline: "h", ImageURN: "not-a-urn"}},
	})
	if err == nil || !strings.Contains(err.Error(), "image URN") {
		t.Fatalf("err = %v, want malformed image URN rejection", err)
	}
}

// ---------------------------------------------------------------------------
// Eighth-round Copilot findings
// ---------------------------------------------------------------------------

// TestDoRequest_POST429NotRetried verifies that a POST receiving a 429 is NOT
// retried: LinkedIn's create endpoints have no idempotency key, so auto-resending
// a POST after a 429 (whose first attempt may already have succeeded upstream)
// could create a duplicate. The server must see exactly ONE POST and the call
// must return the 429 as an error.
func TestDoRequest_POST429NotRetried(t *testing.T) {
	var mu sync.Mutex
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
		WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	_, err := c.doRequest(context.Background(), http.MethodPost, "adCampaigns", map[string]any{"k": "v"}, nil)
	if err == nil {
		t.Fatal("expected an error when a POST is 429'd, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should report the 429 status, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("a non-idempotent POST must NOT be retried on 429: server saw %d POSTs, want exactly 1", posts)
	}
}

// TestDoRequest_GET429StillRetried is the positive counterpart: a GET that gets a
// 429 then a 200 IS still retried (safe/idempotent method), preserving the
// existing rate-limit resilience for read paths.
func TestDoRequest_GET429StillRetried(t *testing.T) {
	var mu sync.Mutex
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gets++
		n := gets
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"urn:li:x:99"}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(),
		WithBaseURL(srv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	out, err := c.doRequest(context.Background(), http.MethodGet, "adCampaigns/1", nil, nil)
	if err != nil {
		t.Fatalf("doRequest GET: %v", err)
	}
	if out.ID != "urn:li:x:99" {
		t.Errorf("id = %q, want urn:li:x:99", out.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 2 {
		t.Errorf("a GET must still retry on 429: server saw %d GETs, want 2 (one 429 + one success)", gets)
	}
}

// TestCreateCampaign_SingleScheduleComputation verifies that the schedule millis
// are computed ONCE up front and threaded through both the campaign-group and the
// campaign create bodies: their runSchedule.start/end must match a single expected
// value derived from the fixed clock via validateSchedule, not be independently
// recomputed (which would drift for a today/past start).
func TestCreateCampaign_SingleScheduleComputation(t *testing.T) {
	var mu sync.Mutex
	var groupSched, campaignSched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		readSched := func() map[string]any {
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			if rs, ok := body["runSchedule"].(map[string]any); ok {
				return rs
			}
			return nil
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			mu.Lock()
			groupSched = readSched()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			campaignSched = readSched()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	// The single expected values, derived from the fixed clock the same way
	// CreateCampaign derives them.
	wantStart, wantEnd := testScheduleMs(t, c)

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if groupSched == nil || campaignSched == nil {
		t.Fatalf("runSchedule not captured: group=%v campaign=%v", groupSched, campaignSched)
	}
	// JSON numbers decode as float64; compare against the single expected millis.
	assertSched := func(label string, sched map[string]any, wantStart, wantEnd int64) {
		gotStart, okS := sched["start"].(float64)
		gotEnd, okE := sched["end"].(float64)
		if !okS || !okE {
			t.Fatalf("%s runSchedule missing numeric start/end: %v", label, sched)
		}
		if int64(gotStart) != wantStart {
			t.Errorf("%s start = %d, want single expected %d", label, int64(gotStart), wantStart)
		}
		if int64(gotEnd) != wantEnd {
			t.Errorf("%s end = %d, want single expected %d", label, int64(gotEnd), wantEnd)
		}
	}
	assertSched("group", groupSched, wantStart, wantEnd)
	assertSched("campaign", campaignSched, wantStart, wantEnd)

	// And, crucially, the group and campaign must agree with EACH OTHER — a single
	// source of truth, not two independent computations.
	if groupSched["start"] != campaignSched["start"] || groupSched["end"] != campaignSched["end"] {
		t.Errorf("group and campaign schedules diverged: group=%v campaign=%v", groupSched, campaignSched)
	}
}

// TestImageURNRE_TightId verifies the tightened image-URN regex rejects a
// trailing space and URL-delimiter values while still accepting a normal
// LinkedIn asset id (alphanumeric plus '-'/'_').
func TestImageURNRE_TightId(t *testing.T) {
	cases := []struct {
		urn  string
		want bool
	}{
		{"urn:li:image:C4E10AQabc_1-2", true},
		{"urn:li:digitalmediaAsset:C4E10AQabc_1-2", true},
		{"urn:li:image: ", false},     // trailing space
		{"urn:li:image:a/b", false},   // URL path delimiter
		{"urn:li:image:a b", false},   // embedded space
		{"urn:li:image:a?b=c", false}, // query delimiters
		{"urn:li:image:", false},      // empty id
		{"not-a-urn", false},
	}
	for _, tc := range cases {
		if got := imageURNRE.MatchString(tc.urn); got != tc.want {
			t.Errorf("imageURNRE.MatchString(%q) = %v, want %v", tc.urn, got, tc.want)
		}
	}
}

// TestCreateCampaign_RejectsProfileWithNoTargetingBeforeAnyPOST verifies that a
// targeting profile PRESENT in the runtime config but with empty skills AND
// groups is rejected up front, before any POST — a misconfigured injected profile
// must not build a campaign whose only include facet is the hardcoded
// jobFunctions fallback. The server fails on any POST.
func TestCreateCampaign_RejectsProfileWithNoTargetingBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	cfg := testConfig()
	// A present-but-empty profile: no skills, no groups.
	cfg.TargetingProfiles = []TargetingProfileConfig{
		{ID: "empty-profile", Label: "Empty"},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "empty-profile",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign with an empty targeting profile: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usable targeting") {
		t.Errorf("error should mention no usable targeting facets, got: %v", err)
	}
}

// TestCreateCampaign_CustomProfileRequiresCloudNativePresent verifies the TS
// validateLinkedInPrerequisites contract: "custom" aliases "cloud-native", which
// must EXIST in the runtime config. When present AND non-empty, custom is allowed;
// when the aliased profile is absent entirely, custom is rejected before any POST.
// (Emptiness parity between custom and cloud-native is covered by
// TestCreateCampaign_EmptyConfigRejectedIdenticallyForCustomAndCloudNative.)
func TestCreateCampaign_CustomProfileRequiresCloudNativePresent(t *testing.T) {
	base := CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "custom",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	}

	// (a) cloud-native present with usable facets: custom is allowed.
	srv := fullFlowServer(t)
	defer srv.Close()
	cfgPresent := testConfig()
	cfgPresent.TargetingProfiles = []TargetingProfileConfig{{ID: "cloud-native", Skills: []string{"urn:li:skill:1"}}}
	cPresent := NewClient(Credentials{AccessToken: "t"}, cfgPresent, WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := cPresent.CreateCampaign(context.Background(), base); err != nil {
		t.Fatalf("custom with cloud-native present must be allowed, got error: %v", err)
	}

	// (b) cloud-native absent: custom is rejected before any POST.
	noPost := noPOSTServer(t)
	defer noPost.Close()
	cfgAbsent := testConfig()
	cfgAbsent.TargetingProfiles = nil
	cAbsent := NewClient(Credentials{AccessToken: "t"}, cfgAbsent, WithBaseURL(noPost.URL), WithClock(fixedClock()))
	if _, err := cAbsent.CreateCampaign(context.Background(), base); err == nil {
		t.Fatal("custom with no cloud-native profile configured must be rejected before any POST")
	}
}

// TestValidateRegistrationURL_RejectsEmptyHostname verifies a URL whose Host is
// present but Hostname() is empty (e.g. https://:443/path) is rejected.
func TestValidateRegistrationURL_RejectsEmptyHostname(t *testing.T) {
	for _, bad := range []string{"https://:443/path", "https://:80", "http://:/x"} {
		if err := validateRegistrationURL(bad); err == nil {
			t.Errorf("validateRegistrationURL(%q) = nil, want rejection (empty hostname)", bad)
		}
	}
	if err := validateRegistrationURL("https://events.example.org/reg"); err != nil {
		t.Errorf("valid URL rejected: %v", err)
	}
}

// TestCreateCampaign_RejectsBlankOnlyFacetsBeforeAnyPOST verifies that a
// targeting profile whose skills/groups contain only blank/whitespace-only
// entries (e.g. []string{""} or {"  "}) is treated as having NO usable targeting
// and is rejected up front — before any POST — exactly like a genuinely empty
// profile. Such a facet would be dropped before the wire, so it must never make a
// profile look usable.
func TestCreateCampaign_RejectsBlankOnlyFacetsBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()

	cfg := testConfig()
	cfg.TargetingProfiles = []TargetingProfileConfig{
		{ID: "cloud-native", Label: "Cloud Native", Skills: []string{""}, Groups: []string{"   "}},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign with blank-only skills/groups: expected rejection before any POST, got nil")
	}
	if !strings.Contains(err.Error(), "no usable targeting facets") {
		t.Errorf("error should mention no usable targeting facets, got: %v", err)
	}
}

// TestBuildTargetingCriteria_DropsBlankFacets verifies that blank/whitespace-only
// skill/group entries are filtered out (and the rest trimmed) before reaching
// LinkedIn, so a blank facet can never be sent on the wire.
func TestBuildTargetingCriteria_DropsBlankFacets(t *testing.T) {
	cfg := testConfig()
	cfg.TargetingProfiles = []TargetingProfileConfig{
		{ID: "cloud-native", Skills: []string{"urn:li:skill:1", "", "  urn:li:skill:2  "}, Groups: []string{"  "}},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg)
	crit, err := c.buildTargetingCriteria("cloud-native", []string{"urn:li:geo:1"})
	if err != nil {
		t.Fatalf("buildTargetingCriteria: %v", err)
	}
	b, _ := json.Marshal(crit)
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	tc := decoded["targetingCriteria"].(map[string]any)
	and := tc["include"].(map[string]any)["and"].([]any)
	second := and[1].(map[string]any)["or"].(map[string]any)

	skills := second["urn:li:adTargetingFacet:skills"].([]any)
	if len(skills) != 2 {
		t.Fatalf("expected 2 non-blank trimmed skills, got %d: %v", len(skills), skills)
	}
	if skills[0].(string) != "urn:li:skill:1" || skills[1].(string) != "urn:li:skill:2" {
		t.Errorf("blank dropped / trim failed, got skills=%v", skills)
	}
	groups := second["urn:li:adTargetingFacet:groups"].([]any)
	if len(groups) != 0 {
		t.Errorf("blank-only groups should filter to empty, got %v", groups)
	}
}

// TestCreateCampaign_SurfacesGroupWhenCampaignCreateFails verifies that when the
// campaign-create step fails AFTER the campaign group was created, CreateCampaign
// returns a NON-NIL partial *CampaignResult carrying the created CampaignGroupID
// (and the steps so far), so the created permanent group is not silently
// discarded.
func TestCreateCampaign_SurfacesGroupWhenCampaignCreateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			// Campaign creation fails AFTER the group already exists.
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("expected an error when campaign creation fails, got nil")
	}
	if res == nil {
		t.Fatal("expected a non-nil partial CampaignResult carrying the created group, got nil")
	}
	if res.CampaignGroupID != "100" {
		t.Errorf("partial result should carry the created campaign-group ID 100, got %q", res.CampaignGroupID)
	}
	if res.CampaignID != "" {
		t.Errorf("campaign was not created; CampaignID should be empty, got %q", res.CampaignID)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("error should mention the created campaign-group ID, got: %v", err)
	}
}

// TestCreateCampaign_RejectsMalformedFacetBeforeAnyPOST verifies a non-blank but
// malformed skill/group/employer facet is rejected before any POST.
func TestCreateCampaign_RejectsMalformedFacetBeforeAnyPOST(t *testing.T) {
	base := CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	}

	cases := []struct {
		name string
		mut  func(cfg *RuntimeConfig)
	}{
		{"malformed skill", func(cfg *RuntimeConfig) {
			cfg.TargetingProfiles = []TargetingProfileConfig{{ID: "cloud-native", Skills: []string{"not-a-skill-urn"}}}
		}},
		{"malformed group", func(cfg *RuntimeConfig) {
			cfg.TargetingProfiles = []TargetingProfileConfig{{ID: "cloud-native", Groups: []string{"urn:li:group:a b"}}}
		}},
		{"malformed employer exclusion", func(cfg *RuntimeConfig) {
			cfg.TargetingProfiles = []TargetingProfileConfig{{ID: "cloud-native", Skills: []string{"urn:li:skill:1"}}}
			cfg.EmployerExclusions = []string{"nope"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := noPOSTServer(t)
			defer srv.Close()
			cfg := testConfig()
			tc.mut(&cfg)
			c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))
			if _, err := c.CreateCampaign(context.Background(), base); err == nil {
				t.Fatalf("expected %s to be rejected before any POST", tc.name)
			}
		})
	}
}

// TestValidFacets_NamespaceEnforced verifies a facet from the wrong namespace
// (e.g. an organization URN under skills) is rejected.
func TestValidFacets_NamespaceEnforced(t *testing.T) {
	if _, err := validFacets("skills", []string{"urn:li:organization:1111"}); err == nil {
		t.Error("organization URN under skills should be rejected")
	}
	if _, err := validFacets("skills", []string{"urn:li:skill:1"}); err != nil {
		t.Errorf("valid skill URN rejected: %v", err)
	}
	if _, err := validFacets("groups", []string{"urn:li:group:100"}); err != nil {
		t.Errorf("valid group URN rejected: %v", err)
	}
	if _, err := validFacets("employer-exclusions", []string{"urn:li:organization:1111"}); err != nil {
		t.Errorf("valid employer URN rejected: %v", err)
	}
	if _, err := validFacets("employer-exclusions", []string{"urn:li:skill:1"}); err == nil {
		t.Error("skill URN under employer-exclusions should be rejected")
	}
}

// TestCampaignManagerURL_NoDanglingPath verifies the deep link doesn't end in a
// dangling /campaigns/ when no campaign was created.
func TestCampaignManagerURL_NoDanglingPath(t *testing.T) {
	if got := campaignManagerURL("123", ""); strings.HasSuffix(got, "/") {
		t.Errorf("empty campaignID URL = %q, should not end in a dangling slash", got)
	}
	if got := campaignManagerURL("123", "456"); !strings.HasSuffix(got, "/campaigns/456") {
		t.Errorf("URL = %q, want it to end in /campaigns/456", got)
	}
}

// TestValidFacets_RequiresNumericID verifies non-numeric facet ids are rejected.
func TestValidFacets_RequiresNumericID(t *testing.T) {
	if _, err := validFacets("skills", []string{"urn:li:skill:abc"}); err == nil {
		t.Error("non-numeric skill id should be rejected")
	}
	if _, err := validFacets("employer-exclusions", []string{"urn:li:organization:not-real"}); err == nil {
		t.Error("non-numeric organization id should be rejected")
	}
	if _, err := validFacets("skills", []string{"urn:li:skill:12345"}); err != nil {
		t.Errorf("numeric skill id rejected: %v", err)
	}
}

// TestParseRetryAfter_NoOverflow verifies a huge Retry-After value is clamped to
// maxRetryWait rather than overflowing to a negative duration.
func TestParseRetryAfter_NoOverflow(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(fixedClock()))
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "10000000000")
	got := c.parseRetryAfter(resp)
	if got <= 0 || got > maxRetryWait {
		t.Errorf("parseRetryAfter(huge) = %v, want a positive value <= maxRetryWait (%v)", got, maxRetryWait)
	}
}

// TestDoRequest_SendsAuthAndVersionHeaders verifies every request carries the
// Bearer token and the LinkedIn-Version / X-RestLi-Protocol-Version headers.
func TestDoRequest_SendsAuthAndVersionHeaders(t *testing.T) {
	var gotAuth, gotVer, gotProto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVer = r.Header.Get("LinkedIn-Version")
		gotProto = r.Header.Get("X-RestLi-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "tok-abc"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, _ = c.doRequest(context.Background(), http.MethodGet, "adAccounts/1", nil, nil)
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want 'Bearer tok-abc'", gotAuth)
	}
	if gotVer == "" {
		t.Error("LinkedIn-Version header missing")
	}
	if gotProto != "2.0.0" {
		t.Errorf("X-RestLi-Protocol-Version = %q, want 2.0.0", gotProto)
	}
}

// ---------------------------------------------------------------------------
// Eighth-round Copilot findings
// ---------------------------------------------------------------------------

// TestFindByName_404IsError verifies that a 404 on the search call is treated as
// an ERROR, not a clean "not found". A 404 does not prove the searched resource
// is absent — it can mean a wrong finder/account/collection path — so it must
// propagate rather than let a find-or-create caller proceed to a create POST.
func TestFindByName_404IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Missing | CNCF")
	if err == nil {
		t.Fatal("findByName on a 404 search response: expected an error, got nil (a 404 must not be a clean no-match)")
	}
	if id != "" {
		t.Errorf("findByName on error must return empty id, got %q", id)
	}
}

// TestCreateCampaign_404OnSearchDoesNotCreate verifies the end-to-end contract:
// a 404 on the up-front campaign-group search aborts CreateCampaign with an error
// BEFORE any create POST, rather than treating the 404 as "absent" and creating a
// (possibly wrong-path) resource.
func TestCreateCampaign_404OnSearchDoesNotCreate(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST %s — a 404 search must abort before any create", r.URL.Path)
			return
		}
		// Every GET (idempotency search) returns 404.
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("CreateCampaign when the search returns 404: expected an error, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero POSTs when the search 404s, got %d", postCount)
	}
}

// TestFindMatch_InconclusiveCapIsError verifies that if the API keeps returning a
// non-empty nextPageToken past the maxListPages cap, findByName returns an
// explicit error rather than a false no-match — so the find-or-create caller does
// NOT proceed to create a duplicate. This is the behavior that keeps a mature
// account (whose matching collection exceeds one cap's worth of pages) safe.
func TestFindMatch_InconclusiveCapIsError(t *testing.T) {
	var mu sync.Mutex
	var getCount int
	// Never returns the sought match and always advertises a further page, so the
	// walk can only terminate by hitting the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[{"name":"Other","status":"ACTIVE","id":1}],"metadata":{"nextPageToken":"more"}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaigns", "Never Matches")
	if err == nil {
		t.Fatal("findByName that never exhausts the cursor: expected an inconclusive-cap error, got nil")
	}
	if id != "" {
		t.Errorf("inconclusive cap must not report a match, got id %q", id)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("inconclusive-cap error should warn it aborts to avoid a duplicate, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The walk must stop exactly at the cap, not loop forever.
	if getCount != maxListPages {
		t.Errorf("expected the walk to stop at the %d-page cap, got %d GETs", maxListPages, getCount)
	}
}

// TestFindMatch_LargeAccountUnderCapSucceeds verifies the cap is high enough that
// an account with many pages of results still resolves a real match: the match
// sits well past the OLD cap of 25 pages (page 30), proving a mature account is
// no longer starved. The API exhausts its cursor on the matching page.
func TestFindMatch_LargeAccountUnderCapSucceeds(t *testing.T) {
	const matchPage = 30 // beyond the previous 25-page cap
	var mu sync.Mutex
	var page int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		page++
		n := page
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n < matchPage {
			_, _ = io.WriteString(w, `{"elements":[{"name":"Other","status":"ACTIVE","id":1}],"metadata":{"nextPageToken":"more"}}`)
			return
		}
		// The match, with an empty nextPageToken (end of results).
		_, _ = io.WriteString(w, `{"elements":[{"name":"Deep Match","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:4242"}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Deep Match")
	if err != nil {
		t.Fatalf("findByName across %d pages (well past the old 25-page cap): %v", matchPage, err)
	}
	if id != "4242" {
		t.Errorf("expected deep-page match id 4242, got %q", id)
	}
}

// TestCreateCampaign_StepWordingNeutralForFoundCampaign verifies the campaign
// step log uses neutral "ensured" wording, not "created ... (PAUSED)":
// createSponsoredCampaign is find-or-create and may return an EXISTING campaign in
// any non-terminal status (including ACTIVE), so a "created (PAUSED)" step could
// be doubly false. Here an existing ACTIVE campaign is found in the group.
func TestCreateCampaign_StepWordingNeutralForFoundCampaign(t *testing.T) {
	const groupID = "100"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch {
			case strings.Contains(r.URL.Path, "adCampaignGroups"):
				// Group is found (ACTIVE), so no group create POST.
				_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | TLF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:`+groupID+`"}],"metadata":{"nextPageToken":""}}`)
			case strings.Contains(r.URL.Path, "adCampaigns"):
				// An EXISTING ACTIVE campaign under the same group — found idempotently.
				_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | TLF | MoFU","status":"ACTIVE","id":"urn:li:sponsoredCampaign:200","campaignGroup":"urn:li:sponsoredCampaignGroup:`+groupID+`"}],"metadata":{"nextPageToken":""}}`)
			default:
				_, _ = io.WriteString(w, `{"elements":[]}`)
			}
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected POST "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	var campaignStep string
	for _, s := range res.Steps {
		if strings.Contains(s, "Campaign") && strings.Contains(s, "200") {
			campaignStep = s
			break
		}
	}
	if campaignStep == "" {
		t.Fatalf("no campaign step recorded; steps=%v", res.Steps)
	}
	// The found campaign is ACTIVE, so a "PAUSED"/"created" step would be false.
	if strings.Contains(campaignStep, "PAUSED") {
		t.Errorf("campaign step must not claim PAUSED for a found campaign: %q", campaignStep)
	}
	if strings.Contains(campaignStep, "created") {
		t.Errorf("campaign step must not claim 'created' for a found campaign: %q", campaignStep)
	}
	if !strings.Contains(campaignStep, "ensured") {
		t.Errorf("campaign step should use neutral 'ensured' wording, got: %q", campaignStep)
	}
}

// TestCreateCampaign_EmptyConfigRejectedIdenticallyForCustomAndCloudNative proves
// the empty-config rejection is applied to the NORMALIZED profile name, so the
// "custom" alias can no longer bypass it. "custom" normalizes to "cloud-native"
// and thus describes identical targeting; an EMPTY cloud-native profile must be
// rejected before any POST whether the caller asks for it directly
// ("cloud-native") or via its alias ("custom"). Previously the check keyed on the
// original name, so custom slipped past a rejection that direct cloud-native hit.
func TestCreateCampaign_EmptyConfigRejectedIdenticallyForCustomAndCloudNative(t *testing.T) {
	base := CampaignInput{
		EventName:       "KubeCon",
		RegistrationURL: "https://events.example.org/reg",
		BudgetUSD:       100,
		StartDate:       "2099-01-01",
		EndDate:         "2099-02-01",
		GeoTargets:      []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:        []CreativeVariant{{IntroText: "a", Headline: "b"}},
	}

	for _, profile := range []string{"cloud-native", "custom"} {
		t.Run(profile, func(t *testing.T) {
			srv := noPOSTServer(t)
			defer srv.Close()

			cfg := testConfig()
			// Aliased profile EXISTS but has EMPTY facets (both nil and blank-only
			// entries count as empty and must be rejected identically for both names).
			cfg.TargetingProfiles = []TargetingProfileConfig{
				{ID: "cloud-native", Label: "Cloud Native", Skills: []string{""}, Groups: nil},
			}
			c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

			in := base
			in.TargetingProfile = profile
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("profile %q with empty cloud-native config: expected rejection before any POST, got nil", profile)
			}
			if !strings.Contains(err.Error(), "no usable targeting facets") {
				t.Errorf("profile %q: error should mention no usable targeting facets, got: %v", profile, err)
			}
		})
	}
}
