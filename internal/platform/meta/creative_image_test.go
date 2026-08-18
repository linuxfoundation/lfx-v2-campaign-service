// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// imageTestInput is the shared happy-path input; variants are supplied per test so
// each case controls exactly which ImageURLs are in play.
func imageTestInput(variants ...AdVariant) CampaignInput {
	return CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://events.example.org/kubecon",
		Objective:       "traffic",
		GeoTargets:      []string{"US"},
		Budget:          500,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		Variants:        variants,
	}
}

// imageTestClient wires a client at srv with the standard test account.
func imageTestClient(srvURL string) *Client {
	return NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srvURL),
		WithClock(fixedMetaClock()),
	)
}

// TestCreateCampaignUploadsImageAndAttachesHash is the core mapping test: it asserts
// the /adimages REQUEST BODY carries the caller's URL, and that the hash Meta
// returned lands in link_data.image_hash — and in NO other field. Distinctive
// values are used for every input so a field cross-wiring is detectable.
func TestCreateCampaignUploadsImageAndAttachesHash(t *testing.T) {
	imageCap := newBodyCapture()
	creativeCap := newBodyCapture()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			imageCap.set(decodeBody(t, r))
			// Meta keys the images map by an arbitrary key (the source filename).
			_, _ = io.WriteString(w, `{"images":{"hero-banner.png":{"hash":"HASH_DISTINCTIVE_9f8e7d","url":"https://scontent.example/x"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{
			PrimaryText: "PRIMARY_TEXT_MARKER",
			Headline:    "HEADLINE_MARKER",
			Description: "DESC_MARKER",
			ImageURL:    "https://cdn.example.org/distinct-image.png",
		},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if res.AdCount != 1 {
		t.Fatalf("ad count = %d, want 1", res.AdCount)
	}

	// --- /adimages request body ---
	imgBody := imageCap.get()
	if imgBody == nil {
		t.Fatalf("no /adimages request was made")
	}
	if got := imgBody["url"]; got != "https://cdn.example.org/distinct-image.png" {
		t.Errorf("adimages url = %v, want the caller's ImageURL", got)
	}
	// The upload must send ONLY the url — no copy fields leaking across.
	for _, leak := range []string{"image_hash", "message", "name", "link"} {
		if _, present := imgBody[leak]; present {
			t.Errorf("adimages body unexpectedly contains %q: %v", leak, imgBody)
		}
	}

	// --- creative request body ---
	creativeBody := creativeCap.get()
	if creativeBody == nil {
		t.Fatalf("no creative body captured")
	}
	oss, ok := creativeBody["object_story_spec"].(map[string]any)
	if !ok {
		t.Fatalf("object_story_spec missing: %v", creativeBody["object_story_spec"])
	}
	linkData, ok := oss["link_data"].(map[string]any)
	if !ok {
		t.Fatalf("link_data missing: %v", oss["link_data"])
	}

	// The hash lands in image_hash...
	if got := linkData["image_hash"]; got != "HASH_DISTINCTIVE_9f8e7d" {
		t.Errorf("link_data.image_hash = %v, want HASH_DISTINCTIVE_9f8e7d", got)
	}
	// ...and in NO other field. Each distinctive value must stay in its own slot.
	if got := linkData["message"]; got != "PRIMARY_TEXT_MARKER" {
		t.Errorf("link_data.message = %v, want PRIMARY_TEXT_MARKER", got)
	}
	if got := linkData["name"]; got != "HEADLINE_MARKER" {
		t.Errorf("link_data.name = %v, want HEADLINE_MARKER", got)
	}
	if got := linkData["description"]; got != "DESC_MARKER" {
		t.Errorf("link_data.description = %v, want DESC_MARKER", got)
	}
	// The image URL itself must NOT be sent as the creative's click destination —
	// that is the registration/UTM URL. A swap here would silently point the ad at
	// the image file.
	link, _ := linkData["link"].(string)
	if !strings.Contains(link, "events.example.org") {
		t.Errorf("link_data.link = %q, want the registration URL", link)
	}
	if strings.Contains(link, "distinct-image.png") {
		t.Errorf("link_data.link leaked the image URL: %q", link)
	}
	// The raw image URL must not appear anywhere in the creative body.
	if got, ok := linkData["image_url"]; ok {
		t.Errorf("link_data.image_url should not be sent, got %v", got)
	}
}

// TestCreateCampaignNoImageOmitsImageHash proves the field is additive: a variant
// with no ImageURL must make NO /adimages call and send NO image_hash key.
func TestCreateCampaignNoImageOmitsImageHash(t *testing.T) {
	creativeCap := newBodyCapture()
	var imageCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			atomic.AddInt32(&imageCalls, 1)
			_, _ = io.WriteString(w, `{"images":{"x":{"hash":"SHOULD_NOT_BE_USED"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeCap.set(decodeBody(t, r))
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "Join us", Headline: "KubeCon 2026"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if n := atomic.LoadInt32(&imageCalls); n != 0 {
		t.Errorf("/adimages called %d times for a variant with no image, want 0", n)
	}

	linkData := creativeLinkData(t, creativeCap.get())
	if got, present := linkData["image_hash"]; present {
		t.Errorf("link_data.image_hash present with no ImageURL: %v", got)
	}
}

// TestCreateCampaignPerVariantImageIsolation proves each variant gets its OWN
// hash — a shared/last-write-wins bug would attach one image to every ad.
func TestCreateCampaignPerVariantImageIsolation(t *testing.T) {
	var mu struct {
		urls   []string
		hashes []string
	}
	creativeBodies := make(chan map[string]any, 8)
	imageURLs := make(chan string, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			b := decodeBody(t, r)
			u, _ := b["url"].(string)
			imageURLs <- u
			// Echo a hash derived from the URL so a mis-pairing is visible.
			switch {
			case strings.Contains(u, "alpha"):
				_, _ = io.WriteString(w, `{"images":{"a.png":{"hash":"HASH_ALPHA"}}}`)
			case strings.Contains(u, "beta"):
				_, _ = io.WriteString(w, `{"images":{"b.png":{"hash":"HASH_BETA"}}}`)
			default:
				t.Errorf("unexpected image url %q", u)
				_, _ = io.WriteString(w, `{"images":{"z.png":{"hash":"HASH_UNKNOWN"}}}`)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			creativeBodies <- decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"creative_x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_x"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "First", Headline: "Alpha head", ImageURL: "https://cdn.example.org/alpha.png"},
		AdVariant{PrimaryText: "Second", Headline: "Beta head", ImageURL: "https://cdn.example.org/beta.png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if res.AdCount != 2 {
		t.Fatalf("ad count = %d, want 2", res.AdCount)
	}

	close(imageURLs)
	for u := range imageURLs {
		mu.urls = append(mu.urls, u)
	}
	if len(mu.urls) != 2 {
		t.Fatalf("adimages calls = %d, want 2 (one per variant)", len(mu.urls))
	}

	close(creativeBodies)
	for b := range creativeBodies {
		ld := creativeLinkData(t, b)
		h, _ := ld["image_hash"].(string)
		name, _ := ld["name"].(string)
		mu.hashes = append(mu.hashes, h)
		// The pairing is what matters: variant 1's headline must sit with ALPHA.
		switch name {
		case "Alpha head":
			if h != "HASH_ALPHA" {
				t.Errorf("variant Alpha got hash %q, want HASH_ALPHA", h)
			}
		case "Beta head":
			if h != "HASH_BETA" {
				t.Errorf("variant Beta got hash %q, want HASH_BETA", h)
			}
		default:
			t.Errorf("unexpected creative headline %q", name)
		}
	}
	if len(mu.hashes) != 2 {
		t.Fatalf("creatives = %d, want 2", len(mu.hashes))
	}
	if mu.hashes[0] == mu.hashes[1] {
		t.Errorf("both creatives got the same hash %q — per-variant isolation broken", mu.hashes[0])
	}
}

// TestValidateVariantImageURL covers the up-front guard. Every rejection case here
// MUST fail before any mutating call — asserted separately below.
func TestValidateVariantImageURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"empty is allowed", "", ""},
		{"whitespace-only is allowed", "   ", ""},
		{"valid https", "https://cdn.example.org/a.png", ""},
		{"http rejected", "http://cdn.example.org/a.png", "must use HTTPS"},
		{"relative rejected", "/images/a.png", "not a valid URL"},
		{"scheme-only rejected", "https://", "not a valid URL"},
		{"port-only authority rejected", "https://:443", "not a valid URL"},
		{"userinfo rejected", urlWithUserinfo("https", "u", "p", "cdn.example.org/a.png"), "embedded credentials"},
		{"non-http scheme rejected", "ftp://cdn.example.org/a.png", "must use HTTPS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVariantImageURL(tc.url)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateVariantImageURL(%q) = %v, want nil", tc.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateVariantImageURL(%q) = nil, want error containing %q", tc.url, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestCreateCampaignBadImageURLSpendsNothing is the money guard: a malformed image
// URL must be rejected BEFORE the first mutating call, so no paid campaign exists.
func TestCreateCampaignBadImageURLSpendsNothing(t *testing.T) {
	var mutations int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&mutations, 1)
			t.Errorf("MUTATING call made despite invalid image URL: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
	}))
	defer srv.Close()

	for _, bad := range []string{
		"http://cdn.example.org/a.png",
		"/relative.png",
		"https://:443",
		urlWithUserinfo("https", "u", "p", "cdn.example.org/a.png"),
	} {
		res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
			AdVariant{PrimaryText: "Join us", Headline: "KubeCon", ImageURL: bad},
		))
		if err == nil {
			t.Fatalf("CreateCampaign(%q) = nil error, want a pre-spend rejection", bad)
		}
		if res != nil {
			t.Errorf("CreateCampaign(%q) returned a result %+v, want nil", bad, res)
		}
		if !strings.Contains(err.Error(), "variant 1") {
			t.Errorf("error %q should identify the offending variant", err.Error())
		}
	}
	if n := atomic.LoadInt32(&mutations); n != 0 {
		t.Fatalf("%d mutating calls were made; want 0", n)
	}
}

// TestUploadAdImageResponseShapes covers the ambiguous/malformed upload replies.
// Each must be an error rather than a silently image-less ad.
func TestUploadAdImageResponseShapes(t *testing.T) {
	cases := []struct {
		name     string
		response string
		wantErr  string
	}{
		{"no images key", `{}`, "returned no image"},
		{"empty images map", `{"images":{}}`, "returned no image"},
		{"empty hash", `{"images":{"a.png":{"hash":""}}}`, "empty image hash"},
		{"whitespace hash", `{"images":{"a.png":{"hash":"   "}}}`, "empty image hash"},
		{"two images", `{"images":{"a.png":{"hash":"H1"},"b.png":{"hash":"H2"}}}`, "expected 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer srv.Close()

			hash, err := imageTestClient(srv.URL).uploadAdImage(context.Background(), "https://cdn.example.org/a.png", 0)
			if err == nil {
				t.Fatalf("uploadAdImage = (%q, nil), want an error", hash)
			}
			if hash != "" {
				t.Errorf("hash = %q on error, want empty", hash)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			// Every one of these is a 2xx we could not use: it must be classified as
			// AMBIGUOUS so the caller says "verify" rather than "definitely failed".
			if !createOutcomeAmbiguous(err) {
				t.Errorf("error %v should be ambiguous (Meta may have stored the image)", err)
			}
		})
	}
}

// TestUploadAdImageNotRetriedOnThrottle pins the retry policy: /adimages is a
// mutating create with no idempotency key, so a 429 must NOT be repeated.
func TestUploadAdImageNotRetriedOnThrottle(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","code":4}}`)
	}))
	defer srv.Close()

	_, err := imageTestClient(srv.URL).uploadAdImage(context.Background(), "https://cdn.example.org/a.png", 0)
	if err == nil {
		t.Fatal("uploadAdImage = nil error on 429, want an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("/adimages called %d times on 429, want exactly 1 (creates are never retried)", n)
	}
}

// TestUploadAdImageErrorsOmitTheURL proves no caller URL (which may carry a
// pre-signed query) and no token reaches the error string.
func TestUploadAdImageErrorsOmitTheURL(t *testing.T) {
	const secretURL = "https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIGNATURE_VALUE&token=SECRET_TOKEN"

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"2xx no images", http.StatusOK, `{"images":{}}`},
		{"4xx rejection", http.StatusBadRequest, `{"error":{"message":"bad image","code":100}}`},
		{"5xx", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := imageTestClient(srv.URL).uploadAdImage(context.Background(), secretURL, 0)
			if err == nil {
				t.Fatal("want an error")
			}
			msg := err.Error()
			for _, secret := range []string{"SECRET_SIGNATURE_VALUE", "SECRET_TOKEN", "X-Amz-Signature", "tok-img"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error message leaked %q: %s", secret, msg)
				}
			}
		})
	}
}

// TestCreateCampaignImageFailureIsNonFatalAndReported proves a failed upload does
// not kill the campaign and does not create a creative or ad for that variant,
// while a sibling variant still succeeds.
func TestCreateCampaignImageFailureIsNonFatalAndReported(t *testing.T) {
	var creativeCalls, adCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			b := decodeBody(t, r)
			if u, _ := b["url"].(string); strings.Contains(u, "broken") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"image could not be fetched","code":100}}`)
				return
			}
			_, _ = io.WriteString(w, `{"images":{"ok.png":{"hash":"HASH_OK"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			atomic.AddInt32(&creativeCalls, 1)
			_, _ = io.WriteString(w, `{"id":"creative_ok"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			atomic.AddInt32(&adCalls, 1)
			_, _ = io.WriteString(w, `{"id":"ad_ok"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "Bad one", Headline: "Broken img", ImageURL: "https://cdn.example.org/broken.png"},
		AdVariant{PrimaryText: "Good one", Headline: "Working img", ImageURL: "https://cdn.example.org/ok.png"},
	))
	// A per-variant failure is NON-FATAL: the campaign still succeeds.
	if err != nil {
		t.Fatalf("CreateCampaign error = %v, want nil (per-variant image failure is non-fatal)", err)
	}
	if res == nil {
		t.Fatal("result is nil")
	}
	// The partial result must be preserved — the campaign and ad set were created.
	if res.CampaignID != "120100000000123" {
		t.Errorf("campaign id = %q, want it preserved in the partial result", res.CampaignID)
	}
	if res.AdSetID != "120200000000456" {
		t.Errorf("adset id = %q, want it preserved", res.AdSetID)
	}
	// Only the good variant produced an ad.
	if res.AdCount != 1 {
		t.Errorf("ad count = %d, want 1 (the image-failing variant makes no ad)", res.AdCount)
	}
	if n := atomic.LoadInt32(&creativeCalls); n != 1 {
		t.Errorf("adcreatives called %d times, want 1 — a failed image upload must NOT create a creative", n)
	}
	if n := atomic.LoadInt32(&adCalls); n != 1 {
		t.Errorf("ads called %d times, want 1", n)
	}
	// The failure must be SURFACED in Steps, not silently dropped.
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "Ad 1") {
		t.Errorf("Steps do not report the failed variant 1: %v", res.Steps)
	}
	// ...and must not leak the token.
	if strings.Contains(joined, "tok-img") {
		t.Errorf("Steps leaked the access token: %v", res.Steps)
	}
}

// creativeLinkData pulls object_story_spec.link_data out of a captured body.
func creativeLinkData(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		t.Fatalf("no creative body captured")
	}
	oss, ok := body["object_story_spec"].(map[string]any)
	if !ok {
		t.Fatalf("object_story_spec missing: %v", body["object_story_spec"])
	}
	ld, ok := oss["link_data"].(map[string]any)
	if !ok {
		t.Fatalf("link_data missing: %v", oss["link_data"])
	}
	return ld
}
