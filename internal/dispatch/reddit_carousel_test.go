// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Three stored PNGs of the asset brief, the carousel's cards (#661).
var rcCards = []string{
	"aaaaaaaa-1111-4888-9999-aaaaaaaaaaaa",
	"aaaaaaaa-2222-4888-9999-aaaaaaaaaaaa",
	"aaaaaaaa-3333-4888-9999-aaaaaaaaaaaa",
}

func carouselSizer() *fakeAssetSizer {
	f := &fakeAssetSizer{sizes: map[[3]string]int64{}, mimes: map[[3]string]string{}}
	for _, id := range rcCards {
		f.sizes[[3]string{"cncf", raBrief, id}] = 4096
	}
	return f
}

// carouselExtra is a carouselCards config fragment, one card per asset id.
func carouselExtra(ids ...string) string {
	cards := make([]string, len(ids))
	for i, id := range ids {
		cards[i] = fmt.Sprintf(`{"imageAssetId":%q,"headline":"Speaker %d","destinationUrl":"https://events.example.org/s%d?token=SECRET"}`, id, i+1, i+1)
	}
	return `,"carouselCards":[` + strings.Join(cards, ",") + `]`
}

// TestRedditCarousel_AuthorsACarouselPostFromSignedURLs: with the flag on, a carousel config
// authors one CAROUSEL post with one content entry per card, in order: each media_url is an image
// signed URL for that card's PNG (verifying ONLY as an image), each caption the card's headline
// and each destination the card's URL with the UTM parameters. The persisted row records the
// asset ids and no signature, and its snapshot drops the destination's query.
func TestRedditCarousel_AuthorsACarouselPostFromSignedURLs(t *testing.T) {
	t.Setenv(constants.EnvRedditCarouselEnabled, "true")
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	signer := testAssetSigner(t)
	d.SetCreativeAssetURLs(carouselSizer(), signer)

	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(carouselExtra(strings.ToUpper(rcCards[0]), rcCards[1], rcCards[2])))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want created", camp.Status)
	}
	h.mu.Lock()
	postType, content := h.postType, h.content
	h.mu.Unlock()
	if postType != "CAROUSEL" || len(content) != len(rcCards) {
		t.Fatalf("post = %q with %d entries, want CAROUSEL with %d", postType, len(content), len(rcCards))
	}
	row := string(camp.ConfigSnapshot) + string(camp.Result)
	for i, c := range content {
		p, b, a, exp, sig, q := signedParts(t, c.MediaURL)
		if a != rcCards[i] || q.Has(assetsign.KindParam) {
			t.Fatalf("card %d media_url %q is not an image URL for its asset", i+1, c.MediaURL)
		}
		if !signer.Verify(p, b, a, exp, sig) || signer.VerifyKind(assetsign.KindVideo, p, b, a, exp, sig) {
			t.Fatalf("card %d URL must verify as an image and never as a video", i+1)
		}
		if strings.Contains(row, sig) {
			t.Fatalf("card %d signature leaked into the persisted campaign row", i+1)
		}
		if c.Caption != fmt.Sprintf("Speaker %d", i+1) {
			t.Fatalf("card %d caption = %q", i+1, c.Caption)
		}
		if !strings.HasPrefix(c.DestinationURL, fmt.Sprintf("https://events.example.org/s%d?", i+1)) ||
			!strings.Contains(c.DestinationURL, "token=SECRET") || !strings.Contains(c.DestinationURL, "utm_source=reddit") ||
			!strings.Contains(c.DestinationURL, fmt.Sprintf("utm_content=card-%d", i+1)) {
			t.Fatalf("card %d destination_url = %q, want the card URL with its query, the UTM parameters and utm_content=card-%d", i+1, c.DestinationURL, i+1)
		}
	}
	if strings.Contains(string(camp.ConfigSnapshot), "SECRET") {
		t.Fatalf("a card destination's query leaked into the snapshot: %s", camp.ConfigSnapshot)
	}
	// The snapshot records the caller's card URL (reduced to scheme+host), never the tagged URL
	// sent to Reddit: the per-card utm_content is derived at send time and is not replayed.
	if strings.Contains(string(camp.ConfigSnapshot), "utm_content") {
		t.Fatalf("the snapshot stored a tagged card destination: %s", camp.ConfigSnapshot)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), rcCards[2]) {
		t.Fatalf("snapshot should record the card asset ids, got %s", camp.ConfigSnapshot)
	}
}

// TestRedditCarousel_PostRefusalDegradesAndNeverPersistsSignatures: a refused CAROUSEL post keeps
// the image path's partial rule (campaign and ad group, no ad, created_degraded) and an error
// body echoing a card URL leaks no signature.
func TestRedditCarousel_PostRefusalDegradesAndNeverPersistsSignatures(t *testing.T) {
	t.Setenv(constants.EnvRedditCarouselEnabled, "true")
	h := &redditAssetHarness{postsFail: true}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(carouselSizer(), testAssetSigner(t))
	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(carouselExtra(rcCards[0], rcCards[1])))
	if err != nil {
		t.Fatalf("dispatch: %v (a post refusal must degrade)", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want created_degraded", camp.Status)
	}
	_, mediaURL, _ := h.captured()
	_, _, _, _, sig, _ := signedParts(t, mediaURL)
	if strings.Contains(string(camp.ConfigSnapshot)+string(camp.Result), sig) {
		t.Fatal("a signature leaked into the persisted campaign row")
	}
}

// TestRedditCarousel_RefusedBeforeAnyUpstreamCall covers every Dispatch refusal: each is
// pre-create (claim released) and made no Reddit call.
func TestRedditCarousel_RefusedBeforeAnyUpstreamCall(t *testing.T) {
	videoCard := carouselSizer()
	videoCard.mimes[[3]string{"cncf", raBrief, rcCards[1]}] = model.MimeTypeMP4
	missing := carouselSizer()
	delete(missing.sizes, [3]string{"cncf", raBrief, rcCards[2]})
	empty := carouselSizer()
	empty.sizes[[3]string{"cncf", raBrief, rcCards[0]}] = 0
	seven := make([]string, 7)
	for i := range seven {
		seven[i] = rcCards[i%len(rcCards)]
	}
	cases := []struct {
		name    string
		flag    string
		sizer   *fakeAssetSizer
		enabled bool
		extra   string
		want    string
	}{
		{"flag off", "", carouselSizer(), true, carouselExtra(rcCards...), constants.EnvRedditCarouselEnabled},
		{"flag not exactly true", "TRUE", carouselSizer(), true, carouselExtra(rcCards...), constants.EnvRedditCarouselEnabled},
		{"one card", "true", carouselSizer(), true, carouselExtra(rcCards[0]), "1 cards supplied; a carousel takes 2 to 6"},
		{"seven cards", "true", carouselSizer(), true, carouselExtra(seven...), "7 cards supplied"},
		{"with imageAssetId", "true", carouselSizer(), true, carouselExtra(rcCards...) + `,"imageAssetId":"` + raAsset + `"`, "cannot be combined"},
		{"with imageUrl", "true", carouselSizer(), true, carouselExtra(rcCards...) + `,"imageUrl":"https://x.example/a.png"`, "cannot be combined"},
		{"with postUrl", "true", carouselSizer(), true, carouselExtra(rcCards...) + `,"postUrl":"t3_abc123"`, "cannot be combined"},
		{"stored assets off", "true", carouselSizer(), false, carouselExtra(rcCards...), "stored creative assets are not enabled"},
		{"malformed card id", "true", carouselSizer(), true, carouselExtra(rcCards[0], "../x"), "carouselCards[2]: imageAssetId"},
		{"blank headline", "true", carouselSizer(), true, `,"carouselCards":[{"imageAssetId":"` + rcCards[0] + `","headline":"A","destinationUrl":"https://e.example"},{"imageAssetId":"` + rcCards[1] + `","headline":" ","destinationUrl":"https://e.example"}]`, "carouselCards[2]: headline is required"},
		{"long headline", "true", carouselSizer(), true, `,"carouselCards":[{"imageAssetId":"` + rcCards[0] + `","headline":"` + strings.Repeat("h", 181) + `","destinationUrl":"https://e.example"},{"imageAssetId":"` + rcCards[1] + `","headline":"B","destinationUrl":"https://e.example"}]`, "headline is 181 characters (max 180)"},
		{"relative destination", "true", carouselSizer(), true, `,"carouselCards":[{"imageAssetId":"` + rcCards[0] + `","headline":"A","destinationUrl":"/register"},{"imageAssetId":"` + rcCards[1] + `","headline":"B","destinationUrl":"https://e.example"}]`, "carouselCards[1]: destinationUrl must be an absolute http or https URL"},
		{"card is a video", "true", videoCard, true, carouselExtra(rcCards...), "is not an image; redditConfig.carouselCards[2].imageAssetId accepts PNG or JPEG"},
		{"card missing for this brief", "true", missing, true, carouselExtra(rcCards...), "does not exist for this brief"},
		{"card without bytes", "true", empty, true, carouselExtra(rcCards...), "no stored bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditCarouselEnabled, tc.flag)
			h := &redditAssetHarness{}
			d := h.dispatcher(t)
			var signer *assetsign.Signer
			if tc.enabled {
				signer = testAssetSigner(t)
			}
			d.SetCreativeAssetURLs(tc.sizer, signer)
			camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(tc.extra))
			requirePreCreate(t, err)
			if camp != nil {
				t.Fatalf("a refusal must return no campaign, got %+v", camp)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error %q echoes a destination URL's query", err)
			}
			if n := h.calls.Load(); n != 0 {
				t.Fatalf("%d Reddit calls were made before the refusal", n)
			}
		})
	}
}

// TestRedditCarousel_PreflightRefusesSynchronously: every config-only rule is a 400
// (ErrCreateConfigInvalid, PreflightRefusal) before any job exists, the flag first; a valid
// carousel config is not refused by the pre-check (its asset rows are Dispatch's to check).
func TestRedditCarousel_PreflightRefusesSynchronously(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	d.SetCreativeAssetURLs(carouselSizer(), testAssetSigner(t))
	pre := func(extra string) error {
		return d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(extra))
	}
	cases := []struct {
		name, flag, perVariant, cfg, want string
	}{
		{"flag off", "", "", carouselExtra(rcCards...), constants.EnvRedditCarouselEnabled},
		// Off, an unrelated mistyped field must not turn the gated carousel into an accepted job.
		{"flag off beside a broken field", "", "", carouselExtra(rcCards...) + `,"keywords":"not-a-list"`, constants.EnvRedditCarouselEnabled},
		{"one card", "true", "", carouselExtra(rcCards[0]), "a carousel takes 2 to 6"},
		{"with imageAssetId", "true", "", carouselExtra(rcCards...) + `,"imageAssetId":"` + raAsset + `"`, "cannot be combined"},
		{"malformed id", "true", "", carouselExtra(rcCards[0], "x"), "not a valid asset id"},
		{"missing destination", "true", "", `,"carouselCards":[{"imageAssetId":"` + rcCards[0] + `","headline":"A"},{"imageAssetId":"` + rcCards[1] + `","headline":"B","destinationUrl":"https://e.example"}]`, "carouselCards[1]: destinationUrl is required"},
		{"credentials in destination", "true", "", `,"carouselCards":[{"imageAssetId":"` + rcCards[0] + `","headline":"A","destinationUrl":"https://u:p@e.example"},{"imageAssetId":"` + rcCards[1] + `","headline":"B","destinationUrl":"https://e.example"}]`, "embedded credentials"},
		{"mistyped cards", "true", "", `,"carouselCards":{"imageAssetId":"x"}`, "must be an array"},
		{"mistyped card field, flag off", "", "", `,"carouselCards":[{"imageAssetId":1}]`, "must be an array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditCarouselEnabled, tc.flag)
			t.Setenv(constants.EnvRedditVideoEnabled, "")
			t.Setenv(constants.EnvRedditAdPerVariantEnabled, tc.perVariant)
			err := pre(tc.cfg)
			if tc.want == "" {
				// A carousel beside a video field is refused by whichever gate runs first.
				if !errors.Is(err, domain.ErrCreateConfigInvalid) {
					t.Fatalf("err = %v, want a 400", err)
				}
				return
			}
			requireConfigInvalid(t, err, tc.want)
		})
	}

	t.Setenv(constants.EnvRedditCarouselEnabled, "true")
	if err := pre(carouselExtra(rcCards...)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("valid carousel config: err = %v, want ErrPreflightNotApplicable", err)
	}
	// With both flags on, the video gate passes a well-formed video, so the carousel's own
	// exclusivity rule is what refuses the mix.
	t.Setenv(constants.EnvRedditVideoEnabled, "true")
	requireConfigInvalid(t, pre(carouselExtra(rcCards...)+`,"videoAssetId":"`+rvVideo+`","thumbnailAssetId":"`+rvThumb+`"`),
		"carouselCards cannot be combined with postUrl, imageUrl, imageAssetId, videoAssetId or thumbnailAssetId")
	t.Setenv(constants.EnvRedditVideoEnabled, "")
	// With one ad per variant on, a carousel with two variants would silently drop all but the
	// first: refused, while one variant is the one-ad carousel create.
	t.Setenv(constants.EnvRedditAdPerVariantEnabled, "true")
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds,
		json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","variants":[{"headline":"A"},{"headline":"B"}]`+carouselExtra(rcCards...)+`}}`)),
		"one ad per variant applies to image posts only")
	if err := pre(carouselExtra(rcCards...)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("one-variant carousel with ad-per-variant on: err = %v, want ErrPreflightNotApplicable", err)
	}
}

// TestRedditSnapshotConfig_SanitizesCarouselCards: the stored copy reduces a card destination
// to scheme+host and keeps the asset id, while the config sent to Reddit keeps the full URL.
func TestRedditSnapshotConfig_SanitizesCarouselCards(t *testing.T) {
	cfg := redditConfig{CarouselCards: []redditCarouselCard{{
		ImageAssetID: rcCards[0], Headline: "Talk", DestinationURL: "https://e.example/reset/SECRETPATH?token=SECRET",
	}}}
	snap := redditSnapshotConfig(cfg)
	if got := snap.CarouselCards[0]; strings.Contains(got.DestinationURL, "SECRET") || got.ImageAssetID != rcCards[0] || got.Headline != "Talk" {
		t.Fatalf("snapshot card = %+v", got)
	}
	if !strings.Contains(cfg.CarouselCards[0].DestinationURL, "token=SECRET") {
		t.Fatal("the snapshot must not rewrite the config sent to Reddit")
	}
}
