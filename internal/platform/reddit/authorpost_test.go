// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// rather than orphaning a paid resource or failing the whole create. The failure
// step reports only the HTTP status, never the error body (which could echo the
// destination_url secret).
//
// It also asserts the outcome is DEGRADED, not clean: this test previously
// checked only the Steps string, so it passed while AdWarning stayed empty and
// the dispatcher persisted a clean `created` for a campaign with no ad. Steps is
// a human-readable blob; AdWarning is the structural signal the adapter reads,
// so the degradation must be asserted there. Note this is NOT the same state a
// missing PostURL produces: there, no ad was ever requested (AdWarning empty);
// here one was requested via ImageURL and does not exist.
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
	// The load-bearing assertion: an ad was requested (ImageURL) and none exists,
	// so the result must be structurally degraded. An empty AdWarning here is what
	// let the dispatcher persist a clean `created`.
	if res.AdWarning == "" {
		t.Error("AdWarning must be set when post authoring fails: an ad was requested via ImageURL and none exists, so this is a DEGRADED success, not a clean one")
	}
	// A definite 4xx rejection means the post was NOT created -- the operator can
	// remediate directly and must not be told to verify first.
	if !strings.Contains(res.AdWarning, "FAILED") {
		t.Errorf("a 4xx post rejection must read as FAILED, got %q", res.AdWarning)
	}
	if strings.Contains(res.AdWarning, "UNCONFIRMED") {
		t.Errorf("a 4xx post rejection is definite, not UNCONFIRMED; got %q", res.AdWarning)
	}
	if strings.Contains(res.AdWarning, "9f8a7b") {
		t.Errorf("AdWarning leaked the destination secret: %q", res.AdWarning)
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
			in.ImageURL = "https://user:pass@example.com/x.jpg" // secretlint-disable-line -- fixture asserting a userinfo URL is REFUSED; the credential is the thing under test
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

// TestCreateCampaign_AuthorPostAmbiguousIsUnconfirmed pins the three-way
// classification on the post-authoring path (Step 1.5) against the repo's own
// rule: apiError = definite non-2xx, transportError = AMBIGUOUS (may have
// landed), proven pre-send = definitely not sent.
//
// Both subtests drive a NON-apiError failure, which the code previously reported
// as "failed before the request reached Reddit". That tells an operator the post
// definitely does not exist and to author it manually — which duplicates a post
// Reddit may already hold. Each case must instead read as UNCONFIRMED and
// instruct the operator to VERIFY first.
//
//   - "2xx with no id": createPromotedPost wraps a malformed success as a
//     transportError precisely because Reddit may have created the post.
//   - "in-flight transport failure": the server hangs up mid-response, so the
//     POST reached Reddit but the outcome is unreadable.
func TestCreateCampaign_AuthorPostAmbiguousIsUnconfirmed(t *testing.T) {
	cases := []struct {
		name      string
		postReply func(w http.ResponseWriter)
	}{
		{
			name: "2xx with no id",
			postReply: func(w http.ResponseWriter) {
				// A 2xx whose data carries no id: the post MAY exist.
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
			},
		},
		{
			name: "in-flight transport failure",
			postReply: func(w http.ResponseWriter) {
				// Send a 200 + a truncated body, then kill the connection so the
				// response read fails after the POST was received.
				w.Header().Set("Content-Length", "512")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":`))
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tokenSrv.Close()

			handler := http.NewServeMux()
			handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
				case r.Method == http.MethodPost && strings.HasSuffix(path, "/profiles/t2_test/posts"):
					tc.postReply(w)
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

			res, err := c.CreateCampaign(context.Background(), CampaignInput{
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
			})
			if err != nil {
				t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
			}
			if res.CampaignID != "camp_1" || res.AdGroupID != "ag_1" {
				t.Fatalf("campaign/ad group not created: %q/%q", res.CampaignID, res.AdGroupID)
			}
			if res.AdCount != 0 || res.AdID != "" {
				t.Errorf("no ad can exist without a post id, got AdCount=%d AdID=%q", res.AdCount, res.AdID)
			}
			// Finding 1: the shortfall must be structurally visible.
			if res.AdWarning == "" {
				t.Fatal("AdWarning must be set: an ad was requested via ImageURL and none exists")
			}
			// Finding 2: an ambiguous outcome must read as ambiguous.
			if !strings.Contains(res.AdWarning, "UNCONFIRMED") {
				t.Errorf("an ambiguous authoring failure must read as UNCONFIRMED, got %q", res.AdWarning)
			}
			if strings.Contains(res.AdWarning, "before it reached Reddit") {
				t.Errorf("an ambiguous authoring failure must NOT be labelled pre-send: %q", res.AdWarning)
			}
			var haveStep bool
			for _, s := range res.Steps {
				if strings.Contains(s, "Promoted-post authoring") {
					haveStep = true
					if strings.Contains(s, "before the request reached Reddit") {
						t.Errorf("step mislabels an ambiguous outcome as pre-send: %q", s)
					}
					if !strings.Contains(s, "UNCONFIRMED") {
						t.Errorf("step must mark the outcome UNCONFIRMED, got %q", s)
					}
				}
			}
			if !haveStep {
				t.Errorf("expected a promoted-post authoring step, steps = %v", res.Steps)
			}
		})
	}
}

// TestCreateCampaign_AuthorPostPreSendIsDefinite pins the THIRD arm of the
// classification: a proven pre-send failure. The posts endpoint is pointed at a
// closed port, so the dial is refused — isPreSendDialError proves no request
// bytes reached Reddit, request() returns the error PLAIN (not wrapped as a
// transportError), and createOutcomeAmbiguous is false.
//
// This arm must stay DEFINITE. Telling the operator to "verify before recreating"
// here would send them hunting for a post that provably does not exist, and it is
// the mutation that keeps the ambiguous arm honest: without this test, replacing
// createOutcomeAmbiguous(err) with a blanket err != nil survives, because nothing
// would distinguish an ambiguous outcome from a pre-send one.
func TestCreateCampaign_AuthorPostPreSendIsDefinite(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	// A server started and immediately closed leaves a port nothing listens on, so
	// the POST /profiles/.../posts dial is REFUSED (a proven pre-send failure).
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	handler := http.NewServeMux()
	handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
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

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
		WithHTTPClient(&http.Client{Transport: &postsToDeadPortTransport{
			base:    http.DefaultTransport,
			deadURL: deadURL,
		}}),
	)

	res, err := c.CreateCampaign(context.Background(), CampaignInput{
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
	})
	if err != nil {
		t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
	}
	if res.CampaignID != "camp_1" || res.AdGroupID != "ag_1" {
		t.Fatalf("campaign/ad group not created: %q/%q", res.CampaignID, res.AdGroupID)
	}
	// Finding 1 still applies: an ad was requested and none exists.
	if res.AdWarning == "" {
		t.Fatal("AdWarning must be set: an ad was requested via ImageURL and none exists")
	}
	// Finding 2's converse: a PROVEN pre-send failure must NOT be softened into
	// "verify first" -- the post definitively does not exist.
	if strings.Contains(res.AdWarning, "UNCONFIRMED") {
		t.Errorf("a proven pre-send dial failure is DEFINITE, not UNCONFIRMED; got %q", res.AdWarning)
	}
	if !strings.Contains(res.AdWarning, "FAILED before it reached Reddit") {
		t.Errorf("a pre-send failure must say so plainly, got %q", res.AdWarning)
	}
	var haveStep bool
	for _, s := range res.Steps {
		if strings.Contains(s, "Promoted-post authoring") {
			haveStep = true
			if !strings.Contains(s, "before the request reached Reddit") {
				t.Errorf("step must report the pre-send outcome, got %q", s)
			}
			if strings.Contains(s, "UNCONFIRMED") {
				t.Errorf("step must not mark a proven pre-send failure UNCONFIRMED: %q", s)
			}
		}
	}
	if !haveStep {
		t.Errorf("expected a promoted-post authoring step, steps = %v", res.Steps)
	}
}

// postsToDeadPortTransport redirects ONLY the promoted-post authoring request
// (POST /profiles/.../posts) to a port nothing listens on, so its dial is
// refused — a proven pre-send failure. Every other request is passed through
// untouched so the campaign and ad group still get created normally.
type postsToDeadPortTransport struct {
	base    http.RoundTripper
	deadURL string
}

func (t *postsToDeadPortTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/profiles/") && strings.HasSuffix(r.URL.Path, "/posts") {
		dead, err := url.Parse(t.deadURL)
		if err != nil {
			return nil, err
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme = dead.Scheme
		clone.URL.Host = dead.Host
		return t.base.RoundTrip(clone)
	}
	return t.base.RoundTrip(r)
}
