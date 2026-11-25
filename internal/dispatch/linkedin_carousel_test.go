// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// liCarouselAssetB is the second card image of the carousel fixture.
const liCarouselAssetB = "0b7d9e2c-1a2b-4c3d-8e4f-5a6b7c8d9e0f"

// liCarouselConfig builds a format "carousel" linkedInConfig from raw card objects and any extra
// top-level fields.
func liCarouselConfig(extra string, cards ...string) json.RawMessage {
	if extra != "" {
		extra = "," + extra
	}
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"format":"carousel","carouselIntroText":"Meet the keynote speakers",
		"carouselCards":[` + strings.Join(cards, ",") + `]` + extra + `}}`)
}

func liCard(assetID, extra string) string {
	c := `{"imageAssetId":"` + assetID + `","headline":"Speaker"`
	if extra != "" {
		c += "," + extra
	}
	return c + "}"
}

// liCarouselAssets holds two PNG card images of the test brief.
func liCarouselAssets(t *testing.T) *multiCreativeAssets {
	return &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		msAssetID:        {ID: msAssetID, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 1080, 1080)},
		liCarouselAssetB: {ID: liCarouselAssetB, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 600, 600)},
	}}
}

// TestLinkedInCarousel_DispatchEndToEnd drives PreflightCreate and Dispatch with the flag on: a
// CAROUSEL campaign whose one creative is a carousel post of the two stored images, recorded as a
// clean create (one creative asked, one made), with the snapshot holding canonical asset ids, the
// card headline and only the destination's origin.
func TestLinkedInCarousel_DispatchEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCarouselEnabled, "true")
	api := newLIImageAPI(t)
	d := newLIImageDispatcher(api, liCarouselAssets(t), nil)
	cfg := liCarouselConfig("",
		liCard(strings.ToUpper(msAssetID), ""),
		liCard(liCarouselAssetB, `"destinationUrl":"https://events.example/speakers/b?token=s3cret"`))

	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate on a valid carousel = %v, want ErrPreflightNotApplicable", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q (a carousel is one creative)", camp.Status, campaignStatusCreated)
	}
	if len(api.campaigns) != 1 || api.campaigns[0]["format"] != "CAROUSEL" {
		t.Fatalf("campaign bodies = %v, want one of format CAROUSEL", api.campaigns)
	}
	if got := api.count("POST /images"); got != 2 {
		t.Errorf("image uploads = %d, want 2", got)
	}
	if len(api.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(api.posts))
	}
	content, _ := api.posts[0]["content"].(map[string]any)
	carousel, _ := content["carousel"].(map[string]any)
	cards, _ := carousel["cards"].([]any)
	if len(cards) != 2 {
		t.Fatalf("carousel = %v, want 2 cards", carousel)
	}
	second, _ := cards[1].(map[string]any)
	if lp, _ := second["landingPage"].(string); !strings.HasPrefix(lp, "https://events.example/speakers/b?") || !strings.Contains(lp, "utm_content=card-2") {
		t.Errorf("card 2 landingPage = %q, want its destination tagged card-2", lp)
	}
	if got := api.count("POST /adAccounts/123456789/creatives"); got != 1 {
		t.Errorf("creatives = %d, want 1", got)
	}
	snap := string(camp.ConfigSnapshot)
	if strings.Contains(snap, strings.ToUpper(msAssetID)) || !strings.Contains(snap, msAssetID) {
		t.Errorf("snapshot must store the canonical asset id: %s", snap)
	}
	if strings.Contains(snap, "s3cret") || strings.Contains(snap, "/speakers/b") || !strings.Contains(snap, `"destinationUrl":"https://events.example"`) {
		t.Errorf("snapshot must keep only the destination's origin: %s", snap)
	}
	if !strings.Contains(snap, `"carouselIntroText":"Meet the keynote speakers"`) {
		t.Errorf("snapshot lost the intro text: %s", snap)
	}
}

// TestLinkedInCarousel_RefusedBeforeAnythingIsCreated: the flag, the request shape and the assets
// are decided before any LinkedIn request, the claim and the asset reservation are released, and
// every shape refusal is also PreflightCreate's synchronous 400.
func TestLinkedInCarousel_RefusedBeforeAnythingIsCreated(t *testing.T) {
	two := []string{liCard(msAssetID, ""), liCard(liCarouselAssetB, "")}
	cases := []struct {
		name      string
		flag      string
		leadGen   bool
		cfg       json.RawMessage
		assets    func(*testing.T) *multiCreativeAssets
		preflight bool // PreflightCreate refuses it too
	}{
		{"feature off", "", false, liCarouselConfig("", two...), liCarouselAssets, true},
		{"feature misspelled", "TRUE", false, liCarouselConfig("", two...), liCarouselAssets, true},
		{"one card", "true", false, liCarouselConfig("", two[0]), liCarouselAssets, true},
		{"eleven cards", "true", false, liCarouselConfig("", strings.Split(strings.Repeat(two[0]+"|", 11), "|")[:11]...), liCarouselAssets, true},
		{"variants beside the cards", "true", false, liCarouselConfig(`"variants":[`+liVariant("")+`]`, two...), liCarouselAssets, true},
		{"malformed asset id", "true", false, liCarouselConfig("", liCard("not-a-uuid", ""), two[1]), liCarouselAssets, true},
		{"headline over 45", "true", false, liCarouselConfig("", two[0], `{"imageAssetId":"`+liCarouselAssetB+`","headline":"`+strings.Repeat("h", 46)+`"}`), liCarouselAssets, true},
		{"relative destination", "true", false, liCarouselConfig("", two[0], liCard(liCarouselAssetB, `"destinationUrl":"/speakers"`)), liCarouselAssets, true},
		{"numeric headline", "true", false, liCarouselConfig("", two[0], `{"imageAssetId":"`+liCarouselAssetB+`","headline":7}`), liCarouselAssets, true},
		{"unknown card key", "true", false, liCarouselConfig("", two[0], liCard(liCarouselAssetB, `"destination":"https://x.example"`)), liCarouselAssets, true},
		{"null cards", "true", false, json.RawMessage(strings.Replace(string(liCarouselConfig("", two...)), `"carouselCards":[`, `"carouselCards":null,"x":[`, 1)), liCarouselAssets, true},
		{"numeric intro text", "true", false, json.RawMessage(strings.Replace(string(liCarouselConfig("", two...)), `"carouselIntroText":"Meet the keynote speakers"`, `"carouselIntroText":5`, 1)), liCarouselAssets, true},
		{"cards without the carousel format", "true", false, json.RawMessage(strings.Replace(string(liCarouselConfig(`"variants":[`+liVariant("")+`]`, two...)), `"format":"carousel",`, "", 1)), liCarouselAssets, true},
		{"lead generation carousel", "true", true, liCarouselConfig(`"objective":"lead_generation","leadGenForm":"12345"`, two...), liCarouselAssets, true},
		{"asset of another brief", "true", false, liCarouselConfig("", two[0], liCard("11111111-2222-4333-8444-555555555555", "")), liCarouselAssets, false},
		{"video named as a card image", "true", false, liCarouselConfig("", two...), func(t *testing.T) *multiCreativeAssets {
			a := liCarouselAssets(t)
			a.assets[liCarouselAssetB] = &model.CreativeAsset{ID: liCarouselAssetB, MimeType: model.MimeTypeMP4, Bytes: liTestMP4(liVideoSize)}
			return a
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsCarouselEnabled, tc.flag)
			if tc.leadGen {
				t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, "true")
			}
			api := newLIImageAPI(t)
			reserver := NewAssetReserver(10<<20, testAssetReserveWait)
			d := newLIImageDispatcher(api, tc.assets(t), reserver)
			perr := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, tc.cfg)
			if tc.preflight != !errors.Is(perr, domain.ErrPreflightNotApplicable) {
				t.Errorf("PreflightCreate = %v, want refused=%v", perr, tc.preflight)
			}
			if tc.preflight && !errors.Is(perr, domain.ErrCreateConfigInvalid) {
				t.Errorf("PreflightCreate = %v, want ErrCreateConfigInvalid (a 400)", perr)
			}
			if tc.flag != "true" && (perr == nil || !strings.Contains(perr.Error(), constants.EnvLinkedInAdsCarouselEnabled)) {
				t.Errorf("PreflightCreate = %v, want the refusal to name %s", perr, constants.EnvLinkedInAdsCarouselEnabled)
			}
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, tc.cfg)
			if camp != nil || !isNoUpstreamCreate(err) {
				t.Fatalf("want a NoUpstreamCreate refusal, got camp=%v err=%v", camp, err)
			}
			if n := api.count(""); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn; a refused carousel must send none", n)
			}
			if rel, ok := reserver.reserve(context.Background(), 10<<20); !ok {
				t.Error("a refused dispatch leaked its asset reservation")
			} else {
				rel()
			}
		})
	}
}

// TestLinkedInCarousel_ImageRefusedByTheCarouselRule: a 4400x2000 PNG passes the single-image
// rules (up to 7680x4320) and fails only the carousel's 4320x4320 ceiling, so the refusal proves
// the card image was checked under the carousel rules, before any LinkedIn request.
func TestLinkedInCarousel_ImageRefusedByTheCarouselRule(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCarouselEnabled, "true")
	api := newLIImageAPI(t)
	assets := liCarouselAssets(t)
	assets.assets[liCarouselAssetB] = &model.CreativeAsset{ID: liCarouselAssetB, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 4400, 2000)}
	d := newLIImageDispatcher(api, assets, nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds,
		liCarouselConfig("", liCard(msAssetID, ""), liCard(liCarouselAssetB, "")))
	if camp != nil || !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), "carousel ads accept at most 4320x4320") {
		t.Fatalf("want the carousel image refusal before any create, got camp=%v err=%v", camp, err)
	}
	// Decided by the dispatcher's asset check (resolveLinkedInImages), not left to the client's
	// own pre-request backstop.
	if strings.Contains(err.Error(), "before any upstream create") || !strings.Contains(err.Error(), "creative asset "+liCarouselAssetB) {
		t.Errorf("err = %v; want the dispatcher's asset refusal naming %s", err, liCarouselAssetB)
	}
	if n := api.count(""); n != 0 {
		t.Errorf("%d request(s) reached LinkedIn", n)
	}
}

// liMetadataAssets adds the bytes-free metadata read to multiCreativeAssets, answering each asset's
// RECORDED width and height as the production repository does.
type liMetadataAssets struct{ *multiCreativeAssets }

func (m liMetadataAssets) GetAssetMetadata(_ context.Context, _, _, assetID string) (*model.CreativeAsset, error) {
	a, ok := m.assets[assetID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	meta := *a
	meta.Bytes = nil
	return &meta, nil
}

// TestLinkedInCarousel_CardImagesMustBeSquare (decision 2026-11-25): a card image that is not
// exactly 1:1 is a synchronous 400 when its recorded dimensions say so, and is refused again by
// Dispatch from the image header before any LinkedIn request; a square one, or one whose
// dimensions were never recorded, passes PreflightCreate.
func TestLinkedInCarousel_CardImagesMustBeSquare(t *testing.T) {
	for _, tc := range []struct {
		name            string
		w, h            int  // card 2's pixels
		recorded        bool // card 2's width and height are on its row
		preflight400    bool
		dispatchRefused bool
	}{
		{"square", 600, 600, true, false, false},
		{"landscape, recorded", 1200, 627, true, true, true},
		{"one pixel off, recorded", 1080, 1081, true, true, true},
		{"landscape, not recorded", 1200, 627, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsCarouselEnabled, "true")
			api := newLIImageAPI(t)
			assets := liCarouselAssets(t)
			assets.assets[msAssetID].Width, assets.assets[msAssetID].Height = 1080, 1080
			b := &model.CreativeAsset{ID: liCarouselAssetB, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, tc.w, tc.h)}
			if tc.recorded {
				b.Width, b.Height = tc.w, tc.h
			}
			assets.assets[liCarouselAssetB] = b
			d := newLIImageDispatcher(api, liMetadataAssets{assets}, nil)
			cfg := liCarouselConfig("", liCard(msAssetID, ""), liCard(liCarouselAssetB, ""))

			ctx := model.WithCreatePreflightBrief(context.Background(), testBrief())
			perr := d.PreflightCreate(ctx, "cncf", model.ProviderLinkedInAds, cfg)
			if tc.preflight400 {
				if !errors.Is(perr, domain.ErrCreateConfigInvalid) || !strings.Contains(perr.Error(), "carouselCards[1].imageAssetId") || !strings.Contains(perr.Error(), "must be square (1:1)") {
					t.Errorf("PreflightCreate = %v, want a 400 naming carouselCards[1] as not square", perr)
				}
			} else if !errors.Is(perr, domain.ErrPreflightNotApplicable) {
				t.Errorf("PreflightCreate = %v, want ErrPreflightNotApplicable", perr)
			}

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if !tc.dispatchRefused {
				if err != nil || camp == nil {
					t.Fatalf("Dispatch of square cards: camp=%v err=%v", camp, err)
				}
				return
			}
			if camp != nil || !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), "must be square (1:1)") {
				t.Fatalf("Dispatch = camp %v err %v; want the square refusal before any create", camp, err)
			}
			if n := api.count(""); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn", n)
			}
		})
	}
}

// TestLinkedInCarousel_DefaultCreateUnchanged: with the flag on, a create that names no carousel
// field is today's single-image create — no campaign format, no carousel post.
func TestLinkedInCarousel_DefaultCreateUnchanged(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCarouselEnabled, "true")
	api := newLIImageAPI(t)
	d := newLIImageDispatcher(api, nil, nil)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(""))); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(api.campaigns) != 1 {
		t.Fatalf("campaigns = %d", len(api.campaigns))
	}
	if _, set := api.campaigns[0]["format"]; set {
		t.Errorf("a default create sent format %v", api.campaigns[0]["format"])
	}
	content, _ := api.posts[0]["content"].(map[string]any)
	if _, ok := content["article"]; !ok {
		t.Errorf("a default create's post is not an article: %v", content)
	}
}
