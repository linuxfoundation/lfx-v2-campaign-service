// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The dispatch-layer half of the Video (YouTube) path, for the reason
// googleads_pmax_wiring_test.go states for Performance Max: video_test.go proves
// CreateVideoCampaign builds the right payload FROM a CampaignInput and
// video_creative_test.go proves every bound, but neither can prove that the wire config
// is mapped onto that input at all. A dropped `channel` or a `videoCreative` the mapper
// never reads leaves both suites green while the feature is unreachable through the API.
//
// These tests originally exercised the WHOLE cascade from here. They no longer can: the
// Google Ads API cannot create a Video campaign, so dispatch refuses the channel before
// anything is sent (see googleads.CreateVideoCampaign). What survives is the half that is
// still reachable and still matters — the mapper, proved by WHICH refusal each wire shape
// produces — plus the assertion that the refusal lands before the budget mutate. The
// cascade's own payload proofs moved down to the googleads package, where they run against
// createVideoCampaignCascade directly and keep it correct for the day Google opens
// creation.
//
// Adoption and reporting are untouched. Google supports FETCHING Video campaigns; only
// creation is impossible, which is why the refusal sits in the create switch rather than
// in ValidateCampaignInputKind — the latter runs before the adoption branch and would
// refuse legitimate Video adoption too.

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

// channel: "video" must reach the kind switch and be REFUSED there — not accepted and
// quietly created as a Search campaign, and not run as a cascade that mutates a budget
// before Google rejects the campaign.
//
// This replaces the pair of tests that used to assert a VIDEO/VIDEO_ACTION campaign body
// and a videoResponsiveAd on the wire. They could not be kept: the Google Ads API cannot
// create a Video campaign at all (see googleads.CreateVideoCampaign), so there is no
// campaign body to assert. What is asserted instead is the thing that actually costs
// money if it regresses — that NOTHING is sent.
func TestGoogleAds_VideoCreateIsRefusedBeforeAnyMutate(t *testing.T) {
	for name, creative := range map[string]map[string]any{
		"with a creative":    videoWireCreative(),
		"without a creative": nil,
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, creative))
			if err == nil {
				t.Fatal("Dispatch accepted a Video create; the Google Ads API cannot create Video campaigns")
			}
			if !errors.Is(err, googleads.ErrVideoCreateUnsupported) {
				t.Errorf("error = %v, want it to wrap ErrVideoCreateUnsupported rather than read as a malformed request", err)
			}
			// nil campaign is the pre-create contract: the orchestrator releases its claim
			// because nothing exists upstream to reconcile.
			if camp != nil {
				t.Errorf("nothing may be created for a refused channel, got %+v", camp)
			}

			cap.mu.Lock()
			defer cap.mu.Unlock()
			// The binding assertion. The budget is step 1 and campaigns:mutate is step 2, so
			// a refusal that arrived one step late would leave a real, billable
			// CampaignBudget behind on every attempt — and a retry composes the same budget
			// name and fails at DUPLICATE_NAME instead, so the orphan is never reconciled.
			if cap.sawBudget {
				t.Error("a budget was created for a channel that can never carry a campaign — the refusal came too late")
			}
			if len(cap.campaigns) != 0 || len(cap.adGroups) != 0 || len(cap.adGroupAds) != 0 || cap.assets != nil {
				t.Errorf("Google was mutated for a refused channel: %d campaigns, %d adGroups, %d ads, assets=%v",
					len(cap.campaigns), len(cap.adGroups), len(cap.adGroupAds), cap.assets != nil)
			}
		})
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

// callToActions is the one optional list, and optional must mean optional. It can no
// longer be shown by creating the ad, so it is shown by WHICH refusal comes back: an
// omitted callToActions must reach the channel refusal, where the four required lists
// above stop at their own validation refusals before it. Paired with the table above,
// that still pins which of the four lists is required — the distinction a reader of the
// config struct alone cannot check — and it keeps the validator honest while the create
// path is closed, so the day Google opens Video creation the optionality is still
// correct rather than rediscovered.
func TestGoogleAds_VideoCallToActionsAreOptional(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	creative := videoWireCreative()
	delete(creative, "callToActions")

	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, videoWireConfig(t, creative))
	if err == nil {
		t.Fatal("Dispatch accepted a Video create; the Google Ads API cannot create Video campaigns")
	}
	if !errors.Is(err, googleads.ErrVideoCreateUnsupported) {
		t.Fatalf("a creative with no call to action was refused as malformed rather than reaching the "+
			"channel refusal, so callToActions is being treated as required: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("a budget was created for a channel that can never carry a campaign")
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
