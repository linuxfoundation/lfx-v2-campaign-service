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

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Page 1: no match, but a nextPageToken advertising a further page.
			// The offset param `start` must NOT be used by the client.
			if r.URL.Query().Get("start") != "" {
				t.Errorf("cursor pagination must not send offset `start`, got %q", r.URL.Query().Get("start"))
			}
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
