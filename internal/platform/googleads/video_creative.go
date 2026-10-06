// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The Video channel's creative is a RESPONSIVE VIDEO AD: one or more YouTube videos
// plus four independent text lists, from which Google assembles the in-stream,
// in-feed and Shorts formats a VIDEO_ACTION campaign serves.
//
// It is the cheapest creative of the four channels to validate, because the only
// binary asset is a video this client never holds: a YouTube video is referenced BY
// ID and uploaded to YouTube out of band, so there is no fetch phase here at all.
// Demand Gen and Performance Max both download image bytes before the budget mutate
// (see fetchSlotImages) precisely because an unreachable image would otherwise fail
// after a paid campaign exists; a video id has no equivalent — whether the video
// exists is Google's to judge, and it judges it at the assets:mutate, which this
// cascade runs AFTER the campaign. That is a documented asymmetry, not an oversight:
// this client has no YouTube credentials, and inventing a lookup would make
// ValidateCampaignInput send a request, which its contract forbids.
const (
	// minVideoAdVideos/maxVideoAdVideos bound the YouTube videos on one responsive
	// video ad. At least one is Google's own requirement — a video ad with no video
	// is not a thing the API will create — and five is the ceiling the ad accepts.
	minVideoAdVideos = 1
	maxVideoAdVideos = 5

	// The four text limits below are the VIDEO responsive ad's own, and they are
	// deliberately NOT the RSA/Demand Gen constants next door.
	//
	// maxHeadlineWeight is 30 and maxDescriptionWeight is 90 in ad_copy.go, and both
	// are wrong here: a video ad's short headline is 15 and its description line is
	// 70. Reusing the neighbours' numbers would accept copy at preflight that Google
	// then refuses at the adGroupAds:mutate — which on this cascade lands after the
	// budget, the campaign and the ad group have all committed. The long headline is
	// the one that happens to coincide with maxDescriptionWeight at 90; it is spelled
	// out rather than aliased, because the coincidence is not a relationship.
	//
	// Stated as DISPLAY WIDTH, measured by textWeight, for the reason that helper
	// gives: Google states every one of these as "maximum display width", so
	// double-width characters count twice here exactly as they do on the other
	// channels.
	maxVideoHeadlineWeight     = 15
	maxVideoLongHeadlineWeight = 90
	maxVideoDescriptionWeight  = 70
	maxVideoCallToActionWeight = 10

	// Count bounds. Headlines, long headlines and descriptions each have a minimum
	// because a responsive video ad with none of one of them does not assemble into
	// any format; calls to action are wholly optional and Google picks a default
	// button when the list is empty.
	minVideoHeadlines     = 1
	maxVideoHeadlines     = 5
	minVideoLongHeadlines = 1
	maxVideoLongHeadlines = 5
	minVideoDescriptions  = 1
	maxVideoDescriptions  = 5
	maxVideoCallToActions = 5
)

// VideoCreative is the responsive video ad a Video campaign serves.
//
// COMPANION BANNERS ARE NOT OFFERED. VideoResponsiveAdInfo has a
// companion_banners field, and omitting it is a deliberate scope boundary rather
// than a gap this type hides: a companion banner is an IMAGE asset, so supporting
// it would pull the whole slot/fetch/dimension apparatus demandgen_creative.go
// carries into a channel that otherwise needs none of it. There is no field to
// supply one, so nothing is silently dropped — the refuse-don't-drop doctrine is
// satisfied by the absence itself, and an operator who wants a companion banner
// adds it to the created ad in the Google Ads UI.
type VideoCreative struct {
	// YouTubeVideoIDs are the BARE video ids ("dQw4w9WgXcQ"), not URLs. A URL is
	// refused rather than parsed — see validateYouTubeVideoIDs for why guessing
	// which substring of a share link is the id is the wrong trade.
	YouTubeVideoIDs []string
	// Headlines are the SHORT headlines, display width 15. LongHeadlines are the
	// separate 90-width field, not a longer entry in the same list: Google treats
	// them as different assets and formats that show one do not show the other.
	Headlines     []string
	LongHeadlines []string
	Descriptions  []string
	// CallToActions are the button labels ("Register", "Learn more"). Optional.
	CallToActions []string
}

// empty reports whether the caller supplied no creative at all, which leaves the
// cascade's "campaign and ad group, no ad" behaviour in place.
func (v VideoCreative) empty() bool {
	return len(v.YouTubeVideoIDs) == 0 && len(v.Headlines) == 0 && len(v.LongHeadlines) == 0 &&
		len(v.Descriptions) == 0 && len(v.CallToActions) == 0
}

// videoCreativePlan is the validated creative, resolved in the PURE preflight.
type videoCreativePlan struct {
	videos        []string
	headlines     []string
	longHeadlines []string
	descriptions  []string
	callToActions []string
	// present is false when the caller asked for no creative.
	present bool
}

// validateVideoCreative resolves the video creative WITHOUT sending anything, and is
// called from preflightCampaignKind with every other validator so the adoption path
// refuses exactly what the create path refuses.
func validateVideoCreative(kind string, in CampaignInput) (videoCreativePlan, error) {
	v := in.VideoCreative
	if v.empty() {
		return videoCreativePlan{}, nil
	}
	// Refused as a channel capability, exactly as its three siblings refuse off their
	// own channels: VideoResponsiveAdInfo is not a shape a Search ad group, a Demand
	// Gen ad group or a Performance Max asset group accepts.
	if kind != campaignKindVideo {
		return videoCreativePlan{}, fmt.Errorf("google-ads Video creative (YouTube videos, headlines, descriptions) is supported on Video campaigns only, not on %s", kind)
	}

	plan := videoCreativePlan{present: true}

	videos, err := validateYouTubeVideoIDs("Video", "Video responsive ad", maxVideoAdVideos, v.YouTubeVideoIDs)
	if err != nil {
		return videoCreativePlan{}, err
	}
	if len(videos) < minVideoAdVideos {
		return videoCreativePlan{}, fmt.Errorf("google-ads Video responsive ad needs at least %d YouTube video, got %d", minVideoAdVideos, len(videos))
	}
	plan.videos = videos

	headlines, err := validateCreativeText("Video responsive ad", "headline", v.Headlines, minVideoHeadlines, maxVideoHeadlines, maxVideoHeadlineWeight)
	if err != nil {
		return videoCreativePlan{}, err
	}
	plan.headlines = headlines

	longHeadlines, err := validateCreativeText("Video responsive ad", "long headline", v.LongHeadlines, minVideoLongHeadlines, maxVideoLongHeadlines, maxVideoLongHeadlineWeight)
	if err != nil {
		return videoCreativePlan{}, err
	}
	plan.longHeadlines = longHeadlines

	descriptions, err := validateCreativeText("Video responsive ad", "description", v.Descriptions, minVideoDescriptions, maxVideoDescriptions, maxVideoDescriptionWeight)
	if err != nil {
		return videoCreativePlan{}, err
	}
	plan.descriptions = descriptions

	// Zero minimum: the list is optional, and validateCreativeText's min check is
	// satisfied by 0 when none were supplied.
	callToActions, err := validateCreativeText("Video responsive ad", "call to action", v.CallToActions, 0, maxVideoCallToActions, maxVideoCallToActionWeight)
	if err != nil {
		return videoCreativePlan{}, err
	}
	plan.callToActions = callToActions

	return plan, nil
}

// ---------------------------------------------------------------------------
// Creating the video assets and the ad
// ---------------------------------------------------------------------------

// videoAssetCreate is an Asset create carrying a YouTube video and nothing else.
//
// Kept separate from performanceMaxAssetCreate even though the video arm is
// identical, for the reason every other channel's asset struct states: that type is
// a three-way oneof whose text and image arms mean nothing here, and one struct
// spanning two channels makes every reader work out which half applies. Name is
// never set, as on the other channels.
type videoAssetCreate struct {
	YouTubeVideoAsset youTubeVideoAssetCreate `json:"youtubeVideoAsset"`
}

// adVideoAsset is AdVideoAsset: a reference to an already-created YouTube video
// asset by its resource name. The sibling of adImageAsset, and separate for the same
// reason — the field it lands in accepts only one of the two.
type adVideoAsset struct {
	Asset string `json:"asset"`
}

// videoResponsiveAdInfo mirrors VideoResponsiveAdInfo. The four text arrays and the
// video array are all required-in-practice and guaranteed non-empty by
// validateVideoCreative, except CallToActions, which is omitempty because Google
// distinguishes an absent optional array from an empty one and supplies its own
// default button when the field is absent.
//
// companionBanners, breadcrumb1 and breadcrumb2 are deliberately absent — see
// VideoCreative for the companion-banner boundary; the breadcrumbs are display-path
// decoration this client has no input for.
type videoResponsiveAdInfo struct {
	Headlines     []adTextAsset  `json:"headlines"`
	LongHeadlines []adTextAsset  `json:"longHeadlines"`
	Descriptions  []adTextAsset  `json:"descriptions"`
	CallToActions []adTextAsset  `json:"callToActions,omitempty"`
	Videos        []adVideoAsset `json:"videos"`
}

// videoAdCreate is the "ad" object nested in an adGroupAd create, the Video sibling
// of adCreate and demandGenAdCreate.
type videoAdCreate struct {
	FinalUrls         []string               `json:"finalUrls"`
	VideoResponsiveAd *videoResponsiveAdInfo `json:"videoResponsiveAd"`
}

// videoAdGroupAdCreate is the AdGroupAd wrapper.
type videoAdGroupAdCreate struct {
	AdGroup string        `json:"adGroup"`
	Status  string        `json:"status"`
	Ad      videoAdCreate `json:"ad"`
}

// createVideoAd creates the YouTube video assets and then the ad that references
// them.
//
// Two mutates, the same shape createDemandGenAd uses, and for the same reason:
// Google has no call that creates an asset and attaches it. The second mutate uses
// THE RESOURCE NAMES GOOGLE RETURNED, never names rebuilt from the parsed ids.
//
// The ad is created PAUSED, matching the campaign and every other channel; the
// existing status cascade flips it along with the campaign when the operator
// launches.
//
// Returns the created asset ids and the ad id. The asset ids come back even when the
// ad itself failed, because video assets created with nothing referencing them are
// account-level litter the operator can find and remove.
func (c *Client) createVideoAd(ctx context.Context, adGroupResource, adGroupID, finalURL string, plan videoCreativePlan) (assetIDs []string, adID string, err error) {
	if !plan.present {
		return nil, "", nil
	}

	assetOps := make([]mutateOperation, 0, len(plan.videos))
	for _, id := range plan.videos {
		assetOps = append(assetOps, mutateOperation{Create: videoAssetCreate{
			YouTubeVideoAsset: youTubeVideoAssetCreate{YouTubeVideoID: id},
		}})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		// A 5xx or a timeout on a mutating POST is an UNKNOWN outcome, not a failure:
		// the assets may well have been created, and an operator who read "failed"
		// would retry into a second set of account-level video assets.
		if createOutcomeAmbiguous(err) {
			return nil, "", fmt.Errorf("google-ads video asset creation UNCONFIRMED (%d video(s) may exist; ad group %s created — verify in Google Ads before retrying): %w", len(assetOps), adGroupID, err)
		}
		return nil, "", fmt.Errorf("google-ads video asset creation failed (%d video(s); ad group %s created): %w", len(assetOps), adGroupID, err)
	}
	var assetResults mutateResponse
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil {
		return nil, "", fmt.Errorf("google-ads video asset creation UNCONFIRMED (ad group %s created; 2xx with a malformed mutate response for %d video(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetOps))
	}
	// EXACT equality, like every other create-path mutate in this package: a short
	// response leaves videos unaccounted for, and an extra result is a response that
	// does not describe what was sent.
	if len(assetResults.Results) != len(assetOps) {
		// The body PARSED, so whatever ids it carried are real — and they are the only
		// handle an operator has on assets that may already exist, so they go back WITH
		// the error rather than being dropped alongside it.
		return c.parsedAssetIDs(assetResults), "", fmt.Errorf("google-ads video asset creation UNCONFIRMED (ad group %s created; 2xx returned %d result(s) for %d video(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetResults.Results), len(assetOps))
	}

	videos := make([]adVideoAsset, 0, len(assetOps))
	assetIDs = make([]string, 0, len(assetOps))
	for i, r := range assetResults.Results {
		resource := r.ResourceName
		// assetID checks the kind, the account AND the numeric trailing id, returning
		// "" for anything else. Without it a wrong-account assets resource would be
		// referenced by the ad and persisted as this campaign's.
		id := c.assetID(resource)
		if id == "" {
			return assetIDs, "", fmt.Errorf("google-ads video asset creation UNCONFIRMED (ad group %s created; malformed asset resource name %q at index %d — assets may exist — verify in Google Ads before retrying)", adGroupID, resource, i)
		}
		assetIDs = append(assetIDs, id)
		videos = append(videos, adVideoAsset{Asset: resource})
	}

	ad := &videoResponsiveAdInfo{
		Headlines:     textAssets(plan.headlines),
		LongHeadlines: textAssets(plan.longHeadlines),
		Descriptions:  textAssets(plan.descriptions),
		CallToActions: textAssets(plan.callToActions),
		Videos:        videos,
	}
	adReq := mutateRequest{Operations: []mutateOperation{{Create: videoAdGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  "PAUSED",
		Ad:      videoAdCreate{FinalUrls: []string{finalURL}, VideoResponsiveAd: ad},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return assetIDs, "", fmt.Errorf("google-ads video ad creation UNCONFIRMED (ad group %s and %d video asset(s) created; the ad may exist — verify in Google Ads before retrying): %w", adGroupID, len(assetIDs), err)
		}
		return assetIDs, "", fmt.Errorf("google-ads video ad creation failed (ad group %s and %d video asset(s) created): %w", adGroupID, len(assetIDs), err)
	}
	var adResults mutateResponse
	if uErr := json.Unmarshal(adResp, &adResults); uErr != nil || len(adResults.Results) != 1 {
		return assetIDs, "", fmt.Errorf("google-ads video ad creation UNCONFIRMED (ad group %s created; 2xx with a malformed/short mutate response — the ad may exist — verify in Google Ads before retrying)", adGroupID)
	}
	adResource := adResults.Results[0].ResourceName
	// requireNumericID=false: the trailing segment is the composite
	// "{adGroupId}~{adId}", split and checked by adGroupAdID just below.
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return assetIDs, "", fmt.Errorf("google-ads video ad creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID == "" {
		return assetIDs, "", fmt.Errorf("google-ads video ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	// The resource name must describe the ad group this ad was created under. A
	// mismatch means the response is not about this call, so the ad id is not
	// trustworthy enough to persist and later toggle.
	if returnedAdGroupID != adGroupID {
		return assetIDs, "", fmt.Errorf("google-ads video ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
	}
	return assetIDs, adID, nil
}

// videoCreativeStep summarises what the ad was built from, for the operator-facing
// step list.
func videoCreativeStep(plan videoCreativePlan) string {
	return fmt.Sprintf("%d video(s), %d headline(s), %d long headline(s), %d description(s)",
		len(plan.videos), len(plan.headlines), len(plan.longHeadlines), len(plan.descriptions))
}

// videoAdGroupName composes the ad group's name, the Demand Gen sibling's shape with
// this channel's suffix. Suffixed rather than bare so two channels' ad groups under
// one brief are told apart by anyone reconciling them by name.
func videoAdGroupName(in CampaignInput) string {
	return strings.TrimSpace(in.EventName) + " - Video"
}
