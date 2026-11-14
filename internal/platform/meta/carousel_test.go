// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// carouselServer stubs the create flow and records every /adimages upload, creative and ad.
type carouselServer struct {
	t              *testing.T
	mu             sync.Mutex
	uploads        int
	creatives      []map[string]any
	ads            int
	creativeStatus int
	uploadStatus   int
}

func (s *carouselServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			s.uploads++
			if s.uploadStatus != 0 {
				w.WriteHeader(s.uploadStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"bad image","code":100}}`)
				return
			}
			_, _ = io.WriteString(w, `{"images":{"creative":{"hash":"HASH_`+strconv.Itoa(s.uploads)+`"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			s.creatives = append(s.creatives, decodeBody(s.t, r))
			if s.creativeStatus != 0 {
				w.WriteHeader(s.creativeStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid child attachment","code":100}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			s.ads++
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func carouselInput() CampaignInput {
	in := metaFieldInput()
	in.Format = "Carousel"
	in.CallToAction = "SIGN_UP"
	speaker := []byte("\x89PNG\r\n\x1a\nSPEAKER")
	in.Variants = []AdVariant{
		{PrimaryText: "Meet the speakers", Headline: "Keynotes", Description: "Day 1", ImageURL: "https://cdn.example.org/keynote.png"},
		{Headline: "Security track", ImageAssetID: "a1", ImageBytes: speaker, ImageMIME: "image/png"},
		{PrimaryText: "Meet the speakers", Headline: "AI track", ImageAssetID: "a1", ImageBytes: speaker, ImageMIME: "image/png"},
	}
	return in
}

// TestCreateCampaign_CarouselIsOneAdWithACardPerVariant pins the carousel request: ONE creative whose
// link_data.child_attachments are the variants in order — each with its headline as name, its
// description, its image (picture for a URL, image_hash for stored bytes, uploaded once per distinct
// image), its own UTM link and the button — variant 1's primaryText as the message, card 1's link as
// the ad link, and ONE ad.
func TestCreateCampaign_CarouselIsOneAdWithACardPerVariant(t *testing.T) {
	cs := &carouselServer{t: t}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()
	res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), carouselInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.uploads != 1 || len(cs.creatives) != 1 || cs.ads != 1 {
		t.Fatalf("uploads=%d creatives=%d ads=%d, want 1, 1 and 1", cs.uploads, len(cs.creatives), cs.ads)
	}
	if cs.creatives[0]["name"] != "KubeCon - Carousel" {
		t.Errorf("creative name = %v", cs.creatives[0]["name"])
	}
	spec, _ := cs.creatives[0]["object_story_spec"].(map[string]any)
	link, _ := spec["link_data"].(map[string]any)
	cards, _ := link["child_attachments"].([]any)
	if link["message"] != "Meet the speakers" || len(cards) != 3 {
		t.Fatalf("link_data = %v; want variant 1's message and 3 cards", link)
	}
	if adLink, _ := link["link"].(string); !strings.Contains(adLink, "utm_content=variant-1") {
		t.Errorf("ad link = %q, want card 1's UTM link", adLink)
	}
	wantName := []string{"Keynotes", "Security track", "AI track"}
	for i, raw := range cards {
		card, _ := raw.(map[string]any)
		cardLink, _ := card["link"].(string)
		cta, _ := card["call_to_action"].(map[string]any)
		value, _ := cta["value"].(map[string]any)
		if card["name"] != wantName[i] || !strings.Contains(cardLink, "utm_content=variant-"+strconv.Itoa(i+1)) ||
			cta["type"] != "SIGN_UP" || value["link"] != cardLink {
			t.Fatalf("card %d = %v", i+1, card)
		}
		_, hasPicture := card["picture"]
		_, hasHash := card["image_hash"]
		if (i == 0) != hasPicture || (i > 0) != hasHash {
			t.Fatalf("card %d image fields = %v", i+1, card)
		}
	}
	if c0, _ := cards[0].(map[string]any); c0["description"] != "Day 1" {
		t.Errorf("card 1 description = %v", c0["description"])
	}
	if res.AdCount != 1 || res.Format != FormatCarousel || len(res.Ads) != 1 ||
		strings.Join(res.Ads[0].CardImageHashes, ",") != ",HASH_1,HASH_1" {
		t.Fatalf("result = AdCount %d Format %q Ads %+v", res.AdCount, res.Format, res.Ads)
	}
}

// TestCreateCampaign_CarouselFailuresAreDegradedNotRetried: a card image the upload refuses creates
// no creative, and a creative Meta refuses creates no ad; either is a degraded success naming the
// failure, with the campaign and ad set kept.
func TestCreateCampaign_CarouselFailuresAreDegradedNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uploadStatus  int
		creative      int
		wantCreatives int
		want          string
	}{
		{"card image refused", http.StatusBadRequest, 0, 0, "Carousel ad failed: card 2 image upload did not complete, so no creative or ad was created"},
		{"creative refused", 0, http.StatusBadRequest, 1, "Carousel ad failed: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &carouselServer{t: t, uploadStatus: tc.uploadStatus, creativeStatus: tc.creative}
			srv := httptest.NewServer(cs.handler())
			defer srv.Close()
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), carouselInput())
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			cs.mu.Lock()
			defer cs.mu.Unlock()
			if len(cs.creatives) != tc.wantCreatives || cs.ads != 0 || res.AdCount != 0 || res.AdSetID == "" {
				t.Fatalf("creatives=%d ads=%d AdCount=%d AdSetID=%q", len(cs.creatives), cs.ads, res.AdCount, res.AdSetID)
			}
			if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, tc.want) {
				t.Fatalf("steps %q do not contain %q", steps, tc.want)
			}
		})
	}
}

// TestCreateCampaign_CarouselRefusalsSendNothing: every carousel rule is a config-invalid pre-flight
// refusal with no request sent.
func TestCreateCampaign_CarouselRefusalsSendNothing(t *testing.T) {
	card := func(h string) AdVariant {
		return AdVariant{Headline: h, ImageURL: "https://cdn.example.org/" + h + ".png"}
	}
	first := AdVariant{PrimaryText: "Meet the speakers", Headline: "K", ImageURL: "https://cdn.example.org/k.png"}
	eleven := []AdVariant{first}
	for i := 0; i < 10; i++ {
		eleven = append(eleven, card("c"+strconv.Itoa(i)))
	}
	noImage := card("x")
	noImage.ImageURL = ""
	video := card("v")
	video.VideoAssetID, video.VideoBytes, video.VideoMIME = "v1", []byte("mp4"), MimeTypeVideoMP4
	otherText := card("o")
	otherText.PrimaryText = "Something else"
	noMessage := first
	noMessage.PrimaryText = ""
	for _, tc := range []struct {
		name, format string
		variants     []AdVariant
		want         string
	}{
		{"unknown format", "slideshow", []AdVariant{first, card("a")}, `metaConfig.format: "slideshow" is not one of single, carousel`},
		{"one card", "carousel", []AdVariant{first}, "a carousel needs 2 to 10 variants, one per card; got 1"},
		{"eleven cards", "carousel", eleven, "got 11"},
		{"card without an image", "carousel", []AdVariant{first, noImage}, "metaConfig.variants[1]: a carousel card needs an image"},
		{"video card", "carousel", []AdVariant{first, video}, "metaConfig.variants[1].videoAssetId: a carousel card carries an image, not a video"},
		{"a second message", "carousel", []AdVariant{first, otherText}, "metaConfig.variants[1].primaryText: a carousel has one message"},
		{"no message", "carousel", []AdVariant{noMessage, card("a")}, "metaConfig.variants[0].primaryText"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Format, in.Variants = tc.format, tc.variants
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid refusal containing %q", res, err, tc.want)
			}
		})
	}
}

// TestCreateCampaign_SingleFormatKeepsOneAdPerVariant: "single" (and blank) is today's flow — every
// variant's primaryText is still required and each variant gets its own ad.
func TestCreateCampaign_SingleFormatKeepsOneAdPerVariant(t *testing.T) {
	for _, f := range []string{"", "single", " SINGLE "} {
		cs := &carouselServer{t: t}
		srv := httptest.NewServer(cs.handler())
		in := carouselInput()
		in.Format = f
		_, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "variant 2 must have non-empty primary text") {
			t.Fatalf("format %q: CreateCampaign err = %v; want variant 2's missing primaryText refused", f, err)
		}
	}
}
