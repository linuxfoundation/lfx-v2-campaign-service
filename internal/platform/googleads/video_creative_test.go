// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// validateVideoCreative — the PURE preflight
// ---------------------------------------------------------------------------

// TestValidateVideoCreative_AbsentIsNotAnError: a caller who supplies no creative gets
// a campaign and an ad group and no ad, which is the documented shape. Treating the
// absence as an error would refuse a create upstream would have accepted.
func TestValidateVideoCreative_AbsentIsNotAnError(t *testing.T) {
	for _, kind := range []string{campaignKindVideo, campaignKindSearch, campaignKindDemandGen, campaignKindPerformanceMax} {
		plan, err := validateVideoCreative(kind, CampaignInput{})
		if err != nil {
			t.Errorf("%s: unexpected error: %v", kind, err)
		}
		if plan.present {
			t.Errorf("%s: plan.present = true for an absent creative", kind)
		}
	}
}

// TestValidateVideoCreative_RefusedOffVideo is the refuse-don't-drop doctrine: a
// VideoResponsiveAdInfo is not a shape a Search ad group, a Demand Gen ad group or a
// Performance Max asset group accepts, so a caller who sends one to those channels is
// told, not quietly given a campaign without the creative they asked for.
func TestValidateVideoCreative_RefusedOffVideo(t *testing.T) {
	in := CampaignInput{VideoCreative: videoCreativeFixture()}
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen, campaignKindPerformanceMax} {
		_, err := validateVideoCreative(kind, in)
		if err == nil {
			t.Errorf("%s: a Video creative must be REFUSED, not dropped", kind)
			continue
		}
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("%s: error must name the channel that refused it: %v", kind, err)
		}
	}
}

func TestValidateVideoCreative_Bounds(t *testing.T) {
	// Each case mutates the valid fixture in exactly one way, so a failure names the
	// bound that moved rather than "the fixture is invalid".
	fifteen := strings.Repeat("a", maxVideoHeadlineWeight)
	cases := []struct {
		name    string
		mutate  func(*VideoCreative)
		wantErr bool
	}{
		{"the fixture itself is valid", func(*VideoCreative) {}, false},
		{"headline at the limit", func(v *VideoCreative) { v.Headlines = []string{fifteen} }, false},
		{"headline one over", func(v *VideoCreative) { v.Headlines = []string{fifteen + "a"} }, true},
		{"long headline at the limit", func(v *VideoCreative) {
			v.LongHeadlines = []string{strings.Repeat("a", maxVideoLongHeadlineWeight)}
		}, false},
		{"long headline one over", func(v *VideoCreative) {
			v.LongHeadlines = []string{strings.Repeat("a", maxVideoLongHeadlineWeight+1)}
		}, true},
		{"description at the limit", func(v *VideoCreative) {
			v.Descriptions = []string{strings.Repeat("a", maxVideoDescriptionWeight)}
		}, false},
		{"description one over", func(v *VideoCreative) {
			v.Descriptions = []string{strings.Repeat("a", maxVideoDescriptionWeight+1)}
		}, true},
		{"call to action at the limit", func(v *VideoCreative) {
			v.CallToActions = []string{strings.Repeat("a", maxVideoCallToActionWeight)}
		}, false},
		{"call to action one over", func(v *VideoCreative) {
			v.CallToActions = []string{strings.Repeat("a", maxVideoCallToActionWeight+1)}
		}, true},
		// A responsive video ad with no short headline, no long headline or no
		// description does not assemble into any format Google serves.
		{"no headlines", func(v *VideoCreative) { v.Headlines = nil }, true},
		{"no long headlines", func(v *VideoCreative) { v.LongHeadlines = nil }, true},
		{"no descriptions", func(v *VideoCreative) { v.Descriptions = nil }, true},
		// Calls to action are the one optional list — Google picks a default button.
		{"no calls to action", func(v *VideoCreative) { v.CallToActions = nil }, false},
		{"too many headlines", func(v *VideoCreative) {
			v.Headlines = repeatText("hi", maxVideoHeadlines+1)
		}, true},
		{"too many descriptions", func(v *VideoCreative) {
			v.Descriptions = repeatText("a description", maxVideoDescriptions+1)
		}, true},
		{"too many calls to action", func(v *VideoCreative) {
			v.CallToActions = repeatText("Register", maxVideoCallToActions+1)
		}, true},
		{"videos at the limit", func(v *VideoCreative) {
			v.YouTubeVideoIDs = videoIDs(maxVideoAdVideos)
		}, false},
		{"too many videos", func(v *VideoCreative) {
			v.YouTubeVideoIDs = videoIDs(maxVideoAdVideos + 1)
		}, true},
		// A share URL is refused rather than parsed: guessing which substring of a link
		// is the id would send a wrong id to an assets:mutate that runs after the
		// campaign exists.
		{"a URL instead of a bare id", func(v *VideoCreative) {
			v.YouTubeVideoIDs = []string{"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}
		}, true},
		// Text supplied with no video: the creative is PRESENT, so the missing video is
		// a refusal rather than the absent-creative path.
		{"text but no video", func(v *VideoCreative) { v.YouTubeVideoIDs = nil }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creative := videoCreativeFixture()
			tc.mutate(&creative)
			_, err := validateVideoCreative(campaignKindVideo, CampaignInput{VideoCreative: creative})
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got none — accepting here means failing AFTER the budget, campaign and ad group have committed")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v — over-refusal is the one failure mode this preflight may not have", err)
			}
		})
	}
}

// videoIDs builds n distinct WELL-FORMED YouTube ids. repeatText's marker character
// is not a YouTube id character, so using it here would make the over-the-count case
// pass on the character check instead of the count it is testing.
func videoIDs(n int) []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "dQw4w9WgXc"+string(alphabet[i%len(alphabet)]))
	}
	return out
}

// repeatText builds n distinct entries, because a validator that deduplicates would
// otherwise make an over-the-count case pass for the wrong reason.
func repeatText(base string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, base+strings.Repeat("!", i))
	}
	return out
}

// TestValidateVideoCreative_PlanCarriesEveryList guards the plumbing between the
// validator and the payload: a list validated and then not carried would be silently
// dropped from the ad.
func TestValidateVideoCreative_PlanCarriesEveryList(t *testing.T) {
	creative := videoCreativeFixture()
	creative.CallToActions = []string{"Register"}
	plan, err := validateVideoCreative(campaignKindVideo, CampaignInput{VideoCreative: creative})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.present {
		t.Fatal("plan.present = false for a supplied creative")
	}
	if len(plan.videos) != 1 || len(plan.headlines) != 1 || len(plan.longHeadlines) != 1 ||
		len(plan.descriptions) != 1 || len(plan.callToActions) != 1 {
		t.Errorf("plan dropped a list: %+v", plan)
	}
}

// ---------------------------------------------------------------------------
// createVideoAd — the two mutates
// ---------------------------------------------------------------------------

func videoAdPlan(t *testing.T) videoCreativePlan {
	t.Helper()
	plan, err := validateVideoCreative(campaignKindVideo, CampaignInput{VideoCreative: videoCreativeFixture()})
	if err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	return plan
}

// createVideoAdClient serves only the two creative mutates; anything else is a test
// failure, because createVideoAd is reached with the campaign and ad group already
// created and must send nothing else.
func createVideoAdClient(t *testing.T, assetH, adH http.HandlerFunc) *Client {
	t.Helper()
	srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			assetH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			adH(w, r)
		default:
			t.Errorf("createVideoAd sent an unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return demandGenClient(t, srv)
}

// Every arm below asserts the same two things, and they are the point of the test: the
// error says UNCONFIRMED rather than "failed" — the assets may exist, and an operator
// who reads "failed" retries into a second set of account-level video assets — and any
// asset ids that ARE known come back WITH the error rather than being dropped beside
// it, because they are the only handle on that litter.
func TestCreateVideoAd_UnconfirmedArms(t *testing.T) {
	respond := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	okAssets := respond(`{"results":[{"resourceName":"customers/1234567890/assets/900"}]}`)

	cases := []struct {
		name         string
		assetH, adH  http.HandlerFunc
		wantAssetIDs int
	}{
		{
			name:   "a 5xx on the asset mutate",
			assetH: serverError,
			adH:    failHandler(t, "adGroupAds:mutate"),
		},
		{
			name:   "a 2xx whose body is not a mutate response",
			assetH: respond(`not json`),
			adH:    failHandler(t, "adGroupAds:mutate"),
		},
		{
			// A short response leaves videos unaccounted for. The ids it DID carry are
			// real, so they come back.
			name:         "fewer results than videos sent",
			assetH:       respond(`{"results":[]}`),
			adH:          failHandler(t, "adGroupAds:mutate"),
			wantAssetIDs: 0,
		},
		{
			// assetID checks kind, account AND the numeric id. Without it a wrong-account
			// resource would be referenced by the ad and persisted as this campaign's.
			name:   "an asset resource name for another account",
			assetH: respond(`{"results":[{"resourceName":"customers/9999999999/assets/900"}]}`),
			adH:    failHandler(t, "adGroupAds:mutate"),
		},
		{
			name:         "a 5xx on the ad mutate",
			assetH:       okAssets,
			adH:          serverError,
			wantAssetIDs: 1,
		},
		{
			name:         "an ad resource name that is not an adGroupAd",
			assetH:       okAssets,
			adH:          respond(`{"results":[{"resourceName":"customers/1234567890/ads/444"}]}`),
			wantAssetIDs: 1,
		},
		{
			// The resource name must describe the ad group this ad was created under; a
			// mismatch means the response is not about this call, so the ad id is not
			// trustworthy enough to persist and later toggle.
			name:         "an ad reported under a different ad group",
			assetH:       okAssets,
			adH:          respond(`{"results":[{"resourceName":"customers/1234567890/adGroupAds/999~444"}]}`),
			wantAssetIDs: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := createVideoAdClient(t, tc.assetH, tc.adH)
			assetIDs, adID, err := c.createVideoAd(context.Background(),
				"customers/1234567890/adGroups/333", "333", "https://example.org/register", videoAdPlan(t))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Errorf("error must be UNCONFIRMED, not a flat failure — a retry would duplicate: %v", err)
			}
			if adID != "" {
				t.Errorf("adID = %q, want empty on an unconfirmed outcome", adID)
			}
			if len(assetIDs) != tc.wantAssetIDs {
				t.Errorf("assetIDs = %v, want %d: known ids are the only handle on account-level litter", assetIDs, tc.wantAssetIDs)
			}
		})
	}
}

// TestCreateVideoAd_DefiniteAssetFailureIsNotUnconfirmed is the other side: a definite
// 4xx means nothing was created, so claiming it MIGHT have been would send an operator
// hunting assets that do not exist.
func TestCreateVideoAd_DefiniteAssetFailureIsNotUnconfirmed(t *testing.T) {
	c := createVideoAdClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"invalid video id"}}`))
	}, failHandler(t, "adGroupAds:mutate"))

	_, _, err := c.createVideoAd(context.Background(),
		"customers/1234567890/adGroups/333", "333", "https://example.org/register", videoAdPlan(t))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a definite 4xx created nothing and must not be reported as unknown: %v", err)
	}
}

// TestCreateVideoAd_AbsentPlanSendsNothing: the cascade guards on plan.present, and so
// does this function. Both, because a creative-less create must not reach the API at
// all — an assets:mutate with zero operations is a request Google rejects.
func TestCreateVideoAd_AbsentPlanSendsNothing(t *testing.T) {
	c := createVideoAdClient(t, failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate"))
	assetIDs, adID, err := c.createVideoAd(context.Background(),
		"customers/1234567890/adGroups/333", "333", "https://example.org/register", videoCreativePlan{})
	if err != nil || adID != "" || assetIDs != nil {
		t.Errorf("got (%v, %q, %v), want no call and no result", assetIDs, adID, err)
	}
}

// TestVideoAdGroupName_IsChannelSuffixed: two channels' ad groups under one brief are
// told apart by name by whoever reconciles them, so the suffix is load-bearing.
func TestVideoAdGroupName_IsChannelSuffixed(t *testing.T) {
	got := videoAdGroupName(CampaignInput{EventName: "  KubeCon Europe 2026  "})
	if got != "KubeCon Europe 2026 - Video" {
		t.Errorf("videoAdGroupName = %q", got)
	}
}
