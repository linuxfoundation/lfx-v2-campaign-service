// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// A stored MP4 and a stored PNG thumbnail of the asset brief (#549 gap 5).
const (
	rvVideo = "77777777-7777-4888-9999-aaaaaaaaaaaa"
	rvThumb = "88888888-7777-4888-9999-aaaaaaaaaaaa"
)

func videoSizer() *fakeAssetSizer {
	return &fakeAssetSizer{
		sizes: map[[3]string]int64{{"cncf", raBrief, rvVideo}: 20 << 20, {"cncf", raBrief, rvThumb}: 4096},
		mimes: map[[3]string]string{{"cncf", raBrief, rvVideo}: model.MimeTypeMP4},
	}
}

func videoExtra(video, thumb string) string {
	return `,"videoAssetId":"` + video + `","thumbnailAssetId":"` + thumb + `"`
}

// signedParts splits a minted URL into the route's inputs.
func signedParts(t *testing.T, raw string) (p, b, a string, exp int64, sig string, q url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, "https://lfx-api.example.org"+assetsign.RoutePrefix) {
		t.Fatalf("%q is not a signed asset URL", raw)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, assetsign.RoutePrefix), "/")
	if len(segs) != 3 {
		t.Fatalf("%q has %d path segments", raw, len(segs))
	}
	exp, _ = strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	return segs[0], segs[1], segs[2], exp, u.Query().Get("sig"), u.Query()
}

// TestRedditVideo_AuthorsAVideoPostFromSignedURLs: with the flag on, a video config authors a
// VIDEO post whose media_url is a kind=video signed URL for the MP4 (verifying ONLY as a video)
// and whose thumbnail_url is an image signed URL for the PNG (verifying ONLY as an image). The
// persisted row records both asset ids and neither signature.
func TestRedditVideo_AuthorsAVideoPostFromSignedURLs(t *testing.T) {
	t.Setenv(constants.EnvRedditVideoEnabled, "true")
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	signer := testAssetSigner(t)
	d.SetCreativeAssetURLs(videoSizer(), signer)

	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(videoExtra(strings.ToUpper(rvVideo), rvThumb)))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want created", camp.Status)
	}
	if h.postType != "VIDEO" {
		t.Fatalf("post type = %q, want VIDEO", h.postType)
	}
	p, b, a, exp, sig, q := signedParts(t, h.mediaURL)
	if a != rvVideo || b != raBrief || q.Get(assetsign.KindParam) != assetsign.KindVideoValue {
		t.Fatalf("media_url %q is not a video URL for the stored MP4", h.mediaURL)
	}
	if !signer.VerifyKind(assetsign.KindVideo, p, b, a, exp, sig) || signer.Verify(p, b, a, exp, sig) {
		t.Fatal("the video URL must verify as a video and never as an image")
	}
	tp, tb, ta, texp, tsig, tq := signedParts(t, h.thumbURL)
	if ta != rvThumb || tq.Has(assetsign.KindParam) {
		t.Fatalf("thumbnail_url %q is not an image URL for the thumbnail", h.thumbURL)
	}
	if !signer.Verify(tp, tb, ta, texp, tsig) || signer.VerifyKind(assetsign.KindVideo, tp, tb, ta, texp, tsig) {
		t.Fatal("the thumbnail URL must verify as an image and never as a video")
	}
	row := string(camp.ConfigSnapshot) + string(camp.Result)
	if strings.Contains(row, sig) || strings.Contains(row, tsig) {
		t.Fatal("a signature leaked into the persisted campaign row")
	}
	if !strings.Contains(string(camp.ConfigSnapshot), "videoAssetId") || !strings.Contains(string(camp.ConfigSnapshot), "thumbnailAssetId") {
		t.Fatalf("snapshot should record both asset references, got %s", camp.ConfigSnapshot)
	}
}

// TestRedditVideo_PostRefusalDegradesAndNeverPersistsSignatures: a refused VIDEO post (the
// outcome if the unprobed shape were wrong) keeps the image path's partial rule — the campaign
// exists with no ad, recorded created_degraded — and an error body echoing BOTH URLs leaks
// neither signature.
func TestRedditVideo_PostRefusalDegradesAndNeverPersistsSignatures(t *testing.T) {
	t.Setenv(constants.EnvRedditVideoEnabled, "true")
	h := &redditAssetHarness{postsFail: true}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(videoSizer(), testAssetSigner(t))
	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(videoExtra(rvVideo, rvThumb)))
	if err != nil {
		t.Fatalf("dispatch: %v (a post refusal must degrade)", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want created_degraded", camp.Status)
	}
	_, _, _, _, vsig, _ := signedParts(t, h.mediaURL)
	_, _, _, _, tsig, _ := signedParts(t, h.thumbURL)
	row := string(camp.ConfigSnapshot) + string(camp.Result)
	if strings.Contains(row, vsig) || strings.Contains(row, tsig) {
		t.Fatal("a signature leaked into the persisted campaign row")
	}
}

// TestRedditVideo_RefusedBeforeAnyUpstreamCall covers every Dispatch refusal: each is pre-create
// (claim released) and made no Reddit call.
func TestRedditVideo_RefusedBeforeAnyUpstreamCall(t *testing.T) {
	swapped := &fakeAssetSizer{
		sizes: map[[3]string]int64{{"cncf", raBrief, rvVideo}: 20 << 20, {"cncf", raBrief, rvThumb}: 4096},
		mimes: map[[3]string]string{{"cncf", raBrief, rvThumb}: model.MimeTypeMP4},
	}
	videoThumb := &fakeAssetSizer{
		sizes: map[[3]string]int64{{"cncf", raBrief, rvVideo}: 20 << 20, {"cncf", raBrief, rvThumb}: 4096},
		mimes: map[[3]string]string{{"cncf", raBrief, rvVideo}: model.MimeTypeMP4, {"cncf", raBrief, rvThumb}: model.MimeTypeMP4},
	}
	noThumb := &fakeAssetSizer{
		sizes: map[[3]string]int64{{"cncf", raBrief, rvVideo}: 20 << 20},
		mimes: map[[3]string]string{{"cncf", raBrief, rvVideo}: model.MimeTypeMP4},
	}
	emptyVideo := &fakeAssetSizer{
		sizes: map[[3]string]int64{{"cncf", raBrief, rvVideo}: 0, {"cncf", raBrief, rvThumb}: 4096},
		mimes: map[[3]string]string{{"cncf", raBrief, rvVideo}: model.MimeTypeMP4},
	}
	cases := []struct {
		name    string
		flag    string
		sizer   *fakeAssetSizer
		enabled bool
		extra   string
		want    string
	}{
		{"flag off", "", videoSizer(), true, videoExtra(rvVideo, rvThumb), constants.EnvRedditVideoEnabled},
		{"flag not exactly true", "TRUE", videoSizer(), true, videoExtra(rvVideo, rvThumb), constants.EnvRedditVideoEnabled},
		{"flag off, thumbnail only", "", videoSizer(), true, `,"thumbnailAssetId":"` + rvThumb + `"`, constants.EnvRedditVideoEnabled},
		{"video without thumbnail", "true", videoSizer(), true, `,"videoAssetId":"` + rvVideo + `"`, "Reddit requires a thumbnail"},
		{"thumbnail without video", "true", videoSizer(), true, `,"thumbnailAssetId":"` + rvThumb + `"`, "thumbnailAssetId needs videoAssetId"},
		{"video with imageAssetId", "true", videoSizer(), true, videoExtra(rvVideo, rvThumb) + `,"imageAssetId":"` + raAsset + `"`, "cannot be combined"},
		{"video with imageUrl", "true", videoSizer(), true, videoExtra(rvVideo, rvThumb) + `,"imageUrl":"https://x.example/a.png"`, "cannot be combined"},
		{"video with postUrl", "true", videoSizer(), true, videoExtra(rvVideo, rvThumb) + `,"postUrl":"t3_abc123"`, "cannot be combined"},
		{"stored assets off", "true", videoSizer(), false, videoExtra(rvVideo, rvThumb), "stored creative assets are not enabled"},
		{"malformed video id", "true", videoSizer(), true, videoExtra("../x", rvThumb), "videoAssetId"},
		{"malformed thumbnail id", "true", videoSizer(), true, videoExtra(rvVideo, "nope"), "thumbnailAssetId"},
		{"same asset twice", "true", videoSizer(), true, videoExtra(rvVideo, rvVideo), "name the same asset"},
		{"video is an image", "true", swapped, true, videoExtra(rvVideo, rvThumb), "is not a video; redditConfig.videoAssetId accepts a stored video/mp4"},
		{"thumbnail is a video", "true", videoThumb, true, videoExtra(rvVideo, rvThumb), "is not an image; redditConfig.thumbnailAssetId accepts PNG or JPEG"},
		{"thumbnail missing for this brief", "true", noThumb, true, videoExtra(rvVideo, rvThumb), "does not exist for this brief"},
		{"video without bytes", "true", emptyVideo, true, videoExtra(rvVideo, rvThumb), "no stored bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditVideoEnabled, tc.flag)
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
			if n := h.calls.Load(); n != 0 {
				t.Fatalf("%d Reddit calls were made before the refusal", n)
			}
		})
	}
}

// TestRedditVideo_PreflightRefusesSynchronously: every config-only rule is a 400
// (ErrCreateConfigInvalid, PreflightRefusal) before any job exists, the flag first; a valid video
// config is not refused by the pre-check (its asset rows are Dispatch's to check).
func TestRedditVideo_PreflightRefusesSynchronously(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	d.SetCreativeAssetURLs(videoSizer(), testAssetSigner(t))
	pre := func(extra string) error {
		return d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(extra))
	}

	t.Setenv(constants.EnvRedditVideoEnabled, "")
	requireConfigInvalid(t, pre(videoExtra(rvVideo, rvThumb)), constants.EnvRedditVideoEnabled)
	// Off, an unrelated mistyped field must not turn the gated video into an accepted job.
	requireConfigInvalid(t, pre(videoExtra(rvVideo, rvThumb)+`,"keywords":"not-a-list"`), constants.EnvRedditVideoEnabled)

	t.Setenv(constants.EnvRedditVideoEnabled, "true")
	if err := pre(videoExtra(rvVideo, rvThumb)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("valid video config: err = %v, want ErrPreflightNotApplicable", err)
	}
	requireConfigInvalid(t, pre(`,"videoAssetId":"`+rvVideo+`"`), "thumbnailAssetId")
	requireConfigInvalid(t, pre(videoExtra(rvVideo, rvThumb)+`,"imageAssetId":"`+raAsset+`"`), "cannot be combined")
	requireConfigInvalid(t, pre(videoExtra(rvVideo, rvVideo)), "name the same asset")
	requireConfigInvalid(t, pre(videoExtra("x", rvThumb)), "not a valid asset id")
}

// TestScrubRedditOutcome_ScrubsEveryMintedURL: a video post mints two URLs, and an echo of
// EITHER in the error, the ad warning or a step must lose its signature, while errors.Is still
// sees the original error.
func TestScrubRedditOutcome_ScrubsEveryMintedURL(t *testing.T) {
	video := "https://h/_campaigns/creative-assets/p/b/v?exp=1&kind=video&sig=VIDEOSECRET"
	thumb := "https://h/_campaigns/creative-assets/p/b/t?exp=1&sig=THUMBSECRET"
	base := errors.New("fetch failed: " + video + " and " + thumb)
	res := &reddit.CampaignResult{AdWarning: "warn " + thumb, Steps: []string{"step " + video, "ok"}}
	err := scrubRedditOutcome(base, res, "", video, thumb)
	all := err.Error() + res.AdWarning + strings.Join(res.Steps, "|")
	if strings.Contains(all, "VIDEOSECRET") || strings.Contains(all, "THUMBSECRET") {
		t.Fatalf("a signature survived the scrub: %s", all)
	}
	if !errors.Is(err, base) {
		t.Fatal("the scrubbed error must still wrap the original")
	}
	if scrubRedditOutcome(nil, nil, video) != nil {
		t.Fatal("a nil error must stay nil")
	}
}
