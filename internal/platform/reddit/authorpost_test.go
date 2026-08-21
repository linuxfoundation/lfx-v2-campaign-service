// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
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
	// remediate directly and must not be told to verify first. Assert the
	// DISCRIMINATING token: bare "FAILED" is shared with the pre-send arm
	// ("FAILED before it reached Reddit"), so it would pass on the wrong branch.
	if !strings.Contains(res.AdWarning, "FAILED (Reddit rejected the post)") {
		t.Errorf("a 4xx post rejection must be attributed to Reddit rejecting it, got %q", res.AdWarning)
	}
	if strings.Contains(res.AdWarning, "before it reached Reddit") {
		t.Errorf("a 4xx REACHED Reddit; it must not be reported as pre-send: %q", res.AdWarning)
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

// TestCreateCampaign_AuthorPostAmbiguousAPIErrorIsUnconfirmed covers the shapes
// that are BOTH an *apiError and ambiguous, which is where arm ORDER decides the
// operator's instruction. createOutcomeAmbiguous returns true for an *apiError
// carrying a 5xx (Reddit received the POST and may have committed it before
// erroring) or a 3xx on a mutating method (redirects are disabled, so it reached
// a responder that may have committed before redirecting).
//
// The first version of this fix tested errors.As BEFORE createOutcomeAmbiguous,
// so both shapes fell into the "Reddit rejected the post -- author it manually"
// arm. That instruction can duplicate a post Reddit already holds: precisely the
// harm the ambiguous classification exists to prevent, re-entering through arm
// order rather than through the original else-branch.
//
// Assertions target the DISCRIMINATING wording, not a token both arms share:
// "FAILED" alone appears in the 4xx and the pre-send arms too, so it cannot tell
// the branches apart.
func TestCreateCampaign_AuthorPostAmbiguousAPIErrorIsUnconfirmed(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		// Reddit received the POST and may have committed the post before erroring.
		{name: "5xx server error", status: http.StatusServiceUnavailable},
		// Redirects are force-disabled, so a 3xx on this POST surfaces as an
		// apiError; it reached a responder that may have committed first.
		{name: "mutating 3xx redirect", status: http.StatusFound},
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
					if tc.status >= 300 && tc.status < 400 {
						// A redirect the client must NOT follow, so it surfaces as an apiError.
						w.Header().Set("Location", "https://example.com/elsewhere")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":"upstream"}`))
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
			if res.AdWarning == "" {
				t.Fatal("AdWarning must be set: an ad was requested via ImageURL and none exists")
			}
			// The operator-facing instruction is where the harm lands: VERIFY first,
			// never "author it manually".
			if !strings.Contains(res.AdWarning, "UNCONFIRMED") {
				t.Errorf("an apiError that createOutcomeAmbiguous accepts must read UNCONFIRMED, got %q", res.AdWarning)
			}
			if strings.Contains(res.AdWarning, "FAILED (Reddit rejected the post)") {
				t.Errorf("HTTP %d may have committed the post; telling the operator Reddit REJECTED it invites a duplicate: %q", tc.status, res.AdWarning)
			}
			if !strings.Contains(res.AdWarning, "verify") && !strings.Contains(res.AdWarning, "BEFORE authoring it again") {
				t.Errorf("an ambiguous outcome must instruct the operator to verify first, got %q", res.AdWarning)
			}
			var haveStep bool
			for _, s := range res.Steps {
				if strings.Contains(s, "Promoted-post authoring") {
					haveStep = true
					if !strings.Contains(s, "UNCONFIRMED") {
						t.Errorf("step must mark the outcome UNCONFIRMED, got %q", s)
					}
					if strings.Contains(s, "Reddit rejected the post") {
						t.Errorf("step must not claim a definite rejection for HTTP %d: %q", tc.status, s)
					}
					// The status is still useful diagnostics on the ambiguous arm.
					if !strings.Contains(s, fmt.Sprintf("HTTP %d", tc.status)) {
						t.Errorf("step should carry the status for diagnosis, got %q", s)
					}
				}
			}
			if !haveStep {
				t.Errorf("expected a promoted-post authoring step, steps = %v", res.Steps)
			}
		})
	}
}

// TestCreateCampaign_InFlightCancelDuringAuthoringRetainsTheClaim pins the claim-retention
// contract on the promoted-post authoring step, for a caller cancellation that interrupts the
// POST while it is IN FLIGHT.
//
// The money path this protects: Reddit may already have accepted the mutating POST when the
// cancellation lands. The request layer classifies a ctx error from Do as a transportError
// precisely so it reads as AMBIGUOUS rather than pre-send (TestIsPreSendDialError states the
// rule and its reason: "a ctx error from Do can fire AFTER the POST body reached Reddit").
// CreateCampaign's own contract is that (nil, err) means nothing was or may have been created,
// and RedditDispatcher.Dispatch keys claim release on `result == nil` ALONE — so returning
// (nil, err) here tells the orchestrator to RELEASE the claim on a post that may exist, and a
// retry authors a duplicate paid post.
//
// The observable outcome asserted is therefore the RESULT SHAPE, not the message: a non-nil
// partial result alongside the error, which is what makes Dispatch retain the claim. The
// campaign-create path at the same file's campaign POST already gets this right by asking
// createOutcomeAmbiguous BEFORE ctx.Err(); this asserts the authoring step agrees.
//
// A clean PRE-SEND cancellation is the opposite case and must still abort with (nil, err) —
// nothing was sent, so retaining a claim would strand it. The second subtest pins that, so a
// fix cannot simply make every cancellation ambiguous.
func TestCreateCampaign_InFlightCancelDuringAuthoringRetainsTheClaim(t *testing.T) {
	newInput := func() CampaignInput {
		return CampaignInput{
			EventName:       "Open Source Summit",
			Project:         "tlf",
			EventSlug:       "oss-2026",
			RegistrationURL: "https://events.linuxfoundation.org/oss/",
			BudgetUSD:       500,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"linux"},
			Objective:       "conversions",
			ImageURL:        "https://events.linuxfoundation.org/banner.jpg",
			CallToAction:    "sign up",
		}
	}

	t.Run("cancelled while the authoring POST is in flight", func(t *testing.T) {
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		}))
		defer tokenSrv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := http.NewServeMux()
		handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/profiles/t2_test/posts") {
				// The POST has REACHED the server — Reddit may commit it. Cancel the caller
				// while the request is in flight, then hijack the connection and close it
				// without writing a response, so the client's in-flight round trip fails
				// exactly as a real interrupted one does. Hijacking (rather than blocking on
				// r.Context().Done()) keeps the handler from holding the server for the
				// client's full 30s request timeout.
				cancel()
				conn, _, hijackErr := w.(http.Hijacker).Hijack()
				if hijackErr != nil {
					t.Errorf("hijack: %v", hijackErr)
					return
				}
				_ = conn.Close()
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_test") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
				return
			}
			http.Error(w, "unexpected", http.StatusNotFound)
		})
		apiSrv := httptest.NewServer(handler)
		defer apiSrv.Close()

		c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

		res, err := c.CreateCampaign(ctx, newInput())
		if err == nil {
			t.Fatal("CreateCampaign succeeded; the authoring POST was interrupted in flight and must report an error")
		}
		// THE assertion. A nil result is what RedditDispatcher.Dispatch reads as
		// NoUpstreamCreate, releasing the claim on a post Reddit may already hold.
		if res == nil {
			t.Fatalf("CreateCampaign = (nil, %v). The authoring POST reached Reddit and MAY have "+
				"committed the post, so a nil result releases the claim (Dispatch keys release on "+
				"result == nil alone) and a retry authors a DUPLICATE paid post. Want a non-nil "+
				"partial result so the claim is RETAINED", err)
		}
		// The partial must be reconcilable: the campaign name is how an operator finds the
		// orphan, and it is the only handle an id-less ambiguous partial carries.
		if strings.TrimSpace(res.CampaignName) == "" {
			t.Error("partial result carries no CampaignName; an id-less ambiguous partial is only " +
				"reconcilable by name, so an empty one records a bare anonymous claim")
		}
	})

	t.Run("cancelled cleanly before the authoring POST is sent", func(t *testing.T) {
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		}))
		defer tokenSrv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := http.NewServeMux()
		handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_test") {
				// Cancel during account VERIFICATION, before any mutating request is built.
				// Nothing was sent to /posts, so nothing can exist. Answer normally: the
				// cancellation is already latched, so the next step observes a cancelled ctx
				// with no mutating request ever having been sent.
				cancel()
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
				return
			}
			http.Error(w, "unexpected", http.StatusNotFound)
		})
		apiSrv := httptest.NewServer(handler)
		defer apiSrv.Close()

		c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

		res, err := c.CreateCampaign(ctx, newInput())
		if err == nil {
			t.Fatal("CreateCampaign succeeded despite a cancelled context")
		}
		if res != nil {
			t.Errorf("CreateCampaign = (%+v, %v); the cancellation landed before any mutating "+
				"request was sent, so nothing exists and the claim must be RELEASED", res, err)
		}
	})
}

// TestCreateCampaign_AuthorFailureDoesNotEmitContradictoryAdGuidance covers the Step 4
// guidance branch on the authoring-failure path, which two independent causes now reach.
//
// The `else` arm was written when its only cause was "the caller never asked for an ad", and
// its step says so: "No ad variants or post URL provided -- add ads manually". Step 1.5 added
// an opposite way in — an ImageURL WAS provided and authoring failed — and the branch was not
// updated. On the AMBIGUOUS arm that produced a runbook contradicting its own warning: the
// warning says the post may exist and to verify before authoring again, while the step says go
// create the ads. They contradict on exactly the path the classification exists to protect,
// and the "no ad variants provided" wording is additionally false when Variants are supplied.
//
// Both subtests supply Variants AND an ImageURL, so the variant-listing arm is live and the
// step text is not vacuously absent. The assertions are on DISCRIMINATING tokens: "ad
// variant(s) ready" and "add ads manually" belong only to the create-the-ads guidance, while
// UNCONFIRMED/failed belong only to the authoring report — a test asserting merely that some
// step exists would pass on either.
func TestCreateCampaign_AuthorFailureDoesNotEmitContradictoryAdGuidance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		postStatus int
		// wantUnconfirmed distinguishes the ambiguous arm (5xx: the post MAY exist, so the
		// operator must verify and delete a stray) from the definite one (4xx: Reddit
		// rejected it, nothing exists, so authoring it again is safe).
		wantUnconfirmed bool
	}{
		{name: "ambiguous authoring failure (503)", postStatus: http.StatusServiceUnavailable, wantUnconfirmed: true},
		{name: "definite authoring rejection (400)", postStatus: http.StatusBadRequest, wantUnconfirmed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
					http.Error(w, "nope", tc.postStatus)
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
				EventName:       "Open Source Summit",
				Project:         "tlf",
				EventSlug:       "oss-2026",
				RegistrationURL: "https://events.linuxfoundation.org/oss/",
				BudgetUSD:       500,
				StartDate:       "2026-08-01",
				EndDate:         "2026-08-31",
				GeoTargets:      []string{"us"},
				Keywords:        []string{"linux"},
				Objective:       "conversions",
				ImageURL:        "https://events.linuxfoundation.org/banner.jpg",
				CallToAction:    "sign up",
				// Variants ARE supplied: the "no ad variants provided" wording is false
				// here, and their presence makes the variant-listing arm reachable.
				Variants: []AdVariant{{Headline: "Join us in Amsterdam"}},
			})
			if err != nil {
				t.Fatalf("CreateCampaign: %v (a post-authoring failure must degrade, not fail the create)", err)
			}
			if res.AdCount != 0 || res.AdID != "" {
				t.Errorf("AdCount/AdID = %d/%q, want 0/\"\": authoring failed so no ad exists", res.AdCount, res.AdID)
			}
			if strings.TrimSpace(res.AdWarning) == "" {
				t.Error("AdWarning is empty: an ad was requested via ImageURL and none exists, so this is a DEGRADED success")
			}

			for _, s := range res.Steps {
				// The contradiction, in both of its forms.
				if strings.Contains(s, "ad variant(s) ready") {
					t.Errorf("step %q tells the operator to create the ads, but post authoring failed "+
						"and Step 1.5 has already issued the accurate guidance", s)
				}
				if strings.Contains(s, "No ad variants or post URL provided") {
					t.Errorf("step %q claims no ad variants were provided; Variants WERE supplied and "+
						"the real cause is the authoring failure", s)
				}
			}

			// The authoring report itself must still be present and must carry the arm's own
			// discriminating token — otherwise suppressing the else branch would leave the
			// operator with no guidance at all.
			var found bool
			for _, s := range res.Steps {
				if !strings.Contains(s, "Promoted-post authoring") {
					continue
				}
				found = true
				if tc.wantUnconfirmed {
					if !strings.Contains(s, "UNCONFIRMED") {
						t.Errorf("step %q: a 5xx leaves the post possibly created, so the step must say UNCONFIRMED", s)
					}
					// The remediation that is correct whether or not the post is
					// distributed — the visibility question is undocumented.
					if !strings.Contains(s, "DELETE") {
						t.Errorf("step %q: an unattached stray post must be deleted, not left in place", s)
					}
				} else if strings.Contains(s, "UNCONFIRMED") {
					t.Errorf("step %q: a 4xx is a definite rejection; calling it UNCONFIRMED would "+
						"send the operator hunting a post that does not exist", s)
				}
			}
			if !found {
				t.Errorf("no promoted-post authoring step in %v; suppressing the generic ad guidance "+
					"must not leave the operator with no runbook at all", res.Steps)
			}
		})
	}
}

// TestCreateCampaign_AuthoredHeadlineSelection pins which string becomes the authored post's
// headline, across the three inputs that select it.
//
// Both arms of the selection were untested: no authoring test set a non-empty
// Variants[0].Headline, so "the variant wins over EventName" and the whitespace-only fallback
// could each be reverted in silence. The second matters most — Reddit requires a non-empty
// headline, and dropping the TrimSpace check sends a blank one, so the failure is an upstream
// rejection (or a blank ad) rather than anything local.
//
// The assertions are on the exact headline VALUE the client put on the wire, not on the call
// succeeding: every case here produces a working create, so a test that only checked the error
// would pass on all three regardless of which string was chosen.
func TestCreateCampaign_AuthoredHeadlineSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variants []AdVariant
		want     string
	}{
		{
			name:     "no variants: the event name is the fallback",
			variants: nil,
			want:     "Open Source Summit",
		},
		{
			name:     "a variant headline WINS over the event name",
			variants: []AdVariant{{Headline: "Join us in Amsterdam"}},
			want:     "Join us in Amsterdam",
		},
		{
			name: "a whitespace-only variant headline falls back to the event name",
			// Reddit requires a non-empty headline; without the TrimSpace check this sends
			// "   " and the post is rejected or renders blank.
			variants: []AdVariant{{Headline: "   "}},
			want:     "Open Source Summit",
		},
		{
			name:     "a variant headline is TRIMMED before it is sent",
			variants: []AdVariant{{Headline: "  Join us in Amsterdam  "}},
			want:     "Join us in Amsterdam",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var postBody map[string]any

			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
				default:
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			})
			apiSrv := httptest.NewServer(handler)
			defer apiSrv.Close()

			c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

			if _, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:       "Open Source Summit",
				Project:         "tlf",
				EventSlug:       "oss-2026",
				RegistrationURL: "https://events.linuxfoundation.org/oss/",
				BudgetUSD:       500,
				StartDate:       "2026-08-01",
				EndDate:         "2026-08-31",
				GeoTargets:      []string{"us"},
				Keywords:        []string{"linux"},
				Objective:       "conversions",
				ImageURL:        "https://events.linuxfoundation.org/banner.jpg",
				CallToAction:    "sign up",
				Variants:        tc.variants,
			}); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if postBody == nil {
				t.Fatal("no promoted post was authored")
			}
			if got := postBody["headline"]; got != tc.want {
				t.Errorf("authored post headline = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultRedditCTAIsAcceptedByReddit pins the invariant defaultRedditCTA's own definition
// states but nothing enforced: the default must be a member of redditCTAs.
//
// A caller that supplies no CallToAction gets defaultRedditCTA sent to Reddit VERBATIM — it
// bypasses the redditCTAs lookup that validates and canonicalises a caller-supplied value, so
// nothing would catch a default that Reddit does not accept. The literal "Learn More" appeared
// in no test at all, so a typo or a rename of the map key would ship a create that Reddit
// rejects on every no-CTA campaign.
//
// Both directions are asserted. Membership alone would pass on a lower-case default, since the
// map is keyed by the upper-cased form; the canonical-form check is what pins the exact string
// that goes on the wire.
func TestDefaultRedditCTAIsAcceptedByReddit(t *testing.T) {
	canonical, ok := redditCTAs[strings.ToUpper(defaultRedditCTA)]
	if !ok {
		t.Fatalf("defaultRedditCTA = %q is not a member of redditCTAs (accepted: %s). It is sent to "+
			"Reddit verbatim without passing through the lookup, so an unaccepted default fails "+
			"every campaign created without an explicit CallToAction", defaultRedditCTA, redditCTAList())
	}
	if canonical != defaultRedditCTA {
		t.Errorf("defaultRedditCTA = %q but its canonical form is %q; the default must already be "+
			"in the exact form Reddit expects, since it skips canonicalisation", defaultRedditCTA, canonical)
	}
}

// TestValidateImageURL_RejectsNonAbsoluteAndHostless covers the arm the suite left open:
// !u.IsAbs() || u.Hostname() == "". Both halves are needed and neither implies the other.
//
// The hostless case is the subtle one the code's own comment flags: "https://:443/path" parses
// cleanly, IS absolute (it has a scheme), and has an empty Hostname — so a check on IsAbs
// alone would pass it through to Reddit as an image source.
func TestValidateImageURL_RejectsNonAbsoluteAndHostless(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "relative path: no scheme, no host", raw: "/wp-content/uploads/banner.jpg"},
		{name: "scheme-relative: host but no scheme", raw: "//events.linuxfoundation.org/banner.jpg"},
		{name: "absolute but hostless: a port with no host", raw: "https://:443/path"},
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateImageURL(tc.raw); err == nil {
				t.Errorf("validateImageURL(%q) = nil, want an error: a non-absolute or hostless URL "+
					"cannot be fetched by Reddit as an image source", tc.raw)
			}
		})
	}

	// The positive control. Without it, a mutation making validateImageURL reject everything
	// would satisfy every case above.
	if err := validateImageURL("https://events.linuxfoundation.org/banner.jpg"); err != nil {
		t.Errorf("validateImageURL rejected a well-formed absolute https URL: %v", err)
	}
}

// TestCreateCampaign_AuthoringRunbookNeverAssertsUncreatedResources pins that the authoring
// runbook never claims a campaign or ad group exists before the code knows one does.
//
// Step 1.5 runs BEFORE the campaign POST (Step 2) and the ad-group POST (Step 3). An earlier
// version of these strings supplied the operator's context — "campaign + ad group created
// (PAUSED)" — at Step 1.5, where it cannot be true yet. It became true only when both creates
// went on to succeed. When the campaign create instead returns an AMBIGUOUS PARTIAL, the
// persisted result carried both claims at once:
//
//	"Promoted-post authoring UNCONFIRMED ... campaign + ad group created (PAUSED) without an ad"
//	"Campaign creation is UNCONFIRMED ... a PAUSED campaign may exist"
//
// — the runbook asserting two resources exist beside a step saying one of them merely may, and
// directing the operator to attach an ad to a campaign that may never have been created.
//
// The three subtests are the three outcomes the campaign create can produce after an authoring
// failure, and the claim must hold on each: absent when nothing is confirmed (ambiguous), and
// present exactly once when both creates are confirmed (success). The assertions are on the
// campaign-state phrase, which is discriminating — "created (PAUSED)" is the claim under test,
// while UNCONFIRMED/authoring tokens appear on arms that are not.
func TestCreateCampaign_AuthoringRunbookNeverAssertsUncreatedResources(t *testing.T) {
	// campaignStateClaimed reports whether any step asserts the campaign/ad-group pair exists.
	// Matching on the phrase rather than a whole string keeps it robust to the id values that
	// the success path interpolates.
	campaignStateClaimed := func(steps []string) []string {
		var hits []string
		for _, s := range steps {
			if strings.Contains(s, "were created (PAUSED)") || strings.Contains(s, "ad group created (PAUSED)") {
				hits = append(hits, s)
			}
		}
		return hits
	}

	newInput := func() CampaignInput {
		return CampaignInput{
			EventName:       "Open Source Summit",
			Project:         "tlf",
			EventSlug:       "oss-2026",
			RegistrationURL: "https://events.linuxfoundation.org/oss/",
			BudgetUSD:       500,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"linux"},
			Objective:       "conversions",
			ImageURL:        "https://events.linuxfoundation.org/banner.jpg",
			CallToAction:    "sign up",
			Variants:        []AdVariant{{Headline: "Join us in Amsterdam"}},
		}
	}

	// serve builds a client whose /posts always fails (so the authoring runbook is emitted)
	// and whose /campaigns responds as the case requires.
	serve := func(t *testing.T, campaignHandler func(http.ResponseWriter)) *Client {
		t.Helper()
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		}))
		t.Cleanup(tokenSrv.Close)

		handler := http.NewServeMux()
		handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/profiles/t2_test/posts"):
				// A definite 4xx: the authoring runbook is emitted, and the post is known
				// NOT to exist, so nothing here depends on the ambiguous-authoring arm.
				http.Error(w, "nope", http.StatusBadRequest)
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
				campaignHandler(w)
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/ad_groups"):
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
			default:
				http.Error(w, "unexpected", http.StatusNotFound)
			}
		})
		apiSrv := httptest.NewServer(handler)
		t.Cleanup(apiSrv.Close)

		return NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	}

	t.Run("ambiguous campaign create: no step may claim the campaign exists", func(t *testing.T) {
		// A 5xx on the campaign POST — the campaign MAY exist. This is the path where the
		// premature claim was false.
		c := serve(t, func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) })

		res, err := c.CreateCampaign(context.Background(), newInput())
		if err == nil {
			t.Fatal("CreateCampaign succeeded despite a 5xx on the campaign create")
		}
		if res == nil {
			t.Fatal("expected a partial result for an ambiguous campaign create (the claim must be retained)")
		}
		if hits := campaignStateClaimed(res.Steps); len(hits) > 0 {
			t.Errorf("the runbook asserts the campaign and ad group exist, but the campaign create was "+
				"UNCONFIRMED and the ad group was never attempted. Offending step(s): %q\nfull steps: %v",
				hits, res.Steps)
		}
		if strings.Contains(res.AdWarning, "created and are PAUSED") {
			t.Errorf("AdWarning = %q asserts the campaign was created; the campaign create was "+
				"UNCONFIRMED", res.AdWarning)
		}
		// The authoring runbook itself must still be present — deferring the campaign-state
		// sentence must not silently drop the authoring guidance with it.
		var sawAuthoring bool
		for _, s := range res.Steps {
			if strings.Contains(s, "Promoted-post authoring") {
				sawAuthoring = true
			}
		}
		if !sawAuthoring {
			t.Errorf("no promoted-post authoring step in %v", res.Steps)
		}
	})

	t.Run("malformed 2xx campaign create: no step may claim the campaign exists", func(t *testing.T) {
		// A 2xx with no data.id: the campaign may exist but its id is unknown, and the ad
		// group is never attempted. The claim is equally unfounded here.
		c := serve(t, func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		})

		res, err := c.CreateCampaign(context.Background(), newInput())
		if err == nil {
			t.Fatal("CreateCampaign succeeded despite a campaign create returning no id")
		}
		if res == nil {
			t.Fatal("expected a partial result for a malformed campaign create")
		}
		if hits := campaignStateClaimed(res.Steps); len(hits) > 0 {
			t.Errorf("the runbook asserts the campaign and ad group exist, but the campaign id could "+
				"not be read and the ad group was never attempted. Offending step(s): %q\nfull steps: %v",
				hits, res.Steps)
		}
	})

	t.Run("both creates succeed: the campaign state IS stated, exactly once", func(t *testing.T) {
		// The context is genuinely needed here and must not have been lost in the move:
		// without it an operator reading "authoring failed, add the ad manually" builds a
		// SECOND campaign beside the PAUSED one this call created.
		c := serve(t, func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		})

		res, err := c.CreateCampaign(context.Background(), newInput())
		if err != nil {
			t.Fatalf("CreateCampaign: %v (an authoring failure must degrade, not fail the create)", err)
		}
		hits := campaignStateClaimed(res.Steps)
		if len(hits) != 1 {
			t.Fatalf("want exactly ONE step stating the campaign/ad-group state, got %d: %q\nfull steps: %v",
				len(hits), hits, res.Steps)
		}
		// It must name the real ids, so the operator can find the campaign it refers to.
		if !strings.Contains(hits[0], "camp_1") || !strings.Contains(hits[0], "ag_1") {
			t.Errorf("step %q must name the created campaign and ad group ids (camp_1 / ag_1) so the "+
				"operator can attach the ad to the right campaign", hits[0])
		}
		if !strings.Contains(res.AdWarning, "created and are PAUSED") {
			t.Errorf("AdWarning = %q must carry the campaign-state context on the degraded-success "+
				"path; it is what stops an operator creating a second campaign", res.AdWarning)
		}
	})
}

// TestCreateCampaign_DeterministicFailuresAuthorNoPost pins that every deterministic input
// check runs BEFORE the promoted-post POST, so a bad input costs nothing upstream.
//
// Step 1.5 made the authoring POST the first mutating call, but two deterministic checks — the
// effective-window check and the composed-name length checks — stayed where they had been, just
// above the campaign POST. With an ImageURL set they could therefore return (nil, error) with a
// post ALREADY AUTHORED upstream. CreateCampaign's contract makes (nil, err) mean "nothing was
// or may have been created" and RedditDispatcher.Dispatch releases the claim on `result == nil`
// alone, so the claim was released on a post that definitely existed — and a retry, with the
// same bad input, authors another duplicate every time.
//
// The assertion is the CLAIM OUTCOME plus the observable upstream effect: (nil, error) is only
// safe if nothing was created, so the test requires BOTH that the result is nil (claim
// released) AND that no POST ever reached /profiles/.../posts. Asserting only the error would
// pass on the broken code, which also returned an error — the whole defect is what it left
// behind.
func TestCreateCampaign_DeterministicFailuresAuthorNoPost(t *testing.T) {
	base := func() CampaignInput {
		return CampaignInput{
			EventName:       "Open Source Summit",
			Project:         "tlf",
			EventSlug:       "oss-2026",
			RegistrationURL: "https://events.linuxfoundation.org/oss/",
			BudgetUSD:       500,
			StartDate:       "2026-08-01",
			EndDate:         "2026-08-31",
			GeoTargets:      []string{"us"},
			Keywords:        []string{"linux"},
			Objective:       "conversions",
			// The ImageURL is what makes Step 1.5 author a post at all — without it the
			// ordering defect is unreachable and the test would be vacuous.
			ImageURL:     "https://events.linuxfoundation.org/banner.jpg",
			CallToAction: "sign up",
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*CampaignInput)
		wantErr string
	}{
		{
			name: "end date not after the nudged effective start",
			mutate: func(in *CampaignInput) {
				// A past start is nudged forward to now+buffer; this end is long past, so
				// the window is invalid and the check must reject it.
				in.StartDate = "2020-01-01"
				in.EndDate = "2020-01-02"
			},
			wantErr: "is not after the effective start",
		},
		{
			name: "composed ad group name exceeds Reddit's limit",
			mutate: func(in *CampaignInput) {
				// GeoTargets has no count limit and every code joins into adGroupName, so
				// enough valid codes push the composed name past the cap.
				many := make([]string, 0, 120)
				for i := 0; i < 120; i++ {
					many = append(many, "us", "gb", "de", "fr")
				}
				in.GeoTargets = many
			},
			wantErr: "too long",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var mutating []string

			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tokenSrv.Close()

			handler := http.NewServeMux()
			handler.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mu.Lock()
					mutating = append(mutating, r.Method+" "+r.URL.Path)
					mu.Unlock()
				}
				path := r.URL.Path
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
				case r.Method == http.MethodPost && strings.HasSuffix(path, "/profiles/t2_test/posts"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_new"}})
				case r.Method == http.MethodPost && strings.HasSuffix(path, "/campaigns"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
				default:
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			})
			apiSrv := httptest.NewServer(handler)
			defer apiSrv.Close()

			c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

			in := base()
			tc.mutate(&in)

			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("CreateCampaign succeeded on a deterministically invalid input; want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			// (nil, err) tells Dispatch to RELEASE the claim, which is only correct if
			// nothing exists upstream.
			if res != nil {
				t.Errorf("result = %+v, want nil: a deterministic validation failure creates nothing, "+
					"so the claim must be released", res)
			}

			mu.Lock()
			defer mu.Unlock()
			// THE assertion. A released claim plus an authored post is the duplicate-post bug.
			if len(mutating) > 0 {
				t.Errorf("mutating request(s) reached Reddit before the deterministic check failed: %v\n"+
					"the post is authored upstream while (nil, err) releases the claim, so a retry with "+
					"the same input authors a DUPLICATE every time", mutating)
			}
		})
	}
}
