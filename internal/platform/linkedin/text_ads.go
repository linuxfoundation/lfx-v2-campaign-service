// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// LinkedIn Text Ads (#586).
//
// A Text Ad campaign is campaign type TEXT_AD, and its creatives carry their copy inline as
// content.textAd — there is no dark post. Sources, li-lms-2026-02 (the pinned apiVersion
// 202602), consulted 2026-11-13:
//
//   - "Text Ads API"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/advertising-targeting/version/text-ads-integrations?view=li-lms-2026-02):
//     the creative's content is {"textAd": {"headline", "description", "landingPage", "image"}};
//     headline "Use up to 25 characters, including spaces" (required), description "Use up to
//     75 characters, including spaces" (required), landingPage a URL (required), image an
//     ImageUrn "size is 100 x 100 pixels and <= 2MB, format is either a JPG or PNG file. If
//     empty, there's none" (optional). Placement: "the right rail of various LinkedIn desktop
//     pages".
//   - "Create and Manage Campaigns": campaign type TEXT_AD, "Text-based ads that show up in the
//     right column or top of the page on LinkedIn"; its Text ads example sends no `format` and
//     offsiteDeliveryEnabled false.
//   - "Campaign Objectives" (Campaign Objective API Mapping): for Website Visits and Website
//     Conversions the Text format offers only MANUAL bidding — optimizationTargetType None with
//     costType CPC ("Landing page clicks") or CPM ("Impressions"). There is no MAX_CLICK or
//     MAX_CONVERSION row for Text, so the automatic plan (WEBSITE_VISIT / MAX_CLICK) cannot be
//     created as a Text Ad, and a text create requires bidding.strategy "manual". The manual
//     plan (WEBSITE_CONVERSION / NONE / CPC / unitCost) is exactly the documented Text row.

// Campaign formats a create may ask for (CampaignInput.Format). Empty is FormatSingleImage.
const (
	// FormatSingleImage is the default: SPONSORED_UPDATES single-image/article Sponsored
	// Content, each variant a dark post plus a creative referencing it.
	FormatSingleImage = "single_image"
	// FormatText is a TEXT_AD campaign whose creatives are CampaignInput.TextAds.
	FormatText = "text"
)

// LinkedIn campaign types this client creates.
const (
	campaignTypeSponsoredUpdates = "SPONSORED_UPDATES"
	campaignTypeTextAd           = "TEXT_AD"
)

// Text Ad content limits ("Text Ads API" schema, above). They are REFUSED, never truncated: a
// cut-off headline is copy nobody approved.
const (
	TextAdHeadlineMaxChars    = 25
	TextAdDescriptionMaxChars = 75
	// MaxTextAdImageBytes reads "<= 2MB" decimally, so no file LinkedIn could count as over
	// 2 MB is ever sent (the same reading as MaxImageBytes).
	MaxTextAdImageBytes = 2_000_000
	// textAdImageSide is the documented "100 x 100 pixels".
	textAdImageSide = 100
)

// maxTextAds bounds the creatives one text create sends. LinkedIn caps a campaign at 100
// creatives (see listCreativeURNs); the bound is checked before anything exists.
const maxTextAds = 100

// TextAd is one Text Ad creative. The landing page is not caller input: like a single-image
// variant's article link it is the brief's registration URL with this ad's UTM parameters
// (BuildUTMURL), so it is validated once, by validateRegistrationURL.
type TextAd struct {
	Headline    string `json:"headline"`
	Description string `json:"description"`
	// ImageURN is an optional Images-API urn:li:image the caller already owns. Mutually
	// exclusive with ImageAssetID.
	ImageURN string `json:"imageUrn,omitempty"`
	// ImageAssetID optionally names a stored creative asset uploaded through the Images API,
	// a key into CampaignInput.Images, exactly as CreativeVariant.ImageAssetID.
	ImageAssetID string `json:"imageAssetId,omitempty"`
}

// ErrFormatInvalid marks a format, or a creative set, that does not match the format. Its
// sentences are service-authored and quote no caller text.
var ErrFormatInvalid = errors.New("linkedin: campaign format is not acceptable")

// NormalizeFormat returns the canonical format for raw, or ErrFormatInvalid. Empty and
// FormatSingleImage both mean the default.
func NormalizeFormat(raw string) (string, error) {
	switch raw {
	case "", FormatSingleImage:
		return FormatSingleImage, nil
	case FormatText:
		return FormatText, nil
	}
	return "", fmt.Errorf("%w: format must be %q (the default) or %q", ErrFormatInvalid, FormatSingleImage, FormatText)
}

// ValidateTextAds checks the content of every text ad before anything is created: headline and
// description present after the normalisation the wire copy gets, within LinkedIn's limits,
// and a well-formed image reference. It does not read images (see ValidateTextAdImage).
func ValidateTextAds(ads []TextAd) error {
	if len(ads) == 0 {
		return fmt.Errorf("%w: a text create requires at least one entry in textAds", ErrFormatInvalid)
	}
	if len(ads) > maxTextAds {
		return fmt.Errorf("%w: textAds has %d entries; a LinkedIn campaign holds at most %d creatives", ErrFormatInvalid, len(ads), maxTextAds)
	}
	for i, ad := range ads {
		n := i + 1
		head := normalizeCreativeText(ad.Headline)
		desc := normalizeCreativeText(ad.Description)
		if head == "" {
			return fmt.Errorf("%w: text ad %d headline is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid, n)
		}
		if c := len([]rune(head)); c > TextAdHeadlineMaxChars {
			return fmt.Errorf("%w: text ad %d headline is %d characters; LinkedIn allows at most %d, including spaces", ErrFormatInvalid, n, c, TextAdHeadlineMaxChars)
		}
		if desc == "" {
			return fmt.Errorf("%w: text ad %d description is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid, n)
		}
		if c := len([]rune(desc)); c > TextAdDescriptionMaxChars {
			return fmt.Errorf("%w: text ad %d description is %d characters; LinkedIn allows at most %d, including spaces", ErrFormatInvalid, n, c, TextAdDescriptionMaxChars)
		}
		if ad.ImageURN != "" && ad.ImageAssetID != "" {
			return fmt.Errorf("%w: text ad %d names both an image URN and a stored image asset; supply one", ErrFormatInvalid, n)
		}
		if ad.ImageURN != "" && !imageURNRE.MatchString(ad.ImageURN) {
			return fmt.Errorf("%w: text ad %d image URN is malformed: expected an Images-API urn:li:image:<id>", ErrFormatInvalid, n)
		}
	}
	return nil
}

// ValidateTextAdImage reports whether in can be uploaded as a Text Ad image: PNG or JPEG, at
// most MaxTextAdImageBytes, exactly 100x100. It decodes only the header and never quotes bytes.
func ValidateTextAdImage(in ImageUpload) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the image has no bytes", ErrImageInvalid)
	}
	if len(in.Data) > MaxTextAdImageBytes {
		return fmt.Errorf("%w: the image is %d bytes; LinkedIn text ads accept at most %d", ErrImageInvalid, len(in.Data), MaxTextAdImageBytes)
	}
	var wantFormat string
	switch in.MimeType {
	case "image/png":
		wantFormat = "png"
	case "image/jpeg":
		wantFormat = "jpeg"
	default:
		return fmt.Errorf("%w: only PNG and JPEG images are supported", ErrImageInvalid)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(in.Data))
	if err != nil {
		return fmt.Errorf("%w: the image header could not be decoded", ErrImageInvalid)
	}
	if format != wantFormat {
		return fmt.Errorf("%w: the image bytes are %s, not the recorded %s", ErrImageInvalid, domain.CallerValue(format), domain.MIMEValue(in.MimeType))
	}
	if cfg.Width != textAdImageSide || cfg.Height != textAdImageSide {
		return fmt.Errorf("%w: the image is %dx%d; LinkedIn text ads require %dx%d", ErrImageInvalid, cfg.Width, cfg.Height, textAdImageSide, textAdImageSide)
	}
	return nil
}

// ValidateImageForFormat applies the image rules of format: ValidateTextAdImage for FormatText,
// ValidateImage otherwise.
func ValidateImageForFormat(format string, in ImageUpload) error {
	if format == FormatText {
		return ValidateTextAdImage(in)
	}
	return ValidateImage(in)
}

// MaxImageBytesForFormat is the stored-asset size ceiling of format.
func MaxImageBytesForFormat(format string) int64 {
	if format == FormatText {
		return MaxTextAdImageBytes
	}
	return MaxImageBytes
}

// campaignTypeFor is the LinkedIn campaign type of a normalised format.
func campaignTypeFor(format string) string {
	if format == FormatText {
		return campaignTypeTextAd
	}
	return campaignTypeSponsoredUpdates
}

// adFormatNameSegment is the campaign name's Ad Format segment (the sixth). "Static" is every
// single-image campaign's, byte for byte as before; "Text" keeps a text campaign's name — and so
// the by-name reuse lookup — apart from a single-image campaign of the same event and objective.
func adFormatNameSegment(format string) string {
	if format == FormatText {
		return "Text"
	}
	return "Static"
}

// createTextAdCreative creates a DRAFT Text Ad creative and returns its URN. Like
// createCreative it has no name-based reconciliation lookup, and it reports a malformed 2xx as
// a transportError (may exist). The headline and description were checked by ValidateTextAds
// against the same normalisation applied here.
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to have passed
// resolveAccountID in CreateCampaign.
func (c *Client) createTextAdCreative(ctx context.Context, accountID, campaignID string, ad TextAd, landingPage, imageURN, adName string) (string, error) {
	textAd := map[string]any{
		"headline":    normalizeCreativeText(ad.Headline),
		"description": normalizeCreativeText(ad.Description),
		"landingPage": landingPage,
	}
	if imageURN != "" {
		textAd["image"] = imageURN
	}
	body := map[string]any{
		"campaign":       "urn:li:sponsoredCampaign:" + campaignID,
		"intendedStatus": "DRAFT",
		"content":        map[string]any{"textAd": textAd},
	}
	if adName != "" {
		if len([]rune(adName)) > 255 {
			adName = truncateRunes(adName, 255)
		}
		body["name"] = adName
	}
	creativesPath := fmt.Sprintf("adAccounts/%s/creatives", accountID)
	resp, err := c.doRequest(ctx, http.MethodPost, creativesPath, body, nil, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", &transportError{Method: http.MethodPost, Path: creativesPath, Err: fmt.Errorf("text ad creative creation returned no ID")}
	}
	if trailingID(resp.ID.String()) == "" {
		return "", &transportError{Method: http.MethodPost, Path: creativesPath, Err: fmt.Errorf("text ad creative creation returned an ID %q with an empty trailing segment", resp.ID.String())}
	}
	return resp.ID.String(), nil
}

// createTextAds creates one DRAFT Text Ad creative per in.TextAds entry under campaignID and
// returns how many were created, the Steps, and the first failure. NOT idempotent, exactly as
// the single-image loop: a creative has no name-based lookup and LinkedIn no upsert, so the
// error tells the caller not to retry blindly. An ambiguous failure (transport/timeout after
// send, a 5xx, an undecodable 2xx, an in-flight cancellation) is reported UNCONFIRMED, since
// the creative may exist. There is no dark post, so nothing else can be orphaned.
//
// Each ad's landing page is the registration URL with that ad's UTM parameters, the same
// BuildUTMURL a single-image variant's article link gets, numbered from 1.
func (c *Client) createTextAds(ctx context.Context, accountID, campaignID, campaignName, eventName, reg string, in CampaignInput, imageURNs map[string]string) (int, []string, error) {
	var steps []string
	created := 0
	for i, ad := range in.TextAds {
		landing := BuildUTMURL(reg, in.HSToken, campaignName, i+1)
		imageURN := ad.ImageURN
		if ad.ImageAssetID != "" {
			imageURN = imageURNs[ad.ImageAssetID]
		}
		adName := fmt.Sprintf("%s | text-%d", eventName, i+1)
		id, err := c.createTextAdCreative(ctx, accountID, campaignID, ad, landing, imageURN, adName)
		if err != nil {
			if createOutcomeAmbiguous(err) {
				steps = append(steps, fmt.Sprintf("Text ad %d outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating (a creative has no idempotency lookup)", i+1))
				return created, steps, fmt.Errorf("text ad %d creative UNCONFIRMED after %d of %d text ad(s) created: %w; the creative MAY have been created and has no idempotency lookup, so do NOT blindly retry; inspect the returned partial result", i+1, created, len(in.TextAds), err)
			}
			return created, steps, fmt.Errorf("text ad %d creative failed after %d of %d text ad(s) created: %w; do NOT blindly retry (would duplicate the %d created creative(s)); inspect the returned partial result", i+1, created, len(in.TextAds), err, created)
		}
		steps = append(steps, fmt.Sprintf("Text ad creative (DRAFT): %s", id))
		created++
	}
	return created, steps, nil
}
