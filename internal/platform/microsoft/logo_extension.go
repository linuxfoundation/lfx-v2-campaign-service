// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// Campaign logo extension at create (#563, gap 13).
//
// A Search campaign created here may carry one campaign-level LogoAdExtension built from a stored
// PNG/JPEG creative asset of the brief. The transport is the image extension's
// (image_extension.go), one item per call, classified definite or UNCONFIRMED by the same rules:
//
//  1. AdExtensionsAssociations/Query filtered to LogoAdExtension on this campaign. "You can
//     associate your logo extensions at the account, campaign, or ad group levels, but only one
//     extension is allowed per level", so a campaign that already has one (a retry that reused
//     it, or a human) gets nothing uploaded or added. A lookup that cannot answer adds nothing.
//     https://learn.microsoft.com/en-us/advertising/guides/ad-extensions?view=bingads-13
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/adextensionstypefilter?view=bingads-13
//  2. AddMedia — POST Media with one Image {MediaType "Image1x1", Data base64}. Image1x1 is the
//     Image sub type whose shape is the logo's: "1:1", minimum "128 width x 128 height".
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/image?view=bingads-13
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/addmedia?view=bingads-13
//  3. AddAdExtensions — POST AdExtensions with one
//     {Type "LogoAdExtension", BusinessLogo <media id>, BusinessName}. No DomainName is sent.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/logoadextension?view=bingads-13
//  4. SetAdExtensionsAssociations — POST AdExtensionsAssociations/Set, AssociationType "Campaign".
//
// Limits, all checked before any request:
//   - BusinessLogo: "must have a 1:1 aspect ratio. The minimum image size is 128px by 128px. The
//     maximum image size is 5120 KB." The upload path is tighter on bytes: Image.Data "can contain
//     a maximum of 102,400 characters" of base64, so at most MaxImageBytes (76,800) decoded bytes,
//     the bound the image extension already applies. The largest side is 2048: "The maximum width
//     and height in pixels are 2592 and 2048 independently", and a square is bounded by the
//     smaller of the two.
//   - Format: PNG or JPEG only, the stored creative asset types, decoded from the header and
//     matched to the recorded MIME type. Anything else, video/mp4 included, is refused.
//   - BusinessName: required, "Maximum length is 25 characters".
//
// Not verified live: that LogoAdExtension.BusinessLogo takes an AddMedia id of an Image1x1, and
// that Microsoft serves the campaign-level logo over its automatic account-level one.
// ---------------------------------------------------------------------------

const (
	adExtensionTypeLogo = "LogoAdExtension"
	mediaTypeImage1x1   = "Image1x1"

	// FieldLogoAssetID / FieldLogoBusinessName are the microsoftConfig keys, named in refusals.
	FieldLogoAssetID      = "logoAssetId"
	FieldLogoBusinessName = "logoBusinessName"

	// MinLogoSide / MaxLogoSide bound the square logo's side in pixels.
	MinLogoSide = 128
	MaxLogoSide = 2048
	// MaxLogoBusinessNameRunes is LogoAdExtension.BusinessName's limit.
	MaxLogoBusinessNameRunes = 25
)

// ErrLogoInvalid marks a logo this client will not send: empty, too large, not a PNG/JPEG,
// undecodable, not square, outside the side bounds, or with an unusable business name. Detected
// before any request, so nothing was uploaded or attached.
var ErrLogoInvalid = errors.New("microsoft-ads: image is not acceptable for a logo extension")

// LogoInput is the logo to attach to a campaign as a LogoAdExtension.
type LogoInput struct {
	// Data is the DECODED image bytes (the client base64-encodes them for AddMedia).
	Data []byte
	// MimeType is the verified type of Data: image/png or image/jpeg.
	MimeType string
	// BusinessName is the extension's BusinessName: required, at most 25 characters.
	BusinessName string
}

// LogoAttachResult reports what AttachCampaignLogo did. Ids are reported even alongside an error,
// so a partially-built extension can be found.
type LogoAttachResult struct {
	// MediaID is the account-library media id AddMedia returned ("" when nothing was uploaded).
	MediaID string
	// AdExtensionID is the logo extension this run created, or the one already associated.
	AdExtensionID string
	// AlreadyAttached is true when the campaign already had a logo extension, so this run
	// uploaded and created nothing.
	AlreadyAttached bool
	// Steps is a human-readable trail (ids and fixed text, never upstream message text).
	Steps []string
}

// ValidateLogoBusinessName trims s and checks it is a usable BusinessName: non-blank, valid UTF-8,
// at most 25 characters, no control or format character. The value is never echoed.
func ValidateLogoBusinessName(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: the business name is not valid UTF-8", ErrLogoInvalid)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("%w: the business name contains a control or format character", ErrLogoInvalid)
		}
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return "", fmt.Errorf("%w: the business name is required", ErrLogoInvalid)
	}
	if n := utf8.RuneCountInString(t); n > MaxLogoBusinessNameRunes {
		return "", fmt.Errorf("%w: the business name is %d characters; the limit is %d", ErrLogoInvalid, n, MaxLogoBusinessNameRunes)
	}
	return t, nil
}

// ValidateLogo reports whether in can be sent as a LogoAdExtension. It decodes only the image
// HEADER, after the byte bound, and quotes neither the bytes nor the business name.
func ValidateLogo(in LogoInput) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the image has no bytes", ErrLogoInvalid)
	}
	if len(in.Data) > MaxImageBytes {
		return fmt.Errorf("%w: the image is %d bytes; Microsoft Advertising accepts at most %d", ErrLogoInvalid, len(in.Data), MaxImageBytes)
	}
	var wantFormat string
	switch in.MimeType {
	case mimeTypeImagePNG:
		wantFormat = imageFormatPNG
	case mimeTypeImageJPEG:
		wantFormat = imageFormatJPEG
	default:
		return fmt.Errorf("%w: only PNG and JPEG images are supported, not %s", ErrLogoInvalid, domain.MIMEValue(in.MimeType))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(in.Data))
	if err != nil {
		return fmt.Errorf("%w: the image header could not be decoded", ErrLogoInvalid)
	}
	if format != wantFormat {
		return fmt.Errorf("%w: the image bytes are %s, not the recorded %s", ErrLogoInvalid, domain.CallerValue(format), domain.MIMEValue(in.MimeType))
	}
	if cfg.Width != cfg.Height {
		return fmt.Errorf("%w: the image is %dx%d; a logo must be square (1:1)", ErrLogoInvalid, cfg.Width, cfg.Height)
	}
	if cfg.Width < MinLogoSide {
		return fmt.Errorf("%w: the image is %dx%d; a logo must be at least %dx%d", ErrLogoInvalid, cfg.Width, cfg.Height, MinLogoSide, MinLogoSide)
	}
	if cfg.Width > MaxLogoSide {
		return fmt.Errorf("%w: the image is %dx%d; a logo may be at most %dx%d", ErrLogoInvalid, cfg.Width, cfg.Height, MaxLogoSide, MaxLogoSide)
	}
	_, err = ValidateLogoBusinessName(in.BusinessName)
	return err
}

// ---- wire shapes ------------------------------------------------------------

type msLogoAdExtension struct {
	Type         string      `json:"Type"`
	BusinessLogo json.Number `json:"BusinessLogo"`
	BusinessName string      `json:"BusinessName"`
}

type addLogoAdExtensionsRequest struct {
	AccountId    json.Number         `json:"AccountId"`
	AdExtensions []msLogoAdExtension `json:"AdExtensions"`
}

// ---- the operation ----------------------------------------------------------

// AttachCampaignLogo uploads in to the account's media library and attaches it to the campaign as
// a campaign-level LogoAdExtension, unless the campaign already has one.
//
// The result is never nil. A nil error means the campaign now has a logo extension (created here,
// or pre-existing when AlreadyAttached). A non-nil error means it does not, or may not:
// IsOutcomeUnconfirmed(err) tells "may have been attached" from a definite failure.
func (c *Client) AttachCampaignLogo(ctx context.Context, campaignID string, in LogoInput) (*LogoAttachResult, error) {
	r := &LogoAttachResult{}
	if err := c.validateAccountIDs(); err != nil {
		return r, err
	}
	if !idRE.MatchString(campaignID) {
		return r, fmt.Errorf("microsoft-ads logo extension: campaign id %q is not a Microsoft id", clipID(campaignID))
	}
	// Re-validated here, not only by the dispatcher, so no caller reaches AddMedia with a logo
	// Microsoft would refuse.
	if err := ValidateLogo(in); err != nil {
		return r, err
	}
	data := base64.StdEncoding.EncodeToString(in.Data)
	if len(data) > maxImageBase64Chars {
		return r, fmt.Errorf("%w: the encoded image is %d characters; the limit is %d", ErrLogoInvalid, len(data), maxImageBase64Chars)
	}
	name, _ := ValidateLogoBusinessName(in.BusinessName)
	account := json.Number(c.account.AccountID)
	campaign := json.Number(campaignID)

	// 1. Find-first: one logo per campaign, and AddMedia is not content-addressed. A failed read
	// refuses rather than uploads; only a read was sent, so the failure is definite (%v, not %w,
	// for the reason AttachCampaignImage gives).
	existing, _, err := c.findCampaignExtension(ctx, account, campaign, adExtensionTypeLogo, "a logo")
	if err != nil {
		return r, fmt.Errorf("microsoft-ads logo extension: could not check the campaign for an existing logo extension, so none was added: %v", err)
	}
	if existing != "" {
		r.AdExtensionID = existing
		r.AlreadyAttached = true
		r.Steps = append(r.Steps, fmt.Sprintf("Logo extension already associated with the campaign: %s (nothing uploaded)", existing))
		return r, nil
	}

	// 2. AddMedia. Not idempotent, so a 429 is not retried.
	body, err := c.doRequest(ctx, http.MethodPost, "Media", addMediaRequest{
		AccountId: account,
		Media:     []msImageMedia{{Type: "Image", MediaType: mediaTypeImage1x1, Data: data}},
	}, false)
	if err == nil {
		r.MediaID, err = mediaIdentity(body)
	}
	if err != nil {
		return r, classifyImageStep("microsoft-ads logo extension: logo upload", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Logo uploaded to the account media library: %s", r.MediaID))

	// 3. AddAdExtensions.
	body, err = c.doRequest(ctx, http.MethodPost, "AdExtensions", addLogoAdExtensionsRequest{
		AccountId: account,
		AdExtensions: []msLogoAdExtension{{
			Type:         adExtensionTypeLogo,
			BusinessLogo: json.Number(r.MediaID),
			BusinessName: name,
		}},
	}, false)
	if err == nil {
		r.AdExtensionID, err = adExtensionIdentity(body)
	}
	if err != nil {
		return r, classifyImageStep("microsoft-ads logo extension: extension create (the uploaded media is unused)", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Logo extension created: %s", r.AdExtensionID))

	// 4. Associate with the campaign.
	if err := c.associateCampaignExtension(ctx, account, campaign, r.AdExtensionID); err != nil {
		return r, classifyImageStep("microsoft-ads logo extension: campaign association", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Logo extension associated with campaign %s", campaignID))
	return r, nil
}
