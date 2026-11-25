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
)

func TestValidateCarousel(t *testing.T) {
	card := func(k int) CarouselCard {
		return CarouselCard{ImageAssetID: "asset-" + string(rune('a'+k)), Headline: "Speaker " + string(rune('A'+k))}
	}
	cards := func(n int) []CarouselCard {
		out := make([]CarouselCard, n)
		for i := range out {
			out[i] = card(i)
		}
		return out
	}
	with := func(f func([]CarouselCard)) []CarouselCard { c := cards(2); f(c); return c }
	const intro = "Meet the KubeCon 2099 keynote speakers"
	for _, tc := range []struct {
		name  string
		cards []CarouselCard
		intro string
		want  string // "" accepted
	}{
		{"two cards", cards(2), intro, ""},
		{"ten cards", cards(10), intro, ""},
		{"one card", cards(1), intro, "carouselCards has 1 entries; a LinkedIn carousel takes 2 to 10"},
		{"eleven cards", cards(11), intro, "carouselCards has 11 entries"},
		{"no cards", nil, intro, "carouselCards has 0 entries"},
		{"intro at 255", cards(2), strings.Repeat("i", 255), ""},
		{"intro at 256", cards(2), strings.Repeat("i", 256), "carouselIntroText is 256 characters"},
		{"no intro", cards(2), "  ", "carouselIntroText is required"},
		{"headline at 45", with(func(c []CarouselCard) { c[1].Headline = strings.Repeat("h", 45) }), intro, ""},
		{"headline of 45 multibyte runes", with(func(c []CarouselCard) { c[1].Headline = strings.Repeat("é", 45) }), intro, ""},
		{"headline at 46", with(func(c []CarouselCard) { c[1].Headline = strings.Repeat("h", 46) }), intro, "carousel card 2 headline is 46 characters"},
		{"dash-only headline", with(func(c []CarouselCard) { c[0].Headline = "—" }), intro, "carousel card 1 headline is required"},
		{"no image", with(func(c []CarouselCard) { c[1].ImageAssetID = " " }), intro, "carousel card 2 needs an imageAssetId"},
		{"destination", with(func(c []CarouselCard) { c[0].DestinationURL = "https://events.example.org/speakers?x=1" }), intro, ""},
		{"padded destination", with(func(c []CarouselCard) { c[0].DestinationURL = "  https://events.example.org/s  " }), intro, ""},
		{"relative destination", with(func(c []CarouselCard) { c[0].DestinationURL = "/speakers" }), intro, "carousel card 1 destinationUrl must be absolute"},
		{"ftp destination", with(func(c []CarouselCard) { c[0].DestinationURL = "ftp://events.example.org/x" }), intro, "carousel card 1 destinationUrl must use an http or https scheme"},
		{"userinfo destination", with(func(c []CarouselCard) { c[1].DestinationURL = "https://u:secret@events.example.org/x" }), intro, "carousel card 2 destinationUrl must not contain embedded credentials"},
		{"malformed query", with(func(c []CarouselCard) { c[0].DestinationURL = "https://events.example.org/x?a=%zz" }), intro, "malformed query"},
		{"destination at 2001", with(func(c []CarouselCard) {
			c[0].DestinationURL = "https://e.example/" + strings.Repeat("p", 2001-len("https://e.example/"))
		}), intro, "carousel card 1 destinationUrl is 2001 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCarousel(tc.cards, tc.intro)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrFormatInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want ErrFormatInvalid containing %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the refusal quotes the destination: %v", err)
			}
		})
	}
}

func TestValidateCarouselImage(t *testing.T) {
	big := append(pngHeaderOnly(1080, 1080), make([]byte, MaxCarouselImageBytes)...)
	for _, tc := range []struct {
		name string
		in   ImageUpload
		want string
	}{
		{"1080x1080 png", ImageUpload{Data: testPNG(t, 1080, 1080), MimeType: "image/png"}, ""},
		{"jpeg", ImageUpload{Data: testJPEG(t, 600, 600), MimeType: "image/jpeg"}, ""},
		// Smaller than a single-image ad's minimum: the carousel spec sets no minimum.
		{"small square", ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/png"}, ""},
		{"wider than 4320", ImageUpload{Data: pngHeaderOnly(4321, 100), MimeType: "image/png"}, "accept at most 4320x4320"},
		{"taller than 4320", ImageUpload{Data: pngHeaderOnly(100, 4321), MimeType: "image/png"}, "accept at most 4320x4320"},
		{"over 10 MB", ImageUpload{Data: big, MimeType: "image/png"}, "at most 10000000"},
		{"gif", ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/gif"}, "only PNG and JPEG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCarouselImage(tc.in)
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
	// The format picks the carousel rules: a 100x100 image is a carousel card but not a single image.
	img := ImageUpload{Data: testPNG(t, 100, 100), MimeType: "image/png"}
	if err := ValidateImageForFormat(FormatCarousel, img); err != nil {
		t.Errorf("a 100x100 image was refused for a carousel: %v", err)
	}
	if MaxImageBytesForFormat(FormatCarousel) != MaxCarouselImageBytes {
		t.Errorf("MaxImageBytesForFormat(carousel) = %d", MaxImageBytesForFormat(FormatCarousel))
	}
	if got, err := NormalizeFormat("carousel"); err != nil || got != FormatCarousel {
		t.Errorf("NormalizeFormat(carousel) = %q, %v", got, err)
	}
}

// carouselServer records every request of a carousel create.
type carouselServer struct {
	mu        sync.Mutex
	calls     []string
	campaign  map[string]any
	posts     []map[string]any
	creatives []map[string]any
	images    int
	postCode  int    // non-zero: the post POST answers this status
	existing  string // non-empty: the campaign-search answer
}

func (s *carouselServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images"):
			s.images++
			id := "IMG" + string(rune('A'+s.images-1))
			_, _ = io.WriteString(w, `{"value":{"uploadUrl":"http://`+r.Host+`/dms-uploads/`+id+`","image":"urn:li:image:`+id+`"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
			_, _ = io.WriteString(w, `{"status":"AVAILABLE"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns") && s.existing != "":
			_, _ = io.WriteString(w, s.existing)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			s.campaign = body
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			s.posts = append(s.posts, body)
			if s.postCode != 0 {
				w.WriteHeader(s.postCode)
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			s.creatives = append(s.creatives, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "tok-1"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()),
		WithImagePolling(time.Millisecond, 200*time.Millisecond), withRetryBaseDelay(time.Millisecond))
}

func (s *carouselServer) posted(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// carouselInput is a valid three-card carousel: cards 1 and 3 share asset-a, card 2 is asset-b
// with its own destination.
func carouselInput(t *testing.T) CampaignInput {
	t.Helper()
	in := imageCampaignInput(map[string]ImageUpload{
		"asset-a": {Data: testPNG(t, 1080, 1080), MimeType: "image/png"},
		"asset-b": {Data: testJPEG(t, 800, 800), MimeType: "image/jpeg"},
	})
	in.Format = FormatCarousel
	in.HSToken = "hs-tok"
	in.CarouselIntroText = "Meet the keynote speakers"
	in.CarouselCards = []CarouselCard{
		{ImageAssetID: "asset-a", Headline: "Speaker A"},
		{ImageAssetID: "asset-b", Headline: "Speaker B", DestinationURL: "https://events.example.org/speakers/b#bio"},
		{ImageAssetID: "asset-a", Headline: "Speaker C"},
	}
	return in
}

// TestCreateCampaign_CarouselWireBodies pins every LinkedIn body of a carousel create: a
// SPONSORED_UPDATES campaign of format CAROUSEL named "Carousel" whose bidding and delivery are
// the default create's, one upload per distinct asset, ONE dark post carrying the cards in order
// (each card's image URN, headline and own landing page tagged utm_content=card-<k>), and ONE
// creative referencing it.
func TestCreateCampaign_CarouselWireBodies(t *testing.T) {
	s := &carouselServer{}
	res, err := s.client(t).CreateCampaign(context.Background(), carouselInput(t))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CreativeCount != 1 || !res.BidApplied {
		t.Errorf("result = %+v; want one creative and the bid applied", res)
	}
	c := s.campaign
	if c["type"] != "SPONSORED_UPDATES" || c["format"] != "CAROUSEL" {
		t.Errorf("campaign type/format = %v/%v, want SPONSORED_UPDATES/CAROUSEL", c["type"], c["format"])
	}
	if c["objectiveType"] != ObjectiveWebsiteVisit || c["optimizationTargetType"] != OptimizationTargetMaxClick || c["offsiteDeliveryEnabled"] != false {
		t.Errorf("campaign bidding/delivery = %v/%v/%v; want the default create's", c["objectiveType"], c["optimizationTargetType"], c["offsiteDeliveryEnabled"])
	}
	if _, set := c["audienceExpansionEnabled"]; set {
		t.Error("audienceExpansionEnabled sent; the default create sends none")
	}
	if name, _ := c["name"].(string); !strings.Contains(name, " | Carousel | ") || strings.Contains(name, "Static") {
		t.Errorf("campaign name = %q, want the Ad Format segment Carousel", name)
	}
	if s.images != 2 {
		t.Errorf("images uploaded = %d, want 2 (one per distinct asset)", s.images)
	}
	if len(s.posts) != 1 || len(s.creatives) != 1 {
		t.Fatalf("posts %d, creatives %d; want 1 and 1", len(s.posts), len(s.creatives))
	}
	post := s.posts[0]
	if post["commentary"] != "Meet the keynote speakers" {
		t.Errorf("commentary = %v", post["commentary"])
	}
	if dist, _ := post["distribution"].(map[string]any); dist["feedDistribution"] != "NONE" {
		t.Errorf("not a dark post: %v", dist)
	}
	content, _ := post["content"].(map[string]any)
	carousel, _ := content["carousel"].(map[string]any)
	cards, _ := carousel["cards"].([]any)
	if len(cards) != 3 {
		t.Fatalf("cards = %v, want 3", carousel)
	}
	wantImage := []string{"urn:li:image:IMGA", "urn:li:image:IMGB", "urn:li:image:IMGA"}
	wantTitle := []string{"Speaker A", "Speaker B", "Speaker C"}
	wantHost := []string{"/kubecon", "/speakers/b", "/kubecon"}
	for i, raw := range cards {
		card, _ := raw.(map[string]any)
		media, _ := card["media"].(map[string]any)
		if media["id"] != wantImage[i] || media["title"] != wantTitle[i] {
			t.Errorf("card %d media = %v", i+1, media)
		}
		lp, _ := card["landingPage"].(string)
		u, err := url.Parse(lp)
		if err != nil {
			t.Fatalf("card %d landingPage %q: %v", i+1, lp, err)
		}
		q := u.Query()
		if u.Path != wantHost[i] || q.Get("utm_content") != "card-"+string(rune('1'+i)) || q.Get("utm_source") != "linkedin" || q.Get("utm_campaign") != "hs-tok" {
			t.Errorf("card %d landingPage = %q; want path %s tagged utm_content=card-%d", i+1, lp, wantHost[i], i+1)
		}
		if i == 1 && u.Fragment != "bio" {
			t.Errorf("card 2 lost its fragment: %q", lp)
		}
	}
	if first, _ := cards[0].(map[string]any); post["contentLandingPage"] != first["landingPage"] {
		t.Errorf("contentLandingPage = %v, want card 1's landing page", post["contentLandingPage"])
	}
	cr := s.creatives[0]
	if ref, _ := cr["content"].(map[string]any); ref["reference"] != "urn:li:share:300" {
		t.Errorf("creative content = %v", cr["content"])
	}
	if name, _ := cr["name"].(string); name != "KubeCon | carousel-1" {
		t.Errorf("creative name = %q", name)
	}
	if _, set := cr["leadgenCallToAction"]; set {
		t.Error("a carousel creative carries leadgenCallToAction")
	}
}

// TestCreateCampaign_CarouselRefusedBeforeAnyRequest: every carousel rule the client owns is
// decided before the first LinkedIn request.
func TestCreateCampaign_CarouselRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CampaignInput)
		want string
	}{
		{"variants beside the cards", func(in *CampaignInput) {
			in.Variants = []CreativeVariant{{IntroText: "i", Headline: "h"}}
		}, "not variants"},
		{"cards without the carousel format", func(in *CampaignInput) { in.Format = "" }, "accepted only with format \"carousel\""},
		{"intro without the carousel format", func(in *CampaignInput) {
			in.Format, in.CarouselCards = FormatSingleImage, nil
			in.Variants = []CreativeVariant{{IntroText: "i", Headline: "h"}}
		}, "accepted only with format \"carousel\""},
		{"one card", func(in *CampaignInput) { in.CarouselCards = in.CarouselCards[:1] }, "takes 2 to 10 cards"},
		{"image not supplied", func(in *CampaignInput) { in.CarouselCards[1].ImageAssetID = "asset-z" }, "carousel card 2 names stored image asset"},
		{"image over 4320", func(in *CampaignInput) {
			in.Images["asset-b"] = ImageUpload{Data: pngHeaderOnly(5000, 100), MimeType: "image/png"}
		}, "carousel card 2 image asset"},
		{"lead generation", func(in *CampaignInput) {
			in.Objective = ObjectiveLeadGeneration
			in.LeadGen = &LeadGen{FormID: "123"}
		}, "format \"carousel\" is not available with objective LEAD_GENERATION"},
		{"tagged landing page over 2000", func(in *CampaignInput) {
			in.CarouselCards[0].DestinationURL = "https://e.example/" + strings.Repeat("p", 1990-len("https://e.example/"))
		}, "carousel card 1 landing page is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &carouselServer{}
			in := carouselInput(t)
			tc.edit(&in)
			res, err := s.client(t).CreateCampaign(context.Background(), in)
			if err == nil || res != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got res=%v err=%v; want nil and an error containing %q", res, err, tc.want)
			}
			if n := len(s.calls); n != 0 {
				t.Errorf("%d request(s) sent: %v", n, s.calls)
			}
		})
	}
}

// TestCreateCampaign_CarouselPostFailureKeepsThePartial: a carousel post that fails after the
// campaign exists returns the partial (campaign id, zero creatives, minted image URNs), never
// nil, so the dispatcher retains the claim; an ambiguous failure says UNCONFIRMED.
func TestCreateCampaign_CarouselPostFailureKeepsThePartial(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want string
	}{
		{"definite refusal", http.StatusUnprocessableEntity, "carousel dark post failed"},
		{"ambiguous server error", http.StatusInternalServerError, "carousel dark post UNCONFIRMED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &carouselServer{postCode: tc.code}
			res, err := s.client(t).CreateCampaign(context.Background(), carouselInput(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if res == nil || res.CampaignID != "200" || res.CreativeCount != 0 || len(res.ImageURNs) != 2 {
				t.Fatalf("partial = %+v; want campaign 200, no creative, both image URNs", res)
			}
			if s.posted("POST /adAccounts/123456789/creatives") != 0 {
				t.Error("a creative was posted after the carousel post failed")
			}
		})
	}
}

// TestFindCampaignByNameInGroup_RefusesAnotherSponsoredFormat: a same-name SPONSORED_UPDATES
// campaign of another creative kind (image, video, carousel) is never reused (#665).
func TestFindCampaignByNameInGroup_RefusesAnotherSponsoredFormat(t *testing.T) {
	for _, tc := range []struct {
		name, existing, want string
		reuse                bool
	}{
		{"carousel reuses carousel", "CAROUSEL", campaignFormatCarousel, true},
		{"carousel refuses an image campaign", "STANDARD_UPDATE", campaignFormatCarousel, false},
		{"carousel refuses a video campaign", "SINGLE_VIDEO", campaignFormatCarousel, false},
		{"image refuses a carousel campaign", "CAROUSEL", "", false},
		{"video refuses a carousel campaign", "CAROUSEL", campaignFormatSingleVideo, false},
		{"image reuses an image campaign", "STANDARD_UPDATE", "", true},
		{"carousel reuses an unreported format", "", campaignFormatCarousel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[{"name":"N","status":"PAUSED","id":555,"type":"SPONSORED_UPDATES","format":"`+tc.existing+`","campaignGroup":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{}}`)
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			id, err := c.findCampaignByNameInGroup(context.Background(), "adAccounts/123456789/adCampaigns", "N", "100", "", "", campaignTypeSponsoredUpdates, tc.want)
			if tc.reuse {
				if err != nil || id != "555" {
					t.Fatalf("id %q, err %v; want reuse of 555", id, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "with format "+tc.existing) {
				t.Fatalf("id %q, err %v; want the format-mismatch refusal", id, err)
			}
		})
	}
}

// TestBuildUTMURLWithContent: a carousel card's tag replaces only utm_content; BuildUTMURL keeps
// its variant tag byte for byte.
func TestBuildUTMURLWithContent(t *testing.T) {
	const base, name = "https://events.example.org/reg?src=x", "Events | KubeCon | NA"
	card := buildUTMURLWithContent(base, "hs", name, "card-3")
	variant := BuildUTMURL(base, "hs", name, 3)
	if !strings.Contains(card, "utm_content=card-3") || !strings.Contains(variant, "utm_content=variant-3") {
		t.Fatalf("card %q, variant %q", card, variant)
	}
	if strings.Replace(card, "card-3", "variant-3", 1) != variant {
		t.Errorf("the two differ beyond utm_content: %q vs %q", card, variant)
	}
}
