// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func xTestMP4(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x00\x00\x00\x18ftypmp42")
	return b
}

// briefVideo serves one MP4 asset under xAssetID for testBrief.
func briefVideo(v []byte) *xScopedCreativeAssets {
	a := briefAsset(v)
	b := testBrief()
	a.assets[b.ProjectID+"/"+b.ID+"/"+xAssetID].MimeType = model.MimeTypeMP4
	return a
}

func xVideoConfig(extra string) json.RawMessage {
	return json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"Watch the keynote","asUserId":"u1","videoAssetId":"` + xAssetID + `"` + extra + `}}`)
}

// Gap 7 (#564): videoAssetId sits behind its own default-off flag (a synchronous 400 naming it,
// refused again at dispatch before any X call) and its config-only rules are synchronous 400s.
func TestTwitterVideo_GateAndRules(t *testing.T) {
	t.Setenv(constants.EnvTwitterVideoEnabled, "")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefVideo(xTestMP4(900)), nil)
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, xVideoConfig("")), constants.EnvTwitterVideoEnabled)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xVideoConfig(""))
	requirePreCreate(t, err)
	x.mu.Lock()
	total := x.total
	x.mu.Unlock()
	if total != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", total)
	}

	t.Setenv(constants.EnvTwitterVideoEnabled, "true")
	t.Setenv(constants.EnvTwitterFollowerLookalikesEnabled, "true")
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	ids := make([]string, 31)
	for i := range ids {
		ids[i] = `"` + strconv.Itoa(100+i) + `"`
	}
	// The gap 9 age bucket and gender count toward the video cap too (#564): 29 + 1 + 1 = 31.
	ageGender := `,"followerLookalikeUserIds":[` + strings.Join(ids[:29], ",") + `],"ageBucket":"AGE_OVER_18","gender":"MALE"`
	base := `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10",`
	for raw, want := range map[string]string{
		string(xVideoConfig(`,"imageAssetId":"` + xAssetID + `"`)):                           "image or a video, not both",
		base + `"tweetId":"123","videoAssetId":"` + xAssetID + `"}}`:                         "requires tweetText (or variants) and no tweetId",
		base + `"videoAssetId":"` + xAssetID + `"}}`:                                         "requires tweetText (or variants) and no tweetId",
		base + `"tweetText":"t","videoAssetId":"not-a-uuid"}}`:                               "is not a valid asset id",
		base + `"tweetText":"t","videoAssetId":7}}`:                                          "twitterConfig.videoAssetId: must be a string",
		string(xVideoConfig(`,"followerLookalikeUserIds":[` + strings.Join(ids, ",") + `]`)): "at most 30 targeting criteria",
		string(xVideoConfig(ageGender)):                                                      "this one names 31",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, json.RawMessage(raw)), want)
	}
}

// On: the stored MP4 is uploaded as amplify_video and attached to the authored tweet.
func TestTwitterVideo_DispatchAttachesTheVideo(t *testing.T) {
	t.Setenv(constants.EnvTwitterVideoEnabled, "true")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefVideo(xTestMP4(900)), nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xVideoConfig(""))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	x.mu.Lock()
	q, init := x.tweetQuery, x.initBody
	x.mu.Unlock()
	if !strings.Contains(init, `"media_category":"amplify_video"`) || !strings.Contains(init, `"media_type":"video/mp4"`) {
		t.Errorf("initialize body %s, want amplify_video and video/mp4", init)
	}
	if !strings.Contains(q, "media_keys=3_42") || camp.Status != campaignStatusCreated {
		t.Fatalf("tweet query %q status %q, want the video key on a created row", q, camp.Status)
	}
}

// An image in the video slot, and a video in the image slot (#586), are refused before any X call.
func TestTwitterVideo_SlotsRefuseTheOtherMedia(t *testing.T) {
	t.Setenv(constants.EnvTwitterVideoEnabled, "true")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xVideoConfig(""))
	requireNoUpstreamCreate(t, err)
	if !strings.Contains(err.Error(), "is not a video") {
		t.Errorf("image in the video slot: %v", err)
	}
	d = newXCreativeDispatcher(srv.URL, briefVideo(xTestMP4(900)), nil)
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(""))
	requireNoUpstreamCreate(t, err)
	if !strings.Contains(err.Error(), "is a video, not an image") {
		t.Errorf("video in the image slot: %v", err)
	}
	x.mu.Lock()
	total := x.total
	x.mu.Unlock()
	if total != 0 {
		t.Fatalf("%d X calls; want 0", total)
	}
}

// The video can be the website card's media: a card with a videoAssetId and no imageAssetId passes.
func TestTwitterVideo_IsCardMedia(t *testing.T) {
	t.Setenv(constants.EnvTwitterVideoEnabled, "true")
	t.Setenv(constants.EnvTwitterWebsiteCardEnabled, "true")
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefVideo(xTestMP4(900)), nil)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xVideoConfig(`,"card":{"headline":"Keynote"}`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if x.count("card") != 1 {
		t.Fatalf("cards = %d, want 1", x.count("card"))
	}
}
