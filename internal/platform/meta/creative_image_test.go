// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestCreateCampaignAttachesPictureURL is the core mapping test: it asserts the
// caller's image URL lands in link_data.picture — the DOCUMENTED by-URL field —
// that NO /adimages call is made, and that the URL reaches no other field.
// Distinctive values are used for every input so a field cross-wiring is detectable.
func TestCreateCampaignAttachesPictureURL(t *testing.T) {
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
			// The undocumented upload edge must never be called.
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
	// The /adimages "url" parameter is not documented; the by-URL feature must be
	// delivered by link_data.picture alone, with no upload round-trip.
	if n := atomic.LoadInt32(&imageCalls); n != 0 {
		t.Errorf("/adimages called %d times, want 0 — the url parameter is undocumented", n)
	}

	linkData := creativeLinkData(t, creativeCap.get())

	// The URL lands in picture...
	if got := linkData["picture"]; got != "https://cdn.example.org/distinct-image.png" {
		t.Errorf("link_data.picture = %v, want the caller's ImageURL", got)
	}
	// ...and image_hash is never sent alongside it — the reference states the two
	// are mutually exclusive ("Specify this field or image_hash but not both").
	if got, present := linkData["image_hash"]; present {
		t.Errorf("link_data.image_hash must not accompany picture: %v", got)
	}
	// Each distinctive value must stay in its own slot.
	if got := linkData["message"]; got != "PRIMARY_TEXT_MARKER" {
		t.Errorf("link_data.message = %v, want PRIMARY_TEXT_MARKER", got)
	}
	if got := linkData["name"]; got != "HEADLINE_MARKER" {
		t.Errorf("link_data.name = %v, want HEADLINE_MARKER", got)
	}
	if got := linkData["description"]; got != "DESC_MARKER" {
		t.Errorf("link_data.description = %v, want DESC_MARKER", got)
	}
	// The image URL must NOT be the creative's click destination — that is the
	// registration/UTM URL. A swap would silently point the ad at the image file.
	link, _ := linkData["link"].(string)
	if !strings.Contains(link, "events.example.org") {
		t.Errorf("link_data.link = %q, want the registration URL", link)
	}
	if strings.Contains(link, "distinct-image.png") {
		t.Errorf("link_data.link leaked the image URL: %q", link)
	}
	// image_url is NOT a documented AdCreativeLinkData field; never send it.
	if got, ok := linkData["image_url"]; ok {
		t.Errorf("link_data.image_url is not a documented field, got %v", got)
	}
}

// TestCreateCampaignNoImageOmitsPicture proves the field is additive: a variant
// with no ImageURL must send NO picture key (and still make no /adimages call).
func TestCreateCampaignNoImageOmitsPicture(t *testing.T) {
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
	if got, present := linkData["picture"]; present {
		t.Errorf("link_data.picture present with no ImageURL: %v", got)
	}
	if got, present := linkData["image_hash"]; present {
		t.Errorf("link_data.image_hash present with no ImageURL: %v", got)
	}
}

// TestCreateCampaignPerVariantImageIsolation proves each variant carries its OWN
// picture URL — a shared/last-write-wins bug would put one image on every ad.
func TestCreateCampaignPerVariantImageIsolation(t *testing.T) {
	creativeBodies := make(chan map[string]any, 8)
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
	if n := atomic.LoadInt32(&imageCalls); n != 0 {
		t.Errorf("/adimages called %d times, want 0", n)
	}

	close(creativeBodies)
	var pictures []string
	for b := range creativeBodies {
		ld := creativeLinkData(t, b)
		pic, _ := ld["picture"].(string)
		name, _ := ld["name"].(string)
		pictures = append(pictures, pic)
		// The pairing is what matters: variant 1's headline must sit with alpha.png.
		switch name {
		case "Alpha head":
			if pic != "https://cdn.example.org/alpha.png" {
				t.Errorf("variant Alpha got picture %q, want alpha.png", pic)
			}
		case "Beta head":
			if pic != "https://cdn.example.org/beta.png" {
				t.Errorf("variant Beta got picture %q, want beta.png", pic)
			}
		default:
			t.Errorf("unexpected creative headline %q", name)
		}
	}
	if len(pictures) != 2 {
		t.Fatalf("creatives = %d, want 2", len(pictures))
	}
	if pictures[0] == pictures[1] {
		t.Errorf("both creatives got the same picture %q — per-variant isolation broken", pictures[0])
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

// TestCreativeRejectionNotRetriedOnThrottle pins the retry policy: the creative
// create carrying the picture URL is a mutating create with no idempotency key,
// so a 429 must NOT be repeated.
func TestCreativeRejectionNotRetriedOnThrottle(t *testing.T) {
	var creativeCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			atomic.AddInt32(&creativeCalls, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","code":4}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "Join us", Headline: "KubeCon", ImageURL: "https://cdn.example.org/a.png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error = %v, want nil (per-variant failure is non-fatal)", err)
	}
	if n := atomic.LoadInt32(&creativeCalls); n != 1 {
		t.Errorf("adcreatives called %d times on 429, want exactly 1 (creates are never retried)", n)
	}
}

// TestImageURLDoesNotReachStepsWhenMetaEchoesIt is the privacy regression test
// dealako asked for: a 4xx whose Meta message ECHOES the rejected picture URL
// verbatim. The pre-signed query is a bearer credential, and Steps are persisted,
// so neither the signature nor the token may survive into the returned Steps.
func TestImageURLDoesNotReachStepsWhenMetaEchoesIt(t *testing.T) {
	const secretURL = "https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIGNATURE_VALUE&token=SECRET_TOKEN"

	// Each case is a DIFFERENT Steps rendering path in the per-variant handler:
	// a plain 4xx failure and an ambiguous 5xx (the UNCONFIRMED wording).
	for _, tc := range []struct {
		name     string
		status   int
		wantStep string
	}{
		{"4xx rejection renders the failed wording", http.StatusBadRequest, "Ad 1 failed"},
		{"5xx rejection renders the UNCONFIRMED wording", http.StatusInternalServerError, "UNCONFIRMED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
					_, _ = io.WriteString(w, `{"data":[]}`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
					_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
					_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
					_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
					// Meta echoes the rejected parameter's value in error.message.
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"Invalid parameter picture: could not fetch `+secretURL+`","code":100}}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
				AdVariant{PrimaryText: "Join us", Headline: "KubeCon", ImageURL: secretURL},
			))
			if err != nil {
				t.Fatalf("CreateCampaign error = %v, want nil (per-variant failure is non-fatal)", err)
			}
			joined := strings.Join(res.Steps, "\n")
			if !strings.Contains(joined, tc.wantStep) {
				t.Fatalf("Steps do not contain %q, so the leak path was not exercised: %v", tc.wantStep, res.Steps)
			}
			for _, secret := range []string{"SECRET_SIGNATURE_VALUE", "SECRET_TOKEN", "X-Amz-Signature", "tok-img"} {
				if strings.Contains(joined, secret) {
					t.Errorf("persisted Steps leaked %q: %v", secret, res.Steps)
				}
			}
			// The step should still identify WHICH image failed, minus the credential.
			if !strings.Contains(joined, "cdn.example.org/a.png") {
				t.Errorf("Steps lost the identifying image path entirely: %v", res.Steps)
			}
		})
	}
}

// TestScrubURLFromErr covers the sink-side scrubber directly. The scrubber's rule
// is structural: a URL carrying a query/fragment means upstream text is withheld
// entirely; a URL without one is safe to render alongside the message.
func TestScrubURLFromErr(t *testing.T) {
	const secretURL = "https://cdn.example.org/a.png?sig=SECRET_SIG"

	t.Run("nil error", func(t *testing.T) {
		if got := scrubURLFromErr(nil, secretURL, 300); got != "" {
			t.Errorf("scrubURLFromErr(nil) = %q, want empty", got)
		}
	})
	t.Run("verbatim echo of a secret-bearing URL is withheld", func(t *testing.T) {
		err := fmt.Errorf("could not fetch %s", secretURL)
		got := scrubURLFromErr(err, secretURL, 300)
		if strings.Contains(got, "SECRET_SIG") {
			t.Errorf("message still carries the signature: %q", got)
		}
		if !strings.Contains(got, "cdn.example.org/a.png") {
			t.Errorf("message lost the identifying path: %q", got)
		}
	})
	t.Run("percent-encoded echo is withheld", func(t *testing.T) {
		err := fmt.Errorf("could not fetch %s", url.QueryEscape(secretURL))
		if got := scrubURLFromErr(err, secretURL, 300); strings.Contains(got, "SECRET_SIG") {
			t.Errorf("percent-encoded echo not withheld: %q", got)
		}
	})
	t.Run("empty imageURL leaves the message intact", func(t *testing.T) {
		err := fmt.Errorf("plain failure")
		if got := scrubURLFromErr(err, "", 300); got != "plain failure" {
			t.Errorf("scrubURLFromErr with empty url = %q, want the message unchanged", got)
		}
	})
	t.Run("a query-less URL is replaced in place, message kept", func(t *testing.T) {
		const plain = "https://cdn.example.org/a.png"
		err := fmt.Errorf("could not fetch %s", plain)
		got := scrubURLFromErr(err, plain, 300)
		if !strings.Contains(got, "could not fetch") {
			t.Errorf("a URL with no secret material must keep the diagnostic: %q", got)
		}
	})
	t.Run("still truncates", func(t *testing.T) {
		err := fmt.Errorf("%s", strings.Repeat("x", 500))
		if got := scrubURLFromErr(err, "", 50); len([]rune(got)) > 51 {
			t.Errorf("scrubURLFromErr did not clamp length: %d runes", len([]rune(got)))
		}
	})
}

// TestCreateCampaignImageFailureIsNonFatalAndReported proves a creative rejected
// over its picture URL does not kill the campaign and makes no ad for that
// variant, while a sibling variant still succeeds.
func TestCreateCampaignImageFailureIsNonFatalAndReported(t *testing.T) {
	var creativeCalls, adCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			atomic.AddInt32(&creativeCalls, 1)
			b := decodeBody(t, r)
			oss, _ := b["object_story_spec"].(map[string]any)
			ld, _ := oss["link_data"].(map[string]any)
			if pic, _ := ld["picture"].(string); strings.Contains(pic, "broken") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"image could not be fetched","code":100}}`)
				return
			}
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
	if n := atomic.LoadInt32(&creativeCalls); n != 2 {
		t.Errorf("adcreatives called %d times, want 2 (one attempt per variant)", n)
	}
	if n := atomic.LoadInt32(&adCalls); n != 1 {
		t.Errorf("ads called %d times, want 1 — a rejected creative must NOT create an ad", n)
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

// TestScrubURLFromErrFailsClosed pins the structural withholding rule. The cases
// below are exactly the ones a substring-matching redactor cannot handle: `do`
// truncates a non-Graph body at 300 runes (clipping a signature mid-value), and a
// proxy may line-wrap or re-encode the echo. None of these can be proven clean by
// scanning for the secret, so the scrubber must withhold on the URL's SHAPE.
func TestScrubURLFromErrFailsClosed(t *testing.T) {
	const secretURL = "https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIG_ABCDEF&e=1"

	t.Run("truncated echo is withheld, not emitted", func(t *testing.T) {
		err := fmt.Errorf("proxy error: upstream refused https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIG_ABC")
		got := scrubURLFromErr(err, secretURL, 300)
		if strings.Contains(got, "SECRET_SIG") {
			t.Errorf("fail-open: a clipped signature survived scrubbing: %q", got)
		}
		if !strings.Contains(got, "cdn.example.org/a.png") {
			t.Errorf("withheld message lost the identifying image path: %q", got)
		}
	})

	t.Run("line-wrapped echo is withheld", func(t *testing.T) {
		err := fmt.Errorf("body: https://cdn.example.org/a.png?X-Amz-Sig\nnature=SECRET_SIG_ABCDEF")
		if got := scrubURLFromErr(err, secretURL, 300); strings.Contains(got, "SECRET_SIG_ABCDEF") {
			t.Errorf("fail-open: a wrapped echo kept the full signature: %q", got)
		}
	})

	// The case that defeated the previous prefix-scanning verifier: a SHORT parameter
	// name (skipped as below the minimum run length) whose VALUE is wrapped mid-token,
	// so no contiguous run of it survives for a substring check to find. Withholding on
	// the URL's shape is what makes this safe; searching the text never could.
	t.Run("short param name with a wrapped value is withheld", func(t *testing.T) {
		const shortParamURL = "https://cdn.example.org/a.png?sig=SECRET_SIG"
		err := fmt.Errorf("proxy error: upstream refused https://cdn.example.org/a.png?sig=SEC\nRET_SIG")
		got := scrubURLFromErr(err, shortParamURL, 300)
		if strings.Contains(got, "SEC\nRET_SIG") || strings.Contains(got, "SECRET_SIG") {
			t.Errorf("fail-open: a wrapped short-param value survived: %q", got)
		}
		if !strings.Contains(got, "cdn.example.org/a.png") {
			t.Errorf("withheld message lost the identifying image path: %q", got)
		}
	})

	t.Run("a fragment-only URL is withheld", func(t *testing.T) {
		const fragURL = "https://cdn.example.org/b.png#SECRET_FRAG"
		err := fmt.Errorf("upstream echoed https://cdn.example.org/b.png#SECRET_FRAG")
		if got := scrubURLFromErr(err, fragURL, 300); strings.Contains(got, "SECRET_FRAG") {
			t.Errorf("fail-open: a fragment secret survived: %q", got)
		}
	})

	// Even the withheld rendering is caller-controlled in its path component, so it
	// must honor the same clamp as the normal return path.
	t.Run("the withheld message is clamped to max", func(t *testing.T) {
		longPath := "https://cdn.example.org/" + strings.Repeat("p", 400) + ".png?sig=SECRET_SIG"
		err := fmt.Errorf("upstream refused")
		got := scrubURLFromErr(err, longPath, 100)
		if len([]rune(got)) > 101 {
			t.Errorf("withheld message was not clamped: %d runes", len([]rune(got)))
		}
	})

	t.Run("a clean unrelated message is withheld too when the URL is secret-bearing", func(t *testing.T) {
		// The rule is structural: it does not depend on whether THIS message happened
		// to echo the URL, because the next upstream rendering may.
		err := fmt.Errorf("meta API POST /adcreatives failed (400): Invalid parameter")
		got := scrubURLFromErr(err, secretURL, 300)
		if strings.Contains(got, "Invalid parameter") {
			t.Errorf("upstream text must be withheld for a secret-bearing URL, got %q", got)
		}
	})

	// An unparseable value is the case where the delimiter scan is least trustworthy,
	// so urlHasSecretMaterial reports it as secret-bearing and the message is withheld.
	t.Run("an unparseable URL is treated as secret-bearing", func(t *testing.T) {
		// A control character makes url.Parse fail outright.
		badURL := "https://cdn.example.org/a.png\x7f?sig=SECRET_SIG"
		err := fmt.Errorf("upstream refused SECRET_SIG for that image")
		got := scrubURLFromErr(err, badURL, 300)
		if strings.Contains(got, "SECRET_SIG") {
			t.Errorf("fail-open: an unparseable URL let upstream text through: %q", got)
		}
	})

	t.Run("a URL with no query is never withheld", func(t *testing.T) {
		err := fmt.Errorf("could not fetch https://cdn.example.org/plain.png")
		got := scrubURLFromErr(err, "https://cdn.example.org/plain.png", 300)
		if !strings.Contains(got, "could not fetch") {
			t.Errorf("a URL carrying no secret material must not trigger withholding, got %q", got)
		}
	})
}
