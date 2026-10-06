// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Display's creative is a RESPONSIVE DISPLAY AD, and it is the third creative in this
// package that holds caller-supplied image URLs — so it fetches, and everything the
// Demand Gen file comment says about why this service downloads the bytes itself rather
// than handing Google a URL applies here unchanged: Google's image asset takes `data`,
// not an address, so somebody has to do the fetch and it cannot be Google.
//
// What is NOT shared is the slot table. Three of this channel's four slots carry ratios
// or minimums that differ from the Demand Gen and Performance Max slots of the same
// name, which is exactly why those two already keep separate tables rather than one
// table with a channel column — see demandGenImageSlots and performanceMaxImageSlots.
// The validators, the fetcher and the geometry check are shared; only the rules are not.
//
// The one shape difference a reader should expect: ResponsiveDisplayAdInfo's LONG
// HEADLINE is a single text asset, not a list. Performance Max takes one to five long
// headlines and Video takes one to five; this channel takes exactly one, and the plan
// holds it as a string rather than a slice so the payload cannot accidentally be fed a
// second one that Google would reject after the ad group exists.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	// Count bounds for the responsive display ad's text. Headlines and descriptions
	// take one to five, as on Demand Gen; the long headline is a scalar and so has no
	// count bound at all.
	minDisplayHeadlines    = 1
	maxDisplayHeadlines    = 5
	minDisplayDescriptions = 1
	maxDisplayDescriptions = 5

	// Display WIDTHS, stated as Google states them — "maximum display width" — and so
	// measured by textWeight, where a double-width character counts twice.
	//
	// Spelled out rather than aliased to the RSA or Demand Gen constants they happen to
	// equal, for the reason video_creative.go gives about its own 90: the coincidence
	// is not a relationship, and an alias turns a future divergence in one channel's
	// limits into a silent change in another's.
	maxDisplayHeadlineWeight     = 30
	maxDisplayLongHeadlineWeight = 90
	maxDisplayDescriptionWeight  = 90
	maxDisplayBusinessNameWeight = 25

	// maxDisplayCallToActionRunes bounds the optional call-to-action button text.
	// Google documents no display width for this field on a responsive display ad, so
	// this is a payload bound rather than an upstream limit being mirrored —
	// deliberately generous, because refusing text Google would have accepted is the
	// costlier mistake. The same judgement maxDemandGenCallToActionRunes makes.
	maxDisplayCallToActionRunes = 30

	// maxDisplayMarketingImages is the COMBINED ceiling across the landscape and square
	// marketing arrays, which is how Google states it. Logos are counted separately and
	// are not part of this total.
	maxDisplayMarketingImages = 15

	// maxDisplayLogos bounds EACH logo array — landscape and square — separately,
	// because Google states the limit per array rather than across the pair.
	maxDisplayLogos = 5
)

// DisplayCreative is the responsive display ad for a Display campaign. It is DISPLAY
// ONLY, refused at preflight on every other channel, and is the fifth member of the
// per-channel creative family alongside DemandGenCreative, PerformanceMaxCreative and
// VideoCreative.
//
// Left empty the Display cascade creates the campaign and its ad group and no ad — a
// campaign that cannot serve, said so in the closing steps, kept rather than refused for
// the reason its siblings are kept: that shape is also what adoption of a campaign whose
// ad was built by hand upstream looks like.
type DisplayCreative struct {
	// MarketingImages (1.91:1) and SquareMarketingImages (1:1) are reciprocally
	// required — Google documents each as required when the other is absent — so
	// neither alone is mandatory and the pair is. Together they are capped at
	// maxDisplayMarketingImages.
	MarketingImages       []string
	SquareMarketingImages []string
	// LogoImages (4:1) and SquareLogoImages (1:1) are both OPTIONAL on this channel,
	// unlike the Demand Gen logo, which Google requires. Each array is capped on its
	// own at maxDisplayLogos.
	LogoImages       []string
	SquareLogoImages []string
	// Headlines are the SHORT headlines, one to five. LongHeadline is a single
	// required string, not a list — see the file comment.
	Headlines    []string
	LongHeadline string
	Descriptions []string
	// BusinessName is the advertiser/brand name. Required by Google.
	BusinessName string
	// CallToActionText is optional; Google supplies its own button text when absent.
	CallToActionText string
}

// empty reports whether the caller asked for no Display creative at all.
func (d DisplayCreative) empty() bool {
	return len(d.MarketingImages) == 0 &&
		len(d.SquareMarketingImages) == 0 &&
		len(d.LogoImages) == 0 &&
		len(d.SquareLogoImages) == 0 &&
		len(d.Headlines) == 0 &&
		len(d.Descriptions) == 0 &&
		strings.TrimSpace(d.LongHeadline) == "" &&
		strings.TrimSpace(d.BusinessName) == "" &&
		strings.TrimSpace(d.CallToActionText) == ""
}

// displayImageSlots is every image slot for this channel, in the order the ad payload
// lists them. Ratios and minimums are Google's own, from ResponsiveDisplayAdInfo in
// google/ads/googleads/v23/common/ad_type_infos.proto.
//
// Each label NAMES ITS CHANNEL, as the Demand Gen and Performance Max tables do and for
// the same reason: the slot validators are shared across all three, and three of this
// channel's four slots differ in ratio or minimum from a slot of the same name
// elsewhere, so an error reading only "logo image" would not tell an operator which
// creative they got wrong.
var displayImageSlots = []imageSlot{
	{label: "Display marketing image", jsonKey: "marketingImages", ratioW: 191, ratioH: 100, minW: 600, minH: 314},
	{label: "Display square marketing image", jsonKey: "squareMarketingImages", ratioW: 1, ratioH: 1, minW: 300, minH: 300},
	{label: "Display logo image", jsonKey: "logoImages", ratioW: 4, ratioH: 1, minW: 512, minH: 128},
	{label: "Display square logo image", jsonKey: "squareLogoImages", ratioW: 1, ratioH: 1, minW: 128, minH: 128},
}

// Indices into displayImageSlots. The two rules that name specific slots — the combined
// marketing cap and the per-array logo ceiling — address them by index rather than by
// matching on a label, so renaming a label cannot silently change which rule applies.
const (
	displayMarketingSlotIndex       = 0
	displaySquareMarketingSlotIndex = 1
	displayLogoSlotIndex            = 2
	displaySquareLogoSlotIndex      = 3
)

// displayCreativePlan is the validated creative: the per-slot image URLs in the order
// they will be fetched and attached, and the text already trimmed.
//
// urls is POSITIONALLY parallel to displayImageSlots, exactly as demandGenCreativePlan's
// is to its own table — the fetch needs the slot's shape rules and the ad payload needs
// the slot's json key, and neither is recoverable from a bare URL.
type displayCreativePlan struct {
	urls             [][]string
	headlines        []string
	longHeadline     string
	descriptions     []string
	businessName     string
	callToActionText string
	// present is false when the caller asked for no creative, which leaves the
	// campaign-and-ad-group-but-no-ad shape intact.
	present bool
}

// validateDisplayCreative resolves the creative input WITHOUT sending or fetching
// anything. Called from preflightCampaignKind alongside every other validator, so the
// adoption path refuses exactly what the create path refuses.
//
// What it cannot check is whether a URL actually serves a decodable image of the right
// shape. That is fetchDisplayImages' job, and it still runs before the budget mutate.
func validateDisplayCreative(kind string, in CampaignInput) (displayCreativePlan, error) {
	d := in.DisplayCreative
	if d.empty() {
		return displayCreativePlan{}, nil
	}
	// Refused as a channel capability, exactly as its three sibling creative validators
	// refuse off their own channels: ResponsiveDisplayAdInfo is not a shape a Search,
	// Demand Gen, Performance Max or Video ad group accepts.
	if kind != campaignKindDisplay {
		return displayCreativePlan{}, fmt.Errorf("google-ads Display creative (marketing images, long headline, business name) is supported on Display campaigns only, not on %s", kind)
	}

	plan := displayCreativePlan{present: true, urls: make([][]string, len(displayImageSlots))}

	raw := [][]string{d.MarketingImages, d.SquareMarketingImages, d.LogoImages, d.SquareLogoImages}
	for i, slot := range displayImageSlots {
		urls, err := validateImageURLs(slot, raw[i])
		if err != nil {
			return displayCreativePlan{}, err
		}
		plan.urls[i] = urls
	}

	// The marketing ceiling is stated as a combined total across the two arrays, so it
	// is checked once across them rather than twice per array — two arrays of 14 is 28
	// images and satisfies every per-array reading.
	marketing := len(plan.urls[displayMarketingSlotIndex]) + len(plan.urls[displaySquareMarketingSlotIndex])
	if marketing > maxDisplayMarketingImages {
		return displayCreativePlan{}, fmt.Errorf("google-ads responsive display ad accepts at most %d marketing images across both shapes, got %d", maxDisplayMarketingImages, marketing)
	}
	// At least one of the two REQUIRED shapes. Google's wording is reciprocal — each is
	// required when the other is absent — so neither alone is mandatory and the pair is.
	if marketing == 0 {
		return displayCreativePlan{}, errors.New("google-ads responsive display ad needs at least one marketing image or one square marketing image (Google requires each when the other is absent)")
	}
	// Both logo arrays are OPTIONAL here, which is the difference from Demand Gen worth
	// stating: that channel refuses an ad with no logo, this one does not, so there is a
	// ceiling and no floor.
	if n := len(plan.urls[displayLogoSlotIndex]); n > maxDisplayLogos {
		return displayCreativePlan{}, fmt.Errorf("google-ads responsive display ad accepts at most %d logo images, got %d", maxDisplayLogos, n)
	}
	if n := len(plan.urls[displaySquareLogoSlotIndex]); n > maxDisplayLogos {
		return displayCreativePlan{}, fmt.Errorf("google-ads responsive display ad accepts at most %d square logo images, got %d", maxDisplayLogos, n)
	}

	headlines, err := validateCreativeText("responsive display ad", "headline", d.Headlines, minDisplayHeadlines, maxDisplayHeadlines, maxDisplayHeadlineWeight)
	if err != nil {
		return displayCreativePlan{}, err
	}
	plan.headlines = headlines

	descriptions, err := validateCreativeText("responsive display ad", "description", d.Descriptions, minDisplayDescriptions, maxDisplayDescriptions, maxDisplayDescriptionWeight)
	if err != nil {
		return displayCreativePlan{}, err
	}
	plan.descriptions = descriptions

	// The long headline is validated inline rather than through validateCreativeText,
	// because that helper's whole contract is a LIST with count bounds and this field is
	// a scalar. Wrapping the string in a one-element slice to reuse it would produce
	// count errors ("needs at least 1 long headline") for a field that cannot have a
	// count, which is a worse message than the one below.
	longHeadline := strings.TrimSpace(d.LongHeadline)
	if longHeadline == "" {
		return displayCreativePlan{}, errors.New("google-ads responsive display ad requires a long headline (Google marks the field required, and it is a single headline rather than a list)")
	}
	if w := textWeight(longHeadline); w > maxDisplayLongHeadlineWeight {
		return displayCreativePlan{}, fmt.Errorf("google-ads responsive display ad long headline %q has a display width of %d, exceeding the %d limit", capForError(longHeadline), w, maxDisplayLongHeadlineWeight)
	}
	plan.longHeadline = longHeadline

	name := strings.TrimSpace(d.BusinessName)
	if name == "" {
		return displayCreativePlan{}, errors.New("google-ads responsive display ad requires a business name (Google marks the field required)")
	}
	if w := textWeight(name); w > maxDisplayBusinessNameWeight {
		return displayCreativePlan{}, fmt.Errorf("google-ads Display business name %q has a display width of %d, exceeding the %d limit", capForError(name), w, maxDisplayBusinessNameWeight)
	}
	plan.businessName = name

	cta := strings.TrimSpace(d.CallToActionText)
	if n := utf8.RuneCountInString(cta); n > maxDisplayCallToActionRunes {
		return displayCreativePlan{}, fmt.Errorf("google-ads Display call to action %q is %d characters, exceeding the %d limit", capForError(cta), n, maxDisplayCallToActionRunes)
	}
	plan.callToActionText = cta

	return plan, nil
}

// fetchDisplayImages downloads every image this creative references, in slot order.
//
// A one-line wrapper over the shared fetcher, exactly as fetchDemandGenImages is, and
// kept for the same reason: the slot TABLE is the channel-specific part, and naming the
// channel at the call site is what keeps the caller in display.go from having to know
// which table belongs to it.
func (c *Client) fetchDisplayImages(ctx context.Context, plan displayCreativePlan) ([]fetchedImage, error) {
	if !plan.present {
		return nil, nil
	}
	return c.fetchSlotImages(ctx, displayImageSlots, plan.urls)
}

// ---------------------------------------------------------------------------
// Creating the assets and the ad
// ---------------------------------------------------------------------------

// responsiveDisplayAdInfo mirrors ResponsiveDisplayAdInfo.
//
// LongHeadline is a single adTextAsset and is NOT omitempty: it is required, and a
// marshalled zero value is what a reader needs to see fail loudly rather than a field
// that quietly vanishes. Every image array IS omitempty, because Google distinguishes an
// absent optional array from an empty one, and the marketing pair is guaranteed to have
// at least one member between them by validateDisplayCreative.
type responsiveDisplayAdInfo struct {
	MarketingImages       []adImageAsset `json:"marketingImages,omitempty"`
	SquareMarketingImages []adImageAsset `json:"squareMarketingImages,omitempty"`
	LogoImages            []adImageAsset `json:"logoImages,omitempty"`
	SquareLogoImages      []adImageAsset `json:"squareLogoImages,omitempty"`
	Headlines             []adTextAsset  `json:"headlines"`
	LongHeadline          adTextAsset    `json:"longHeadline"`
	Descriptions          []adTextAsset  `json:"descriptions"`
	BusinessName          string         `json:"businessName"`
	CallToActionText      string         `json:"callToActionText,omitempty"`
}

// displayAdCreate is the "ad" object nested in an adGroupAd create, and
// displayAdGroupAdCreate the create payload for adGroupAds:mutate on this channel. Both
// are separate from their Demand Gen and Search namesakes for the reason those two are
// separate from each other: the sibling's ad-type arm has no meaning here, and one
// struct spanning several would make every reader check which half applies.
type displayAdCreate struct {
	FinalUrls           []string                 `json:"finalUrls"`
	ResponsiveDisplayAd *responsiveDisplayAdInfo `json:"responsiveDisplayAd,omitempty"`
}

type displayAdGroupAdCreate struct {
	AdGroup string          `json:"adGroup"`
	Status  string          `json:"status"`
	Ad      displayAdCreate `json:"ad"`
}

// createDisplayAd uploads the fetched images as account-level assets and then creates
// the ad that references them.
//
// Two mutates, the same shape createDemandGenAd uses and for the same reason: Google has
// no single call that creates an asset and attaches it. The second mutate uses THE
// RESOURCE NAMES GOOGLE RETURNED, never names rebuilt from the parsed ids.
//
// The ad is created PAUSED, matching every other channel's ad. The AD GROUP above it is
// created ENABLED, as Demand Gen's and Video's are — Search is the only channel here
// that pauses its ad group. The campaign is PAUSED either way, so nothing serves.
//
// Returns the created asset ids and the ad id. Both are reported even when the later
// stage fails, because assets created with no ad referencing them are account-level
// litter the operator can find and remove.
func (c *Client) createDisplayAd(ctx context.Context, adGroupResource, adGroupID, finalURL string, plan displayCreativePlan, images []fetchedImage) (assetIDs []string, adID string, err error) {
	if len(images) == 0 {
		return nil, "", nil
	}

	assetOps := make([]mutateOperation, 0, len(images))
	for _, img := range images {
		assetOps = append(assetOps, mutateOperation{Create: demandGenAssetCreate{
			ImageAsset: imageAssetCreate{Data: base64Image(img.data)},
		}})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		// A 5xx or a timeout on a mutating POST is an UNKNOWN outcome, not a failure:
		// the assets may well have been created, and an operator who believed "failed"
		// would retry into a second set of account-level image assets.
		if createOutcomeAmbiguous(err) {
			return nil, "", fmt.Errorf("google-ads display image asset creation UNCONFIRMED (%d image(s) may exist; ad group %s created — verify in Google Ads before retrying): %w", len(assetOps), adGroupID, err)
		}
		return nil, "", fmt.Errorf("google-ads display image asset creation failed (%d image(s); ad group %s created): %w", len(assetOps), adGroupID, err)
	}
	var assetResults mutateResponse
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil {
		return nil, "", fmt.Errorf("google-ads display image asset creation UNCONFIRMED (ad group %s created; 2xx with a malformed mutate response for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetOps))
	}
	// EXACT equality, like every other create-path mutate in this package: one operation
	// per image means a short response leaves images unaccounted for, and an extra
	// result is a response that does not describe what was sent.
	if len(assetResults.Results) != len(assetOps) {
		// The body PARSED, so whatever ids it did carry are real — and they are the only
		// handle an operator has on assets that may already exist. They go back WITH the
		// error rather than being dropped alongside it.
		return c.parsedAssetIDs(assetResults), "", fmt.Errorf("google-ads display image asset creation UNCONFIRMED (ad group %s created; 2xx returned %d result(s) for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetResults.Results), len(assetOps))
	}

	// Positional: result i is the asset for images[i], which carries its slot.
	perSlot := make([][]adImageAsset, len(displayImageSlots))
	assetIDs = make([]string, 0, len(assetOps))
	for i, r := range assetResults.Results {
		resource := r.ResourceName
		// assetID checks the kind, the account AND the numeric trailing id, returning ""
		// for anything else. Without it a wrong-account assets resource would be
		// referenced by the ad and persisted as this campaign's.
		id := c.assetID(resource)
		if id == "" {
			return assetIDs, "", fmt.Errorf("google-ads display image asset creation UNCONFIRMED (ad group %s created; malformed asset resource name %q at index %d — assets may exist — verify in Google Ads before retrying)", adGroupID, resource, i)
		}
		assetIDs = append(assetIDs, id)
		perSlot[images[i].slot] = append(perSlot[images[i].slot], adImageAsset{Asset: resource})
	}

	ad := &responsiveDisplayAdInfo{
		MarketingImages:       perSlot[displayMarketingSlotIndex],
		SquareMarketingImages: perSlot[displaySquareMarketingSlotIndex],
		LogoImages:            perSlot[displayLogoSlotIndex],
		SquareLogoImages:      perSlot[displaySquareLogoSlotIndex],
		Headlines:             textAssets(plan.headlines),
		LongHeadline:          adTextAsset{Text: plan.longHeadline},
		Descriptions:          textAssets(plan.descriptions),
		BusinessName:          plan.businessName,
		CallToActionText:      plan.callToActionText,
	}
	adReq := mutateRequest{Operations: []mutateOperation{{Create: displayAdGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  "PAUSED",
		Ad:      displayAdCreate{FinalUrls: []string{finalURL}, ResponsiveDisplayAd: ad},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return assetIDs, "", fmt.Errorf("google-ads display ad creation UNCONFIRMED (ad group %s and %d image asset(s) created; the ad may exist — verify in Google Ads before retrying): %w", adGroupID, len(assetIDs), err)
		}
		return assetIDs, "", fmt.Errorf("google-ads display ad creation failed (ad group %s and %d image asset(s) created): %w", adGroupID, len(assetIDs), err)
	}
	var adResults mutateResponse
	if uErr := json.Unmarshal(adResp, &adResults); uErr != nil || len(adResults.Results) != 1 {
		return assetIDs, "", fmt.Errorf("google-ads display ad creation UNCONFIRMED (ad group %s created; 2xx with a malformed/short mutate response — the ad may exist — verify in Google Ads before retrying)", adGroupID)
	}
	adResource := adResults.Results[0].ResourceName
	// requireNumericID=false: the trailing segment is the composite
	// "{adGroupId}~{adId}", split and checked by adGroupAdID just below.
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return assetIDs, "", fmt.Errorf("google-ads display ad creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID == "" {
		return assetIDs, "", fmt.Errorf("google-ads display ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	// The resource name must describe the ad group this ad was created under. A mismatch
	// means the response is not about this call, so the ad id is not trustworthy enough
	// to persist and later toggle.
	if returnedAdGroupID != adGroupID {
		return assetIDs, "", fmt.Errorf("google-ads display ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
	}
	return assetIDs, adID, nil
}

// displayCreativeStep summarises what the ad was built from, for the operator-facing
// step list.
func displayCreativeStep(plan displayCreativePlan) string {
	marketing := len(plan.urls[displayMarketingSlotIndex]) + len(plan.urls[displaySquareMarketingSlotIndex])
	logos := len(plan.urls[displayLogoSlotIndex]) + len(plan.urls[displaySquareLogoSlotIndex])
	return fmt.Sprintf("%d marketing image(s), %d logo(s), %d headline(s), %d description(s)",
		marketing, logos, len(plan.headlines), len(plan.descriptions))
}

// displayAdGroupName composes the single ad group this channel creates, mirroring
// videoAdGroupName: one group per campaign, named off the event so an operator reading
// the Google Ads UI sees which brief it came from.
func displayAdGroupName(in CampaignInput) string {
	return strings.TrimSpace(in.EventName) + " - Display"
}
