// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Demand Gen video responsive ad (#460)
// ---------------------------------------------------------------------------
//
// The second Demand Gen ad type this client builds, beside the image multi-asset ad in
// demandgen_creative.go: an Ad whose oneof arm is `demandGenVideoResponsiveAd`
// (DemandGenVideoResponsiveAdInfo). It is NOT the Video channel's videoResponsiveAd
// (video_creative.go): that one hangs off a VIDEO campaign, which the API cannot create.
//
// The shape is what a validateOnly probe on 2026-11-03 (v23) accepted:
//
//   - videos [{asset}] naming YOUTUBE_VIDEO assets; logoImages [{asset}] naming an IMAGE
//     asset (a 900x900 square logo was accepted); headlines, longHeadlines and
//     descriptions as [{text}]; businessName as an AdTextAsset OBJECT, not a string;
//     callToActions [{asset}] naming a CALL_TO_ACTION asset, optional.
//   - ad.name is REQUIRED for this ad type: without it the mutate is refused with
//     REQUIRED at ad_group_ad_operation.create.ad.name.
//   - the YouTube and call-to-action assets can be created in the SAME googleAds:mutate
//     as the ad, named by temporary (negative-id) resource names the ad references.
//
// The logo is an IMAGE and Google will not fetch a URL (see demandgen_creative.go), so
// its bytes are fetched before the budget mutate by the same fetchSlotImages, and
// uploaded by the same assets:mutate the image ad uses. That upload is a separate call
// BEFORE the atomic one: an image asset created by a temporary name inside the
// googleAds:mutate is not a shape the probe exercised, while an existing asset's resource
// name in logoImages is.

const (
	// minDemandGenVideoLogos/maxDemandGenVideoLogos: exactly one. The probe sent one logo,
	// and no source cited in this repository states how many this ad type accepts — so
	// the cap is conservative rather than the image ad's 1-5, which is that ad type's own
	// documented range. A second logo refused here costs the caller an edit; one refused
	// by Google lands after the campaign and ad group exist.
	minDemandGenVideoLogos = 1
	maxDemandGenVideoLogos = 1
	// maxDemandGenVideoHeadlineWeight is this ad type's own headline width: a validateOnly
	// probe on 2026-11-03 accepted a 40-character headline and refused 41
	// (stringLengthError TOO_LONG). Not maxHeadlineWeight (30), which the IMAGE ad keeps
	// because its own limit has not been probed.
	maxDemandGenVideoHeadlineWeight = 40
	// minDemandGenVideoLongHeadlines/maxDemandGenVideoLongHeadlines bound the long
	// headlines, which the image ad does not have. Required because the probe's accepted
	// shape carried them; five, borrowed from the responsive video ad (video_creative.go).
	// The display width is maxVideoLongHeadlineWeight (90), which the 2026-11-03 probe
	// accepted for long headlines and descriptions alike.
	minDemandGenVideoLongHeadlines = 1
	maxDemandGenVideoLongHeadlines = 5
	// demandGenVideoAdNameSuffix composes ad.name from the ad group name, so the name is
	// deterministic: the same brief yields the same ad name, and an operator reconciling an
	// UNCONFIRMED ad looks for exactly this string.
	demandGenVideoAdNameSuffix = " - Video ad"
	// maxAdNameRunes is Google's Ad.name limit: a validateOnly probe on 2026-11-03 accepted
	// 255 characters and refused 256 (TOO_LONG). The composed video ad name is checked
	// against it before the budget mutate (validateDemandGenVideoAdName).
	maxAdNameRunes = 255
)

// demandGenVideoCallToActions is the CallToActionType values DemandGenVideoCreative
// accepts: exactly the values a validateOnly googleAds:mutate probe on 2026-11-14 (#604,
// v23, account 8666746580) accepted on a Demand Gen video ad. The first five were also
// accepted on 2026-11-03. The list is deliberately the probed set rather than Google's whole
// enum: names that look plausible are not all values (REGISTER and GET_STARTED got 400
// "Invalid value" on 2026-11-03), so anything outside it is refused before the budget.
// Google also took the lower-case enum names ("sign_up"); this client takes only the
// canonical spelling and sends it unchanged. Widen it value by value, each with a probe.
var demandGenVideoCallToActions = []string{
	"SIGN_UP", "LEARN_MORE", "BOOK_NOW", "VISIT_SITE", "WATCH_NOW",
	"APPLY_NOW", "CONTACT_US", "DOWNLOAD", "GET_QUOTE", "SUBSCRIBE", "SHOP_NOW",
}

// demandGenVideoLogoSlot is the logo's slot: the image ad's logo geometry (square, at
// least 128x128 — Google states the same rule for this ad type's logo_images), under a
// label that names this creative so an error says which of the two logos was wrong.
var demandGenVideoLogoSlot = func() imageSlot {
	s := demandGenImageSlots[logoSlotIndex]
	s.label = "Demand Gen video logo image"
	return s
}()

// DemandGenVideoCreative is a Demand Gen VIDEO responsive ad: YouTube videos plus text
// and a logo. DEMAND GEN ONLY, and an alternative to DemandGenCreative rather than an
// addition to it — the pair describes one ad, image or video, so setting both is refused
// (see validateDemandGenVideoCreative); several ads go in DemandGenAds (#474).
//
// Breadcrumbs (display-URL path text) are not offered: there is no field, so nothing is
// silently dropped; an operator who wants them adds them in the Google Ads UI.
type DemandGenVideoCreative struct {
	// YouTubeVideoIDs are BARE video ids, 1-5; a URL is refused, exactly as on the
	// Video and Performance Max creatives (validateYouTubeVideoIDs).
	YouTubeVideoIDs []string
	// LogoImages is required: exactly one square image, as a URL or a resolved
	// stored-asset entry, like every other image slot (validateImageEntries).
	LogoImages []string
	// Headlines are 1-5 at width 40 and Descriptions 1-5 at width 90; LongHeadlines are
	// 1-5 at width 90.
	Headlines     []string
	LongHeadlines []string
	Descriptions  []string
	// BusinessName is required, display width 25, as on the image ad.
	BusinessName string
	// CallToAction is an optional CallToActionType (SIGN_UP, LEARN_MORE, …); see
	// demandGenVideoCallToActions for the accepted set. Empty sends no call to action.
	CallToAction string
}

func (v DemandGenVideoCreative) empty() bool {
	return len(v.YouTubeVideoIDs) == 0 && len(v.LogoImages) == 0 && len(v.Headlines) == 0 &&
		len(v.LongHeadlines) == 0 && len(v.Descriptions) == 0 &&
		strings.TrimSpace(v.BusinessName) == "" && strings.TrimSpace(v.CallToAction) == ""
}

// demandGenVideoPlan is the validated video creative.
type demandGenVideoPlan struct {
	videos        []string
	logos         []string
	headlines     []string
	longHeadlines []string
	descriptions  []string
	businessName  string
	callToAction  string
	// resolved is CampaignInput.ResolvedImages, carried for the fetch phase as the image
	// plan carries it.
	resolved map[string][]byte
	present  bool
}

// validateDemandGenVideoCreative resolves CampaignInput.DemandGenVideoCreative WITHOUT
// sending or fetching anything, from preflightCampaignKind like every sibling creative.
func validateDemandGenVideoCreative(kind string, in CampaignInput) (demandGenVideoPlan, error) {
	if in.DemandGenVideoCreative.empty() {
		return demandGenVideoPlan{}, nil
	}
	if kind != campaignKindDemandGen {
		return demandGenVideoPlan{}, fmt.Errorf("google-ads Demand Gen video creative (YouTube videos, logo, headlines) is supported on Demand Gen campaigns only, not on %s", kind)
	}
	if !in.DemandGenCreative.empty() {
		return demandGenVideoPlan{}, errDemandGenTwoCreatives("the campaign")
	}
	return validateDemandGenVideoSpec(in.DemandGenVideoCreative, in.ResolvedImages)
}

// errDemandGenTwoCreatives refuses an image and a video creative on one ad. One creative
// field pair describes ONE ad; several ads on one ad group are named explicitly (#474,
// DemandGenAds or an ad group's Ads), so each has a label and its own AdIDs entry.
func errDemandGenTwoCreatives(owner string) error {
	return fmt.Errorf("google-ads: %s sets both a Demand Gen image creative and a Demand Gen video creative; one creative pair describes one ad, so set one of them (list several ads in DemandGenAds, or an ad group's Ads, for both)", owner)
}

// validateDemandGenVideoSpec checks one video creative. The channel fence is the caller's.
func validateDemandGenVideoSpec(v DemandGenVideoCreative, resolved map[string][]byte) (demandGenVideoPlan, error) {
	const owner = "Demand Gen video ad"
	plan := demandGenVideoPlan{present: true, resolved: resolved}
	videos, err := validateYouTubeVideoIDs("Demand Gen video", owner, maxVideoAdVideos, v.YouTubeVideoIDs)
	if err != nil {
		return demandGenVideoPlan{}, err
	}
	if len(videos) < minVideoAdVideos {
		return demandGenVideoPlan{}, fmt.Errorf("google-ads %s needs at least %d YouTube video, got %d", owner, minVideoAdVideos, len(videos))
	}
	plan.videos = videos

	logos, err := validateImageEntries(demandGenVideoLogoSlot, v.LogoImages, resolved)
	if err != nil {
		return demandGenVideoPlan{}, err
	}
	if n := len(logos); n < minDemandGenVideoLogos || n > maxDemandGenVideoLogos {
		return demandGenVideoPlan{}, fmt.Errorf("google-ads %s needs exactly %d logo image, got %d", owner, maxDemandGenVideoLogos, n)
	}
	plan.logos = logos

	if plan.headlines, err = validateCreativeText(owner, "headline", v.Headlines, minDemandGenHeadlines, maxDemandGenHeadlines, maxDemandGenVideoHeadlineWeight); err != nil {
		return demandGenVideoPlan{}, err
	}
	if plan.longHeadlines, err = validateCreativeText(owner, "long headline", v.LongHeadlines, minDemandGenVideoLongHeadlines, maxDemandGenVideoLongHeadlines, maxVideoLongHeadlineWeight); err != nil {
		return demandGenVideoPlan{}, err
	}
	if plan.descriptions, err = validateCreativeText(owner, "description", v.Descriptions, minDemandGenDescriptions, maxDemandGenDescriptions, maxDescriptionWeight); err != nil {
		return demandGenVideoPlan{}, err
	}

	name := strings.TrimSpace(v.BusinessName)
	if name == "" {
		return demandGenVideoPlan{}, errors.New("google-ads Demand Gen video ad requires a business name (Google marks the field required)")
	}
	if w := textWeight(name); w > maxDemandGenBusinessNameWeight {
		return demandGenVideoPlan{}, fmt.Errorf("google-ads Demand Gen video ad business name %q has a display width of %d, exceeding the %d limit", capForError(name), w, maxDemandGenBusinessNameWeight)
	}
	plan.businessName = name

	if cta := strings.TrimSpace(v.CallToAction); cta != "" {
		ok := false
		for _, allowed := range demandGenVideoCallToActions {
			ok = ok || cta == allowed
		}
		if !ok {
			return demandGenVideoPlan{}, fmt.Errorf("google-ads Demand Gen video ad call to action %q is not one this client accepts (%s); omit it to send none", capForError(cta), strings.Join(demandGenVideoCallToActions, ", "))
		}
		plan.callToAction = cta
	}
	return plan, nil
}

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// callToActionAssetCreate is CallToActionAsset.
type callToActionAssetCreate struct {
	CallToAction string `json:"callToAction"`
}

// demandGenVideoAssetCreate is an Asset created INSIDE the googleAds:mutate, under a
// temporary resource name the ad references. Exactly one arm is set. No name is sent:
// the 2026-11-05 probes showed Google refusing an unnamed IMAGE asset (see imageAssetName),
// and the logo, the one image here, is created separately, by assets:mutate.
type demandGenVideoAssetCreate struct {
	ResourceName      string                   `json:"resourceName"`
	YouTubeVideoAsset *youTubeVideoAssetCreate `json:"youtubeVideoAsset,omitempty"`
	CallToActionAsset *callToActionAssetCreate `json:"callToActionAsset,omitempty"`
}

// adCallToActionAsset is AdCallToActionAsset: a reference to a CALL_TO_ACTION asset.
type adCallToActionAsset struct {
	Asset string `json:"asset"`
}

// demandGenVideoResponsiveAdInfo mirrors DemandGenVideoResponsiveAdInfo in the shape the
// probe accepted. CallToActions is omitempty because the call to action is optional.
type demandGenVideoResponsiveAdInfo struct {
	Headlines     []adTextAsset         `json:"headlines"`
	LongHeadlines []adTextAsset         `json:"longHeadlines"`
	Descriptions  []adTextAsset         `json:"descriptions"`
	Videos        []adVideoAsset        `json:"videos"`
	LogoImages    []adImageAsset        `json:"logoImages"`
	BusinessName  adTextAsset           `json:"businessName"`
	CallToActions []adCallToActionAsset `json:"callToActions,omitempty"`
}

// demandGenVideoAdCreate is the "ad" object. Name is required for this ad type (see the
// file comment); the oneof key is pinned by TestAdTypeWireKeysMatchTheAdOneof.
type demandGenVideoAdCreate struct {
	Name                       string                          `json:"name"`
	FinalUrls                  []string                        `json:"finalUrls"`
	DemandGenVideoResponsiveAd *demandGenVideoResponsiveAdInfo `json:"demandGenVideoResponsiveAd"`
}

type demandGenVideoAdGroupAdCreate struct {
	AdGroup string                 `json:"adGroup"`
	Status  string                 `json:"status"`
	Ad      demandGenVideoAdCreate `json:"ad"`
}

// demandGenVideoAdName composes ad.name from the ad group's name.
func demandGenVideoAdName(adGroupName string) string {
	return adGroupName + demandGenVideoAdNameSuffix
}

// validateDemandGenVideoAdName refuses a composed video ad name over Google's Ad.name
// limit before the budget mutate; Google refuses it only at the ad's mutate, after the
// campaign and ad group exist. name is the composed ad.name: demandGenVideoAdName's for
// the single unnamed ad, "<ad group name> - <label>" for a DemandGenAd (#474). One check
// and one message for both, so the two paths cannot drift apart.
func validateDemandGenVideoAdName(name string) error {
	if n := utf8.RuneCountInString(name); n > maxAdNameRunes {
		return fmt.Errorf("google-ads: Demand Gen video ad name %q is %d characters, over Google's %d; shorten EventName, the ad group's name or the ad's name", capForError(name), n, maxAdNameRunes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Creating the ad
// ---------------------------------------------------------------------------

// createDemandGenVideoAd uploads the logo, then creates the YouTube and call-to-action
// assets AND the ad in one atomic googleAds:mutate (partialFailure false): either every
// one of them exists or none does. The logo goes first, by assets:mutate, for the reason
// the file comment gives.
//
// The ad is created PAUSED, like every ad here. Returns the created asset ids — logo,
// then videos, then the call to action — and the ad id; the asset ids come back even when
// a later stage failed, because assets nothing references are litter an operator removes.
func (c *Client) createDemandGenVideoAd(ctx context.Context, adGroupResource, adGroupID, adName, finalURL string, plan demandGenVideoPlan, logos []fetchedImage) (assetIDs []string, adID string, err error) {
	if len(logos) == 0 {
		return nil, "", fmt.Errorf("google-ads: demand gen video creative is present but no logo was fetched for it; refusing to create an ad without one")
	}

	// Step A: the logo, by the same assets:mutate the image ad's images use.
	logoOps := make([]mutateOperation, 0, len(logos))
	for _, img := range logos {
		logoOps = append(logoOps, mutateOperation{Create: demandGenAssetCreate{Name: imageAssetName(img.data), ImageAsset: imageAssetCreate{Data: base64Image(img.data)}}})
	}
	logoResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: logoOps}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, "", fmt.Errorf("google-ads demand gen video logo asset creation UNCONFIRMED (%d image(s) may exist; ad group %s created — verify in Google Ads before retrying): %w", len(logoOps), adGroupID, err)
		}
		return nil, "", fmt.Errorf("google-ads demand gen video logo asset creation failed (%d image(s); ad group %s created): %w", len(logoOps), adGroupID, err)
	}
	var logoResults mutateResponse
	if uErr := json.Unmarshal(logoResp, &logoResults); uErr != nil {
		return nil, "", fmt.Errorf("google-ads demand gen video logo asset creation UNCONFIRMED (ad group %s created; 2xx with a malformed mutate response — assets may exist — verify in Google Ads before retrying)", adGroupID)
	}
	if len(logoResults.Results) != len(logoOps) {
		return c.parsedAssetIDs(logoResults), "", fmt.Errorf("google-ads demand gen video logo asset creation UNCONFIRMED (ad group %s created; 2xx returned %d result(s) for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(logoResults.Results), len(logoOps))
	}
	logoAssets := make([]adImageAsset, 0, len(logoOps))
	for i, r := range logoResults.Results {
		id := c.assetID(r.ResourceName)
		if id == "" {
			return assetIDs, "", fmt.Errorf("google-ads demand gen video logo asset creation UNCONFIRMED (ad group %s created; malformed asset resource name %q at index %d — assets may exist — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		assetIDs = append(assetIDs, id)
		// THE RESOURCE NAME GOOGLE RETURNED, never one rebuilt from the parsed id.
		logoAssets = append(logoAssets, adImageAsset{Asset: r.ResourceName})
	}

	// Step B: the YouTube and call-to-action assets and the ad, in ONE googleAds:mutate.
	// Temporary ids are negative and scoped to this one request; Google returns the real
	// resource names, index-aligned with the operations.
	tempAsset := func(n int) string {
		return "customers/" + c.account.CustomerID + "/assets/-" + strconv.Itoa(n)
	}
	ops := make([]googleAdsMutateOperation, 0, len(plan.videos)+2)
	videos := make([]adVideoAsset, 0, len(plan.videos))
	for i, id := range plan.videos {
		temp := tempAsset(i + 1)
		ops = append(ops, googleAdsMutateOperation{AssetOperation: &mutateOperation{Create: demandGenVideoAssetCreate{
			ResourceName: temp, YouTubeVideoAsset: &youTubeVideoAssetCreate{YouTubeVideoID: id},
		}}})
		videos = append(videos, adVideoAsset{Asset: temp})
	}
	var ctas []adCallToActionAsset
	if plan.callToAction != "" {
		temp := tempAsset(len(plan.videos) + 1)
		ops = append(ops, googleAdsMutateOperation{AssetOperation: &mutateOperation{Create: demandGenVideoAssetCreate{
			ResourceName: temp, CallToActionAsset: &callToActionAssetCreate{CallToAction: plan.callToAction},
		}}})
		ctas = []adCallToActionAsset{{Asset: temp}}
	}
	assetOpCount := len(ops)
	ops = append(ops, googleAdsMutateOperation{AdGroupAdOperation: &mutateOperation{Create: demandGenVideoAdGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  "PAUSED",
		Ad: demandGenVideoAdCreate{
			Name:      adName,
			FinalUrls: []string{finalURL},
			DemandGenVideoResponsiveAd: &demandGenVideoResponsiveAdInfo{
				Headlines:     textAssets(plan.headlines),
				LongHeadlines: textAssets(plan.longHeadlines),
				Descriptions:  textAssets(plan.descriptions),
				Videos:        videos,
				LogoImages:    logoAssets,
				BusinessName:  adTextAsset{Text: plan.businessName},
				CallToActions: ctas,
			},
		},
	}}})
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("googleAds:mutate"), googleAdsMutateRequest{MutateOperations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation UNCONFIRMED (ad group %s and %d logo asset(s) created; the ad and its %d video/call-to-action asset(s) may exist — verify in Google Ads before retrying): %w", adName, adGroupID, len(assetIDs), assetOpCount, err)
		}
		return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation failed (ad group %s and %d logo asset(s) created; the ad and its %d video/call-to-action asset(s) are created in one atomic request, so none of them exists): %w", adName, adGroupID, len(assetIDs), assetOpCount, err)
	}
	var results googleAdsMutateResponse
	if uErr := json.Unmarshal(resp, &results); uErr != nil {
		return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation UNCONFIRMED "+
			"(ad group %s and %d logo asset(s) created; 2xx with a malformed mutate response — "+
			"the ad and its assets may exist — verify in Google Ads before retrying)",
			adName, adGroupID, len(assetIDs))
	}
	if len(results.MutateOperationResponses) != len(ops) {
		// The body PARSED, so every asset id it did carry that passes assetID's account and
		// shape checks is a real handle on an asset that may exist; it goes back WITH the
		// error, as the logo upload's short arm returns parsedAssetIDs. The logo count is
		// taken first so the message still counts only the logo assets.
		logoCount := len(assetIDs)
		assetIDs = uniqueIDs(append(assetIDs, c.parsedMutateAssetIDs(results)...))
		return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation UNCONFIRMED "+
			"(ad group %s and %d logo asset(s) created; "+
			"2xx returned %d result(s) for %d operation(s) — "+
			"the ad and its assets may exist — verify in Google Ads before retrying)",
			adName, adGroupID, logoCount, len(results.MutateOperationResponses), len(ops))
	}
	// An unusable asset result does not stop the walk (#481 review): a foreign-account video
	// result followed by this account's call to action must still record the call to action,
	// so every same-account id goes back, once, with the error naming the FIRST bad index.
	firstBad, badWhy := -1, ""
	seen := newResultIDSet(assetOpCount)
	for i, r := range results.MutateOperationResponses[:assetOpCount] {
		id := ""
		if r.AssetResult != nil {
			id = c.assetID(r.AssetResult.ResourceName)
		}
		if id == "" {
			if firstBad < 0 {
				firstBad = i
				badWhy = fmt.Sprintf("no usable asset resource name at index %d", i)
			}
			continue
		}
		// A repeat of the SAME video or call to action is Google returning the existing asset
		// (result_ids.go): the ad's references to both temporary names resolved to it, and
		// its id is reported once. Any other repeat is unusable.
		first, dup := seen.reuse(id, i, func(j int) bool {
			return sameDemandGenVideoAsset(ops[j].AssetOperation, ops[i].AssetOperation)
		})
		if dup != "" {
			if firstBad < 0 {
				firstBad, badWhy = i, dup
			}
			continue
		}
		if first {
			assetIDs = append(assetIDs, id)
		}
	}
	if firstBad >= 0 {
		return uniqueIDs(assetIDs), "", fmt.Errorf(
			"google-ads demand gen video ad %q creation UNCONFIRMED (ad group %s "+
				"created; %s — the ad and its assets may exist — verify in Google Ads "+
				"before retrying)",
			adName, adGroupID, badWhy)
	}
	adResult := results.MutateOperationResponses[assetOpCount].AdGroupAdResult
	adResource := ""
	if adResult != nil {
		adResource = adResult.ResourceName
	}
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adName, adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID != adGroupID {
		return assetIDs, "", fmt.Errorf("google-ads demand gen video ad %q creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q does not name an ad in that ad group — verify in Google Ads before retrying)", adName, adGroupID, adResource)
	}
	return assetIDs, adID, nil
}

// demandGenVideoCreativeStep summarises the video ad for the step list.
func demandGenVideoCreativeStep(plan demandGenVideoPlan) string {
	s := fmt.Sprintf("%d YouTube video(s), %d logo(s), %d headline(s), %d long headline(s), %d description(s)",
		len(plan.videos), len(plan.logos), len(plan.headlines), len(plan.longHeadlines), len(plan.descriptions))
	if plan.callToAction != "" {
		s += ", call to action " + plan.callToAction
	}
	return s
}

// sameDemandGenVideoAsset reports whether two asset operations of the video ad's atomic request
// create the same content: the same YouTube video or the same call to action. The temporary
// resource names always differ, so they are not compared.
func sameDemandGenVideoAsset(a, b *mutateOperation) bool {
	if a == nil || b == nil {
		return false
	}
	x, ok1 := a.Create.(demandGenVideoAssetCreate)
	y, ok2 := b.Create.(demandGenVideoAssetCreate)
	if !ok1 || !ok2 {
		return false
	}
	x.ResourceName, y.ResourceName = "", ""
	return reflect.DeepEqual(x, y)
}
