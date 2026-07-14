// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// urlWithUserinfo composes a URL with embedded userinfo at runtime, so the test
// source never contains a literal "user:pass@host" — which secretlint's
// basic-auth rule (MegaLinter) flags as a credential and fails CI on. The
// composed value still exercises the userinfo-rejection path.
func urlWithUserinfo(scheme, user, pass, hostAndRest string) string {
	cred := user
	if pass != "" {
		cred += ":" + pass
	}
	return scheme + "://" + cred + "@" + hostAndRest
}

// TestValidateRegistrationURL_RejectsUserinfo verifies a URL carrying embedded
// credentials (user[:password]@host) is rejected, matching the Reddit and Meta
// clients. Embedded credentials in an ad destination leak secrets.
func TestValidateRegistrationURL_RejectsUserinfo(t *testing.T) {
	for _, raw := range []string{
		urlWithUserinfo("https", "user", "s3cr3t", "example.com/register"),
		urlWithUserinfo("https", "user", "", "example.com/register"),
	} {
		if err := validateRegistrationURL(raw); err == nil {
			t.Errorf("validateRegistrationURL(%q) = nil, want error", raw)
		}
	}
	// A credential-free URL still passes.
	if err := validateRegistrationURL("https://example.com/register"); err != nil {
		t.Errorf("validateRegistrationURL(clean) = %v, want nil", err)
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
			Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
			Project:          "tlf",
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

	// ResolveGeoTargets does not resolve "Atlantis" (unknown): it returns an empty
	// resolved slice and reports "Atlantis" as unresolved.
	geos, _ := ResolveGeoTargets([]string{"Atlantis"})
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
	if gotGroup != "Events | KubeCon | tlf" {
		t.Errorf("group name = %q, want %q", gotGroup, "Events | KubeCon | tlf")
	}
	if !strings.Contains(gotCampaign, "| KubeCon |") {
		t.Errorf("campaign name must embed the trimmed event name, got %q", gotCampaign)
	}
	if strings.Contains(gotCampaign, "KubeCon  ") || strings.Contains(gotCampaign, "  KubeCon") {
		t.Errorf("campaign name embeds untrimmed event name, got %q", gotCampaign)
	}
}

// TestCreateCampaign_ProjectRequiredAndTrimmed verifies Project is required (no
// silent default — a hardcoded default would mis-attribute non-TLF campaigns)
// and, when supplied, is trimmed and embedded as the single source of truth in
// the resource names.
func TestCreateCampaign_ProjectRequiredAndTrimmed(t *testing.T) {
	// Empty / whitespace-only Project must be REJECTED before any POST.
	for _, empty := range []string{"", "   "} {
		srv := noPOSTServer(t)
		c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        "KubeCon",
			Project:          empty,
			RegistrationURL:  "https://events.example.org/reg",
			BudgetUSD:        100,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil || !strings.Contains(err.Error(), "project is required") {
			t.Errorf("Project=%q: err = %v, want 'project is required'", empty, err)
		}
		srv.Close()
	}

	// A padded valid Project like "  cncf  " is trimmed and embedded.
	var mu sync.Mutex
	var groupName, campaignName string
	srv := captureResourceNamesServer(t, &mu, &groupName, &campaignName)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		Project:          "  cncf  ",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign with padded Project: %v", err)
	}
	mu.Lock()
	gotGroup, gotCampaign := groupName, campaignName
	mu.Unlock()
	if gotGroup != "Events | KubeCon | cncf" {
		t.Errorf("group name = %q, want %q", gotGroup, "Events | KubeCon | cncf")
	}
	if !strings.HasSuffix(gotCampaign, "| cncf | MoFU") {
		t.Errorf("campaign name must use trimmed project, got %q", gotCampaign)
	}
	if gotCampaign != strings.TrimSpace(gotCampaign) {
		t.Errorf("campaign name has surrounding whitespace: %q", gotCampaign)
	}
}

// TestCreateCampaign_SanitizesPipeInEventNameAndProject verifies "|" in EventName
// or Project is sanitized to "-" so it can't inject extra pipe-delimited fields
// into the campaign name. The values are accepted (not rejected) — this tests
// SANITIZATION, not rejection.
func TestCreateCampaign_SanitizesPipeInEventNameAndProject(t *testing.T) {
	var mu sync.Mutex
	var groupName, campaignName string
	srv := captureResourceNamesServer(t, &mu, &groupName, &campaignName)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "Kube|Con",
		Project:          "cn|cf",
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
	gotGroup := groupName
	mu.Unlock()
	// "|" replaced with "-" so the name has exactly its schema's pipe count.
	if gotGroup != "Events | Kube-Con | cn-cf" {
		t.Errorf("group name = %q, want %q (pipes sanitized to dashes)", gotGroup, "Events | Kube-Con | cn-cf")
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
				Project:          "tlf",
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
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
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
				Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
		Project:          "tlf",
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
				Project:          "tlf",
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
				Project:          "tlf",
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

// TestCreateCampaign_BudgetMinimumChecksRoundedValue verifies that the budget
// minimum is validated against the 2-decimal-rounded value actually SENT to
// LinkedIn (strconv.FormatFloat(_, 'f', 2, 64)), not the raw float. A value just
// under the minimum that ROUNDS UP to the minimum (e.g. 9.999 -> "10.00") is
// accepted, while a value that rounds BELOW the minimum (e.g. 9.994 -> "9.99") is
// rejected. Analogous for the lifetime minimum ($100).
func TestCreateCampaign_BudgetMinimumChecksRoundedValue(t *testing.T) {
	accepted := []struct {
		name     string
		budget   float64
		lifetime bool
	}{
		{"9.999 daily rounds to 10.00", 9.999, false},
		{"99.999 lifetime rounds to 100.00", 99.999, true},
	}
	for _, tc := range accepted {
		t.Run("accepted/"+tc.name, func(t *testing.T) {
			srv := fullFlowServer(t)
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "E",
				Project:          "tlf",
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
				t.Fatalf("CreateCampaign with %s should be accepted (rounds up to the minimum), got error: %v", tc.name, err)
			}
		})
	}

	rejected := []struct {
		name     string
		budget   float64
		lifetime bool
	}{
		{"9.994 daily rounds to 9.99", 9.994, false},
		{"99.994 lifetime rounds to 99.99", 99.994, true},
	}
	for _, tc := range rejected {
		t.Run("rejected/"+tc.name, func(t *testing.T) {
			srv := noPOSTServer(t)
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			_, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "E",
				Project:          "tlf",
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
				t.Fatalf("CreateCampaign with %s: expected rejection (rounds below the minimum), got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "minimum") {
				t.Errorf("error should mention the LinkedIn budget minimum, got: %v", err)
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
		Project:          "tlf",
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
		Project:          "tlf",
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

// TestCreateCampaign_AcceptsEmptySkillsGroupsWhenJobFunctionsPresent verifies
// that a targeting profile PRESENT in the runtime config but with empty skills
// AND groups is ACCEPTED, mirroring the TS source: buildTargetingCriteria always
// injects the hardcoded jobFunctions into the include block, so the assembled
// targeting criteria is non-empty even without profile-specific skills/groups.
// The full flow must succeed (no rejection before the POSTs).
func TestCreateCampaign_AcceptsEmptySkillsGroupsWhenJobFunctionsPresent(t *testing.T) {
	srv := fullFlowServer(t)
	defer srv.Close()

	cfg := testConfig()
	// A present-but-empty profile: no skills, no groups. jobFunctions keep the
	// assembled criteria non-empty, so this must be accepted.
	cfg.TargetingProfiles = []TargetingProfileConfig{
		{ID: "empty-profile", Label: "Empty"},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "empty-profile",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign with empty skills/groups but jobFunctions present must be accepted, got error: %v", err)
	}
}

// TestValidatePrerequisites_RejectsOnlyTrulyEmptyCriteria verifies that the
// non-empty-criteria check keys off the FINAL assembled include facets: with
// jobFunctions present (the normal case) an empty-skills/groups profile is
// accepted, and only a truly-empty assembled criteria (no skills, groups, AND no
// jobFunctions) is rejected. Both custom and cloud-native behave identically.
func TestValidatePrerequisites_RejectsOnlyTrulyEmptyCriteria(t *testing.T) {
	for _, profile := range []string{"cloud-native", "custom"} {
		t.Run(profile, func(t *testing.T) {
			cfg := testConfig()
			cfg.TargetingProfiles = []TargetingProfileConfig{
				{ID: "cloud-native", Label: "Cloud Native"}, // empty skills/groups
			}
			c := NewClient(Credentials{AccessToken: "t"}, cfg, WithClock(fixedClock()))

			// jobFunctions present: assembled criteria non-empty -> accepted.
			if err := c.validatePrerequisites(cfg.DefaultAccountID, profile); err != nil {
				t.Fatalf("profile %q with jobFunctions present must be accepted, got: %v", profile, err)
			}

			// Simulate a truly-empty assembled criteria by temporarily emptying the
			// package-level jobFunctions: now skills, groups, AND jobFunctions are all
			// empty, so the assembled criteria would be empty and must be rejected.
			saved := jobFunctions
			jobFunctions = nil
			t.Cleanup(func() { jobFunctions = saved })
			if err := c.validatePrerequisites(cfg.DefaultAccountID, profile); err == nil {
				t.Fatalf("profile %q with truly-empty assembled criteria must be rejected", profile)
			}
		})
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
		Project:          "tlf",
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

// TestCreateCampaign_AcceptsBlankOnlyFacetsWhenJobFunctionsPresent verifies that
// a targeting profile whose skills/groups contain only blank/whitespace-only
// entries (e.g. []string{""} or {"  "}) is ACCEPTED: those blanks are dropped
// before the wire, but buildTargetingCriteria still injects the hardcoded
// jobFunctions, so the assembled include criteria is non-empty. This mirrors the
// TS source, which accepts empty/blank skills/groups as long as jobFunctions keep
// the criteria non-empty.
func TestCreateCampaign_AcceptsBlankOnlyFacetsWhenJobFunctionsPresent(t *testing.T) {
	srv := fullFlowServer(t)
	defer srv.Close()

	cfg := testConfig()
	cfg.TargetingProfiles = []TargetingProfileConfig{
		{ID: "cloud-native", Label: "Cloud Native", Skills: []string{""}, Groups: []string{"   "}},
	}
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaign with blank-only skills/groups but jobFunctions present must be accepted, got error: %v", err)
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
		Project:          "tlf",
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
		Project:          "tlf",
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
		t.Errorf("valid organization employer URN rejected: %v", err)
	}
	// The documented service contract (docs/api-catalog.md) specifies employer
	// exclusions as urn:li:company:<id>; that form MUST be accepted.
	if _, err := validFacets("employer-exclusions", []string{"urn:li:company:33275771"}); err != nil {
		t.Errorf("documented urn:li:company employer URN rejected: %v", err)
	}
	if out, err := validFacets("employer-exclusions", []string{"urn:li:company:33275771", "urn:li:organization:1111"}); err != nil {
		t.Errorf("mixed company+organization employer URNs rejected: %v", err)
	} else if len(out) != 2 {
		t.Errorf("mixed employer URNs: got %d, want 2", len(out))
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
	if _, err := validFacets("employer-exclusions", []string{"urn:li:company:not-real"}); err == nil {
		t.Error("non-numeric company id should be rejected")
	}
	if _, err := validFacets("skills", []string{"urn:li:skill:12345"}); err != nil {
		t.Errorf("numeric skill id rejected: %v", err)
	}
}

// TestParseRetryAfter_NoOverflow verifies a huge Retry-After value is clamped to
// maxRetryWait rather than overflowing to a negative duration — including a
// value that exceeds int64 itself (which strconv.Atoi/ParseInt reports as
// ErrRange). Such a value must clamp to the cap, not fall through to the
// HTTP-date branch and return 0 (which would defeat the intended 60s ceiling).
func TestParseRetryAfter_NoOverflow(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"large but fits int64", "10000000000"},
		{"overflows int64 (ErrRange)", "99999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(fixedClock()))
			resp := &http.Response{Header: http.Header{}}
			resp.Header.Set("Retry-After", tc.value)
			got := c.parseRetryAfter(resp)
			if got <= 0 || got > maxRetryWait {
				t.Errorf("parseRetryAfter(%q) = %v, want a positive value <= maxRetryWait (%v)", tc.value, got, maxRetryWait)
			}
		})
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
	if gotVer != apiVersion {
		t.Errorf("LinkedIn-Version = %q, want the exact required contract value %q", gotVer, apiVersion)
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
		Project:          "tlf",
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

// TestFindMatch_RepeatedTokenAborts verifies that a server which keeps returning
// the SAME non-empty nextPageToken is detected as a pagination loop and aborted
// with an inconclusive error after just the repeat — not replayed up to the cap.
func TestFindMatch_RepeatedTokenAborts(t *testing.T) {
	var mu sync.Mutex
	var getCount int
	// Always advertises the same next page token, simulating a stuck cursor.
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
		t.Fatal("repeated page token: expected an inconclusive error, got nil")
	}
	if id != "" {
		t.Errorf("repeated page token must not report a match, got id %q", id)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should warn it aborts to avoid a duplicate, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Page 1 fetches "more" (recorded); page 2 fetches with token "more" and its
	// echoed "more" is detected as already-seen — so it stops at 2 GETs, far below
	// the cap, instead of replaying the stuck cursor maxListPages times.
	if getCount != 2 {
		t.Errorf("expected the repeated-token guard to stop at 2 GETs, got %d", getCount)
	}
}

// TestFindMatch_InconclusiveCapIsError verifies that if the API keeps returning a
// non-empty (and DISTINCT) nextPageToken past the maxListPages cap, findByName
// returns an explicit error rather than a false no-match — so the find-or-create
// caller does NOT proceed to create a duplicate. This keeps a mature account
// (whose matching collection exceeds one cap's worth of pages) safe.
func TestFindMatch_InconclusiveCapIsError(t *testing.T) {
	var mu sync.Mutex
	var getCount int
	// Never returns the sought match and always advertises a further page with a
	// DISTINCT token (so the repeated-token guard doesn't trip); the walk can only
	// terminate by hitting the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		n := getCount
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"elements":[{"name":"Other","status":"ACTIVE","id":1}],"metadata":{"nextPageToken":"tok-%d"}}`, n))
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
			// Distinct token per page (as a real cursor returns), so the
			// repeated-token loop guard doesn't (correctly) trip on a stuck cursor.
			_, _ = io.WriteString(w, fmt.Sprintf(`{"elements":[{"name":"Other","status":"ACTIVE","id":1}],"metadata":{"nextPageToken":"page-%d"}}`, n))
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
				_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:`+groupID+`"}],"metadata":{"nextPageToken":""}}`)
			case strings.Contains(r.URL.Path, "adCampaigns"):
				// An EXISTING ACTIVE campaign under the same group — found idempotently.
				_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | tlf | MoFU","status":"ACTIVE","id":"urn:li:sponsoredCampaign:200","campaignGroup":"urn:li:sponsoredCampaignGroup:`+groupID+`"}],"metadata":{"nextPageToken":""}}`)
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
		Project:          "tlf",
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

// TestCreateCampaign_EmptyConfigHandledIdenticallyForCustomAndCloudNative proves
// the empty-config decision is applied to the NORMALIZED profile name, so the
// "custom" alias behaves EXACTLY like direct "cloud-native". "custom" normalizes
// to "cloud-native" and thus describes identical targeting. Mirroring the TS
// source: an empty cloud-native profile is ACCEPTED (jobFunctions keep the
// assembled criteria non-empty) for BOTH names, and is REJECTED only if the
// assembled criteria would be truly empty (jobFunctions also empty) for BOTH
// names — never one but not the other.
func TestCreateCampaign_EmptyConfigHandledIdenticallyForCustomAndCloudNative(t *testing.T) {
	base := CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://events.example.org/reg",
		BudgetUSD:       100,
		StartDate:       "2099-01-01",
		EndDate:         "2099-02-01",
		GeoTargets:      []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:        []CreativeVariant{{IntroText: "a", Headline: "b"}},
	}

	// (a) Empty cloud-native config but jobFunctions present: ACCEPTED for both.
	for _, profile := range []string{"cloud-native", "custom"} {
		t.Run("accepted/"+profile, func(t *testing.T) {
			srv := fullFlowServer(t)
			defer srv.Close()

			cfg := testConfig()
			cfg.TargetingProfiles = []TargetingProfileConfig{
				{ID: "cloud-native", Label: "Cloud Native", Skills: []string{""}, Groups: nil},
			}
			c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

			in := base
			in.TargetingProfile = profile
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("profile %q with empty cloud-native config + jobFunctions present must be accepted, got: %v", profile, err)
			}
		})
	}

	// (b) Truly-empty assembled criteria (jobFunctions also empty): REJECTED for
	// both, before any POST.
	for _, profile := range []string{"cloud-native", "custom"} {
		t.Run("rejected/"+profile, func(t *testing.T) {
			srv := noPOSTServer(t)
			defer srv.Close()

			cfg := testConfig()
			cfg.TargetingProfiles = []TargetingProfileConfig{
				{ID: "cloud-native", Label: "Cloud Native", Skills: []string{""}, Groups: nil},
			}
			c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))

			saved := jobFunctions
			jobFunctions = nil
			t.Cleanup(func() { jobFunctions = saved })

			in := base
			in.TargetingProfile = profile
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("profile %q with truly-empty assembled criteria: expected rejection before any POST, got nil", profile)
			}
			if !strings.Contains(err.Error(), "empty targeting criteria") {
				t.Errorf("profile %q: error should mention empty targeting criteria, got: %v", profile, err)
			}
		})
	}
}

// TestCreateCampaign_RejectsEmptyAccessTokenBeforeAnyRequest verifies an
// empty/whitespace access token fails fast before any HTTP call, rather than
// sending "Authorization: Bearer " and getting a less actionable API error.
func TestCreateCampaign_RejectsEmptyAccessTokenBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s — should fail before any HTTP call", r.Method, r.URL.Path)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Empty/whitespace-only AND padded tokens must all be rejected before any
	// HTTP call: a padded value like " token " can't be a valid bearer token, so
	// it's a config error, not something to silently trim.
	for _, tok := range []string{"", "   ", "\t\n", " token ", "token\n", "\ttoken"} {
		c := NewClient(Credentials{AccessToken: tok}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        "E",
			Project:          "tlf",
			RegistrationURL:  "https://example.com/reg",
			BudgetUSD:        100,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil {
			t.Errorf("token %q: expected an error, got nil", tok)
		}
	}
}

// ---------------------------------------------------------------------------
// Round 7 Copilot findings
// ---------------------------------------------------------------------------

// TestFindMatch_MatchWithNoUsableIDIsError verifies FINDING 1: a search element
// that SATISFIES the match (same name, live status) but carries no usable id
// under id/$URN/urn must make findByName return an ERROR, not a false ""
// no-match. The search already proved a same-name resource exists; reporting
// "not found" would let a find-or-create caller create a DUPLICATE.
func TestFindMatch_MatchWithNoUsableIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A matching element (right name, ACTIVE) but with EVERY id field empty:
		// id absent, $URN empty, urn empty. No x-restli-id header on a GET search.
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Idless | TLF","status":"ACTIVE","urn":"","$URN":""}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Idless | TLF")
	if err == nil {
		t.Fatalf("expected an error for a matched element with no usable id, got nil (id=%q)", id)
	}
	if id != "" {
		t.Errorf("expected empty id alongside the error, got %q", id)
	}
	if !strings.Contains(err.Error(), "no usable id") {
		t.Errorf("error should explain the id-less match, got: %v", err)
	}
}

// TestCreateCampaign_IdlessGroupMatchIssuesNoCreate verifies FINDING 1 end to
// end: when the campaign-group name lookup returns a MATCHING element with no
// usable id, CreateCampaign must abort with an error and issue NO create POST —
// otherwise it would create a duplicate campaign group despite the search having
// already found one.
func TestCreateCampaign_IdlessGroupMatchIssuesNoCreate(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST %s — an id-less group match must abort before any create", r.URL.Path)
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The group search returns a same-name match (group name is
		// "Events | KubeCon | tlf") but with no usable id.
		if strings.Contains(r.URL.Path, "adCampaignGroups") {
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","urn":""}],"metadata":{"nextPageToken":""}}`)
			return
		}
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("expected CreateCampaign to fail on an id-less group match, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero create POSTs on an id-less match, got %d", postCount)
	}
}

// TestFindMatch_MatchWithTrailingEmptyIDIsError verifies the FINDING 1 refinement:
// a search element that SATISFIES the match and carries a NON-EMPTY raw id/URN
// whose TRAILING segment is empty (e.g. "urn:li:sponsoredCampaignGroup:") must
// make findByName return an ERROR, not "". The raw field passes the non-empty
// guard, but trailingID returns "" — reporting that empty id as a no-match would
// let a find-or-create caller create a DUPLICATE.
func TestFindMatch_MatchWithTrailingEmptyIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Matching element (right name, ACTIVE) whose $URN is a trailing-empty URN:
		// non-empty overall, but trailingID("urn:li:sponsoredCampaignGroup:") == "".
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Trailing | tlf","status":"ACTIVE","$URN":"urn:li:sponsoredCampaignGroup:"}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Trailing | tlf")
	if err == nil {
		t.Fatalf("expected an error for a matched element with a trailing-empty id, got nil (id=%q)", id)
	}
	if id != "" {
		t.Errorf("expected empty id alongside the error, got %q", id)
	}
	if !strings.Contains(err.Error(), "empty trailing segment") {
		t.Errorf("error should explain the trailing-empty id, got: %v", err)
	}
}

// TestCreateCampaign_TrailingEmptyGroupMatchIssuesNoCreate verifies the FINDING 1
// refinement end to end: when the campaign-group name lookup returns a MATCHING
// element whose raw URN is trailing-empty ("urn:li:sponsoredCampaignGroup:"),
// CreateCampaign must abort with an error and issue NO create POST.
func TestCreateCampaign_TrailingEmptyGroupMatchIssuesNoCreate(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST %s — a trailing-empty group match must abort before any create", r.URL.Path)
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "adCampaignGroups") {
			_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","$URN":"urn:li:sponsoredCampaignGroup:"}],"metadata":{"nextPageToken":""}}`)
			return
		}
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("expected CreateCampaign to fail on a trailing-empty group match, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero create POSTs on a trailing-empty match, got %d", postCount)
	}
}

// TestFindMatch_SendsServerSideNameFilter verifies FINDING 2: the search request
// carries a server-side name filter (search=(name:(values:List(<name>)))) so the
// lookup is O(matches), not O(account), and requests the API-max pageSize. It
// also checks that a name containing Rest.li-reserved characters is encoded so it
// can't break out of the List(...) literal.
func TestFindMatch_SendsServerSideNameFilter(t *testing.T) {
	// A name with a comma and parens — Rest.li-reserved inside List(...).
	const lookupName = "Events | KubeCon, Inc. (2026) | TLF"

	var mu sync.Mutex
	var sawSearch, sawPageSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawSearch = r.URL.Query().Get("search")
		sawPageSize = r.URL.Query().Get("pageSize")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Empty result set: a clean 2xx no-match, safe to report absent.
		_, _ = io.WriteString(w, `{"elements":[],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaigns", lookupName)
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty id for an empty result set, got %q", id)
	}

	mu.Lock()
	defer mu.Unlock()
	// r.URL.Query() has already percent-decoded ONE layer, so the value the server
	// observes is the Rest.li-reduced-encoded form: structural delimiters are bare
	// while the NAME's reserved chars remain percent-encoded (%2C, %28, %29).
	if !strings.HasPrefix(sawSearch, "(name:(values:List(") || !strings.HasSuffix(sawSearch, ")))") {
		t.Errorf("search must be a name filter of the form (name:(values:List(...))), got %q", sawSearch)
	}
	// The name's own comma/parens must be Rest.li-encoded so they stay data, not
	// structure. If they were bare, the List(...) literal would be corrupted.
	if !strings.Contains(sawSearch, "KubeCon%2C Inc. %282026%29") {
		t.Errorf("name's reserved chars must be Rest.li-encoded inside List(...), got %q", sawSearch)
	}
	// The bare event-name pipe/spaces survive one url-decode as plain text — they
	// are not Rest.li-structural, so they must NOT be double-encoded.
	if !strings.Contains(sawSearch, "Events | KubeCon") {
		t.Errorf("non-reserved name text must survive as-is, got %q", sawSearch)
	}
	if sawPageSize != "1000" {
		t.Errorf("expected pageSize=1000 (API max), got %q", sawPageSize)
	}
}

// ---------------------------------------------------------------------------
// Ninth-round Copilot findings: id-less CREATE responses
// ---------------------------------------------------------------------------

// baseValidInput returns a CampaignInput that passes all preflight validation so
// tests exercise the create-response paths rather than tripping an earlier guard.
func baseValidInput() CampaignInput {
	return CampaignInput{
		EventName:        "KubeCon",
		Project:          "tlf",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "one"}},
	}
}

// TestCreateCampaign_GroupCreateIDLessURNErrors verifies that a campaign-GROUP
// create response of "urn:li:sponsoredCampaignGroup:" (non-empty but with an
// empty trailing segment) makes CreateCampaign return an error rather than
// proceeding with an invalid group URN.
func TestCreateCampaign_GroupCreateIDLessURNErrors(t *testing.T) {
	var mu sync.Mutex
	var campaignPOSTs int
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
			// Trailing-empty URN: passes the non-empty check but yields "" after
			// trailingID extraction.
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			campaignPOSTs++
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), baseValidInput())
	if err == nil {
		t.Fatal("expected an error for an id-less campaign group create response, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if campaignPOSTs != 0 {
		t.Errorf("no campaign should be created after an id-less group create, got %d campaign POSTs", campaignPOSTs)
	}
}

// TestCreateCampaign_CampaignCreateIDLessURNErrors verifies that a campaign
// create response of "urn:li:sponsoredCampaign:" (non-empty but with an empty
// trailing segment) makes CreateCampaign return an error and does NOT proceed to
// dark-post/creative creation (which would leave an orphaned post).
func TestCreateCampaign_CampaignCreateIDLessURNErrors(t *testing.T) {
	var mu sync.Mutex
	var postAndCreativePOSTs int
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
			// Trailing-empty URN: passes the non-empty check but yields "" after
			// trailingID extraction.
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:"}`)
		case strings.Contains(r.URL.Path, "posts"), strings.Contains(r.URL.Path, "creatives"):
			postAndCreativePOSTs++
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), baseValidInput())
	if err == nil {
		t.Fatal("expected an error for an id-less campaign create response, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if postAndCreativePOSTs != 0 {
		t.Errorf("no dark post or creative should be created after an id-less campaign create, got %d POSTs", postAndCreativePOSTs)
	}
}

// ---------------------------------------------------------------------------
// Ninth-round review findings (David / Copilot on PR #22)
// ---------------------------------------------------------------------------

// TestValidateRegistrationURL_RejectsMalformedQuery verifies a URL whose query
// carries invalid percent-encoding (e.g. "?ticket=%zz") is rejected up front.
// url.Parse accepts it, but url.Query() (used by BuildUTMURL) would silently drop
// the malformed pair, shipping the ad at a different URL. Mirrors the Reddit
// client's malformed-query rejection (Issue A).
func TestValidateRegistrationURL_RejectsMalformedQuery(t *testing.T) {
	for _, bad := range []string{
		"https://events.example.org/reg?ticket=%zz",
		"https://events.example.org/reg?a=%",
		"https://events.example.org/reg?%gg=1",
	} {
		if err := validateRegistrationURL(bad); err == nil {
			t.Errorf("validateRegistrationURL(%q) = nil, want error (malformed query)", bad)
		}
	}
	// A well-formed (properly percent-encoded) query still passes.
	if err := validateRegistrationURL("https://events.example.org/reg?ticket=a%20b&x=1"); err != nil {
		t.Errorf("validateRegistrationURL(well-formed query) = %v, want nil", err)
	}
}

// TestValidateRegistrationURL_ErrorDoesNotLeakURL verifies the validator's error
// messages NEVER echo the caller URL (which can carry secrets in its query or
// userinfo). Every rejection path is checked against the distinctive host/secret
// token. Mirrors the Reddit client, which never surfaces the raw URL (Issue A).
func TestValidateRegistrationURL_ErrorDoesNotLeakURL(t *testing.T) {
	const secret = "s3cr3t-do-not-leak"
	host := "reg.example.org"
	cases := []string{
		// malformed URL (control char forces url.Parse to error)
		"http://" + host + "/\x7f?token=" + secret,
		// non-absolute
		"/relative/path?token=" + secret,
		// malformed query
		"https://" + host + "/reg?token=" + secret + "&bad=%zz",
		// embedded userinfo (composed at runtime so the source has no literal cred)
		urlWithUserinfo("https", "user", secret, host+"/reg"),
		// wrong scheme
		"ftp://" + host + "/reg?token=" + secret,
	}
	for _, raw := range cases {
		err := validateRegistrationURL(raw)
		if err == nil {
			t.Errorf("validateRegistrationURL(%q): want error, got nil", raw)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, secret) {
			t.Errorf("error message leaked the secret query/userinfo: %q", msg)
		}
		if strings.Contains(msg, host) {
			t.Errorf("error message leaked the URL host: %q", msg)
		}
	}
}

// TestAPIError_ErrorOmitsBody verifies apiError.Error() never surfaces the
// upstream response Body (which can reflect request material), while retaining
// the method, path, and status. Mirrors the Reddit client (Issue B).
func TestAPIError_ErrorOmitsBody(t *testing.T) {
	e := &apiError{StatusCode: 400, Method: "POST", Path: "adAccounts/1/adCampaigns", Body: "secret-token-in-body-abc123"}
	msg := e.Error()
	if strings.Contains(msg, "secret-token-in-body-abc123") {
		t.Errorf("apiError.Error() leaked the Body: %q", msg)
	}
	if !strings.Contains(msg, "400") || !strings.Contains(msg, "adCampaigns") || !strings.Contains(msg, "POST") {
		t.Errorf("apiError.Error() should carry method/path/status, got %q", msg)
	}
}

// TestFindByName_EmptyBodySearchIsError verifies a 2xx SEARCH (GET) response with
// an EMPTY body is treated as an error (ambiguous), NOT as a clean "not found".
// A false not-found would let a find-or-create caller create a duplicate paid
// resource. Mirrors Meta/Reddit treating a 2xx-with-no-usable-body as ambiguous
// (Issue C).
func TestFindByName_EmptyBodySearchIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Empty body: no elements, no metadata — cannot prove absence.
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | X | CNCF")
	if err == nil {
		t.Fatal("empty 2xx search body must be an error (ambiguous), got nil (false not-found)")
	}
}

// TestFindByName_DecodesNonExactJSONMediaType verifies a 2xx search body with a
// `+json` media type (e.g. application/vnd.linkedin.normalized+json) — or any
// non-exact Content-Type — is still DECODED, so a real match is found rather than
// mistaken for "not found". Before the fix, the decode was gated on an exact
// "application/json" match, so such a body was left undecoded → false not-found →
// duplicate create (Issue C).
func TestFindByName_DecodesNonExactJSONMediaType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A LinkedIn-flavored +json media type, NOT the exact "application/json".
		w.Header().Set("Content-Type", "application/vnd.linkedin.normalized+json")
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Plus | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:555"}],"metadata":{"nextPageToken":""}}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Plus | CNCF")
	if err != nil {
		t.Fatalf("findByName with +json media type: %v", err)
	}
	if id != "555" {
		t.Errorf("expected match decoded from +json body (id 555), got %q", id)
	}
}

// TestFindByName_EmptyBodyDoesNotTriggerDuplicateCreate verifies the empty-2xx-
// search-body error propagates through the find-or-create flow so NO create POST
// is issued — i.e. an undecodable search never causes a duplicate paid resource
// (Issue C, end to end).
func TestFindByName_EmptyBodyDoesNotTriggerDuplicateCreate(t *testing.T) {
	var mu sync.Mutex
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:1"}`)
			return
		}
		// Search GET: 2xx with an empty body (ambiguous, not "absent").
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), baseValidInput())
	if err == nil {
		t.Fatal("expected an error from the ambiguous empty search body, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("an undecodable search must not trigger a create POST, got %d POSTs", posts)
	}
}

// TestCreateCampaign_AmbiguousCampaignCreateIsUnconfirmed verifies that an
// AMBIGUOUS campaign-create failure (a 5xx — the request reached LinkedIn and may
// have committed) yields an UNCONFIRMED outcome carrying a partial result with the
// campaign NAME (reconcilable by name), NOT a definite failure that a retry would
// treat as safe-to-recreate. Mirrors Meta's createOutcomeAmbiguous handling
// (Issue D).
func TestCreateCampaign_AmbiguousCampaignCreateIsUnconfirmed(t *testing.T) {
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
			// 5xx: LinkedIn received the create and MAY have committed it → ambiguous.
			http.Error(w, "upstream boom", http.StatusBadGateway)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateCampaign(context.Background(), baseValidInput())
	if err == nil {
		t.Fatal("expected an error for the 5xx campaign create, got nil")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("ambiguous (5xx) campaign create should report UNCONFIRMED, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected a partial result carrying the campaign name, got nil")
	}
	if res.CampaignName == "" {
		t.Error("partial result should carry the reconcilable campaign name")
	}
	foundStep := false
	for _, s := range res.Steps {
		if strings.Contains(s, "UNCONFIRMED") {
			foundStep = true
		}
	}
	if !foundStep {
		t.Errorf("expected an UNCONFIRMED step in the result, got steps: %v", res.Steps)
	}
}

// TestCreateCampaign_AmbiguousDarkPostIsUnconfirmed verifies that an AMBIGUOUS
// dark-post failure (a 2xx malformed success with NO id — the post may exist but
// has no reconciliation lookup) reports an UNCONFIRMED outcome rather than a
// definite failure that a blind retry would duplicate. Mirrors Meta's ambiguous
// ad/creative handling (Issue D).
func TestCreateCampaign_AmbiguousDarkPostIsUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			// 2xx with NO id and NO x-restli-id header: malformed success → ambiguous.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), baseValidInput())
	if err == nil {
		t.Fatal("expected an error for the id-less dark post, got nil")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("ambiguous dark post (2xx no id) should report UNCONFIRMED, got: %v", err)
	}
	if res == nil || res.CampaignID == "" {
		t.Fatalf("expected a partial result carrying the created campaign id, got %+v", res)
	}
}

// roundTripFunc adapts a function to http.RoundTripper so a test can inject a
// caller-ctx cancellation mid-flight without a blocking server goroutine.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCreateCampaign_CallerCancelDuringCreateAborts verifies that a CALLER
// context cancellation while a create POST is in flight aborts CLEANLY — the
// returned error wraps context.Canceled and is worded as an "aborted" abort —
// rather than being misclassified as an ambiguous server outcome. doRequest
// wraps context.Canceled as a transportError, so createOutcomeAmbiguous would
// otherwise report a misleading "UNCONFIRMED / verify before recreating" step;
// the ctx.Err() guard at each create-error site must fire first. Contrast with
// TestCreateCampaign_AmbiguousCampaignCreateIsUnconfirmed, where the ctx is NOT
// cancelled and a genuine 5xx must still be UNCONFIRMED.
func TestCreateCampaign_CallerCancelDuringCreateAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Succeed for the group-create POST and all search GETs, but on the campaign
	// create POST cancel the caller ctx and return the context.Canceled error that
	// c.httpClient.Do surfaces mid-flight. Deterministic and non-blocking (no server
	// goroutine to drain). A caller cancellation is a deliberate abort.
	var campaignPosts int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "adCampaigns") && req.Method == http.MethodPost {
			atomic.AddInt32(&campaignPosts, 1)
			cancel()
			return nil, fmt.Errorf("Post %q: %w", req.URL.String(), context.Canceled)
		}
		body := `{"elements":[]}`
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "adCampaignGroups") {
			body = `{"id":"urn:li:sponsoredCampaignGroup:100"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL("http://linkedin.test"),
		WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateCampaign(ctx, baseValidInput())
	if err == nil {
		t.Fatal("expected an error on caller ctx cancellation during the campaign create")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("caller-cancel abort should wrap context.Canceled, got: %v", err)
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("caller-cancel error should be worded as an abort, got: %v", err)
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a caller cancellation must NOT be reported as an ambiguous UNCONFIRMED outcome, got: %v", err)
	}
	if n := atomic.LoadInt32(&campaignPosts); n == 0 {
		t.Fatal("expected the campaign-create POST to be attempted (so the cancel fires in flight)")
	}
	// The abort must not emit a misleading "verify before recreating" UNCONFIRMED step.
	if res != nil {
		for _, s := range res.Steps {
			if strings.Contains(s, "UNCONFIRMED") || strings.Contains(s, "verify before recreating") {
				t.Errorf("caller-cancel abort must not emit an UNCONFIRMED step, got: %q", s)
			}
		}
	}
}

// TestDoRequest_RejectsOversizedResponse verifies a response body larger than
// maxResponseBytes is REJECTED (not silently truncated and mis-parsed). Reading
// maxResponseBytes+1 lets a body of exactly the cap be distinguished from a larger
// truncated one. Mirrors Meta/Reddit's +1 boundary (Issue E).
func TestDoRequest_RejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write a valid-JSON prefix followed by enough padding to exceed the cap, so
		// a naive LimitReader(cap) would truncate to still-parseable JSON and wrongly
		// accept it. maxResponseBytes+64 guarantees we cross the +1 boundary.
		_, _ = io.WriteString(w, `{"elements":[]}`)
		pad := strings.Repeat(" ", maxResponseBytes+64)
		_, _ = io.WriteString(w, pad)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.doRequest(context.Background(), http.MethodGet, "adAccounts/1/adCampaignGroups", nil, map[string]string{"q": "search"})
	if err == nil {
		t.Fatal("expected an error for an oversized response body, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized-response error should mention the cap, got: %v", err)
	}
}

// TestCreateCampaign_SurfacesUnresolvedGeoAsStep verifies that a GeoTarget with an
// empty URN (an unresolved geo the caller still forwarded) is SURFACED as a Step
// rather than silently narrowing the audience, while a valid geo still targets.
// Mirrors how the Meta client surfaces dropped geos (Issue I).
func TestCreateCampaign_SurfacesUnresolvedGeoAsStep(t *testing.T) {
	srv := fullFlowServer(t)
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := baseValidInput()
	// One resolvable geo (URN set) plus one unresolved geo (empty URN, name only).
	in.GeoTargets = []GeoTarget{
		{Label: "United States", URN: "urn:li:geo:103644278"},
		{Label: "Atlantis", URN: ""},
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	foundStep := false
	for _, s := range res.Steps {
		if strings.Contains(s, "not resolved") && strings.Contains(s, "Atlantis") {
			foundStep = true
		}
	}
	if !foundStep {
		t.Errorf("expected a Step surfacing the unresolved geo 'Atlantis', got steps: %v", res.Steps)
	}
}

// TestResolveGeoTargets_ReportsUnresolvedAndIgnoresBlank verifies ResolveGeoTargets
// returns unresolved names (original spelling) and ignores blank/whitespace-only
// inputs entirely (Issue I).
func TestResolveGeoTargets_ReportsUnresolvedAndIgnoresBlank(t *testing.T) {
	resolved, unresolved := ResolveGeoTargets([]string{"Japan", "Narnia", "  ", "", "Gondor"})
	if len(resolved) != 1 || resolved[0].Label != "Japan" {
		t.Errorf("expected only Japan resolved, got %+v", resolved)
	}
	// Blank/whitespace entries are ignored (neither resolved nor reported).
	want := []string{"Narnia", "Gondor"}
	if len(unresolved) != len(want) {
		t.Fatalf("expected unresolved=%v, got %v", want, unresolved)
	}
	for i := range want {
		if unresolved[i] != want[i] {
			t.Errorf("unresolved[%d] = %q, want %q (original spelling)", i, unresolved[i], want[i])
		}
	}
}
