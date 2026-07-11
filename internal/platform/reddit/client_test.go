// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{" us ", "", "  ca"},
		Subreddits:      []string{" r/golang ", "", "  linux  ", "r/ "},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
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
	comms, _ := targeting["communities"].([]any)
	if len(comms) != 2 || comms[0] != "golang" || comms[1] != "linux" {
		t.Errorf("communities = %v, want [golang linux]", comms)
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
		EventName:       "Open Source Summit",
		EventSlug:       "oss-2026",
		RegistrationURL: "https://events.linuxfoundation.org/oss/",
		BudgetUSD:       500,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		GeoTargets:      []string{"us", "ca"},
		Subreddits:      []string{"r/opensource", "linux"},
		Keywords:        []string{"kubernetes", "linux"},
		Interests:       []string{"technology"},
		Objective:       "conversions",
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
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

	// Ad group targeting: communities stripped of r/ prefix, geos uppercased.
	targeting, _ := adGroupBody["targeting"].(map[string]any)
	comms, _ := targeting["communities"].([]any)
	if len(comms) != 2 || comms[0] != "opensource" || comms[1] != "linux" {
		t.Errorf("communities = %v, want [opensource linux]", comms)
	}
	geos, _ := targeting["geolocations"].([]any)
	if len(geos) != 2 || geos[0] != "US" || geos[1] != "CA" {
		t.Errorf("geolocations = %v, want [US CA]", geos)
	}

	// Verify full call sequence.
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
	cases := []struct {
		name string
		in   CampaignInput
	}{
		{"zero budget", CampaignInput{BudgetUSD: 0, StartDate: "2026-01-01", EndDate: "2026-01-02"}},
		{"bad start", CampaignInput{BudgetUSD: 10, StartDate: "01-01-2026", EndDate: "2026-01-02"}},
		{"bad end", CampaignInput{BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "nope"}},
		{"end before start", CampaignInput{BudgetUSD: 10, StartDate: "2026-01-02", EndDate: "2026-01-01"}},
		// A valid RegistrationURL is required so validation reaches the objective
		// check (URL validation runs before objective validation in CreateCampaign).
		{"bad objective", CampaignInput{BudgetUSD: 10, StartDate: "2026-01-01", EndDate: "2026-01-02", RegistrationURL: "https://example.com/reg", Objective: "nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateCampaign(context.Background(), tc.in); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
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
			EventName:       "Open Source Summit",
			EventSlug:       "oss-2026",
			RegistrationURL: "https://events.linuxfoundation.org/oss/",
			BudgetUSD:       500,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			GeoTargets:      geos,
			Subreddits:      []string{"linux"},
			Objective:       "conversions",
		}
	}

	for _, bad := range []string{"USA", "US/CA", "XX"} {
		// Any network call means validation ran too late; fail loudly if hit.
		hit := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hit = true
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
		if hit {
			t.Errorf("geo %q: network call was made before geo validation", bad)
		}
	}
}

func TestBuildRedditUTMURL(t *testing.T) {
	in := CampaignInput{
		EventName:       "Cloud Native Con",
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
}

// TestBuildRedditUTMURL_PreservesFragment verifies finding #4: a URL carrying a
// fragment keeps it at the very end, with UTM params in the query.
func TestBuildRedditUTMURL_PreservesFragment(t *testing.T) {
	in := CampaignInput{
		EventName:       "Frag Test",
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
		var called bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, AccountConfig{AccountID: id},
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:       "No Account",
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
		if called {
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
		var called bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			http.Error(w, "should not be called", http.StatusInternalServerError)
		}))
		c := NewClient(testCreds, AccountConfig{AccountID: id},
			WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		_, err := c.CreateCampaign(context.Background(), baseInput())
		if err == nil {
			srv.Close()
			t.Fatalf("expected error for malformed account ID %q", id)
		}
		if called {
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
	var hits int32
	// gate blocks the single in-flight handler until all callers are parked on
	// the shared result, maximizing the window in which a per-caller refresh
	// (the bug) would show up as extra hits.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	toks := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.refreshToken(context.Background())
			errs <- err
			toks <- tok
		}()
	}

	// Give all callers time to reach and park on the shared single-flight call,
	// then release the one handler that's blocked inside it.
	time.Sleep(100 * time.Millisecond)
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

// TestRefreshToken_InFlightRechecksCache verifies FINDING 1 (round 8): the
// single-flight work function re-checks the cached token under the lock before
// any network call. This covers the "first completes before second dispatches"
// ordering that plain coalescing misses: if a prior refresh COMPLETED (and
// cached a valid token) in the window after this caller failed the fast path
// but before its DoChan work function runs, single-flight will NOT coalesce
// this caller with the prior one (that call already returned). Without the
// in-flight re-check, this caller would then issue a duplicate token request.
// With it, the work function observes the fresh cache and returns it -- no
// network call at all.
//
// The exact interleaving is made deterministic by a call-counting clock: it
// reports an EXPIRED time on its first observation (the fast-path check, which
// must MISS so the caller proceeds into DoChan) and a VALID time on every
// later observation (the work-function re-check, which must HIT and return the
// seeded token). The token endpoint fails the test if it is ever contacted.
// TestRefreshToken_FollowerUsesLeaderResult verifies that a caller arriving
// while another caller's refresh is in flight waits for it and reuses its result
// (via the freshly-populated cache) instead of issuing a second network refresh.
// This is the stdlib coalescer's follower path: the leader holds refreshing=true
// and closes refreshWait on completion; the follower then re-reads the cache.
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
	followerTok := make(chan string, 1)
	go func() {
		tok, err := c.refreshToken(context.Background())
		if err != nil {
			t.Errorf("follower refresh: %v", err)
		}
		followerTok <- tok
	}()

	// Give the follower a moment to reach the wait, then release the leader.
	time.Sleep(50 * time.Millisecond)
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
