// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	// maxBusinessNameRunes is the v23 DemandGenMultiAssetResponsiveDisplayAd businessName
	// limit (≤ 25 characters). Counted in runes, not bytes, so a multi-byte name is measured
	// as Google measures it. Pending live confirmation (research O1); the value is the
	// documented v23 limit.
	maxBusinessNameRunes = 25

	// maxDemandGenHeadlines caps the headline set at the Demand Gen maximum (5), which is
	// LOWER than RSA's 15 (maxHeadlines). composeAdCopy is reused for the copy, then the
	// result is capped here — sending a 6th headline is rejected by the API on this ad type.
	maxDemandGenHeadlines = 5
)

// adImageAsset references a previously-uploaded image asset by resource name from an image
// role on the ad — the {"asset": "customers/{cid}/assets/{id}"} shape each of the ad's three
// image-role arrays holds. Distinct from imageAsset (assets.go), which is the CREATE payload
// carrying the bytes; this is only a reference to an already-created asset.
type adImageAsset struct {
	Asset string `json:"asset"`
}

// demandGenMultiAssetResponsiveDisplayAd is the ad-type payload for a Demand Gen single-image
// ad — the non-carousel/non-video Demand Gen ad. It carries business name + text assets plus
// three image roles, each a distinct aspect ratio (see DemandGenCreative in campaign.go): a
// single file cannot satisfy all three, which is why "single image" is three references. Field
// names are the v23 REST shapes pinned in research.md (§1, D1, D4); O1 confirms them live.
type demandGenMultiAssetResponsiveDisplayAd struct {
	BusinessName          string         `json:"businessName"`
	Headlines             []adTextAsset  `json:"headlines"`
	Descriptions          []adTextAsset  `json:"descriptions"`
	MarketingImages       []adImageAsset `json:"marketingImages"`
	SquareMarketingImages []adImageAsset `json:"squareMarketingImages"`
	LogoImages            []adImageAsset `json:"logoImages"`
}

// demandGenAdInputs is everything the Demand Gen ad create needs, validated and derived
// WITHOUT any request by precomputeDemandGenAd. It holds the resolved image BYTES (not yet
// uploaded) rather than asset resource names — the upload happens later, after precompute has
// proven every local input good, so a bad business name or missing image never uploads an
// asset or, worse, orphans a paid campaign.
type demandGenAdInputs struct {
	mediaFormat  string
	businessName string
	finalURL     string
	headlines    []string
	descriptions []string
	// The three role images, in role order. Each is guaranteed non-empty bytes by precompute.
	marketing CreativeImage
	square    CreativeImage
	logo      CreativeImage
}

// precomputeDemandGenAd validates the creative and derives the ad inputs with NO request,
// mirroring precomputeAdGroupAdInputs for the Search path: an unknown media format, an
// over-length/absent business name, a missing image role, unusable ad copy, or an over-length
// destination URL must fail BEFORE the first mutate — surfacing it after budget/campaign/ad
// group committed would orphan a paid campaign for what is purely local input validation.
//
// Reuses composeAdCopy (the same copy rules as the Search ad), then caps headlines at the
// Demand Gen maximum, which is lower than RSA's. Descriptions already fit (composeAdCopy caps
// at 4 ≤ the Demand Gen max of 5).
func precomputeDemandGenAd(in CampaignInput) (*demandGenAdInputs, error) {
	cr := in.Creative
	if cr == nil {
		return nil, fmt.Errorf("google-ads demand gen ad precompute called with no creative")
	}
	// An unrecognised format is rejected, never defaulted: a typo must not silently build the
	// wrong ad. Only single_image exists today; carousel/video add a case here AND a builder
	// branch in buildDemandGenAd (and their own adCreate ad-type field).
	if cr.MediaFormat != MediaFormatSingleImage {
		return nil, fmt.Errorf("google-ads demand gen ad: unsupported media format %q (only %q is supported)", cr.MediaFormat, MediaFormatSingleImage)
	}

	businessName := strings.TrimSpace(cr.BusinessName)
	if businessName == "" {
		return nil, fmt.Errorf("google-ads demand gen ad requires a non-empty business name")
	}
	if n := utf8.RuneCountInString(businessName); n > maxBusinessNameRunes {
		return nil, fmt.Errorf("google-ads demand gen ad business name is %d characters, exceeding the %d limit", n, maxBusinessNameRunes)
	}

	// Every role must carry resolved bytes. The dispatcher fills Bytes from the asset store
	// (G4); a role with no bytes means the creative was assembled wrong, and uploading an
	// empty asset would 400 late or create a useless asset — caught here instead.
	for _, role := range []struct {
		name string
		img  CreativeImage
	}{
		{"marketing image", cr.MarketingImage},
		{"square marketing image", cr.SquareMarketingImage},
		{"logo", cr.Logo},
	} {
		if len(role.img.Bytes) == 0 {
			return nil, fmt.Errorf("google-ads demand gen ad %s has no image bytes (all three image roles are required)", role.name)
		}
	}

	finalURL, err := buildAdFinalURL(in.RegistrationURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
	if err != nil {
		return nil, fmt.Errorf("google-ads demand gen ad creation aborted before any request (invalid destination URL): %w", err)
	}
	if n := len(finalURL); n > maxFinalURLBytes {
		return nil, fmt.Errorf("google-ads demand gen ad creation aborted before any request (composed ad final URL is %d bytes, exceeding the %d limit; shorten the registration URL)", n, maxFinalURLBytes)
	}

	headlines, descriptions, err := composeAdCopy(in.Headlines, in.Descriptions, in.EventName, in.Project)
	if err != nil {
		return nil, fmt.Errorf("google-ads demand gen ad creation aborted before any request (invalid ad copy): %w", err)
	}
	if len(headlines) > maxDemandGenHeadlines {
		headlines = headlines[:maxDemandGenHeadlines]
	}

	return &demandGenAdInputs{
		mediaFormat:  cr.MediaFormat,
		businessName: businessName,
		finalURL:     finalURL,
		headlines:    headlines,
		descriptions: descriptions,
		marketing:    cr.MarketingImage,
		square:       cr.SquareMarketingImage,
		logo:         cr.Logo,
	}, nil
}

// demandGenAssetRefs holds the three role assets' resource names, in role order, after upload.
type demandGenAssetRefs struct {
	marketing string
	square    string
	logo      string
}

// uploadDemandGenAssets uploads the three role images and returns their resource names. It
// runs BEFORE the budget mutate (see CreateDemandGenCampaign), so a failure here leaves NO
// spending resource behind — only, at worst, a harmless non-spending library asset that a
// retry re-resolves by content address (see uploadImageAsset). The role name is threaded into
// the error so an operator knows WHICH image failed.
func (c *Client) uploadDemandGenAssets(ctx context.Context, in *demandGenAdInputs) (demandGenAssetRefs, error) {
	var refs demandGenAssetRefs
	for _, role := range []struct {
		name string
		img  CreativeImage
		dst  *string
	}{
		{"marketing image", in.marketing, &refs.marketing},
		{"square marketing image", in.square, &refs.square},
		{"logo", in.logo, &refs.logo},
	} {
		rn, err := c.uploadImageAsset(ctx, role.img.Bytes)
		if err != nil {
			return refs, fmt.Errorf("google-ads demand gen %s upload failed: %w", role.name, err)
		}
		*role.dst = rn
	}
	return refs, nil
}

// buildDemandGenAd assembles the ad-type payload for the creative's media format. It is keyed
// on mediaFormat so carousel/video (future formats, spec SC-005) add a case without touching
// the single-image path; precomputeDemandGenAd has already rejected any unknown format, so the
// default arm is a defensive nil (the caller treats nil as "no builder for this format").
func buildDemandGenAd(in *demandGenAdInputs, refs demandGenAssetRefs) *demandGenMultiAssetResponsiveDisplayAd {
	switch in.mediaFormat {
	case MediaFormatSingleImage:
		return &demandGenMultiAssetResponsiveDisplayAd{
			BusinessName:          in.businessName,
			Headlines:             textAssets(in.headlines),
			Descriptions:          textAssets(in.descriptions),
			MarketingImages:       []adImageAsset{{Asset: refs.marketing}},
			SquareMarketingImages: []adImageAsset{{Asset: refs.square}},
			LogoImages:            []adImageAsset{{Asset: refs.logo}},
		}
	default:
		return nil
	}
}

// createDemandGenAd creates the PAUSED Demand Gen ad in the just-created ad group and stamps
// res.AdID. It mirrors the Search path's ad-create block (createAdGroupAndAd in adgroup_ad.go)
// exactly for the ambiguity contract: an ambiguous failure is UNCONFIRMED (the ad may exist),
// a definite 4xx is a clean rejection (Demand Gen ads carry no unique name, so no
// duplicate-collision case), and the returned adGroupAd resource name is validated for kind,
// account, composite shape, and matching ad-group-id half before its ad id is trusted.
//
// res is mutated in place so the caller's partial-result plumbing carries whatever was created
// even when this returns an error.
func (c *Client) createDemandGenAd(ctx context.Context, adGroupResource, adGroupID, finalURL string, in *demandGenAdInputs, refs demandGenAssetRefs, res *CampaignResult) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads demand gen ad creation aborted after ad group %s created (context done before ad create; the ad group has no ad yet): %w", adGroupID, ctxErr)
	}

	adType := buildDemandGenAd(in, refs)
	if adType == nil {
		// Unreachable in practice — precompute rejects unknown formats — but a nil ad-type
		// must never be sent as an empty ad, so fail loudly rather than create a blank ad.
		return fmt.Errorf("google-ads demand gen ad creation aborted (no ad builder for media format %q; ad group %s created)", in.mediaFormat, adGroupID)
	}

	adReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  StatusPaused,
		Ad: adCreate{
			FinalUrls:                              []string{finalURL},
			DemandGenMultiAssetResponsiveDisplayAd: adType,
		},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		return fmt.Errorf("google-ads demand gen ad creation failed (ad group %s created): %w", adGroupID, err)
	}
	adResource, _, err := firstResourceName(adResp)
	if err != nil {
		return fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; 2xx with no/malformed resource name — an ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
	}
	// adGroupAdID validates the resource KIND but not the account; without this a wrong-account
	// adGroupAds resource would still parse and be accepted as this ad. requireNumericID=false:
	// the trailing segment is the composite "{adGroupId}~{adId}" shape adGroupAdID validates.
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID == "" {
		return fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	// The resource name's ad-group-id half must match the ad group this ad was created under —
	// a mismatch means the response doesn't describe the ad this call just created.
	if returnedAdGroupID != adGroupID {
		return fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
	}
	res.AdID = adID
	res.Steps = append(res.Steps, fmt.Sprintf("Demand Gen ad created: %s (PAUSED, %d headlines, %d descriptions, 3 image assets)", adID, len(in.headlines), len(in.descriptions)))
	return nil
}
