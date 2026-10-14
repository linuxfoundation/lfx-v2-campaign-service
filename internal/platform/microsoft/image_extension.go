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
	// Registered for image.DecodeConfig: the only two formats a stored creative asset can be
	// (creative_assets.mime_type CHECK, migration 000028).
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Campaign image extension (LFXV2-2665): attach a stored creative image to the
// Search campaign this client creates.
//
// WHY AN IMAGE AD EXTENSION, AND NOTHING WIDER. This client creates exactly one
// campaign type — a PAUSED Search campaign with a ResponsiveSearchAd — and a
// ResponsiveSearchAd carries text assets only. The two Microsoft surfaces that take an
// uploaded image are the ResponsiveAd (an Audience-campaign ad type this service does not
// create) and the ImageAdExtension, which "you can associate ... with the account or with
// campaigns and ad groups in the account" and which renders with Search ads. The extension
// is therefore the one image surface reachable WITHOUT inventing a new campaign type, and it
// is associated at the CAMPAIGN level so it applies to exactly the campaign this dispatch
// created and to nothing else in the account.
//
// THE CALL SEQUENCE, per the v13 Campaign Management reference:
//
//  1. GetAdExtensionsAssociations — POST AdExtensionsAssociations/Query, filtered to
//     ImageAdExtension on this campaign. An image extension ALREADY associated means a
//     previous run (or a human) attached one, so nothing is uploaded — this is what stops a
//     retry that reuses the campaign from uploading the image a second time.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/getadextensionsassociations?view=bingads-13
//  2. AddMedia — POST Media with one Image {MediaType "GenericImage", Data base64}. Returns
//     the account-scoped MediaId. Media is account-library scoped and is not
//     content-addressed: every AddMedia creates a new MediaId, which is why step 1 exists.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/addmedia?view=bingads-13
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/image?view=bingads-13
//  3. AddAdExtensions — POST AdExtensions with one ImageAdExtension {AlternativeText,
//     ImageMediaIds [MediaId], Layouts ["SearchSingle"]}. The reference documents that an
//     extension submitted with media ids and no Images is converted server-side into the
//     equivalent Images AssetLinks with the Single layout, so no ImageAsset SubType has to be
//     guessed. No DestinationUrl/FinalUrls is sent: "If the URL is not specified for the image
//     ad extension, the URL of the ad is used", which keeps the ad's UTM-tagged destination
//     and puts no new URL on the wire.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/imageadextension?view=bingads-13
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/addadextensions?view=bingads-13
//  4. SetAdExtensionsAssociations — POST AdExtensionsAssociations/Set, AssociationType
//     "Campaign".
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/setadextensionsassociations?view=bingads-13
//
// OUTCOMES. Every failure is classified the way the rest of this client classifies a
// mutation: a definite rejection (a 4xx, a PartialError explaining a null id slot, or a
// failure before any mutating call was sent) means the image is NOT attached; an ambiguous
// one (transport failure, 5xx, mutating 429, or a 2xx that carried no usable id) is
// UNCONFIRMED and IsOutcomeUnconfirmed reports true for it. Upstream message text never
// reaches an error: apiError renders method/path/status only, and PartialErrors are
// rendered as codes only.
//
// A failure after AddMedia leaves an UNUSED media entry in the account library, and one
// after AddAdExtensions leaves an unassociated extension. Both are inert — neither serves
// nor spends — and the result reports their ids so they can be found.
// ---------------------------------------------------------------------------

const (
	// MaxImageBytes is the largest DECODED image Microsoft accepts. The Image.Data element
	// "can contain a maximum of 102,400 characters" of base64, and base64 of n bytes is
	// 4*ceil(n/3) characters, so 76,800 bytes encodes to exactly 102,400 characters.
	// Exported so the dispatcher can refuse an oversized stored asset on its byte_size,
	// before the blob is loaded.
	MaxImageBytes = 76800
	// maxImageBase64Chars is the documented Image.Data ceiling itself, checked on the
	// encoded string as well so the bound holds on the unit Microsoft measures.
	maxImageBase64Chars = 102400

	// GenericImage bounds: "40 width x 20 height" minimum, and "The maximum width and height
	// in pixels are 2592 and 2048 independently". GenericImage is the media type the Image
	// reference recommends and lists as supported for ImageAdExtension; its aspect ratio
	// "Varies", so no ratio is enforced here.
	minImageWidth  = 40
	minImageHeight = 20
	maxImageWidth  = 2592
	maxImageHeight = 2048

	// maxImageAltTextRunes is ImageAdExtension.AlternativeText's limit ("The maximum length
	// for this element is 35 characters"; Add: Required).
	maxImageAltTextRunes = 35

	mediaTypeGenericImage   = "GenericImage"
	adExtensionTypeImage    = "ImageAdExtension"
	associationTypeCampaign = "Campaign"
	imageLayoutSearchSingle = "SearchSingle"
	mimeTypeImagePNG        = "image/png"
	mimeTypeImageJPEG       = "image/jpeg"
	imageFormatPNG          = "png"
	imageFormatJPEG         = "jpeg"
)

// ErrImageInvalid marks an image this client will not send to Microsoft: empty, too large,
// not a PNG/JPEG, undecodable, outside the GenericImage dimension bounds, or carrying an
// unusable alternative text. Detected before any request, so nothing was attached.
var ErrImageInvalid = errors.New("microsoft-ads: image is not acceptable for an image ad extension")

// ImageInput is one image to attach to a campaign as an ImageAdExtension.
type ImageInput struct {
	// Data is the DECODED image bytes (the client base64-encodes them for AddMedia).
	Data []byte
	// MimeType is the verified type of Data: image/png or image/jpeg.
	MimeType string
	// AltText is the extension's AlternativeText: required, at most 35 characters.
	AltText string
}

// ImageAttachResult reports what AttachCampaignImage did. Ids are reported even alongside
// an error, so a partially-built extension can be found and cleaned up.
type ImageAttachResult struct {
	// MediaID is the account-library media id AddMedia returned ("" when nothing was
	// uploaded — including when an existing extension made the upload unnecessary).
	MediaID string
	// AdExtensionID is the image extension now associated with the campaign: the one this
	// run created, or the pre-existing one found by the lookup.
	AdExtensionID string
	// AlreadyAttached is true when the campaign already had an image extension associated,
	// so this run uploaded and created nothing.
	AlreadyAttached bool
	// Steps is a human-readable trail in this client's Steps vocabulary (ids and fixed text,
	// never upstream message text).
	Steps []string
}

// imageUnconfirmedError marks an attach step whose outcome is AMBIGUOUS — the mutation may
// have committed upstream. It answers Unconfirmed() so IsOutcomeUnconfirmed reports it.
type imageUnconfirmedError struct{ err error }

func (e *imageUnconfirmedError) Error() string     { return e.err.Error() }
func (e *imageUnconfirmedError) Unwrap() error     { return e.err }
func (e *imageUnconfirmedError) Unconfirmed() bool { return true }

// classifyImageStep wraps a failed mutating step's error so the caller can tell a
// definite rejection from an ambiguous outcome. errNoID (a 2xx that carried no usable id
// and no explaining PartialError) is ambiguous, as everywhere else in this client.
func classifyImageStep(what string, err error) error {
	wrapped := fmt.Errorf("%s: %w", what, err)
	if createOutcomeAmbiguous(err) || errors.Is(err, errNoID) {
		return &imageUnconfirmedError{err: wrapped}
	}
	return wrapped
}

// ValidateImage reports whether in can be sent as an ImageAdExtension image. It decodes
// only the image HEADER (image.DecodeConfig), never the pixels, and is cheap because the
// size check runs first and bounds the input to MaxImageBytes. Neither the bytes nor the
// alternative text are quoted in the error.
func ValidateImage(in ImageInput) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the image has no bytes", ErrImageInvalid)
	}
	if len(in.Data) > MaxImageBytes {
		return fmt.Errorf("%w: the image is %d bytes; Microsoft Advertising accepts at most %d", ErrImageInvalid, len(in.Data), MaxImageBytes)
	}
	var wantFormat string
	switch in.MimeType {
	case mimeTypeImagePNG:
		wantFormat = imageFormatPNG
	case mimeTypeImageJPEG:
		wantFormat = imageFormatJPEG
	default:
		return fmt.Errorf("%w: only PNG and JPEG images are supported", ErrImageInvalid)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(in.Data))
	if err != nil {
		return fmt.Errorf("%w: the image header could not be decoded", ErrImageInvalid)
	}
	if format != wantFormat {
		return fmt.Errorf("%w: the image bytes are %s, not the recorded %s", ErrImageInvalid, format, in.MimeType)
	}
	if cfg.Width < minImageWidth || cfg.Height < minImageHeight {
		return fmt.Errorf("%w: the image is %dx%d; Microsoft Advertising requires at least %dx%d", ErrImageInvalid, cfg.Width, cfg.Height, minImageWidth, minImageHeight)
	}
	if cfg.Width > maxImageWidth || cfg.Height > maxImageHeight {
		return fmt.Errorf("%w: the image is %dx%d; Microsoft Advertising accepts at most %dx%d", ErrImageInvalid, cfg.Width, cfg.Height, maxImageWidth, maxImageHeight)
	}
	return validateImageAltText(in.AltText)
}

// validateImageAltText checks AlternativeText: non-blank, at most 35 characters, valid
// UTF-8 with no control or format characters. The value itself is never echoed.
func validateImageAltText(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return fmt.Errorf("%w: the image alternative text is required", ErrImageInvalid)
	}
	if !utf8.ValidString(t) {
		return fmt.Errorf("%w: the image alternative text is not valid UTF-8", ErrImageInvalid)
	}
	if n := utf8.RuneCountInString(t); n > maxImageAltTextRunes {
		return fmt.Errorf("%w: the image alternative text is %d characters; the limit is %d", ErrImageInvalid, n, maxImageAltTextRunes)
	}
	for _, r := range t {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: the image alternative text contains a control or format character", ErrImageInvalid)
		}
	}
	return nil
}

// ImageAltTextFromName derives a default AlternativeText from a display name: trimmed and
// cut to the 35-character limit on a rune boundary.
func ImageAltTextFromName(name string) string {
	return strings.TrimSpace(truncateRunes(strings.TrimSpace(name), maxImageAltTextRunes))
}

// ---- wire shapes ------------------------------------------------------------

type adExtensionAssociationsQuery struct {
	AccountId       json.Number   `json:"AccountId"`
	AdExtensionType string        `json:"AdExtensionType"`
	AssociationType string        `json:"AssociationType"`
	EntityIds       []json.Number `json:"EntityIds"`
}

// adExtensionAssociationsQueryResponse is the (subset of the) GetAdExtensionsAssociations 200
// body. Each association's EntityId and AssociationType are decoded, not just its extension,
// because the lookup's answer is "THIS campaign already has an image extension" — an extension
// the response attributes to some other entity, or to no entity, proves nothing about this one.
//
// The collection field's PRESENCE is tracked (sawCollection) for the same reason
// updateStatusResponse tracks PartialErrors: `{}` and `{"AdExtensionAssociationCollection":[]}`
// both decode to an empty slice, but only the second answered the question. An unanswered lookup
// read as "none" is how a retry uploads a second image.
type adExtensionAssociationsQueryResponse struct {
	AdExtensionAssociationCollection []*struct {
		AdExtensionAssociations []*struct {
			AdExtension *struct {
				Id   *json.Number `json:"Id"`
				Type string       `json:"Type"`
			} `json:"AdExtension"`
			AssociationType string       `json:"AssociationType"`
			EntityId        *json.Number `json:"EntityId"`
		} `json:"AdExtensionAssociations"`
	} `json:"AdExtensionAssociationCollection"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
	sawCollection bool
}

// UnmarshalJSON records whether AdExtensionAssociationCollection was present as an ARRAY (an
// empty array and an array of null slots are Microsoft's documented "none"; an omitted field or
// a literal null answered nothing), then decodes normally.
func (r *adExtensionAssociationsQueryResponse) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	type alias adExtensionAssociationsQueryResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = adExtensionAssociationsQueryResponse(decoded)
	raw, ok := probe["AdExtensionAssociationCollection"]
	r.sawCollection = ok && len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '['
	return nil
}

type msImageMedia struct {
	Type      string `json:"Type"`
	MediaType string `json:"MediaType"`
	Data      string `json:"Data"`
}

type addMediaRequest struct {
	AccountId json.Number    `json:"AccountId"`
	Media     []msImageMedia `json:"Media"`
}

// addMediaResponse decodes PartialErrors beside MediaIds: a null id slot that a PartialError
// explains is a DEFINITE rejection, and only an unexplained one is errNoID/UNCONFIRMED.
type addMediaResponse struct {
	MediaIds      boundedNumberIDs  `json:"MediaIds"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

type msImageAdExtension struct {
	Type            string        `json:"Type"`
	AlternativeText string        `json:"AlternativeText"`
	ImageMediaIds   []json.Number `json:"ImageMediaIds"`
	Layouts         []string      `json:"Layouts"`
}

type addAdExtensionsRequest struct {
	AccountId    json.Number          `json:"AccountId"`
	AdExtensions []msImageAdExtension `json:"AdExtensions"`
}

type addAdExtensionsResponse struct {
	AdExtensionIdentities []*struct {
		Id *json.Number `json:"Id"`
	} `json:"AdExtensionIdentities"`
	// NestedPartialErrors is BatchErrorCollection[]: a failure can sit at the collection level
	// OR inside a collection's own BatchErrors, so the nested decoder geo.go uses is used here
	// too, and both levels are classified.
	NestedPartialErrors boundedNestedErrorItems `json:"NestedPartialErrors"`
}

type adExtensionIDToEntityID struct {
	AdExtensionId json.Number `json:"AdExtensionId"`
	EntityId      json.Number `json:"EntityId"`
}

type setAdExtensionsAssociationsRequest struct {
	AccountId                           json.Number               `json:"AccountId"`
	AdExtensionIdToEntityIdAssociations []adExtensionIDToEntityID `json:"AdExtensionIdToEntityIdAssociations"`
	AssociationType                     string                    `json:"AssociationType"`
}

// setAdExtensionsAssociationsResponse tracks PartialErrors' PRESENCE exactly as
// updateStatusResponse does: `{"PartialErrors":null}` or `[]` is Microsoft affirming no item
// failed, while `{}` or a top-level null never answered, and must not read as a confirmed
// association.
type setAdExtensionsAssociationsResponse struct {
	PartialErrors    boundedErrorItems `json:"PartialErrors"`
	sawPartialErrors bool
}

func (r *setAdExtensionsAssociationsResponse) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if _, ok := probe["PartialErrors"]; !ok {
		return nil
	}
	type alias setAdExtensionsAssociationsResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	r.PartialErrors = decoded.PartialErrors
	r.sawPartialErrors = true
	return nil
}

// ---- the operation ----------------------------------------------------------

// AttachCampaignImage uploads in to the account's media library and attaches it to the
// campaign as a campaign-level ImageAdExtension, unless the campaign already has one.
//
// The returned result is never nil. A nil error means the campaign now has an image
// extension (created here, or pre-existing when AlreadyAttached). A non-nil error means it
// does not, or may not: IsOutcomeUnconfirmed(err) distinguishes "may have been attached —
// verify before re-attaching" from a definite failure.
func (c *Client) AttachCampaignImage(ctx context.Context, campaignID string, in ImageInput) (*ImageAttachResult, error) {
	r := &ImageAttachResult{}
	if err := c.validateAccountIDs(); err != nil {
		return r, err
	}
	if !idRE.MatchString(campaignID) {
		return r, fmt.Errorf("microsoft-ads image extension: campaign id %q is not a Microsoft id", clipID(campaignID))
	}
	// Re-validated here, not only by the dispatcher, so no caller can reach AddMedia with an
	// image Microsoft would refuse — or one whose base64 exceeds the documented ceiling.
	if err := ValidateImage(in); err != nil {
		return r, err
	}
	data := base64.StdEncoding.EncodeToString(in.Data)
	if len(data) > maxImageBase64Chars {
		return r, fmt.Errorf("%w: the encoded image is %d characters; the limit is %d", ErrImageInvalid, len(data), maxImageBase64Chars)
	}
	account := json.Number(c.account.AccountID)
	campaign := json.Number(campaignID)

	// 1. Find-first. A failed lookup refuses rather than uploads: without it this run cannot
	// tell a first attach from a retry, and AddMedia is not content-addressed. Nothing has
	// been mutated, so the failure is definite.
	//
	// The cause is rendered with %v, NOT wrapped: a read's 5xx/transport error is an apiError or
	// transportError, which IsOutcomeUnconfirmed would report as ambiguous — true of a mutation,
	// false here, where the only call sent was a read and nothing can have been attached.
	existing, err := c.findCampaignImageExtension(ctx, account, campaign)
	if err != nil {
		return r, fmt.Errorf("microsoft-ads image extension: could not check the campaign for an existing image extension, so none was added: %v", err)
	}
	if existing != "" {
		r.AdExtensionID = existing
		r.AlreadyAttached = true
		r.Steps = append(r.Steps, fmt.Sprintf("Image extension already associated with the campaign: %s (nothing uploaded)", existing))
		return r, nil
	}

	// 2. AddMedia. Not idempotent (each call creates a new MediaId), so a 429 is not retried.
	body, err := c.doRequest(ctx, http.MethodPost, "Media", addMediaRequest{
		AccountId: account,
		Media:     []msImageMedia{{Type: "Image", MediaType: mediaTypeGenericImage, Data: data}},
	}, false)
	if err == nil {
		r.MediaID, err = mediaIdentity(body)
	}
	if err != nil {
		return r, classifyImageStep("microsoft-ads image extension: image upload", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Image uploaded to the account media library: %s", r.MediaID))

	// 3. AddAdExtensions.
	body, err = c.doRequest(ctx, http.MethodPost, "AdExtensions", addAdExtensionsRequest{
		AccountId: account,
		AdExtensions: []msImageAdExtension{{
			Type:            adExtensionTypeImage,
			AlternativeText: strings.TrimSpace(in.AltText),
			ImageMediaIds:   []json.Number{json.Number(r.MediaID)},
			Layouts:         []string{imageLayoutSearchSingle},
		}},
	}, false)
	if err == nil {
		r.AdExtensionID, err = adExtensionIdentity(body)
	}
	if err != nil {
		return r, classifyImageStep("microsoft-ads image extension: extension create (the uploaded media is unused)", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Image extension created: %s", r.AdExtensionID))

	// 4. Associate with the campaign.
	body, err = c.doRequest(ctx, http.MethodPost, "AdExtensionsAssociations/Set", setAdExtensionsAssociationsRequest{
		AccountId:                           account,
		AdExtensionIdToEntityIdAssociations: []adExtensionIDToEntityID{{AdExtensionId: json.Number(r.AdExtensionID), EntityId: campaign}},
		AssociationType:                     associationTypeCampaign,
	}, false)
	if err == nil {
		var resp setAdExtensionsAssociationsResponse
		switch uerr := decodeTrusted(body, &resp); {
		case uerr != nil:
			// A 2xx we cannot read: the association may have committed.
			err = fmt.Errorf("decode AdExtensionsAssociations/Set response (%v): %w", uerr, errNoID)
		case !resp.sawPartialErrors:
			// A 2xx that never said whether the item failed: not a confirmation.
			err = fmt.Errorf("AdExtensionsAssociations/Set response omitted PartialErrors: %w", errNoID)
		case resp.PartialErrors.AnyErrors:
			err = fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(resp.PartialErrors.Items))
		case len(resp.PartialErrors.Items) > 0:
			// Present but malformed (`[null]`, `[{}]`): entries that carry no code neither confirm
			// nor reject, so it is not the empty-error success — mirrors putUpdateOutcome.
			err = fmt.Errorf("AdExtensionsAssociations/Set PartialErrors present but carried no error codes: %w", errNoID)
		}
	}
	if err != nil {
		return r, classifyImageStep("microsoft-ads image extension: campaign association", err)
	}
	r.Steps = append(r.Steps, fmt.Sprintf("Image extension associated with campaign %s", campaignID))
	return r, nil
}

// findCampaignImageExtension returns the id of an ImageAdExtension already associated with
// the campaign, or "" when there is none. A read (idempotent). Any association whose
// extension id is unusable, or a PartialError on the read, is an error rather than
// "absent": treating it as absent would upload a second image the lookup could not rule out.
//
// An association is accepted only when it names THIS campaign at Campaign level (EntityId ==
// campaign, AssociationType == "Campaign"). Anything else fails the lookup — the caller then
// refuses to upload — rather than being skipped or accepted: the request asked about exactly one
// campaign, so a response that answers about something else is not an answer to trust either way.
func (c *Client) findCampaignImageExtension(ctx context.Context, account, campaign json.Number) (string, error) {
	body, err := c.doRequest(ctx, http.MethodPost, "AdExtensionsAssociations/Query", adExtensionAssociationsQuery{
		AccountId:       account,
		AdExtensionType: adExtensionTypeImage,
		AssociationType: associationTypeCampaign,
		EntityIds:       []json.Number{campaign},
	}, true)
	if err != nil {
		return "", err
	}
	var resp adExtensionAssociationsQueryResponse
	if uerr := decodeTrusted(body, &resp); uerr != nil {
		return "", fmt.Errorf("decode AdExtensionsAssociations/Query response (%v): %w", uerr, errNoID)
	}
	if resp.PartialErrors.AnyErrors {
		return "", fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(resp.PartialErrors.Items))
	}
	if !resp.sawCollection {
		return "", fmt.Errorf("AdExtensionsAssociations/Query response omitted AdExtensionAssociationCollection: %w", errNoID)
	}
	// EVERY image association is validated before any is trusted: returning at the first valid
	// one would leave a later misattributed or unusable entry unchecked, and a mixed response
	// would then read as AlreadyAttached. The first valid id is kept and returned only once the
	// whole response has passed.
	found := ""
	for _, coll := range resp.AdExtensionAssociationCollection {
		if coll == nil {
			continue
		}
		for _, a := range coll.AdExtensionAssociations {
			if a == nil || a.AdExtension == nil {
				continue
			}
			// Filtered to ImageAdExtension by the request; an explicit other Type is skipped
			// defensively, while a missing Type is trusted to the filter.
			if a.AdExtension.Type != "" && a.AdExtension.Type != adExtensionTypeImage {
				continue
			}
			if a.AssociationType != associationTypeCampaign || numberID(a.EntityId) != campaign.String() {
				return "", fmt.Errorf("an image extension association was returned for an entity other than campaign %s: %w", campaign, errNoID)
			}
			id := numberID(a.AdExtension.Id)
			if id == "" {
				return "", fmt.Errorf("an image extension is associated with the campaign but its id is unusable: %w", errNoID)
			}
			if found == "" {
				found = id
			}
		}
	}
	return found, nil
}

// adExtensionIdentity extracts the created extension id from an AddAdExtensions 200 body,
// classifying BOTH levels of NestedPartialErrors:
//
//   - no errors anywhere + a usable id → success;
//   - an actual error code anywhere on the wire + NO usable id → a DEFINITE rejection
//     (errPartialFailure): Microsoft explained the empty slot. Truncation or null-only
//     placeholders are not an explanation and stay UNCONFIRMED (errNoID);
//   - any error entry (even a null placeholder or a truncated array) + a usable id → NOT clean
//     success. The extension exists, but Microsoft also
//     reported failures against it, so the outcome is UNCONFIRMED (errNoID) and the id is still
//     returned so the extension can be found;
//   - no id and no explaining error, or an unreadable body → errNoID (UNCONFIRMED).
func adExtensionIdentity(body []byte) (string, error) {
	var resp addAdExtensionsResponse
	if err := decodeTrusted(body, &resp); err != nil {
		return "", fmt.Errorf("decode AdExtensionIdentities response (%v): %w", err, errNoID)
	}
	id := ""
	if len(resp.AdExtensionIdentities) > 0 && resp.AdExtensionIdentities[0] != nil {
		id = numberID(resp.AdExtensionIdentities[0].Id)
	}
	// EVIDENCE and INCOMPLETENESS are kept apart. Only evidence — an actual code anywhere on the
	// wire, counted by the decoders' whole-array AnyErrors tallies so an error past the retention
	// cap still counts — can make a null identity a DEFINITE rejection. Truncation or null-only
	// placeholders explain nothing, so a null identity with only those stays UNCONFIRMED: calling
	// it definite would invite a retry that creates a second extension.
	nested := resp.NestedPartialErrors
	evidence := nested.AnyErrors
	incomplete := nested.Truncated || nestedTruncated(nested.Items) || len(nested.Items) > 0
	switch {
	case id == "" && evidence:
		return "", fmt.Errorf("%w: %s", errPartialFailure, nestedErrorCodes(nested.Items))
	case id != "" && (evidence || incomplete):
		return id, fmt.Errorf("extension %s was returned alongside nested error entries (%s): %w", id, nestedErrorCodes(nested.Items), errNoID)
	case id == "":
		return "", fmt.Errorf("AdExtensionIdentities %w", errNoID)
	}
	return id, nil
}

// decodeTrusted refuses a body encoding/json would decode WITHOUT error into something other
// than what its bytes say — malformed UTF-8, an unpaired surrogate escape, or a key declared twice
// (including under the decoder's case fold) — and only then decodes it. Every presence probe in
// this file reads an exact map key while the struct decode is case-folded and last-key-wins, so
// `{"AdExtensionAssociationCollection":[...],"adextensionassociationcollection":[]}` would
// otherwise mark the field present from one spelling and decode it empty from the other: a lookup
// answering "none" that licenses a second AddMedia. The adoption reads apply the same check.
func decodeTrusted(body []byte, v any) error {
	if err := identityjson.Check(body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// mediaIdentity extracts the created MediaId from an AddMedia 200 body. A usable id is success
// only when PartialErrors carries no entry at all: this call sends ONE item, so an error entry is
// positionally about that item, and an id returned alongside one is not a confirmed upload — the
// id is kept (so the media can be found) but errNoID makes it UNCONFIRMED and the later steps stop,
// as adExtensionIdentity does for an identity beside nested errors. A null id is a DEFINITE rejection only on error EVIDENCE — the whole-wire PartialErrors.AnyErrors
// tally, so an error past the 16-entry retention window still counts (and its code is still
// reported when retained); null placeholders or an unexplained null id are UNCONFIRMED (errNoID).
func mediaIdentity(body []byte) (string, error) {
	var resp addMediaResponse
	if err := decodeTrusted(body, &resp); err != nil {
		return "", fmt.Errorf("decode MediaIds response (%v): %w", err, errNoID)
	}
	if len(resp.MediaIds) > 0 {
		if id := numberID(resp.MediaIds[0]); id != "" {
			pe := resp.PartialErrors
			if pe.AnyErrors || pe.Truncated || len(pe.Items) > 0 {
				return id, fmt.Errorf("media %s was returned alongside error entries (%s): %w", id, partialErrorCodes(pe.Items), errNoID)
			}
			return id, nil
		}
	}
	if resp.PartialErrors.AnyErrors {
		return "", fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(resp.PartialErrors.Items))
	}
	return "", fmt.Errorf("MediaIds %w", errNoID)
}
