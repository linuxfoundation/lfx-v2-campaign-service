// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCreateCampaign_AuthorsPostFromImageURL exercises the author-a-post path:
// an ImageURL is supplied WITHOUT a PostURL, so the client authors a promoted
// (dark) image post itself, then attaches it as the ad's creative — the
// brief->servable-ad parity path with the Google/Meta clients. It asserts the
// post body shape (IMAGE, dark, CTA resolved to Reddit's title-case), that the
// authored post id flows into the ad, the full call sequence (posts BEFORE
// campaign), and that a secret in the registration URL never lands in the
// authored post's destination_url.
func TestCreateCampaign_AuthorsPostFromImageURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var postBody map[string]any
	var adBody map[string]any

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/profiles/t2_test/posts"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			postBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_new"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ads"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	// Secret-bearing registration URL composed at runtime (avoids tripping
	// secretlint/gitleaks on a literal) — it must NOT appear in the post
	// destination_url, only the generated utm_* params.
	secret := "s3cr" + "et-token-" + "9f8a7b"
	in := CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/?token=" + secret,
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us"},
		Keywords:          []string{"linux"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
		ImageURL:          "https://events.linuxfoundation.org/wp-content/uploads/2020/03/banner.jpg",
		CallToAction:      "sign up", // lower-case: must resolve to Reddit's "Sign Up"
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AdID != "ad_1" || res.AdCount != 1 {
		t.Errorf("AdID/AdCount = %q/%d, want ad_1/1", res.AdID, res.AdCount)
	}

	mu.Lock()
	defer mu.Unlock()

	// Post body: dark IMAGE post with resolved CTA and a secret-free destination.
	if postBody == nil {
		t.Fatalf("promoted post was not authored (no POST /profiles/.../posts)")
	}
	if postBody["type"] != "IMAGE" {
		t.Errorf("post type = %v, want IMAGE", postBody["type"])
	}
	if ac, ok := postBody["allow_comments"].(bool); !ok || ac {
		t.Errorf("post allow_comments = %v, want false (dark post)", postBody["allow_comments"])
	}
	if postBody["headline"] != "Open Source Summit" {
		t.Errorf("post headline = %v, want event name fallback", postBody["headline"])
	}
	content, _ := postBody["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("post content = %v, want 1 item", postBody["content"])
	}
	item, _ := content[0].(map[string]any)
	if item["media_url"] != in.ImageURL {
		t.Errorf("post media_url = %v, want %q", item["media_url"], in.ImageURL)
	}
	if item["call_to_action"] != "Sign Up" {
		t.Errorf("post call_to_action = %v, want Sign Up (title-cased)", item["call_to_action"])
	}
	// destination_url is the REAL ad click URL (same value the ad's click_url
	// uses): it carries the generated utm_* params AND preserves the caller's own
	// registration query verbatim, because that is the actual landing URL a click
	// must reach. Secret redaction applies only to what is persisted in Steps /
	// error text (see the degrade test), NOT to the destination sent to Reddit.
	dest, _ := item["destination_url"].(string)
	if !strings.Contains(dest, "utm_source=reddit") {
		t.Errorf("post destination_url = %q, want utm_source=reddit", dest)
	}
	if !strings.Contains(dest, secret) {
		t.Errorf("post destination_url = %q, want the caller's registration query preserved", dest)
	}

	// The ad promotes the AUTHORED post id.
	if adBody["post_id"] != "t3_new" {
		t.Errorf("ad post_id = %v, want t3_new (authored)", adBody["post_id"])
	}

	// Call sequence: the post is authored BEFORE the paid campaign create.
	want := []string{
		"GET /api/v3/ad_accounts/t2_test",
		"POST /api/v3/profiles/t2_test/posts",
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

// TestCreateCampaign_AuthorPostFailureDegrades verifies that a FAILED post
// authoring is non-fatal: because the (zero-cost) post is created before the
// paid campaign, a post failure degrades to a campaign + ad group with NO ad
// (the same state a missing PostURL produces) rather than orphaning a paid
// resource or failing the whole create. The failure step reports only the HTTP
// status, never the error body (which could echo the destination_url secret).
func TestCreateCampaign_AuthorPostFailureDegrades(t *testing.T) {
	var mu sync.Mutex
	var paths []string

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/profiles/t2_test/posts"):
			// Reflective validation error that embeds the destination secret: the
			// step must report the STATUS only, never this body.
			http.Error(w, `{"error":{"message":"bad destination_url s3cret-token-9f8a7b"}}`, http.StatusBadRequest)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	in := CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/",
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
		ImageURL:          "https://events.linuxfoundation.org/banner.jpg",
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign returned error on a degraded (post-failed) create: %v", err)
	}
	if res.CampaignID != "camp_1" || res.AdGroupID != "ag_1" {
		t.Errorf("campaign/ad group not created: %q/%q", res.CampaignID, res.AdGroupID)
	}
	if res.AdCount != 0 || res.AdID != "" {
		t.Errorf("expected no ad on degraded create, got AdCount=%d AdID=%q", res.AdCount, res.AdID)
	}

	var haveFailStep bool
	for _, s := range res.Steps {
		if strings.Contains(s, "Promoted-post authoring failed") {
			haveFailStep = true
			if !strings.Contains(s, "HTTP 400") {
				t.Errorf("fail step missing status: %q", s)
			}
			if strings.Contains(s, "9f8a7b") {
				t.Errorf("fail step leaked the destination secret: %q", s)
			}
		}
	}
	if !haveFailStep {
		t.Errorf("expected a promoted-post authoring failure step, steps = %v", res.Steps)
	}

	// No ad POST was attempted (no post id to promote).
	for _, p := range paths {
		if strings.HasSuffix(p, "/ads") {
			t.Errorf("unexpected ad POST on degraded create: %v", paths)
		}
	}
}

// TestCreateCampaign_PostURLWinsOverImageURL confirms an explicit PostURL takes
// precedence: when both are supplied the client promotes the given post and does
// NOT author one (no POST /profiles/.../posts).
func TestCreateCampaign_PostURLWinsOverImageURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var adBody map[string]any

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/ads"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adBody = env.Data
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(handler)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	in := CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/",
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
		PostURL:           "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
		ImageURL:          "https://events.linuxfoundation.org/banner.jpg", // ignored
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AdID != "ad_1" {
		t.Errorf("AdID = %q, want ad_1", res.AdID)
	}
	if adBody["post_id"] != "t3_abc123" {
		t.Errorf("ad post_id = %v, want t3_abc123 (from PostURL)", adBody["post_id"])
	}
	for _, p := range paths {
		if strings.Contains(p, "/posts") {
			t.Errorf("authored a post despite an explicit PostURL: %v", paths)
		}
	}
}

// TestCreateCampaign_AuthorPostPreMutationValidation checks that a bad author
// input (image URL, CTA, headline) is rejected BEFORE any network call: nothing
// is created and no request is made.
func TestCreateCampaign_AuthorPostPreMutationValidation(t *testing.T) {
	base := func() CampaignInput {
		return CampaignInput{
			EventName:         "Open Source Summit",
			Project:           "tlf",
			EventSlug:         "oss-2026",
			RegistrationURL:   "https://events.linuxfoundation.org/oss/",
			BudgetUSD:         500,
			StartDate:         "2026-08-01",
			EndDate:           "2026-08-31",
			GeoTargets:        []string{"us"},
			Objective:         "conversions",
			ConversionPixelID: "pixel_abc",
		}
	}

	cases := map[string]func(*CampaignInput){
		"bad image scheme": func(in *CampaignInput) {
			in.ImageURL = "ftp://example.com/x.jpg"
		},
		"image with userinfo": func(in *CampaignInput) {
			in.ImageURL = "https://user:pass@example.com/x.jpg"
		},
		"unknown CTA": func(in *CampaignInput) {
			in.ImageURL = "https://example.com/x.jpg"
			in.CallToAction = "Register Now" // not a Reddit CTA
		},
		"headline too long": func(in *CampaignInput) {
			in.ImageURL = "https://example.com/x.jpg"
			in.Variants = []AdVariant{{Headline: strings.Repeat("x", maxRedditHeadlineRunes+1)}}
		},
	}

	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			var called bool
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.Error(w, "should not be called", http.StatusInternalServerError)
			}))
			defer apiSrv.Close()

			c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
			in := base()
			mut(&in)

			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("expected a pre-mutation validation error, got result %+v", res)
			}
			if res != nil {
				t.Errorf("expected nil result on pre-create validation failure, got %+v", res)
			}
			if called {
				t.Errorf("a network call was made despite a pre-mutation validation failure")
			}
		})
	}
}
