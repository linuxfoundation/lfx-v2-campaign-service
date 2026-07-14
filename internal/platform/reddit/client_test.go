// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testCreds / testAccount are dummy injected values (no real secrets, no env).
var (
	testCreds   = Credentials{ClientID: "cid", ClientSecret: "secret", RefreshToken: "refresh"}
	testAccount = AccountConfig{AccountID: "t2_test", Label: "Test Account"}
)

// TestTokenRefresh_ReuseAndRefresh verifies the expiry-buffer logic: a token is
// reused while valid, and refreshed once it falls within the buffer of expiry.
func TestTokenRefresh_ReuseAndRefresh(t *testing.T) {
	var mu sync.Mutex
	tokenCalls := 0
	issued := []string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenCalls++
		tok := "token-" + string(rune('A'+tokenCalls-1))
		issued = append(issued, tok)
		mu.Unlock()

		// expires_in of 100s; buffer is 60s -> valid window is now+40s.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 100})
	}))
	defer srv.Close()

	// Controllable clock.
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	current := base
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		current = current.Add(d)
	}

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(now))
	ctx := context.Background()

	// First call issues token-A.
	tok, err := c.refreshToken(ctx)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if tok != "token-A" {
		t.Fatalf("first token = %q, want token-A", tok)
	}

	// 30s later: still valid (within 40s window) -> reuse, no new call.
	advance(30 * time.Second)
	tok, err = c.refreshToken(ctx)
	if err != nil {
		t.Fatalf("reuse refresh: %v", err)
	}
	if tok != "token-A" {
		t.Fatalf("expected reused token-A, got %q", tok)
	}
	mu.Lock()
	if tokenCalls != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 token call after reuse, got %d", tokenCalls)
	}
	mu.Unlock()

	// Advance past the buffer boundary: now = base+50s, expiry = base+100s,
	// expiry-buffer = base+40s. 50s > 40s -> must refresh.
	advance(20 * time.Second) // total +50s
	tok, err = c.refreshToken(ctx)
	if err != nil {
		t.Fatalf("post-buffer refresh: %v", err)
	}
	if tok != "token-B" {
		t.Fatalf("expected refreshed token-B, got %q", tok)
	}
	mu.Lock()
	if tokenCalls != 2 {
		mu.Unlock()
		t.Fatalf("expected 2 token calls after buffer expiry, got %d", tokenCalls)
	}
	mu.Unlock()
}

// TestTokenRefresh_SendsBasicAuthAndGrant checks the request shape.
func TestTokenRefresh_SendsBasicAuthAndGrant(t *testing.T) {
	var reqMu sync.Mutex
	var gotAuth, gotBody, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		reqMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL))
	if _, err := c.refreshToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	reqMu.Lock()
	defer reqMu.Unlock()
	// base64("cid:secret")
	if gotAuth != "Basic Y2lkOnNlY3JldA==" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotUA != redditUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, redditUserAgent)
	}
	if !strings.Contains(gotBody, "grant_type=refresh_token") || !strings.Contains(gotBody, "refresh_token=refresh") {
		t.Errorf("body = %q", gotBody)
	}
}

// TestTokenRefresh_EmptyToken rejects a response with an empty access token
// rather than caching garbage.
func TestTokenRefresh_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL))
	if _, err := c.refreshToken(context.Background()); err == nil {
		t.Fatal("expected error for empty access token")
	}
	// Nothing should have been cached.
	c.mu.Lock()
	cached := c.cachedToken
	c.mu.Unlock()
	if cached != "" {
		t.Errorf("cachedToken = %q, want empty after failed refresh", cached)
	}
}

// TestTokenRefresh_NonPositiveExpiry guards a non-positive expires_in by falling
// back to a short default, so the returned token is not cached as pre-expired.
func TestTokenRefresh_NonPositiveExpiry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 0})
	}))
	defer srv.Close()

	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(now))
	tok, err := c.refreshToken(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "tok" {
		t.Fatalf("token = %q, want tok", tok)
	}
	// The fallback expiry must be far enough in the future that the token stays
	// valid past the expiry buffer, so a second call reuses it (no new request).
	if _, err := c.refreshToken(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("token calls = %d, want 1 (token should be reused, not pre-expired)", calls)
	}
}

// TestCreateCampaign_TrimsGeoAndSubreddits verifies whitespace is trimmed and
// empty geo/subreddit entries are dropped before building targeting.
func TestCreateCampaign_TrimsGeoAndSubreddits(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	var mu sync.Mutex
	var adGroupBody map[string]any
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/subreddits") && r.Method == http.MethodGet:
			// Resolve the queried subreddit name to a t5_ ID.
			name := r.URL.Query().Get("query")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "t5_" + name, "name": name},
			}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adGroupBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Trim Test",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{" us ", "", "  ca"},
		// "R/Golang" is a case-insensitive duplicate of "r/golang" and must be
		// dropped; "r/ " is blank after stripping and must be dropped too.
		Subreddits: []string{" r/golang ", "", "  linux  ", "r/ ", "R/Golang"},
		Keywords:   []string{"k8s"},
		Objective:  "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	targeting, _ := adGroupBody["targeting"].(map[string]any)
	geos, _ := targeting["geolocations"].([]any)
	if len(geos) != 2 || geos[0] != "US" || geos[1] != "CA" {
		t.Errorf("geolocations = %v, want [US CA]", geos)
	}
	// Communities carry the subreddit NAMES (r/ stripped, blanks and
	// case-insensitive duplicates removed), NOT t5_ IDs — Reddit targets
	// communities by name and rejects t5_ IDs.
	comms, _ := targeting["communities"].([]any)
	if len(comms) != 2 || comms[0] != "golang" || comms[1] != "linux" {
		t.Errorf("communities = %v, want [golang linux] (deduped, names not IDs)", comms)
	}
}

// TestExtractRedditPostID_InvalidT3 rejects a t3_ prefix whose remainder is not
// a plausible base36 post ID.
func TestExtractRedditPostID_InvalidT3(t *testing.T) {
	for _, in := range []string{"t3_!!!", "t3_", "t3_ab c", "!!!", ""} {
		if _, err := extractRedditPostID(in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
	// Valid t3_ input still passes.
	got, err := extractRedditPostID("t3_abc123")
	if err != nil {
		t.Fatalf("unexpected error for t3_abc123: %v", err)
	}
	if got != "t3_abc123" {
		t.Errorf("got %q, want t3_abc123", got)
	}
}

// TestCreateCampaign_HappyPath drives the full Campaign -> Ad Group -> Ad flow
// against canned JSON.
func TestCreateCampaign_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var campaignBody map[string]any
	var adGroupBody map[string]any

	handler := http.NewServeMux()

	// Token endpoint.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()

		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/subreddits"):
			name := r.URL.Query().Get("query")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "t5_" + name, "name": name},
			}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			campaignBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_123"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adGroupBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_456"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ads"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_789"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})

	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
	)

	in := CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/",
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us", "ca"},
		Subreddits:        []string{"r/opensource", "linux"},
		Keywords:          []string{"kubernetes", "linux"},
		Interests:         []string{"technology"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
		PostURL:           "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	if res.Platform != "reddit-ads" {
		t.Errorf("Platform = %q", res.Platform)
	}
	if res.CampaignID != "camp_123" {
		t.Errorf("CampaignID = %q, want camp_123", res.CampaignID)
	}
	if res.AdGroupID != "ag_456" {
		t.Errorf("AdGroupID = %q, want ag_456", res.AdGroupID)
	}
	if res.AdID != "ad_789" {
		t.Errorf("AdID = %q, want ad_789", res.AdID)
	}
	if res.AdCount != 1 {
		t.Errorf("AdCount = %d, want 1", res.AdCount)
	}
	if res.RedditURL != redditAdsManagerURL {
		t.Errorf("RedditURL = %q", res.RedditURL)
	}
	wantName := "Events | Open Source Summit | NA | Conversions | Intent | Social | tlf | ToFU"
	if res.CampaignName != wantName {
		t.Errorf("CampaignName = %q, want %q", res.CampaignName, wantName)
	}

	// Campaign body assertions (objective-aware, PAUSED, lifetime, microdollars).
	mu.Lock()
	defer mu.Unlock()
	if campaignBody["configured_status"] != "PAUSED" {
		t.Errorf("campaign configured_status = %v", campaignBody["configured_status"])
	}
	if campaignBody["objective"] != "CONVERSIONS" {
		t.Errorf("campaign objective = %v", campaignBody["objective"])
	}
	if campaignBody["goal_type"] != "LIFETIME_SPEND" {
		t.Errorf("campaign goal_type = %v", campaignBody["goal_type"])
	}
	if gv, _ := campaignBody["goal_value"].(float64); gv != 500_000_000 {
		t.Errorf("campaign goal_value = %v, want 500000000", campaignBody["goal_value"])
	}
	if campaignBody["view_through_conversion_type"] != "SEVEN_DAY_CLICKS_ONE_DAY_VIEW" {
		t.Errorf("campaign vt conv type = %v", campaignBody["view_through_conversion_type"])
	}
	// A conversion campaign must carry the conversion pixel it optimizes toward.
	if campaignBody["conversion_pixel_id"] != "pixel_abc" {
		t.Errorf("campaign conversion_pixel_id = %v, want pixel_abc", campaignBody["conversion_pixel_id"])
	}

	// Ad group targeting: communities carry normalized subreddit NAMES (r/ stripped,
	// not t5_ IDs), geos uppercased.
	targeting, _ := adGroupBody["targeting"].(map[string]any)
	comms, _ := targeting["communities"].([]any)
	if len(comms) != 2 || comms[0] != "opensource" || comms[1] != "linux" {
		t.Errorf("communities = %v, want [opensource linux]", comms)
	}
	geos, _ := targeting["geolocations"].([]any)
	if len(geos) != 2 || geos[0] != "US" || geos[1] != "CA" {
		t.Errorf("geolocations = %v, want [US CA]", geos)
	}
	// The conversion ad group must also carry the conversion pixel.
	if adGroupBody["conversion_pixel_id"] != "pixel_abc" {
		t.Errorf("ad group conversion_pixel_id = %v, want pixel_abc", adGroupBody["conversion_pixel_id"])
	}

	// Verify full call sequence. Subreddit names are sent directly as the
	// `communities` targeting value (no name->ID lookup), so there is no
	// /targeting/subreddits GET.
	want := []string{
		"GET /api/v3/ad_accounts/t2_test",
		"POST /api/v3/ad_accounts/t2_test/campaigns",
		"POST /api/v3/ad_accounts/t2_test/ad_groups",
		"POST /api/v3/ad_accounts/t2_test/ads",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestCreateCampaign_CommunityFallback verifies the retry-without-communities
// path when the ad-group create returns "invalid communities".
func TestCreateCampaign_CommunityFallback(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	var agMu sync.Mutex
	var adGroupAttempts int
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/subreddits") && r.Method == http.MethodGet:
			// The name resolves to an ID; the upstream still rejects it at ad-group
			// create, exercising the 400 "invalid communities" fallback backstop.
			name := r.URL.Query().Get("query")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "t5_" + name, "name": name},
			}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			agMu.Lock()
			adGroupAttempts++
			attempt := adGroupAttempts
			agMu.Unlock()
			if attempt == 1 {
				http.Error(w, "invalid communities: bad-sub", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_2"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Subreddits:      []string{"bad-sub"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	agMu.Lock()
	attempts := adGroupAttempts
	agMu.Unlock()
	if attempts != 2 {
		t.Fatalf("expected 2 ad_group attempts, got %d", attempts)
	}
	if res.AdGroupID != "ag_2" {
		t.Errorf("AdGroupID = %q, want ag_2", res.AdGroupID)
	}
	foundFallback := false
	foundSkipped := false
	for _, s := range res.Steps {
		if strings.Contains(s, "retrying without communities") {
			foundFallback = true
		}
		if strings.Contains(s, "communities skipped -- add manually") {
			foundSkipped = true
		}
	}
	if !foundFallback {
		t.Errorf("expected fallback step in %v", res.Steps)
	}
	// A genuine fallback (subreddits supplied but unusable) SHOULD warn that
	// communities were skipped and need manual action.
	if !foundSkipped {
		t.Errorf("expected communities-skipped warning in %v", res.Steps)
	}
}

// TestCreateCampaign_NoCommunityFallbackOn500 verifies the communities-less retry
// is gated on an HTTP 400: a 500 whose body happens to contain "invalid
// communities" must NOT trigger a second ad-group POST, because after an
// ambiguous server failure the ad group may already exist and a blind retry
// could duplicate it.
func TestCreateCampaign_NoCommunityFallbackOn500(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	var agMu sync.Mutex
	var adGroupAttempts int
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			agMu.Lock()
			adGroupAttempts++
			agMu.Unlock()
			// 500 with the phrase in the body — must NOT be treated as the
			// 400 validation fallback.
			http.Error(w, "invalid communities (but a 500!)", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Subreddits:      []string{"golang"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err == nil {
		t.Fatal("expected an error on a 500 ad-group create")
	}
	agMu.Lock()
	attempts := adGroupAttempts
	agMu.Unlock()
	if attempts != 1 {
		t.Errorf("expected exactly 1 ad_group attempt (no fallback on 500), got %d", attempts)
	}
	// The partial result must still carry the created campaign id for reconcile.
	if res == nil || res.CampaignID != "camp_1" {
		t.Errorf("expected partial result with CampaignID camp_1, got %+v", res)
	}
}

// TestCreateCampaign_AdGroupFailureSurfacesCampaignID verifies that when the
// campaign POST succeeds but ad-group creation then fails, CreateCampaign does
// NOT discard the created (PAUSED) campaign: it returns a partial *CampaignResult
// carrying the campaign id + steps so far, AND an error naming the created
// campaign id + PAUSED status, so the orphan is identifiable for cleanup and a
// retry can reconcile it. It also confirms exactly ONE campaign is created in
// this flow (the failure is on the ad group, not a caller retry).
func TestCreateCampaign_AdGroupFailureSurfacesCampaignID(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	var mu sync.Mutex
	var campaignPosts int
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns") && r.Method == http.MethodPost:
			mu.Lock()
			campaignPosts++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_orphan"}})
		case strings.HasSuffix(path, "/ad_groups") && r.Method == http.MethodPost:
			http.Error(w, "ad group quota exceeded", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})

	if err == nil {
		t.Fatalf("expected ad-group failure error, got nil")
	}
	if !strings.Contains(err.Error(), "camp_orphan") {
		t.Errorf("error must name the created campaign id for orphan cleanup; got: %v", err)
	}
	if !strings.Contains(err.Error(), "PAUSED") {
		t.Errorf("error must note the campaign is PAUSED; got: %v", err)
	}

	// Partial result must be returned (not nil) and carry the created campaign id
	// plus the campaign-created step, so callers can reconcile the orphan.
	if res == nil {
		t.Fatalf("expected partial *CampaignResult on post-campaign failure, got nil")
	}
	if res.CampaignID != "camp_orphan" {
		t.Errorf("partial result CampaignID = %q, want camp_orphan", res.CampaignID)
	}
	foundCampaignStep := false
	for _, s := range res.Steps {
		if strings.Contains(s, "Campaign created: camp_orphan") {
			foundCampaignStep = true
		}
	}
	if !foundCampaignStep {
		t.Errorf("partial result must retain the campaign-created step; got steps %v", res.Steps)
	}

	// Exactly one campaign was created: the failure is on the ad group, and this
	// flow must not re-POST a second campaign.
	mu.Lock()
	posts := campaignPosts
	mu.Unlock()
	if posts != 1 {
		t.Errorf("expected exactly 1 campaign POST, got %d", posts)
	}
}

// TestCreateCampaign_NoSubredditsNoSkipWarning verifies FINDING 3: a normal
// keyword/geo-only campaign (no subreddits supplied) must NOT be reported as
// having skipped communities that need manual action.
func TestCreateCampaign_NoSubredditsNoSkipWarning(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	for _, s := range res.Steps {
		if strings.Contains(s, "communities skipped") || strings.Contains(s, "add manually in Reddit Ads Manager") && strings.Contains(s, "communities") {
			t.Errorf("did not expect communities-skipped warning for a no-subreddits campaign, got step: %q\nall steps: %v", s, res.Steps)
		}
	}
}

func TestCreateCampaign_Validation(t *testing.T) {
	c := NewClient(testCreds, testAccount)
	// Every fixture supplies a valid EventName so each case reaches (and actually
	// exercises) the specific validation it targets -- EventName is the FIRST check
	// in CreateCampaign, so omitting it would make every case fail there and mask
	// the budget/date/objective/pixel/video validations under test. Each case
	// asserts a DISTINCTIVE substring of the expected error so an unrelated earlier
	// validation can't silently satisfy the "expected an error" check.
	cases := []struct {
		name    string
		in      CampaignInput
		wantErr string
	}{
		{
			"zero budget",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 0, StartDate: "2026-01-01", EndDate: "2026-01-02"},
			"invalid budget",
		},
		{
			"bad start",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "01-01-2026", EndDate: "2026-01-02"},
			"invalid start date",
		},
		{
			"bad end",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "nope"},
			"invalid end date",
		},
		{
			"end before start",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-02", EndDate: "2026-01-01"},
			"must be after start date",
		},
		{
			// A valid RegistrationURL is required so validation reaches the objective
			// check (URL validation runs before objective validation).
			"bad objective",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "2026-01-02", RegistrationURL: "https://example.com/reg", Objective: "nope"},
			"unsupported Reddit objective",
		},
		{
			// conversions with no pixel must fail at the pixel check, not earlier.
			"conversions missing pixel",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "2026-01-02", RegistrationURL: "https://example.com/reg", Objective: "conversions"},
			"conversion pixel ID is required",
		},
		{
			// video_views with no goal must fail at the video-goal check.
			"video_views missing goal",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "2026-01-02", RegistrationURL: "https://example.com/reg", Objective: "video_views"},
			"invalid video goal",
		},
		{
			// video_views with an unrecognized goal must also fail the video-goal check.
			"video_views bad goal",
			CampaignInput{EventName: "Ev", Project: "tlf", BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "2026-01-02", RegistrationURL: "https://example.com/reg", Objective: "video_views", VideoGoal: "VIDEO_VIEW_99S"},
			"invalid video goal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateCampaign(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error %q does not contain %q", tc.name, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestCreateCampaign_ConversionPixelRejectedBeforeNetwork verifies the
// conversion-objective flow rejects a missing pixel BEFORE any network call, so a
// conversion campaign is never created and then orphaned at ad-group creation for
// want of a pixel.
func TestCreateCampaign_ConversionPixelRejectedBeforeNetwork(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Conv No Pixel",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "conversions",
	})
	if err == nil {
		t.Fatal("expected rejection for conversions objective with no pixel")
	}
	if !strings.Contains(err.Error(), "conversion pixel ID is required") {
		t.Errorf("error %q does not name the missing conversion pixel", err.Error())
	}
	if called.Load() {
		t.Errorf("a network call was made before the conversion-pixel validation")
	}
}

// TestCreateCampaign_ConversionPixelSentWhenPresent verifies that when a pixel is
// supplied for a conversion objective, it is written into BOTH the campaign and
// ad-group payloads, and the full campaign->ad-group flow completes.
func TestCreateCampaign_ConversionPixelSentWhenPresent(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:         "Conv With Pixel",
		Project:           "tlf",
		RegistrationURL:   "https://example.com/reg",
		BudgetUSD:         100,
		StartDate:         "2026-09-01",
		EndDate:           "2026-09-10",
		GeoTargets:        []string{"us"},
		Keywords:          []string{"k8s"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_xyz",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaignBody, adGroupBody := bodies()
	if campaignBody["conversion_pixel_id"] != "pixel_xyz" {
		t.Errorf("campaign conversion_pixel_id = %v, want pixel_xyz", campaignBody["conversion_pixel_id"])
	}
	if adGroupBody["conversion_pixel_id"] != "pixel_xyz" {
		t.Errorf("ad group conversion_pixel_id = %v, want pixel_xyz", adGroupBody["conversion_pixel_id"])
	}
}

// TestCreateCampaign_PixelIgnoredForNonConversion verifies that a stray
// ConversionPixelID carried by a reused input is NOT sent for a non-conversion
// objective (the field is documented as ignored outside conversions), so an
// objective-inapplicable field is never sent upstream.
func TestCreateCampaign_PixelIgnoredForNonConversion(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:         "Traffic With Stray Pixel",
		Project:           "tlf",
		RegistrationURL:   "https://example.com/reg",
		BudgetUSD:         100,
		StartDate:         "2026-09-01",
		EndDate:           "2026-09-10",
		GeoTargets:        []string{"us"},
		Keywords:          []string{"k8s"},
		Objective:         "traffic",
		ConversionPixelID: "pixel_should_be_ignored",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaignBody, adGroupBody := bodies()
	if _, present := campaignBody["conversion_pixel_id"]; present {
		t.Errorf("campaign body must not carry conversion_pixel_id for a non-conversion objective, got %v", campaignBody["conversion_pixel_id"])
	}
	if _, present := adGroupBody["conversion_pixel_id"]; present {
		t.Errorf("ad group body must not carry conversion_pixel_id for a non-conversion objective, got %v", adGroupBody["conversion_pixel_id"])
	}
}

// TestCreateCampaign_VideoGoalRejectedBeforeNetwork verifies the video_views
// objective rejects a missing/invalid video goal BEFORE any network call, so a
// bare "VIDEO_VIEWS" optimization goal is never sent to Reddit.
func TestCreateCampaign_VideoGoalRejectedBeforeNetwork(t *testing.T) {
	for _, goal := range []string{"", "VIDEO_VIEWS", "VIDEO_VIEW_99S"} {
		var called atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, testAccount,
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       "Vid Bad Goal",
			Project:         "tlf",
			RegistrationURL: "https://example.com/reg",
			BudgetUSD:       100,
			StartDate:       "2026-09-01",
			EndDate:         "2026-09-10",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"k8s"},
			Objective:       "video_views",
			VideoGoal:       goal,
		})
		srv.Close()
		if err == nil {
			t.Errorf("video goal %q: expected rejection, got nil", goal)
		} else if !strings.Contains(err.Error(), "invalid video goal") {
			t.Errorf("video goal %q: error %q does not name the invalid video goal", goal, err.Error())
		}
		if called.Load() {
			t.Errorf("video goal %q: a network call was made before video-goal validation", goal)
		}
	}
}

// TestCreateCampaign_VideoGoalMappedToOptimizationGoal verifies that a valid
// concrete video goal is mapped into the campaign and ad-group optimization_goal
// (replacing the invalid bare "VIDEO_VIEWS"), and is case-insensitive.
func TestCreateCampaign_VideoGoalMappedToOptimizationGoal(t *testing.T) {
	for in, want := range map[string]string{
		"VIDEO_VIEW_6S":  "VIDEO_VIEW_6S",
		"video_view_15s": "VIDEO_VIEW_15S",
	} {
		c, bodies, cleanup := newBodyCaptureServers(t)
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       "Vid Goal",
			Project:         "tlf",
			RegistrationURL: "https://example.com/reg",
			BudgetUSD:       100,
			StartDate:       "2026-09-01",
			EndDate:         "2026-09-10",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"k8s"},
			Objective:       "video_views",
			VideoGoal:       in,
		})
		if err != nil {
			cleanup()
			t.Fatalf("VideoGoal %q: CreateCampaign: %v", in, err)
		}
		campaignBody, adGroupBody := bodies()
		if campaignBody["optimization_goal"] != want {
			t.Errorf("VideoGoal %q: campaign optimization_goal = %v, want %v", in, campaignBody["optimization_goal"], want)
		}
		if adGroupBody["optimization_goal"] != want {
			t.Errorf("VideoGoal %q: ad group optimization_goal = %v, want %v", in, adGroupBody["optimization_goal"], want)
		}
		cleanup()
	}
}

func TestExtractRedditPostID(t *testing.T) {
	cases := map[string]string{
		"https://www.reddit.com/r/golang/comments/abc123/title/": "t3_abc123",
		"https://redd.it/xyz789":                                 "t3_xyz789",
		"t3_already":                                             "t3_already",
		"raw123":                                                 "t3_raw123",
	}
	for in, want := range cases {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
	if _, err := extractRedditPostID("!!!"); err == nil {
		t.Error("expected error for invalid input")
	}
}

// TestExtractRedditPostID_SegmentBoundary verifies FINDING 2: the post-ID
// capture is anchored to a path-segment boundary, so a URL whose comments/<id>
// segment carries trailing junk (e.g. "abc123!!!") is REJECTED rather than
// silently truncated to "t3_abc123". Valid boundary forms still parse.
func TestExtractRedditPostID_SegmentBoundary(t *testing.T) {
	valid := map[string]string{
		"https://www.reddit.com/r/golang/comments/abc123":            "t3_abc123",
		"https://www.reddit.com/r/golang/comments/abc123/":           "t3_abc123",
		"https://www.reddit.com/r/golang/comments/abc123/title-slug": "t3_abc123",
		"https://www.reddit.com/r/golang/comments/abc123?x=1":        "t3_abc123",
	}
	for in, want := range valid {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}

	rejected := []string{
		"https://www.reddit.com/r/golang/comments/abc123!!!",
		"https://www.reddit.com/comments/abc123!!!",
	}
	for _, in := range rejected {
		if got, err := extractRedditPostID(in); err == nil {
			t.Errorf("%s: expected rejection, got %q", in, got)
		}
	}
}

// TestExtractRedditPostID_PathAnchored verifies the post-path regex is anchored
// to the START of the parsed path: only "/r/<sub>/comments/<id>" or
// "/comments/<id>" are accepted. A "comments/<id>" appearing elsewhere in the
// path (e.g. a "/user/comments/<id>" overview) must NOT be promoted to a post ID.
func TestExtractRedditPostID_PathAnchored(t *testing.T) {
	valid := map[string]string{
		"https://www.reddit.com/r/opensource/comments/abc123": "t3_abc123",
		"https://www.reddit.com/comments/abc123":              "t3_abc123",
		"https://www.reddit.com/r/x/comments/abc123/title":    "t3_abc123",
		"https://www.reddit.com/comments/abc123?x=1":          "t3_abc123",
	}
	for in, want := range valid {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}

	rejected := []string{
		"https://www.reddit.com/user/comments/abc123",
		"https://www.reddit.com/foo/comments/abc123",
	}
	for _, in := range rejected {
		if got, err := extractRedditPostID(in); err == nil {
			t.Errorf("%s: expected rejection (comments/<id> not at path start), got %q", in, got)
		}
	}
}

// TestExtractRedditPostID_ShortLinkEscapedPath verifies FINDING 2 (round 8):
// the redd.it short-link branch matches against EscapedPath(), not the decoded
// u.Path (same fix already applied to the reddit.com branch). An encoded
// delimiter like %2F stays literal in EscapedPath, so it cannot smuggle
// trailing junk into an otherwise-valid base36 id. Valid short links still
// parse to t3_<id>.
func TestExtractRedditPostID_ShortLinkEscapedPath(t *testing.T) {
	valid := map[string]string{
		"https://redd.it/abc123":     "t3_abc123",
		"https://redd.it/abc123?x=1": "t3_abc123",
		"https://redd.it/abc123/":    "t3_abc123",
	}
	for in, want := range valid {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}

	rejected := []string{
		// %2F is an encoded '/', part of the single path segment -- it must NOT
		// be treated as a real separator that trims "junk" off a valid id.
		"https://redd.it/abc123%2Fjunk",
		// %21 is an encoded '!' -- also part of the segment, not base36.
		"https://redd.it/abc123%21",
	}
	for _, in := range rejected {
		if got, err := extractRedditPostID(in); err == nil {
			t.Errorf("%s: expected rejection (encoded delimiter must stay in-segment), got %q", in, got)
		}
	}
}

// TestCreateCampaign_InvalidGeoRejectedBeforeNetwork verifies FINDING 2:
// GeoTargets that are not ISO 3166-1 alpha-2 codes are rejected up front, before
// any HTTP call (token or API), so a bad value can't create a campaign that then
// orphans at ad-group creation. Valid lowercase/mixed codes still normalize.
func TestCreateCampaign_InvalidGeoRejectedBeforeNetwork(t *testing.T) {
	baseInput := func(geos []string) CampaignInput {
		return CampaignInput{
			EventName:         "Open Source Summit",
			Project:           "tlf",
			EventSlug:         "oss-2026",
			RegistrationURL:   "https://events.linuxfoundation.org/oss/",
			BudgetUSD:         500,
			StartDate:         "2026-08-01",
			EndDate:           "2026-08-31",
			GeoTargets:        geos,
			Subreddits:        []string{"linux"},
			Objective:         "conversions",
			ConversionPixelID: "pixel_abc",
		}
	}

	for _, bad := range []string{"USA", "US/CA", "XX"} {
		// Any network call means validation ran too late; fail loudly if hit.
		// atomic.Bool: written by the httptest handler goroutine, read by the test
		// goroutine — keeps the negative-path assertion race-free under -race.
		var hit atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hit.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, testAccount,
			WithBaseURL(srv.URL+"/api/v3"),
			WithTokenURL(srv.URL),
			WithNowFunc(fixedRedditClock()),
		)
		_, err := c.CreateCampaign(context.Background(), baseInput([]string{bad}))
		srv.Close()
		if err == nil {
			t.Errorf("geo %q: expected rejection, got nil error", bad)
		}
		if hit.Load() {
			t.Errorf("geo %q: network call was made before geo validation", bad)
		}
	}
}

func TestBuildRedditUTMURL(t *testing.T) {
	in := CampaignInput{
		EventName:       "Cloud Native Con",
		Project:         "tlf",
		EventSlug:       "cnc",
		RegistrationURL: "https://example.com/reg/",
		HSToken:         "hs123",
	}
	got := buildRedditUTMURL(in, 0)
	if !strings.HasPrefix(got, "https://example.com/reg?") {
		t.Errorf("url = %q (trailing slash not trimmed / wrong sep)", got)
	}
	for _, want := range []string{"utm_source=reddit", "utm_medium=paid-social", "utm_campaign=hs123", "utm_content=variant-1", "utm_term=cloud-native-con"} {
		if !strings.Contains(got, want) {
			t.Errorf("url %q missing %q", got, want)
		}
	}
}

// TestBuildRedditUTMURL_PreservesQueryOnTrailingSlash verifies finding #1: a
// trailing slash inside a query value (?next=/) is preserved (only a trailing
// PATH slash is trimmed, not the raw URL string), and a path trailing slash is
// still removed.
func TestBuildRedditUTMURL_PreservesQueryOnTrailingSlash(t *testing.T) {
	in := CampaignInput{
		EventName:       "Query Slash",
		Project:         "tlf",
		EventSlug:       "qs",
		RegistrationURL: "https://example.com/reg?next=/",
		HSToken:         "hs123",
	}
	got := buildRedditUTMURL(in, 0)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/reg" {
		t.Errorf("path = %q, want /reg (trailing path slash removed)", u.Path)
	}
	if u.Query().Get("next") != "/" {
		t.Errorf("next = %q, want %q (query value not corrupted)", u.Query().Get("next"), "/")
	}
	if u.Query().Get("utm_source") != "reddit" {
		t.Errorf("utm_source = %q, want reddit; url=%q", u.Query().Get("utm_source"), got)
	}

	// A pure path trailing slash (no query) must be trimmed.
	got2 := buildRedditUTMURL(CampaignInput{
		EventName:       "Path Slash",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg/",
		HSToken:         "hs123",
	}, 0)
	u2, err := url.Parse(got2)
	if err != nil {
		t.Fatalf("parse %q: %v", got2, err)
	}
	if u2.Path != "/reg" {
		t.Errorf("path = %q, want /reg (trailing slash removed)", u2.Path)
	}
}

// fixedRedditClock pins "now" to a point before the 2026-08/09 campaign windows
// used across the CreateCampaign tests, so those tests stay deterministic
// (start dates remain in the future, end-after-start holds) regardless of the
// real wall clock when the suite runs.
func fixedRedditClock() func() time.Time {
	fixed := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// newBodyCaptureServers spins up token + API servers that capture the campaign and
// ad-group request bodies, returning them plus the client. Used by the
// start-time and ad-group-name tests.
func newBodyCaptureServers(t *testing.T) (*Client, func() (map[string]any, map[string]any), func()) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))

	var mu sync.Mutex
	var campaignBody, adGroupBody map[string]any
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			campaignBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adGroupBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	get := func() (map[string]any, map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		return campaignBody, adGroupBody
	}
	cleanup := func() {
		tokenSrv.Close()
		apiSrv.Close()
	}
	return c, get, cleanup
}

// TestCreateCampaign_SameDayStartNotInPast verifies finding #1: a same-day start
// (whose midnight-UTC timestamp is already past) is adjusted BEFORE the campaign
// POST, so the campaign start_time is not in the past.
func TestCreateCampaign_SameDayStartNotInPast(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	// Fix "now" to midday on the start date so its midnight-UTC timestamp is past.
	fixedNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return fixedNow }

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Same Day",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01", // today
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	campaignBody, adGroupBody := bodies()
	campStart, _ := campaignBody["start_time"].(string)
	ts, ok := parseRedditTimestamp(campStart)
	if !ok {
		t.Fatalf("campaign start_time = %q, unparseable", campStart)
	}
	if !ts.After(fixedNow) {
		t.Errorf("campaign start_time %q is not after now %v (finding #1: sent in the past)", campStart, fixedNow)
	}
	// The nudged start must clear the worst-case retry window for a single create,
	// so it stays in the future even if request honors a Retry-After and resends
	// the same timestamp on a 429 (Copilot: start_time may be past after a retry).
	if !ts.After(fixedNow.Add(redditWorstCaseCreateWait)) {
		t.Errorf("campaign start_time %q is only %v ahead of now; must clear the worst-case retry window %v so a retried request never sends a past start", campStart, ts.Sub(fixedNow), redditWorstCaseCreateWait)
	}
	// The ad group must use the same adjusted start.
	agStart, _ := adGroupBody["start_time"].(string)
	if agStart != campStart {
		t.Errorf("ad group start_time %q != campaign start_time %q", agStart, campStart)
	}
}

// TestCreateCampaign_AdGroupNameUsesTrimmedGeo verifies finding #2: the ad-group
// label is built from the trimmed/uppercased geos, not raw padded input.
func TestCreateCampaign_AdGroupNameUsesTrimmedGeo(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Name Test",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{" us ", "", "  ca"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	_, adGroupBody := bodies()
	name, _ := adGroupBody["name"].(string)
	want := "Events | Name Test | US+CA | Intent | Communities + Keywords"
	if name != want {
		t.Errorf("ad group name = %q, want %q (finding #2: raw vs trimmed)", name, want)
	}
}

// TestCreateCampaign_AdWithoutIDIsWarning verifies finding #3: an /ads 200
// response with no data.id is reported as a warning, not a silent success.
func TestCreateCampaign_AdWithoutIDIsWarning(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(path, "/ads"):
			// 200 OK but no data.id -> malformed success.
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "No Ad ID",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AdCount != 0 {
		t.Errorf("AdCount = %d, want 0 (no data.id must not count as a created ad)", res.AdCount)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty", res.AdID)
	}
	foundWarning := false
	for _, s := range res.Steps {
		if strings.Contains(s, "no ad ID") || strings.Contains(s, "malformed") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a malformed-response warning step, got %v", res.Steps)
	}
	// The degraded outcome must also be exposed structurally so a caller can
	// detect it without parsing Steps and can tell it apart from the no-PostURL
	// case (which also has AdCount == 0 but no AdWarning).
	if res.AdWarning == "" {
		t.Error("AdWarning must be set when an ad is attempted but not confirmed")
	}
}

// TestCreateCampaign_NoPostURLHasNoAdWarning verifies the no-ad path (no PostURL,
// no variants) leaves AdWarning empty, so AdWarning distinguishes a genuinely
// failed/unconfirmed ad from the valid "nothing to create" case.
func TestCreateCampaign_NoPostURLHasNoAdWarning(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "No Ad",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AdWarning != "" {
		t.Errorf("AdWarning = %q, want empty for the no-PostURL path", res.AdWarning)
	}
}

// TestCreateCampaign_AdCreateContextCancelledIsFatal verifies that a CALLER
// context cancellation during the final /ads request is FATAL: CreateCampaign
// returns an error wrapping context.Canceled rather than downgrading it to a
// non-fatal warning + nil-error "success". This lets callers distinguish
// cancellation from a completed campaign, honoring the context-aware contract.
func TestCreateCampaign_AdCreateContextCancelledIsFatal(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(path, "/ads"):
			// Cancel the CALLER's context mid-request so the /ads call fails while
			// ctx.Err() != nil. The client's request is built with this ctx, so
			// Do() aborts with context.Canceled. Give it a bounded window to be
			// observed (never respond OK), but never block the handler forever:
			// the timeout keeps the test from hanging if Do() has already returned.
			cancel()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(ctx, CampaignInput{
		EventName:       "Cancelled Ad",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
	})
	if err == nil {
		t.Fatalf("CreateCampaign returned nil error on caller cancellation during /ads; got result %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

// TestCreateCampaign_AdCreateFailureWithLiveCtxIsWarning verifies that an
// ordinary per-request /ads failure while the caller's context is still live
// stays NON-fatal: CreateCampaign returns the campaign result (nil error) with a
// warning step, so a genuine API error does not abort a successfully created
// campaign/ad group. This is the counterpart to the cancellation case above.
func TestCreateCampaign_AdCreateFailureWithLiveCtxIsWarning(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(path, "/ads"):
			// Genuine per-request API failure with a live caller ctx.
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Ad Fails Live Ctx",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
	})
	if err != nil {
		t.Fatalf("CreateCampaign: a per-request /ads failure with a live ctx must stay non-fatal, got err %v", err)
	}
	if res == nil {
		t.Fatal("CreateCampaign returned nil result; the campaign must still be returned")
	}
	if res.AdCount != 0 || res.AdID != "" {
		t.Errorf("AdCount = %d, AdID = %q, want 0 / empty (ad was not created)", res.AdCount, res.AdID)
	}
	foundWarning := false
	for _, s := range res.Steps {
		if strings.Contains(s, "Ad creation failed") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected an 'Ad creation failed' warning step, got %v", res.Steps)
	}
}

// TestBuildRedditUTMURL_PreservesFragment verifies finding #4: a URL carrying a
// fragment keeps it at the very end, with UTM params in the query.
func TestBuildRedditUTMURL_PreservesFragment(t *testing.T) {
	in := CampaignInput{
		EventName:       "Frag Test",
		Project:         "tlf",
		EventSlug:       "frag",
		RegistrationURL: "https://example.com/reg#tickets",
		HSToken:         "hs123",
	}
	got := buildRedditUTMURL(in, 0)
	// The fragment must be last, after the query.
	if !strings.HasSuffix(got, "#tickets") {
		t.Errorf("url = %q, fragment not preserved at end", got)
	}
	if strings.Contains(got, "#tickets?") {
		t.Errorf("url = %q, query embedded inside fragment (finding #4)", got)
	}
	// UTM params must be in the query (before the fragment).
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Fragment != "tickets" {
		t.Errorf("fragment = %q, want tickets", u.Fragment)
	}
	if u.Query().Get("utm_source") != "reddit" {
		t.Errorf("utm_source = %q, want reddit; url=%q", u.Query().Get("utm_source"), got)
	}
}

// TestExtractRedditPostID_HostSpoofRejected verifies finding #5: a URL whose
// authority is attacker-controlled but contains ".reddit.com" in the path is
// rejected, while a genuine reddit.com URL is accepted.
func TestExtractRedditPostID_HostSpoofRejected(t *testing.T) {
	spoof := "https://evil.example/.reddit.com/comments/abc123"
	if _, err := extractRedditPostID(spoof); err == nil {
		t.Errorf("expected rejection of spoofed host URL %q", spoof)
	}
	genuine := "https://www.reddit.com/r/golang/comments/abc123/title/"
	got, err := extractRedditPostID(genuine)
	if err != nil {
		t.Fatalf("genuine URL rejected: %v", err)
	}
	if got != "t3_abc123" {
		t.Errorf("genuine URL -> %q, want t3_abc123", got)
	}
	// Other host spoof shapes must also fail.
	for _, bad := range []string{
		"https://reddit.com.evil.example/comments/xyz789",
		"http://notreddit.com/comments/xyz789",
	} {
		if _, err := extractRedditPostID(bad); err == nil {
			t.Errorf("expected rejection of %q", bad)
		}
	}
}

// TestDecodeID_RejectsNonStringID verifies finding #3: a non-string data.id
// (bool, object, number, array) is treated as absent rather than coerced into a
// bogus value like "true" or "map[]" that a caller might mistake for a valid ID.
func TestDecodeID_RejectsNonStringID(t *testing.T) {
	cases := map[string]string{
		"boolean id":     `{"id": true}`,
		"object id":      `{"id": {"nested": 1}}`,
		"number id":      `{"id": 123}`,
		"array id":       `{"id": ["a"]}`,
		"null id":        `{"id": null}`,
		"absent id":      `{"other": "x"}`,
		"empty id":       `{"id": ""}`,
		"whitespace id":  `{"id": " "}`,
		"multi-space id": `{"id": "   "}`,
		"tab/newline id": `{"id": "\t\n"}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			got := decodeID(&apiResponse{Data: json.RawMessage(data)})
			if got != "" {
				t.Errorf("decodeID(%s) = %q, want empty", data, got)
			}
		})
	}
	// A genuine string id is still returned (and surrounding whitespace trimmed).
	if got := decodeID(&apiResponse{Data: json.RawMessage(`{"id": "camp_1"}`)}); got != "camp_1" {
		t.Errorf("decodeID string id = %q, want camp_1", got)
	}
	if got := decodeID(&apiResponse{Data: json.RawMessage(`{"id": "  camp_1  "}`)}); got != "camp_1" {
		t.Errorf("decodeID padded id = %q, want camp_1", got)
	}
}

func TestToMicrodollars(t *testing.T) {
	if got := toMicrodollars(500); got != 500_000_000 {
		t.Errorf("toMicrodollars(500) = %d", got)
	}
	if got := toMicrodollars(1.5); got != 1_500_000 {
		t.Errorf("toMicrodollars(1.5) = %d", got)
	}
}

// TestCreateCampaign_EmptyAccountIDFailsFast verifies FINDING 2: a client with an
// empty/whitespace AccountID rejects CreateCampaign before building any request
// path, with no network call at all.
func TestCreateCampaign_EmptyAccountIDFailsFast(t *testing.T) {
	for _, id := range []string{"", "   ", "\t\n"} {
		var called atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, AccountConfig{AccountID: id},
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       "No Account",
			Project:         "tlf",
			RegistrationURL: "https://example.com/reg",
			BudgetUSD:       100,
			StartDate:       "2026-09-01",
			EndDate:         "2026-09-10",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"k8s"},
			Objective:       "traffic",
		})
		if err == nil {
			srv.Close()
			t.Fatalf("expected error for account ID %q", id)
		}
		if called.Load() {
			srv.Close()
			t.Errorf("account ID %q: a network call was made; expected fail-fast with none", id)
		}
		srv.Close()
	}
}

// TestCreateCampaign_MalformedAccountIDRejected verifies FINDING 1: an account
// ID whose format is unsafe (contains a path separator, is "."/"..", or carries
// whitespace/control chars) is rejected up front, before any request path is
// built and with no network call. A well-formed "t2_"-style ID passes the
// format check.
func TestCreateCampaign_MalformedAccountIDRejected(t *testing.T) {
	baseInput := func() CampaignInput {
		return CampaignInput{
			EventName:       "Bad Account",
			Project:         "tlf",
			RegistrationURL: "https://example.com/reg",
			BudgetUSD:       100,
			StartDate:       "2026-09-01",
			EndDate:         "2026-09-10",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"k8s"},
			Objective:       "traffic",
		}
	}

	// Malformed IDs must fail fast with no network call.
	// Note: leading/trailing whitespace is trimmed before validation, so these
	// must carry the unsafe character INSIDE the token, not merely around it.
	for _, id := range []string{"a/b", "..", ".", "t2_x/y", "t2 x", "t2\tx", "t2\nx", "t2.x", "%2e%2e"} {
		var called atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, AccountConfig{AccountID: id},
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), baseInput())
		if err == nil {
			srv.Close()
			t.Fatalf("expected error for malformed account ID %q", id)
		}
		if called.Load() {
			srv.Close()
			t.Errorf("account ID %q: a network call was made; expected fail-fast with none", id)
		}
		srv.Close()
	}

	// A well-formed ID passes the format check: the request proceeds to the
	// network layer, so any error must NOT be the format-rejection error.
	{
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := NewClient(testCreds, AccountConfig{AccountID: "t2_abc"},
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), baseInput())
		if err != nil && strings.Contains(err.Error(), "invalid reddit account ID") {
			t.Errorf("valid account ID t2_abc was rejected by format check: %v", err)
		}
	}
}

// TestCreateCampaign_BudgetRoundsToZeroRejected verifies FINDING 3: a positive
// budget that rounds to zero micro-dollars is rejected before any network call,
// while the smallest budget that rounds to >=1 micro-dollar is accepted.
func TestCreateCampaign_BudgetRoundsToZeroRejected(t *testing.T) {
	// Round-to-zero: 0.0000001 USD * 1e6 = 0.1 -> rounds to 0.
	cReject := NewClient(testCreds, testAccount, WithNowFunc(fixedRedditClock()))
	_, err := cReject.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Tiny Budget",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       0.0000001,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err == nil {
		t.Fatal("expected error for budget that rounds to zero micro-dollars")
	}

	// Boundary: 0.0000005 USD * 1e6 = 0.5 -> rounds to 1 micro-dollar (accepted).
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	_, err = c.CreateCampaign(context.Background(), CampaignInput{
		EventName:       "Min Budget",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       0.0000005,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
	})
	if err != nil {
		t.Fatalf("smallest valid budget rejected: %v", err)
	}
	campaignBody, _ := bodies()
	if gv, _ := campaignBody["goal_value"].(int64); gv != 1 {
		// goal_value is set directly as int64 from budgetMicros before JSON marshal,
		// but the captured map decodes JSON back to float64 -- check via float too.
		if gvf, _ := campaignBody["goal_value"].(float64); gvf != 1 {
			t.Errorf("goal_value = %v, want 1 micro-dollar", campaignBody["goal_value"])
		}
	}
}

// TestBuildRedditUTMURL_PreservesEncodedSlash verifies FINDING 4: an encoded
// %2F in the path is preserved (not corrupted into a real separator that then
// gets trimmed), while a genuine literal trailing slash is still removed. The
// emitted URL must round-trip via url.Parse.
func TestBuildRedditUTMURL_PreservesEncodedSlash(t *testing.T) {
	// Encoded %2F must survive: /reg%2F is a single segment "reg/", not a
	// trailing separator, so nothing should be stripped.
	got := buildRedditUTMURL(CampaignInput{
		EventName:       "Encoded Slash",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg%2F",
		HSToken:         "hs123",
	}, 0)
	if !strings.Contains(got, "%2F") && !strings.Contains(got, "%2f") {
		t.Errorf("url = %q, encoded %%2F not preserved", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.EscapedPath() != "/reg%2F" {
		t.Errorf("escaped path = %q, want /reg%%2F (encoded slash corrupted)", u.EscapedPath())
	}
	if u.Query().Get("utm_source") != "reddit" {
		t.Errorf("utm_source = %q, want reddit; url=%q", u.Query().Get("utm_source"), got)
	}

	// A genuine literal trailing slash is still trimmed.
	got2 := buildRedditUTMURL(CampaignInput{
		EventName:       "Real Slash",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg/",
		HSToken:         "hs123",
	}, 0)
	u2, err := url.Parse(got2)
	if err != nil {
		t.Fatalf("parse %q: %v", got2, err)
	}
	if u2.Path != "/reg" {
		t.Errorf("path = %q, want /reg (trailing slash removed)", u2.Path)
	}
}

// TestRefreshToken_DoesNotBlockOnSlowRefresh verifies FINDING 5: refreshToken
// does not hold the client mutex across the token-endpoint HTTP call, so a second
// caller with an already-cancelled context returns promptly instead of blocking
// behind a slow in-flight refresh.
func TestRefreshToken_DoesNotBlockOnSlowRefresh(t *testing.T) {
	// handlerEntered is closed the first time the token handler is entered, so the
	// test can wait until caller 1 is provably inside the blocking network call
	// before it starts caller 2. This makes the ordering deterministic: a sleep
	// alone cannot prove caller 1 reached the handler, so on a loaded runner
	// caller 2 could otherwise run first and let the test pass even if the
	// mutex-across-network-call bug had returned.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	var once sync.Once
	handlerEntered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(handlerEntered) })
		// Block until released (or the request context is cancelled) to simulate a
		// slow token endpoint.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer srv.Close()
	defer releaseHandler()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	// Caller 1: starts a cold refresh that will block inside the (slow) HTTP call.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = c.refreshToken(context.Background())
	}()

	// Wait until caller 1 is provably inside the blocking handler before starting
	// caller 2. If it never gets there, fail rather than hang.
	select {
	case <-handlerEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("caller 1 never reached the token handler")
	}

	// Caller 2: an already-cancelled context. With caller 1 confirmed blocked in
	// the network call, caller 2 must NOT block behind it; it should observe its
	// own cancellation promptly. If it blocks, the mutex is held across the
	// network call (the bug).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.refreshToken(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected caller 2 to fail with a cancelled context")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller 2 blocked behind caller 1's slow refresh (mutex held across network call)")
	}

	// Release the handler so caller 1 finishes, and confirm it does not hang.
	releaseHandler()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("caller 1 did not finish after the handler was released")
	}
}

// TestExtractRedditPostID_EncodedDelimiterRejected verifies FINDING 3: the
// post-path regex runs against EscapedPath(), so an encoded delimiter cannot act
// as a segment boundary and smuggle trailing junk into an otherwise-valid id.
// "/comments/abc123%3Fjunk" (the encoded '?' is part of the id segment) must be
// REJECTED, while a real literal query "/comments/abc123?x=1" still parses.
func TestExtractRedditPostID_EncodedDelimiterRejected(t *testing.T) {
	rejected := []string{
		"https://www.reddit.com/comments/abc123%3Fjunk", // %3F = encoded '?'
		"https://www.reddit.com/r/golang/comments/abc123%3Fjunk",
		"https://www.reddit.com/comments/abc123%2Fjunk", // %2F = encoded '/'
		"https://www.reddit.com/comments/abc123%23junk", // %23 = encoded '#'
	}
	for _, in := range rejected {
		if got, err := extractRedditPostID(in); err == nil {
			t.Errorf("%s: expected rejection (encoded delimiter is part of the id segment), got %q", in, got)
		}
	}

	// A real literal query delimiter (parsed off into RawQuery, absent from
	// EscapedPath) still yields a clean id.
	valid := map[string]string{
		"https://www.reddit.com/comments/abc123?x=1":          "t3_abc123",
		"https://www.reddit.com/r/golang/comments/abc123?x=1": "t3_abc123",
		"https://www.reddit.com/comments/abc123#frag":         "t3_abc123",
	}
	for in, want := range valid {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}

// TestRefreshToken_SingleFlightCoalesces verifies FINDING 1: N concurrent cold
// callers coalesce into exactly ONE upstream token request whose result they all
// share, rather than firing one refresh per caller (rate-limit amplification).
func TestRefreshToken_SingleFlightCoalesces(t *testing.T) {
	const n = 20
	var hits int32
	// gate blocks the single in-flight handler until all callers are parked on
	// the shared result, maximizing the window in which a per-caller refresh
	// (the bug) would show up as extra hits.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		enterOnce.Do(func() { entered <- struct{}{} })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	// joined counts callers that have provably reached the coalescer's park point
	// (its select on ctx.Done()), observed via countingDoneCtx -- deterministic,
	// unlike a wall-clock sleep that can't guarantee all callers have parked.
	var joined int32
	allJoined := make(chan struct{})
	onPark := func() {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	toks := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.refreshToken(&countingDoneCtx{Context: context.Background(), onDone: onPark})
			errs <- err
			toks <- tok
		}()
	}

	// Wait until the leader is inside the handler and all callers have parked on
	// the shared single-flight call, then release the one handler blocked in it.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for the leader refresh to enter the token handler")
	}
	select {
	case <-allJoined:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for all callers to join the shared in-flight refresh")
	}
	close(release)
	wg.Wait()
	close(errs)
	close(toks)

	for err := range errs {
		if err != nil {
			t.Fatalf("cold caller failed: %v", err)
		}
	}
	for tok := range toks {
		if tok != "tok" {
			t.Errorf("caller got token %q, want tok", tok)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (refresh not coalesced)", got)
	}
}

// TestRefreshToken_FollowerUsesLeaderResult verifies that a caller arriving
// while another caller's refresh is in flight waits for it and reuses its result
// instead of issuing a second network refresh. This is the shared-result
// coalescer's follower path: the leader publishes an in-flight *tokenRefresh and
// closes its done channel on completion; the follower shares that same result.
func TestRefreshToken_FollowerUsesLeaderResult(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	entered := make(chan struct{})
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		enterOnce.Do(func() { close(entered) })
		<-release // hold the leader inside the network call until the follower is waiting
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "leader-tok", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL))

	// Leader starts the refresh and blocks inside the handler.
	leaderTok := make(chan string, 1)
	go func() {
		tok, err := c.refreshToken(context.Background())
		if err != nil {
			t.Errorf("leader refresh: %v", err)
		}
		leaderTok <- tok
	}()

	<-entered // leader is now inside the network call, refreshing=true

	// Follower arrives while the leader is in flight; it must wait, not refresh.
	// followerParked is closed when the follower provably reaches the coalescer's
	// park point (its select on ctx.Done()), observed via countingDoneCtx --
	// deterministic, unlike a wall-clock sleep that can't guarantee the follower
	// has begun waiting.
	followerParked := make(chan struct{})
	var parkOnce sync.Once
	followerTok := make(chan string, 1)
	go func() {
		ctx := &countingDoneCtx{Context: context.Background(), onDone: func() { parkOnce.Do(func() { close(followerParked) }) }}
		tok, err := c.refreshToken(ctx)
		if err != nil {
			t.Errorf("follower refresh: %v", err)
		}
		followerTok <- tok
	}()

	// Wait until the follower has reached its wait, then release the leader.
	select {
	case <-followerParked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for the follower to park on the leader's refresh")
	}
	close(release)

	for _, ch := range []chan string{leaderTok, followerTok} {
		select {
		case tok := <-ch:
			if tok != "leader-tok" {
				t.Errorf("token = %q, want leader-tok", tok)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("refresh did not return in time")
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (follower must reuse the leader's refresh)", got)
	}
}

// TestRefreshToken_CancelledContextReturnsPromptly verifies FINDING 1(b): a
// caller whose context is already cancelled returns promptly with its own ctx
// error and never triggers or blocks on a refresh.
func TestRefreshToken_CancelledContextReturnsPromptly(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.refreshToken(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancelled-context caller to return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled-context caller did not return promptly")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("cancelled caller triggered %d token requests, want 0", got)
	}
}

// countingDoneCtx wraps a context and invokes onDone the first time Done() is
// called. The coalescer selects on ctx.Done() exactly at its park point (after a
// caller has joined the shared in-flight refresh), so this makes "caller has
// joined" observable to a test without any time.Sleep or peeking at internals.
type countingDoneCtx struct {
	context.Context
	once   sync.Once
	onDone func()
}

func (c *countingDoneCtx) Done() <-chan struct{} {
	c.once.Do(c.onDone)
	return c.Context.Done()
}

// TestRefreshToken_FailureSharedNoSerialReLead verifies that a FAILED refresh is
// shared across all current waiters: the leader's error is returned to every
// follower of the same in-flight window, so a token-endpoint outage produces
// exactly ONE upstream hit for the burst rather than N serialized re-leads (each
// follower waking on an empty cache and refreshing again in turn). The endpoint
// returns 500 on the first (and only) hit; if the coalescer re-led per follower
// it would climb to a valid response and record N hits.
func TestRefreshToken_FailureSharedNoSerialReLead(t *testing.T) {
	const n = 20

	var hits int32
	// entered is signaled once by the handler when the leader's refresh is inside
	// it; release then gates that single in-flight handler until the test has
	// PROVEN all N callers are joined to the same in-flight refresh. A per-follower
	// re-lead (the bug) would surface as an extra hit. The handler fails on its
	// first (only) invocation.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			entered <- struct{}{}
		}
		<-release
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// Any subsequent hit would be a serial re-lead: succeed so the assertion on
		// error/hit-count catches the regression clearly.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	// joined counts callers that have reached the coalescer's PARK point. Every
	// caller (leader and followers) selects on ctx.Done() when it joins the shared
	// in-flight refresh -- that select is reached only AFTER the caller has read the
	// (non-nil, because the handler is blocked) in-flight refresh and committed to
	// waiting on it, so it can no longer re-lead. countingCtx.Done() is invoked
	// exactly once per caller, at that select, so once it fires N times all N
	// callers are provably joined to the SAME refresh -- no time.Sleep guesswork.
	// allJoined is signaled when the N-th caller commits.
	var joined int32
	allJoined := make(chan struct{})
	onPark := func() {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.refreshToken(&countingDoneCtx{Context: context.Background(), onDone: onPark})
			errs <- err
		}()
	}

	// Wait for the leader to be inside the handler, then wait until all N callers
	// have joined the shared in-flight refresh, and only THEN release the handler
	// to fail. Bounded selects with t.Fatal guard against a hang.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for the leader refresh to enter the token handler")
	}
	select {
	case <-allJoined:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for all callers to join the shared in-flight refresh")
	}
	close(release)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for refresh callers to return")
	}
	close(errs)

	for err := range errs {
		if err == nil {
			t.Error("waiter got a nil error; a failed refresh must fail all its current waiters")
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (failure must be shared, not re-led per follower)", got)
	}
}

// TestExtractRedditPostID_SchemeLessURL verifies scheme-less Reddit links (as the
// TS extractor accepts) resolve: url.Parse would put a scheme-less authority in
// Path with an empty Host, so the client prepends https:// before parsing.
func TestExtractRedditPostID_SchemeLessURL(t *testing.T) {
	ok := map[string]string{
		"reddit.com/r/golang/comments/abc123":         "t3_abc123",
		"www.reddit.com/r/golang/comments/abc123/tit": "t3_abc123",
		"redd.it/abc123":                   "t3_abc123",
		"reddit.com/comments/abc123?utm=x": "t3_abc123",
	}
	for in, want := range ok {
		got, err := extractRedditPostID(in)
		if err != nil {
			t.Errorf("extractRedditPostID(%q) err = %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("extractRedditPostID(%q) = %q, want %q", in, got, want)
		}
	}
	// A scheme-less NON-Reddit host must still be rejected (no SSRF via prepend).
	for _, bad := range []string{"evil.com/r/x/comments/abc123", "notreddit.com/abc123"} {
		if _, err := extractRedditPostID(bad); err == nil {
			t.Errorf("extractRedditPostID(%q) = nil err, want rejection of non-Reddit host", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// 429 rate-limit retry + backoff (mirrors the Meta/Twitter clients)
// ---------------------------------------------------------------------------

// tinyBackoff shrinks the exponential-backoff base so retry tests don't sleep
// for real seconds. Applied via the unexported withRetryBaseDelay option.
const tinyBackoff = 1 * time.Millisecond

// TestRequest_429ThenSuccess verifies a single 429 is retried and the following
// 200 succeeds. The token endpoint and API endpoint use atomic hit counters so
// the test is race-safe under -race.
func TestRequest_429ThenSuccess(t *testing.T) {
	var tokenHits, apiHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&tokenHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&apiHits, 1)
		if n == 1 {
			// No Retry-After header: exercises the exponential-backoff fallback.
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ok_1"}})
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff),
	)

	resp, err := c.request(context.Background(), http.MethodPost, "/thing", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := decodeID(resp); got != "ok_1" {
		t.Errorf("id = %q, want ok_1", got)
	}
	if n := atomic.LoadInt64(&apiHits); n != 2 {
		t.Errorf("api hits = %d, want 2 (one 429, one success)", n)
	}
}

// TestRequest_429ExhaustsRetries verifies that a 429 on every attempt is
// retried exactly retryMax times (retryMax+1 total sends) and then returns the
// rate-limit error rather than looping forever.
func TestRequest_429ExhaustsRetries(t *testing.T) {
	var apiHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff),
	)

	_, err := c.request(context.Background(), http.MethodPost, "/thing", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("expected a rate-limit error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention the 429 status; got: %v", err)
	}
	// retryMax retries => retryMax+1 total sends.
	if n := atomic.LoadInt64(&apiHits); n != int64(retryMax+1) {
		t.Errorf("api hits = %d, want %d (retryMax+1)", n, retryMax+1)
	}
}

// TestParseRetryAfter covers the header parsing matrix: delay-seconds, HTTP-date,
// absent, non-positive, and unparseable values.
func TestParseRetryAfter(t *testing.T) {
	fixed := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(testCreds, testAccount, WithNowFunc(func() time.Time { return fixed }))

	mk := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}

	// Delay-seconds.
	if d, ok := c.parseRetryAfter(mk("5")); !ok || d != 5*time.Second {
		t.Errorf("delay-seconds: got (%v, %v), want (5s, true)", d, ok)
	}
	// HTTP-date 30s in the future.
	future := fixed.Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := c.parseRetryAfter(mk(future)); !ok || d < 29*time.Second || d > 31*time.Second {
		t.Errorf("http-date future: got (%v, %v), want ~30s true", d, ok)
	}
	// HTTP-date in the past -> unusable.
	past := fixed.Add(-30 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := c.parseRetryAfter(mk(past)); ok || d != 0 {
		t.Errorf("http-date past: got (%v, %v), want (0, false)", d, ok)
	}
	// Absent header.
	if d, ok := c.parseRetryAfter(mk("")); ok || d != 0 {
		t.Errorf("absent: got (%v, %v), want (0, false)", d, ok)
	}
	// Non-positive delay.
	if d, ok := c.parseRetryAfter(mk("0")); ok || d != 0 {
		t.Errorf("zero delay: got (%v, %v), want (0, false)", d, ok)
	}
	if d, ok := c.parseRetryAfter(mk("-5")); ok || d != 0 {
		t.Errorf("negative delay: got (%v, %v), want (0, false)", d, ok)
	}
	// Garbage header.
	if d, ok := c.parseRetryAfter(mk("soon")); ok || d != 0 {
		t.Errorf("garbage: got (%v, %v), want (0, false)", d, ok)
	}
}

// TestRequest_429OverCapResetAborts verifies that a 429 whose Retry-After
// declares a reset beyond maxRetryWait aborts immediately with the rate-limit
// error (no sleep, no further retry) rather than burning retries.
func TestRequest_429OverCapResetAborts(t *testing.T) {
	var apiHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		// Declare a reset far beyond maxRetryWait (60s).
		w.Header().Set("Retry-After", strconv.Itoa(int((maxRetryWait+time.Hour)/time.Second)))
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff),
	)

	_, err := c.request(context.Background(), http.MethodPost, "/thing", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("expected an abort error for over-cap reset, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max wait") {
		t.Errorf("error should note the reset exceeds the cap; got: %v", err)
	}
	// Must abort on the first 429 without retrying.
	if n := atomic.LoadInt64(&apiHits); n != 1 {
		t.Errorf("api hits = %d, want 1 (abort on first over-cap 429)", n)
	}
}

// TestRequest_CtxCancelledDuringBackoff verifies that cancelling the context
// during a 429 backoff returns promptly with the context error rather than
// sleeping out the full wait.
func TestRequest_CtxCancelledDuringBackoff(t *testing.T) {
	var apiHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// hit429 is closed by the handler the first time it serves a 429, so the
	// canceller fires only AFTER the client has entered its backoff sleep --
	// deterministic under load, unlike a wall-clock delay that could cancel during
	// token exchange (before any API hit) and leave apiHits at 0. buffered+sync.Once
	// so a (guarded-against) second hit never panics on a closed channel.
	hit429 := make(chan struct{})
	var once sync.Once
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		once.Do(func() { close(hit429) })
		// Long Retry-After (within the cap) so the client enters a real backoff
		// sleep; the cancel below must break it early.
		w.Header().Set("Retry-After", "30")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff),
	)

	// Cancel once the client has actually received a 429 and is in its backoff
	// sleep, so the cancel provably interrupts the backoff (not token exchange).
	go func() {
		<-hit429
		cancel()
	}()

	start := time.Now()
	_, err := c.request(ctx, http.MethodPost, "/thing", map[string]any{"k": "v"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("request took %v; should have returned promptly on cancel, not slept the full 30s", elapsed)
	}
	if n := atomic.LoadInt64(&apiHits); n != 1 {
		t.Errorf("api hits = %d, want 1 (cancel during first backoff)", n)
	}
}

// TestRequest_429BodyRefreshedPerRetry verifies the request body reader is
// re-created on each attempt: bytes.NewReader is consumed by the first send, so
// a retry must supply a fresh reader or the retried request would carry an empty
// body. The server asserts every attempt received the full JSON body.
func TestRequest_429BodyRefreshedPerRetry(t *testing.T) {
	var apiHits int64
	var mu sync.Mutex
	var bodies []string

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&apiHits, 1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n <= 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ok"}})
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff),
	)

	if _, err := c.request(context.Background(), http.MethodPost, "/thing", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("request: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("got %d attempts, want 3", len(bodies))
	}
	for i, b := range bodies {
		if b != `{"k":"v"}` {
			t.Errorf("attempt %d body = %q, want %q (reader not refreshed per retry)", i+1, b, `{"k":"v"}`)
		}
	}
}

// TestTruncate verifies the rune-aware truncate keeps the first n runes without
// converting the whole (potentially large) string to []rune.
func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 3, "hel"},
		{"hello", 5, "hello"},
		{"hello", 10, "hello"},
		{"héllo", 2, "hé"},   // multi-byte rune counts as one
		{"日本語テスト", 3, "日本語"}, // CJK: 3 runes, 9 bytes
		{"", 3, ""},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// TestTokenRefresh_HugeExpiresInDoesNotOverflow verifies that a very large
// positive expires_in is clamped BEFORE the seconds->nanoseconds conversion, so
// the cached expiry lands in the FUTURE rather than overflowing time.Duration and
// wrapping into the past. Without the clamp, `time.Duration(expiresIn)*time.Second`
// wraps negative for expiresIn beyond ~9.2e9 seconds, producing a pre-expired
// token and forcing a fresh refresh on every call.
func TestTokenRefresh_HugeExpiresInDoesNotOverflow(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Far beyond the ~9.2e9-second overflow point for int64 nanoseconds.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": int64(1) << 60})
	}))
	defer srv.Close()

	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(now))
	tok, err := c.refreshToken(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "tok" {
		t.Fatalf("token = %q, want tok", tok)
	}

	// The clamped expiry must be strictly in the future (not wrapped negative).
	c.mu.Lock()
	expiry := c.tokenExpireAt
	c.mu.Unlock()
	if !expiry.After(base) {
		t.Fatalf("tokenExpireAt = %v is not after now = %v; a huge expires_in overflowed to the past", expiry, base)
	}

	// A second call within the (clamped, still-future) window must reuse the
	// cached token rather than refreshing on every call.
	if _, err := c.refreshToken(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("token calls = %d, want 1 (huge expires_in must not force a refresh every call)", calls)
	}
}

// TestParseRetryAfter_HugeSecondsDoesNotWrap verifies the seconds->Duration
// overflow guard: a validly-parsed but astronomically large Retry-After seconds
// value must NOT wrap negative (which would slip past the caller's over-cap
// abort). It is instead reported as usable but over the cap, so request()'s
// `> maxRetryWait` abort fires. A normal in-range value still returns its exact
// duration.
func TestParseRetryAfter_HugeSecondsDoesNotWrap(t *testing.T) {
	fixed := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(testCreds, testAccount, WithNowFunc(func() time.Time { return fixed }))

	mk := func(v string) *http.Response {
		h := http.Header{}
		h.Set("Retry-After", v)
		return &http.Response{Header: h}
	}

	// A huge positive integer that would overflow time.Duration when multiplied
	// by time.Second. Must be reported usable (true) and strictly positive and
	// strictly greater than maxRetryWait, never a wrapped negative value.
	d, ok := c.parseRetryAfter(mk("99999999999"))
	if !ok {
		t.Fatalf("huge Retry-After: ok = false, want true (usable, over-cap)")
	}
	if d <= 0 {
		t.Fatalf("huge Retry-After: d = %v wrapped non-positive; overflow not guarded", d)
	}
	if d <= maxRetryWait {
		t.Errorf("huge Retry-After: d = %v, want > maxRetryWait (%v) so the caller aborts", d, maxRetryWait)
	}

	// A normal in-range value still parses to its exact duration.
	if d, ok := c.parseRetryAfter(mk("5")); !ok || d != 5*time.Second {
		t.Errorf("normal Retry-After: got (%v, %v), want (5s, true)", d, ok)
	}

	// A value EXACTLY at the cap must NOT be treated as over-cap (off-by-one
	// guard): Retry-After equal to maxRetryWait seconds returns exactly that
	// duration so the caller waits the allowed maximum rather than aborting.
	capSecs := strconv.FormatInt(int64(maxRetryWait/time.Second), 10)
	if d, ok := c.parseRetryAfter(mk(capSecs)); !ok || d != maxRetryWait {
		t.Errorf("at-cap Retry-After (%ss): got (%v, %v), want (%v, true)", capSecs, d, ok, maxRetryWait)
	}
}

// TestCreateCampaign_CtxCancelAfterAdGroupReturnsPartial verifies FINDING 4: a
// context cancellation that lands AFTER both the campaign and ad group are created
// must NOT discard their IDs. CreateCampaign returns an error wrapping
// context.Canceled AND a non-nil *CampaignResult carrying the campaign + ad-group
// IDs (plus steps), so the orphan is identifiable for cleanup/reconciliation and a
// caller retry does not blindly create a duplicate campaign.
func TestCreateCampaign_CtxCancelAfterAdGroupReturnsPartial(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_partial"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_partial"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ads"):
			// Cancel the caller ctx, then fail the ad POST. On return, ctx.Err()
			// is set, exercising the post-ad-group ctx-cancel path.
			cancel()
			http.Error(w, "cancelled", http.StatusBadGateway)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	res, err := c.CreateCampaign(ctx, CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
	})

	if err == nil {
		t.Fatalf("expected an error on ctx-cancel after ad-group creation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error must wrap context.Canceled; got: %v", err)
	}
	if res == nil {
		t.Fatalf("expected a non-nil partial *CampaignResult carrying the created IDs, got nil")
	}
	if res.CampaignID != "camp_partial" {
		t.Errorf("partial result CampaignID = %q, want camp_partial", res.CampaignID)
	}
	if res.AdGroupID != "ag_partial" {
		t.Errorf("partial result AdGroupID = %q, want ag_partial", res.AdGroupID)
	}
	if res.CampaignName == "" {
		t.Errorf("partial result CampaignName must be set")
	}
	if res.RedditURL == "" {
		t.Errorf("partial result RedditURL must be set")
	}
	if len(res.Steps) == 0 {
		t.Errorf("partial result must retain the steps completed so far")
	}
}

// TestCreateCampaign_EmptyEventNameRejected verifies an empty/whitespace
// EventName is rejected before any network call, so a campaign with an empty
// name segment can't be created.
func TestCreateCampaign_EmptyEventNameRejected(t *testing.T) {
	c, _, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	for _, ev := range []string{"", "   ", "\t\n"} {
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       ev,
			RegistrationURL: "https://example.com/reg",
			BudgetUSD:       100,
			StartDate:       "2026-09-01",
			EndDate:         "2026-09-10",
			GeoTargets:      []string{"us"},
			Objective:       "traffic",
		})
		if err == nil {
			t.Errorf("EventName %q: expected rejection, got nil", ev)
		}
	}
}

// TestValidateRegistrationURL_RejectsUserinfo verifies a registration URL
// carrying embedded credentials (userinfo) is rejected before use as an ad
// destination.
func TestValidateRegistrationURL_RejectsUserinfo(t *testing.T) {
	for _, raw := range []string{
		"https://user:password@example.com/reg",
		"https://user@example.com/reg",
	} {
		if err := validateRegistrationURL(raw); err == nil {
			t.Errorf("validateRegistrationURL(%q) = nil, want error", raw)
		}
	}
	// A credential-free URL still passes.
	if err := validateRegistrationURL("https://example.com/reg"); err != nil {
		t.Errorf("validateRegistrationURL(clean) = %v, want nil", err)
	}
}

// TestValidateRegistrationURL_RejectsMalformedQuery verifies a URL whose query
// has invalid percent-encoding is rejected up front — url.Parse accepts it but
// u.Query() would silently drop the pair, changing the ad's destination.
func TestValidateRegistrationURL_RejectsMalformedQuery(t *testing.T) {
	if err := validateRegistrationURL("https://example.com/reg?token=%zz"); err == nil {
		t.Error("validateRegistrationURL with malformed query = nil, want error")
	}
	// A well-formed query still passes.
	if err := validateRegistrationURL("https://example.com/reg?a=b&c=d%20e"); err != nil {
		t.Errorf("validateRegistrationURL(well-formed query) = %v, want nil", err)
	}
}

// TestDisplayRedditUTMURL_StripsSecrets verifies the display click URL (used in
// Steps) drops userinfo and any pre-existing query/fragment secrets, keeping only
// the generated utm_* params — while buildRedditUTMURL (the real click_url sent
// to Reddit) still carries the original query.
func TestDisplayRedditUTMURL_StripsSecrets(t *testing.T) {
	in := CampaignInput{
		EventName:       "KubeCon",
		RegistrationURL: "https://user:s3cr3t@example.com/reg?token=abc123&next=/x#frag",
	}
	display := displayRedditUTMURL(in, 0)
	for _, leak := range []string{"s3cr3t", "token", "abc123", "user:", "#frag"} {
		if strings.Contains(display, leak) {
			t.Errorf("display URL %q leaks %q", display, leak)
		}
	}
	if !strings.Contains(display, "utm_source=reddit") {
		t.Errorf("display URL %q dropped the utm_ params", display)
	}
	// The REAL click URL sent to Reddit must still carry the original query so the
	// destination works; only the display copy is sanitized.
	full := buildRedditUTMURL(in, 0)
	if !strings.Contains(full, "token=abc123") {
		t.Errorf("full click URL %q must retain the original query for the real request", full)
	}
}

// TestExtractRedditPostID_ErrorRedactsURL verifies a rejected URL's secrets are
// not echoed in the returned error.
func TestExtractRedditPostID_ErrorRedactsURL(t *testing.T) {
	_, err := extractRedditPostID("https://tok3n@evil.example.com/comments/abc123?secret=xyz")
	if err == nil {
		t.Fatal("expected an error for a non-Reddit host")
	}
	for _, leak := range []string{"tok3n", "secret", "xyz"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err.Error(), leak)
		}
	}
}

// TestExtractRedditPostID_RejectsUserinfo verifies a Reddit post URL carrying
// userinfo is rejected so a token isn't echoed into Steps.
func TestExtractRedditPostID_RejectsUserinfo(t *testing.T) {
	for _, raw := range []string{
		"https://token@reddit.com/r/go/comments/abc123/x",
		"https://user:pw@www.reddit.com/r/go/comments/abc123/x",
	} {
		if _, err := extractRedditPostID(raw); err == nil {
			t.Errorf("extractRedditPostID(%q) = nil error, want error", raw)
		}
	}
	// A credential-free Reddit URL still resolves.
	got, err := extractRedditPostID("https://www.reddit.com/r/go/comments/abc123/x")
	if err != nil {
		t.Fatalf("extractRedditPostID(clean) = %v", err)
	}
	if got != "t3_abc123" {
		t.Errorf("got %q, want t3_abc123", got)
	}
}

// TestStripSubredditPrefix verifies the case-insensitive "r/" prefix strip.
func TestStripSubredditPrefix(t *testing.T) {
	cases := map[string]string{
		"r/golang": "golang",
		"R/golang": "golang",
		"golang":   "golang",
		"r/":       "",
		"R/":       "",
		"rust":     "rust", // no slash -> not a prefix
		"r":        "r",
	}
	for in, want := range cases {
		if got := stripSubredditPrefix(in); got != want {
			t.Errorf("stripSubredditPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRedactURL verifies the query and fragment (potential token carriers) and
// any userinfo are dropped, leaving scheme://host/path.
func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://www.reddit.com/r/go/comments/abc123/x?token=secret#frag": "https://www.reddit.com/r/go/comments/abc123/x",
		"https://user:pw@reddit.com/p?a=b":                                "https://reddit.com/p",
		"https://reddit.com/clean":                                        "https://reddit.com/clean",
		"not a url?token=secret":                                          "not a url",
		// Unparseable URL carrying userinfo must NOT echo the credential: the
		// %zz makes url.Parse fail, so the raw authority (with user:password)
		// would otherwise be returned. It must be fully redacted instead.
		"https://user:password@example.com/%zz": "[unparseable-url-redacted]",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
