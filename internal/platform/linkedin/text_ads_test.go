// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestNormalizeFormat(t *testing.T) {
	for in, want := range map[string]string{"": FormatSingleImage, "single_image": FormatSingleImage, "text": FormatText} {
		if got, err := NormalizeFormat(in); err != nil || got != want {
			t.Errorf("NormalizeFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"TEXT", "video", " text", "TEXT_AD"} {
		if _, err := NormalizeFormat(in); !errors.Is(err, ErrFormatInvalid) {
			t.Errorf("NormalizeFormat(%q) = %v, want ErrFormatInvalid", in, err)
		}
	}
}

func TestValidateTextAds(t *testing.T) {
	ok := TextAd{Headline: "KubeCon NA 2099", Description: "Join 10,000 cloud native engineers in Atlanta."}
	with := func(f func(*TextAd)) []TextAd { a := ok; f(&a); return []TextAd{a} }
	many := make([]TextAd, maxTextAds+1)
	for i := range many {
		many[i] = ok
	}
	for _, tc := range []struct {
		name string
		in   []TextAd
		want string // "" accepted
	}{
		{"valid", []TextAd{ok}, ""},
		{"headline at 25", with(func(a *TextAd) { a.Headline = strings.Repeat("h", 25) }), ""},
		// Characters, not bytes: 25 multi-byte runes fit.
		{"headline of 25 multibyte runes", with(func(a *TextAd) { a.Headline = strings.Repeat("é", 25) }), ""},
		{"headline at 26", with(func(a *TextAd) { a.Headline = strings.Repeat("h", 26) }), "headline is 26 characters"},
		{"description at 75", with(func(a *TextAd) { a.Description = strings.Repeat("d", 75) }), ""},
		{"description at 76", with(func(a *TextAd) { a.Description = strings.Repeat("d", 76) }), "description is 76 characters"},
		// Surrounding spaces are trimmed before counting, as the wire copy is.
		{"padded headline counts trimmed", with(func(a *TextAd) { a.Headline = "  " + strings.Repeat("h", 25) + "  " }), ""},
		{"no headline", with(func(a *TextAd) { a.Headline = " " }), "headline is required"},
		{"dash-only headline", with(func(a *TextAd) { a.Headline = "—" }), "headline is required"},
		{"no description", with(func(a *TextAd) { a.Description = "" }), "description is required"},
		{"image urn", with(func(a *TextAd) { a.ImageURN = "urn:li:image:C4E22AQGX" }), ""},
		{"legacy asset urn", with(func(a *TextAd) { a.ImageURN = "urn:li:digitalmediaAsset:C4E22" }), "image URN is malformed"},
		{"both image fields", with(func(a *TextAd) { a.ImageURN = "urn:li:image:1"; a.ImageAssetID = "x" }), "both an image URN"},
		{"none", nil, "at least one entry"},
		{"too many", many, "at most 100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTextAds(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrFormatInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want ErrFormatInvalid containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateTextAdImage(t *testing.T) {
	big := append(pngHeaderOnly(100, 100), make([]byte, MaxTextAdImageBytes)...)
	for _, tc := range []struct {
		name string
		in   ImageUpload
		want string
	}{
		{"100x100 png", ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/png"}, ""},
		{"100x100 jpeg", ImageUpload{Data: testJPEG(t, 100, 100), MimeType: "image/jpeg"}, ""},
		{"too wide", ImageUpload{Data: testPNG(t, 101, 100), MimeType: "image/png"}, "require 100x100"},
		{"single-image size", ImageUpload{Data: testPNG(t, 1200, 627), MimeType: "image/png"}, "require 100x100"},
		{"over 2 MB", ImageUpload{Data: big, MimeType: "image/png"}, "at most 2000000"},
		{"gif", ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/gif"}, "only PNG and JPEG"},
		{"mime mismatch", ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/jpeg"}, "not the recorded"},
		{"empty", ImageUpload{MimeType: "image/png"}, "no bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTextAdImage(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrImageInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want ErrImageInvalid containing %q, got %v", tc.want, err)
			}
		})
	}
	// The single-image rules refuse a text ad's image, and the format picks the right rules.
	img := ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/png"}
	if ValidateImageForFormat(FormatSingleImage, img) == nil {
		t.Error("a 100x100 image passed the single-image rules")
	}
	if err := ValidateImageForFormat(FormatText, img); err != nil {
		t.Errorf("a 100x100 image was refused for a text ad: %v", err)
	}
}

// textAdServer records every request of a text create.
type textAdServer struct {
	mu        sync.Mutex
	calls     []string
	campaign  map[string]any
	creatives []map[string]any
	failAt    int // 1-based creative POST that answers failStatus; 0 never
	failCode  int
}

func (s *textAdServer) handler(t *testing.T, base func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images"):
			_, _ = io.WriteString(w, `{"value":{"uploadUrl":"`+base()+`/dms-uploads/IMG1/0?ut=sig","image":"urn:li:image:IMG1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
			_, _ = io.WriteString(w, `{"status":"AVAILABLE"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_ = json.Unmarshal(raw, &s.campaign)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			t.Errorf("a text create sent a dark post")
			http.Error(w, "no posts", http.StatusBadRequest)
		case strings.Contains(r.URL.Path, "creatives"):
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			s.creatives = append(s.creatives, b)
			if s.failAt == len(s.creatives) {
				w.WriteHeader(s.failCode)
				_, _ = io.WriteString(w, `{"status":400,"message":"bad"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:40`+string(rune('0'+len(s.creatives)))+`"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}
}

func textAdInput() CampaignInput {
	in := conversionInput()
	in.Variants = nil
	in.Format = FormatText
	in.ManualCPCBid = 5.04
	in.HSToken = "hs-tok"
	in.TextAds = []TextAd{
		{Headline: "KubeCon NA 2099", Description: "Join cloud native engineers in Atlanta.", ImageURN: "urn:li:image:OWN1"},
		{Headline: " PyTorch Conf ", Description: "Save 20% — register today."},
	}
	return in
}

func newTextAdClient(t *testing.T, s *textAdServer) *Client {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(s.handler(t, func() string { return srv.URL }))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()),
		WithImagePolling(time.Millisecond, time.Second))
}

// TestCreateCampaign_TextAdWireBodies pins the TEXT_AD create end to end: the campaign body
// (type TEXT_AD, the manual WEBSITE_CONVERSION / NONE / CPC plan with unitCost, the Text name
// segment), no dark post, and one DRAFT creative per text ad whose content is textAd with the
// normalised copy, the UTM landing page and the image.
func TestCreateCampaign_TextAdWireBodies(t *testing.T) {
	s := &textAdServer{}
	c := newTextAdClient(t, s)
	res, err := c.CreateCampaign(context.Background(), textAdInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CreativeCount != 2 || !res.BidApplied {
		t.Errorf("CreativeCount = %d, BidApplied = %v; want 2, true", res.CreativeCount, res.BidApplied)
	}
	wantName := "Events | KubeCon | LinkedIn | Conversions | Prospecting | Text | CNCF | MoFU"
	if res.CampaignName != wantName {
		t.Errorf("campaign name = %q, want %q", res.CampaignName, wantName)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.campaign
	for k, want := range map[string]any{
		"type": "TEXT_AD", "objectiveType": "WEBSITE_CONVERSION", "costType": "CPC",
		"optimizationTargetType": "NONE", "status": "PAUSED", "offsiteDeliveryEnabled": false, "name": wantName,
	} {
		if camp[k] != want {
			t.Errorf("campaign %s = %v, want %v", k, camp[k], want)
		}
	}
	if uc, _ := camp["unitCost"].(map[string]any); uc["amount"] != "5.04" || uc["currencyCode"] != "USD" {
		t.Errorf("campaign unitCost = %v, want 5.04 USD", camp["unitCost"])
	}
	if _, has := camp["format"]; has {
		t.Errorf("campaign sends format %v; LinkedIn's Text ads example sends none", camp["format"])
	}
	if len(s.creatives) != 2 {
		t.Fatalf("creatives = %d, want 2", len(s.creatives))
	}
	for i, cr := range s.creatives {
		if cr["campaign"] != "urn:li:sponsoredCampaign:200" || cr["intendedStatus"] != "DRAFT" {
			t.Errorf("creative %d campaign/status = %v/%v", i, cr["campaign"], cr["intendedStatus"])
		}
		content, _ := cr["content"].(map[string]any)
		if _, ref := content["reference"]; ref || len(content) != 1 {
			t.Errorf("creative %d content = %v, want only textAd", i, content)
		}
		ta, _ := content["textAd"].(map[string]any)
		lp, _ := ta["landingPage"].(string)
		u, perr := url.Parse(lp)
		if perr != nil || u.Host != "events.example.org" || u.Query().Get("utm_source") == "" || u.Query().Get("utm_campaign") != "hs-tok" {
			t.Errorf("creative %d landingPage = %q, want the registration URL with UTMs", i, lp)
		}
	}
	first, _ := s.creatives[0]["content"].(map[string]any)["textAd"].(map[string]any)
	if first["headline"] != "KubeCon NA 2099" || first["description"] != "Join cloud native engineers in Atlanta." || first["image"] != "urn:li:image:OWN1" {
		t.Errorf("text ad 1 = %v", first)
	}
	second, _ := s.creatives[1]["content"].(map[string]any)["textAd"].(map[string]any)
	if second["headline"] != "PyTorch Conf" || second["description"] != "Save 20%, register today." {
		t.Errorf("text ad 2 copy not normalised as validated: %v", second)
	}
	if _, has := second["image"]; has {
		t.Errorf("text ad 2 sends an image it did not name: %v", second["image"])
	}
	if s.creatives[1]["name"] != "KubeCon | text-2" {
		t.Errorf("text ad 2 name = %v", s.creatives[1]["name"])
	}
}

// TestCreateCampaign_TextAdStoredImage: a stored asset on a text ad is uploaded under the text
// ad image rules and its URN becomes the creative's image.
func TestCreateCampaign_TextAdStoredImage(t *testing.T) {
	s := &textAdServer{}
	c := newTextAdClient(t, s)
	in := textAdInput()
	in.TextAds[1].ImageAssetID = "asset-1"
	in.Images = map[string]ImageUpload{"asset-1": {Data: testPNG(t, 100, 100), MimeType: "image/png"}}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.ImageURNs["asset-1"] != "urn:li:image:IMG1" {
		t.Errorf("ImageURNs = %v", res.ImageURNs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ta, _ := s.creatives[1]["content"].(map[string]any)["textAd"].(map[string]any)
	if ta["image"] != "urn:li:image:IMG1" {
		t.Errorf("text ad 2 image = %v, want the uploaded urn:li:image:IMG1", ta["image"])
	}
}

// TestCreateCampaign_TextAdRefusedBeforeAnyRequest: every text-create defect is a pre-flight
// refusal with nothing sent.
func TestCreateCampaign_TextAdRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*CampaignInput)
		want string
	}{
		{"automatic bid", func(in *CampaignInput) { in.ManualCPCBid = 0 }, "requires manual bidding"},
		{"variants beside text ads", func(in *CampaignInput) {
			in.Variants = []CreativeVariant{{IntroText: "x", Headline: "y"}}
		}, "not variants"},
		{"no text ads", func(in *CampaignInput) { in.TextAds = nil }, "at least one entry"},
		{"long headline", func(in *CampaignInput) { in.TextAds[0].Headline = strings.Repeat("x", 26) }, "headline is 26"},
		{"unknown format", func(in *CampaignInput) { in.Format = "video" }, "format must be"},
		{"text ads on a single-image create", func(in *CampaignInput) {
			in.Format = ""
			in.Variants = []CreativeVariant{{IntroText: "x", Headline: "y"}}
		}, "only with format"},
		{"unsupplied stored image", func(in *CampaignInput) { in.TextAds[0].ImageURN = ""; in.TextAds[0].ImageAssetID = "a" }, "was not supplied"},
		{"wrong-size stored image", func(in *CampaignInput) {
			in.TextAds[0].ImageURN = ""
			in.TextAds[0].ImageAssetID = "a"
			in.Images = map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}}
		}, "require 100x100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &textAdServer{}
			c := newTextAdClient(t, s)
			in := textAdInput()
			tc.mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil {
				t.Errorf("a partial result for a refused create: %+v", res)
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("CreateCampaign = %v, want a pre-flight refusal containing %q", err, tc.want)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.calls) != 0 {
				t.Errorf("requests sent for a refused create: %v", s.calls)
			}
		})
	}
}

// TestCreateCampaign_TextAdCreativeFailureKeepsThePartial: a refused second creative returns
// the partial result (group, campaign, one creative) and an error that warns against a retry.
func TestCreateCampaign_TextAdCreativeFailureKeepsThePartial(t *testing.T) {
	s := &textAdServer{failAt: 2, failCode: http.StatusBadRequest}
	c := newTextAdClient(t, s)
	res, err := c.CreateCampaign(context.Background(), textAdInput())
	if err == nil || res == nil {
		t.Fatalf("CreateCampaign = %+v, %v; want a partial and an error", res, err)
	}
	if res.CampaignID != "200" || res.CreativeCount != 1 {
		t.Errorf("partial = campaign %q, %d creatives; want 200, 1", res.CampaignID, res.CreativeCount)
	}
	if !strings.Contains(err.Error(), "text ad 2 creative failed after 1 of 2") || !strings.Contains(err.Error(), "do NOT blindly retry") {
		t.Errorf("error = %v", err)
	}
	if res.BidApplied {
		t.Error("BidApplied set on a partial")
	}
}

// TestCreateCampaign_SingleImageUnchanged pins the default: no format sends SPONSORED_UPDATES
// and the Static name segment, as before #586.
func TestCreateCampaign_SingleImageUnchanged(t *testing.T) {
	srv := &conversionServer{}
	var campBody map[string]any
	var mu sync.Mutex
	wrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "Groups") {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(raw, &campBody)
			mu.Unlock()
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		srv.handler(t)(w, r)
	}))
	defer wrap.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(wrap.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), conversionInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !strings.Contains(res.CampaignName, "| Static |") {
		t.Errorf("name = %q, want the Static segment", res.CampaignName)
	}
	mu.Lock()
	defer mu.Unlock()
	if campBody["type"] != "SPONSORED_UPDATES" {
		t.Errorf("type = %v, want SPONSORED_UPDATES", campBody["type"])
	}
}

// TestFindCampaignByNameInGroup_RefusesAnotherCampaignType: a same-name campaign in the group
// with a different type is refused rather than reused (#586).
func TestFindCampaignByNameInGroup_RefusesAnotherCampaignType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[{"name":"N","status":"PAUSED","id":555,"type":"SPONSORED_UPDATES","campaignGroup":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{}}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", "N", "100", "", campaignTypeTextAd); err == nil || !strings.Contains(err.Error(), "with type SPONSORED_UPDATES, not TEXT_AD") {
		t.Fatalf("err = %v, want the type-mismatch refusal", err)
	}
	if id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", "N", "100", "", campaignTypeSponsoredUpdates); err != nil || id != "555" {
		t.Fatalf("same type: id %q, err %v; want reuse of 555", id, err)
	}
}

// TestUpdateCampaignAndCreativesStatus_TextAdCreatives: the pause/resume cascade discovers
// text ad creatives through the same creatives finder (it reads only the id, never the
// content), so a TEXT_AD campaign activates its DRAFT text ads like any other (#586).
func TestUpdateCampaignAndCreativesStatus_TextAdCreatives(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:401","intendedStatus":"DRAFT","content":{"textAd":{"headline":"h","description":"d","landingPage":"https://x.org"}}}],"metadata":{}}`)
			return
		}
		posts = append(posts, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignAndCreativesStatus(context.Background(), "200", StatusActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || !strings.Contains(posts[0], "sponsoredCreative%3A401") || !strings.Contains(posts[1], "adCampaigns/200") {
		t.Errorf("updates = %v, want the text ad creative then the campaign", posts)
	}
}
