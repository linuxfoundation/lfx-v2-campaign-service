// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Search campaign image, business-logo and business-name assets (#477).
//
// The live KubeCon NA 2026 Search campaign carries seven image assets, a business logo and a
// business name, all linked to the CAMPAIGN as campaign assets. An LFX-built Search campaign
// carried none of them. This file adds the three field types, and only the three:
//
//   - AD_IMAGE, SQUARE images only. The validateOnly probe covered a square image, and no slot
//     in this package documents a 1.91:1 shape for this field type, so the landscape shape is
//     not admitted.
//   - BUSINESS_LOGO, at most one, square.
//   - BUSINESS_NAME, at most one, at most 25 display-width units, sent as a TEXT asset.
//
// The images reuse the machinery every other image-bearing creative here uses: the slot table
// shape, validateImageEntries for the entry strings, fetchSlotImages for the bytes (one bounded
// pass, before the budget mutate), validateImageBytes for decode and geometry, and the stored-
// asset entries the dispatcher resolves. What is new is only the slot table and the request.
//
// # One atomic request
//
// The assets and their campaign links go in ONE googleAds:mutate. Each asset is created under a
// TEMPORARY resource name (a negative id), which the campaignAsset link in the same request
// names; Google substitutes the real id. partialFailure is left off, so the request is atomic:
// either every asset exists with its link, or nothing in it was applied. That is the reason for
// the shape rather than the two calls the extension assets take (assets.go): a definite refusal
// here leaves no unlinked account-level asset behind.
//
// # What was verified, and what was not
//
// The 2026-11-03 validateOnly probes (v23, recorded on #477) accepted: a campaignAsset with
// AD_IMAGE and one with BUSINESS_LOGO, each naming an EXISTING square image asset; and a
// BUSINESS_NAME campaignAsset naming a text asset created by temporary id in the same
// googleAds:mutate. NOT LIVE-VERIFIED: an IMAGE asset created by temporary id inside
// googleAds:mutate and linked in that same request — the exact shape sent below. Image asset
// creation itself is the shape Display and Demand Gen send to assets:mutate, and the temporary-
// id link is the shape the BUSINESS_NAME probe accepted; the two have not been probed together.
// The slot minimums (300x300 for AD_IMAGE, 128x128 for BUSINESS_LOGO) and the twenty-image cap
// are Google's documented asset specifications, not probed values.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	// assetFieldAdImage and assetFieldBusinessLogo are the AssetFieldType values the campaign
	// links carry. BUSINESS_NAME is assetFieldBusinessName, shared with Performance Max.
	assetFieldAdImage      = "AD_IMAGE"
	assetFieldBusinessLogo = "BUSINESS_LOGO"

	// maxSearchAdImages bounds the AD_IMAGE list. Google documents up to twenty image assets
	// per campaign; this is that ceiling, not probed. The live reference campaign carries
	// seven.
	maxSearchAdImages = 20

	// maxSearchBusinessLogos is one: a campaign shows one business logo, and #477 asks for one.
	maxSearchBusinessLogos = 1

	// maxSearchBusinessNameWeight is the business name's limit in display-width units, the
	// same 25 and the same double-width accounting as the Demand Gen and Display business
	// names. Spelled out rather than aliased, for the reason display_creative.go gives.
	maxSearchBusinessNameWeight = 25
)

// SearchCampaignAssets are the campaign-level image, logo and business-name assets a Search
// campaign links (#477). SEARCH ONLY: refused at preflight on every other channel.
//
// Every field is optional and each is independent: a logo without a name, or images alone, is
// accepted. Left empty, nothing is fetched and no request is sent, which is the pre-#477 shape.
type SearchCampaignAssets struct {
	// AdImages are AD_IMAGE assets: square (1:1) images, at least 300x300, at most twenty. Each
	// entry is an https URL this client fetches or a stored-asset entry the caller resolved
	// (ResolvedImageRefPrefix), exactly as on the other creatives.
	AdImages []string
	// BusinessLogoImages is the BUSINESS_LOGO asset: at most one square (1:1) image, at least
	// 128x128. A list so it shares the slot machinery; more than one is refused.
	BusinessLogoImages []string
	// BusinessName is the BUSINESS_NAME text asset, at most 25 display-width units.
	BusinessName string
}

// empty reports whether the caller asked for no Search campaign asset at all.
func (a SearchCampaignAssets) empty() bool {
	return len(a.AdImages) == 0 && len(a.BusinessLogoImages) == 0 &&
		strings.TrimSpace(a.BusinessName) == ""
}

// searchImageSlots is the Search campaign's image slots, in the order the assets are sent.
// Each label names its channel, as every slot table here does, because the validators and the
// fetcher are shared and a bare "logo image" would not say which creative was wrong.
var searchImageSlots = []imageSlot{
	{label: "Search ad image", jsonKey: "adImages",
		ratioW: 1, ratioH: 1, minW: 300, minH: 300},
	{label: "Search business logo", jsonKey: "businessLogoImages",
		ratioW: 1, ratioH: 1, minW: 128, minH: 128},
}

// Indices into searchImageSlots, so the rules naming a slot address it by position rather than
// by label.
const (
	searchAdImageSlotIndex      = 0
	searchBusinessLogoSlotIndex = 1
)

// searchImageSlotFieldTypes is the campaignAsset field type each slot links under, positionally
// parallel to searchImageSlots.
var searchImageSlotFieldTypes = []string{assetFieldAdImage, assetFieldBusinessLogo}

// searchAssetPlan is the validated input: the per-slot entries (parallel to searchImageSlots),
// the trimmed business name and the caller's resolved bytes.
type searchAssetPlan struct {
	urls         [][]string
	businessName string
	resolved     map[string][]byte
	present      bool
}

// validateSearchAssets resolves the Search campaign assets WITHOUT sending or fetching anything.
// Called from preflightCampaignKind, so the adoption path refuses exactly what the create path
// refuses. Whether a URL serves a decodable square image is fetchSearchAssetImages' job, which
// still runs before the budget mutate.
func validateSearchAssets(kind string, in CampaignInput) (searchAssetPlan, error) {
	a := in.SearchAssets
	if a.empty() {
		return searchAssetPlan{}, nil
	}
	// Refused as a channel capability: only the Search cascade links these, so accepting them
	// elsewhere would validate and fetch the images and then drop them. Performance Max, Demand
	// Gen and Display carry their own business name and logo on their own creatives.
	if kind != campaignKindSearch {
		return searchAssetPlan{}, fmt.Errorf("google-ads Search campaign assets (ad images, "+
			"business logo, business name) are supported on Search campaigns only, not on %s; "+
			"omit SearchAssets, or use the channel's own creative", kind)
	}
	plan := searchAssetPlan{
		present:  true,
		urls:     make([][]string, len(searchImageSlots)),
		resolved: in.ResolvedImages,
	}
	raw := [][]string{a.AdImages, a.BusinessLogoImages}
	for i, slot := range searchImageSlots {
		entries, err := validateImageEntries(slot, raw[i], in.ResolvedImages)
		if err != nil {
			return searchAssetPlan{}, err
		}
		plan.urls[i] = entries
	}
	if n := len(plan.urls[searchAdImageSlotIndex]); n > maxSearchAdImages {
		return searchAssetPlan{}, fmt.Errorf(
			"google-ads Search campaign accepts at most %d ad images, got %d", maxSearchAdImages, n)
	}
	if n := len(plan.urls[searchBusinessLogoSlotIndex]); n > maxSearchBusinessLogos {
		return searchAssetPlan{}, fmt.Errorf(
			"google-ads Search campaign accepts at most %d business logo, got %d",
			maxSearchBusinessLogos, n)
	}
	name := strings.TrimSpace(a.BusinessName)
	if w := textWeight(name); w > maxSearchBusinessNameWeight {
		return searchAssetPlan{}, fmt.Errorf(
			"google-ads Search business name %q has a display width of %d, exceeding the %d limit",
			capForError(name), w, maxSearchBusinessNameWeight)
	}
	plan.businessName = name
	return plan, nil
}

// fetchSearchAssetImages downloads and shape-checks every image the plan names, through the
// shared fetcher. A one-line wrapper for the reason fetchDisplayImages is one: the slot table
// is the channel-specific part.
func (c *Client) fetchSearchAssetImages(
	ctx context.Context, plan searchAssetPlan,
) ([]fetchedImage, error) {
	if !plan.present {
		return nil, nil
	}
	return c.fetchSlotImages(ctx, searchImageSlots, plan.urls, plan.resolved)
}

// searchAssetCreate is an Asset create inside googleAds:mutate: a temporary resource name and
// exactly one of an image or a text arm. Kept separate from assets.go's assetCreate (the
// extension oneof) and performanceMaxAssetCreate for the reason demandGenAssetCreate states: a
// struct spanning two uses makes every reader work out which half applies.
type searchAssetCreate struct {
	ResourceName string            `json:"resourceName"`
	ImageAsset   *imageAssetCreate `json:"imageAsset,omitempty"`
	TextAsset    *textAssetCreate  `json:"textAsset,omitempty"`
}

// pendingSearchAsset pairs one asset create with the field type its campaign link carries.
type pendingSearchAsset struct {
	create    searchAssetCreate
	fieldType string
}

// searchTempAssetName is the temporary resource name of the i-th asset in the request: -1, -2,
// and so on. Google treats a negative id as a placeholder scoped to the one request.
func (c *Client) searchTempAssetName(i int) string {
	return "customers/" + c.account.CustomerID + "/assets/-" + strconv.Itoa(i+1)
}

// buildSearchAssets turns the plan and the fetched images into the asset list, in a FIXED
// order: images in slot order (ad images, then the logo), then the business name. The order is
// what the positional result check below reads, and a test pins it.
func (c *Client) buildSearchAssets(
	plan searchAssetPlan, images []fetchedImage,
) []pendingSearchAsset {
	out := make([]pendingSearchAsset, 0, len(images)+1)
	for _, img := range images {
		// NO name, for the reason buildPerformanceMaxAssets gives: the obvious label would
		// export the caller's creative URL into the shared account.
		out = append(out, pendingSearchAsset{
			create: searchAssetCreate{
				ResourceName: c.searchTempAssetName(len(out)),
				ImageAsset:   &imageAssetCreate{Data: base64Image(img.data)},
			},
			fieldType: searchImageSlotFieldTypes[img.slot],
		})
	}
	if plan.businessName != "" {
		out = append(out, pendingSearchAsset{
			create: searchAssetCreate{
				ResourceName: c.searchTempAssetName(len(out)),
				TextAsset:    &textAssetCreate{Text: plan.businessName},
			},
			fieldType: assetFieldBusinessName,
		})
	}
	return out
}

// createSearchCampaignAssets creates the Search campaign's image, logo and business-name assets
// and links each to the campaign, in ONE atomic googleAds:mutate — see the file comment.
//
// It returns the created asset ids. Outcomes, by the partial-result contract every step past the
// campaign create follows:
//
//   - a definite refusal: nothing in the request was applied, so no id is returned;
//   - an ambiguous failure (5xx, 429, transport): UNCONFIRMED, and no id is known — the assets
//     were named only by temporary ids;
//   - a 2xx that does not describe every asset and every link as the operation at its index
//     asked: UNCONFIRMED, with every well-formed same-account asset id the response did carry,
//     because the request answered 2xx and so its assets exist.
func (c *Client) createSearchCampaignAssets(
	ctx context.Context, campaignResource, campaignID string,
	plan searchAssetPlan, images []fetchedImage,
) ([]string, error) {
	pending := c.buildSearchAssets(plan, images)
	if len(pending) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads Search campaign assets aborted before any request "+
			"(context already done; campaign %s has none of them): %w", campaignID, ctxErr)
	}

	ops := make([]googleAdsMutateOperation, 0, 2*len(pending))
	for i := range pending {
		ops = append(ops, googleAdsMutateOperation{
			AssetOperation: &mutateOperation{Create: pending[i].create},
		})
	}
	for i := range pending {
		ops = append(ops, googleAdsMutateOperation{
			CampaignAssetOperation: &mutateOperation{Create: campaignAssetCreate{
				Campaign:  campaignResource,
				Asset:     pending[i].create.ResourceName,
				FieldType: pending[i].fieldType,
			}},
		})
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("googleAds:mutate"),
		googleAdsMutateRequest{MutateOperations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads Search campaign assets UNCONFIRMED (campaign %s; "+
				"%d asset(s) and their links may exist — verify in Google Ads before retrying): %w",
				campaignID, len(pending), err)
		}
		return nil, fmt.Errorf("google-ads Search campaign asset creation failed (campaign %s "+
			"created; the %d asset(s) and their links are one atomic request, so none of them "+
			"exists): %w", campaignID, len(pending), err)
	}

	var results googleAdsMutateResponse
	if uErr := json.Unmarshal(resp, &results); uErr != nil {
		return nil, fmt.Errorf("google-ads Search campaign assets UNCONFIRMED (campaign %s; 2xx "+
			"with a malformed mutate response — the %d asset(s) and their links may exist — "+
			"verify in Google Ads before retrying)", campaignID, len(pending))
	}
	ids := c.searchAssetResultIDs(results, len(pending))
	if len(results.MutateOperationResponses) != len(ops) {
		return ids, fmt.Errorf("google-ads Search campaign assets UNCONFIRMED (campaign %s; 2xx "+
			"returned %d result(s) for %d operation(s) — verify in Google Ads before retrying)",
			campaignID, len(results.MutateOperationResponses), len(ops))
	}
	assetIDs := make([]string, 0, len(pending))
	for i := range pending {
		r := results.MutateOperationResponses[i].AssetResult
		id := ""
		if r != nil {
			id = c.assetID(r.ResourceName)
		}
		if id == "" {
			return ids, fmt.Errorf("google-ads Search campaign assets UNCONFIRMED (campaign %s; "+
				"result %d is not an asset of this account — verify in Google Ads before "+
				"retrying)", campaignID, i)
		}
		assetIDs = append(assetIDs, id)
	}
	// Each link result must name exactly the link its operation asked for: this campaign, the
	// asset created at the same index, and the field type. A 2xx naming anything else is not
	// proof that link exists.
	for i := range pending {
		r := results.MutateOperationResponses[len(pending)+i].CampaignAssetResult
		if r == nil {
			return assetIDs, fmt.Errorf("google-ads Search campaign asset linking UNCONFIRMED "+
				"(campaign %s; result %d carries no campaign asset — verify in Google Ads before "+
				"retrying)", campaignID, len(pending)+i)
		}
		gotCampaign, gotAsset, gotField := c.campaignAssetID(r.ResourceName)
		if gotCampaign != campaignID || gotAsset != assetIDs[i] ||
			!strings.EqualFold(gotField, pending[i].fieldType) {
			return assetIDs, fmt.Errorf("google-ads Search campaign asset linking UNCONFIRMED "+
				"(campaign %s; campaignAsset resource name %q at index %d is not the %s link of "+
				"asset %s — verify in Google Ads before retrying)",
				campaignID, r.ResourceName, i, pending[i].fieldType, assetIDs[i])
		}
	}
	return assetIDs, nil
}

// searchAssetResultIDs is every well-formed same-account asset id among the first n results,
// for the UNCONFIRMED arms whose response nonetheless parsed. A wrong-account or malformed name
// is skipped, never recorded.
func (c *Client) searchAssetResultIDs(results googleAdsMutateResponse, n int) []string {
	var out []string
	for i, r := range results.MutateOperationResponses {
		if i >= n {
			break
		}
		if r.AssetResult == nil {
			continue
		}
		if id := c.assetID(r.AssetResult.ResourceName); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// searchAssetStep summarises the linked assets for the step list, naming only what was asked.
func searchAssetStep(plan searchAssetPlan) string {
	parts := make([]string, 0, 3)
	if n := len(plan.urls[searchAdImageSlotIndex]); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ad image(s)", n))
	}
	if len(plan.urls[searchBusinessLogoSlotIndex]) > 0 {
		parts = append(parts, "a business logo")
	}
	if plan.businessName != "" {
		parts = append(parts, "a business name")
	}
	return strings.Join(parts, ", ")
}
