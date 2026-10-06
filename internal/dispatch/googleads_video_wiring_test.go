// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The dispatch-layer half of the Video (YouTube) path, for the reason
// googleads_pmax_wiring_test.go states for Performance Max: video_test.go proves
// CreateVideoCampaign builds the right payload FROM a CampaignInput and
// video_creative_test.go proves every bound, but neither can prove that the wire config
// is mapped onto that input at all. A dropped `channel` or a `videoCreative` the mapper
// never reads leaves both suites green while the feature is unreachable through the API.
//
// Unlike Performance Max, the WHOLE cascade is exercised from here. Video has no image
// fetch — a YouTube video is referenced by id and never held by this client — so there is
// no dial guard to relax and no reason to stop at the campaign shell.

func videoWireCreative() map[string]any {
	return map[string]any{
		"youtubeVideoIds": []string{"dQw4w9WgXcQ"},
		// Display width 12: the Video short headline limit is 15, not the RSA's 30.
		"headlines":     []string{"Register now"},
		"longHeadlines": []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		"descriptions":  []string{"Talks, workshops and the hallway track."},
		"callToActions": []string{"Register"},
	}
}

func videoWireConfig(t *testing.T, creative map[string]any) json.RawMessage {
	t.Helper()
	inner := map[string]any{"budget": 50, "channel": "video"}
	if creative != nil {
		inner["videoCreative"] = creative
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": inner})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

// channel: "video" must reach the kind switch, not merely be accepted and then created as
// a Search campaign. The campaign body is where that shows: VIDEO alone would still be
// ambiguous, so the sub-type is asserted with it — a VIDEO campaign without
// VIDEO_ACTION is a different product that does not bid toward conversions at all.
func TestGoogleAds_VideoChannelReachesTheCampaignShell(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, nil))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("nil campaign on success")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaigns) != 1 {
		t.Fatalf("got %d campaigns:mutate calls, want 1", len(cap.campaigns))
	}
	body := string(cap.campaigns[0])
	for _, want := range []string{`"advertisingChannelType":"VIDEO"`, `"advertisingChannelSubType":"VIDEO_ACTION"`, `"maximizeConversions"`} {
		if !strings.Contains(body, want) {
			t.Errorf("campaign body is missing %s — the wire channel did not reach the kind switch: %s", want, body)
		}
	}
	if strings.Contains(body, `"manualCpc"`) {
		t.Errorf("campaign body carries manualCpc; Video bids only toward conversions: %s", body)
	}
	// Video DOES create an ad group, unlike Performance Max — but with no creative there is
	// no ad, and an ad group with no ad must not be reported as a serving campaign.
	if len(cap.adGroups) != 1 {
		t.Errorf("got %d adGroups:mutate calls, want 1 — Video serves from an ad group", len(cap.adGroups))
	}
	if len(cap.adGroupAds) != 0 {
		t.Errorf("got %d adGroupAds:mutate calls with no creative supplied, want 0", len(cap.adGroupAds))
	}
}

// The creative sub-object end to end: every list must reach the client, become a
// youtubeVideoAsset and a videoResponsiveAd, and come back as recorded ids. Running the
// whole cascade here rather than stopping at a refusal is what distinguishes a mapper that
// carried the lists from one that carried only enough of them to get past the validator.
func TestGoogleAds_VideoCreativeConfigReachesTheAd(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, videoWireCreative()))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("nil campaign on success")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.assets == nil {
		t.Fatal("no assets:mutate call — the YouTube video id never reached the client")
	}
	if !strings.Contains(string(cap.assets), `"youtubeVideoId":"dQw4w9WgXcQ"`) {
		t.Errorf("asset body does not carry the caller's video id as a youtubeVideoAsset: %s", cap.assets)
	}
	if len(cap.adGroupAds) != 1 {
		t.Fatalf("got %d adGroupAds:mutate calls, want 1", len(cap.adGroupAds))
	}
	ad := string(cap.adGroupAds[0])
	// Each list in its OWN slot. Merged headlines and longHeadlines would still produce an
	// ad Google accepts, with the wrong asset field types on half the text.
	for _, want := range []string{
		`"videoResponsiveAd"`,
		`"Register now"`,
		`"KubeCon + CloudNativeCon Europe 2026, Amsterdam"`,
		`"Talks, workshops and the hallway track."`,
		`"Register"`,
	} {
		if !strings.Contains(ad, want) {
			t.Errorf("ad body is missing %s — a creative list was dropped between the wire and the client: %s", want, ad)
		}
	}
	if !strings.Contains(ad, `"longHeadlines"`) || !strings.Contains(ad, `"callToActions"`) {
		t.Errorf("ad body does not carry longHeadlines and callToActions as their own lists: %s", ad)
	}
}

// Each wire list must land in its OWN client slot, and the proof is that omitting one
// produces that list's own refusal. Video is the channel where this matters most: three of
// its four text lists are required and a mapper that merged any two would leave the
// combined count satisfied.
func TestGoogleAds_EachVideoListReachesItsOwnSlot(t *testing.T) {
	cases := map[string]struct {
		omit string
		want string
	}{
		// Each `want` is anchored on the COUNT as well as the label, which is what keeps
		// the headline case from being satisfied by the long-headline message.
		"youtube video ids": {"youtubeVideoIds", "needs at least 1 YouTube video"},
		"headlines":         {"headlines", "needs at least 1 headline"},
		"long headlines":    {"longHeadlines", "needs at least 1 long headline"},
		"descriptions":      {"descriptions", "needs at least 1 description"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			creative := videoWireCreative()
			delete(creative, tc.omit)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, creative))
			if err == nil {
				t.Fatalf("expected %s to be required", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name the missing %s (%q)", err, name, tc.want)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate — a later one strands a paid budget")
			}
		})
	}
}

// callToActions is the one optional list, and optional must mean optional: omitting it
// creates the ad with Google's default rather than refusing. Paired with the table above,
// this pins which of the four lists is required — the distinction a reader of the config
// struct alone cannot check.
func TestGoogleAds_VideoCallToActionsAreOptional(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	creative := videoWireCreative()
	delete(creative, "callToActions")

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, creative)); err != nil {
		t.Fatalf("Dispatch refused a creative with no call to action, which Google defaults: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.adGroupAds) != 1 {
		t.Fatalf("got %d adGroupAds:mutate calls, want 1", len(cap.adGroupAds))
	}
	if strings.Contains(string(cap.adGroupAds[0]), `"callToActions"`) {
		t.Errorf("ad body sends an empty callToActions list; absent means 'use Google's default', empty means 'no text': %s", cap.adGroupAds[0])
	}
}

// The recorded channel type is what the settings readback compares against
// campaign.advertising_channel_type. A snapshot that recorded "video" as SEARCH would
// report drift on every correctly-created Video campaign.
func TestGoogleAdsRecordedChannelType_Video(t *testing.T) {
	got := googleAdsRecordedChannelType(context.Background(), &model.Campaign{
		ConfigSnapshot: json.RawMessage(`{"channel":"video"}`),
	})
	if got == nil {
		t.Fatal("no recorded channel type for a snapshot that names the video channel")
	}
	if *got != googleAdsChannelTypeVideo {
		t.Errorf("recorded channel type = %q, want %q", *got, googleAdsChannelTypeVideo)
	}
}
