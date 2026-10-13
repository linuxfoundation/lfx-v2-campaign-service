// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG header decoder for ValidateImage
	_ "image/png"  // register the PNG header decoder for ValidateImage
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LinkedIn creative image upload (LFXV2-2665).
//
// A variant may carry a stored creative asset (CreativeVariant.ImageAssetID, resolved by the
// dispatcher into CampaignInput.Images). The bytes reach LinkedIn through the Images API and
// the resulting urn:li:image is sent as the dark post's article thumbnail — the SAME field
// CreativeVariant.ImageURN has always filled, so the post shape does not change. The Posts API
// documents exactly this: "To create an article post with thumbnail image, please use Images
// API to upload thumbnail image and use the ImageUrn on thumbnail field"
// (https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/posts-api).
//
// The upload is three calls
// (https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/images-api):
//
//  1. POST /rest/images?action=initializeUpload with {"initializeUploadRequest":{"owner":
//     <organization URN>}} -> {"value":{"uploadUrl","image","uploadUrlExpiresAt"}}. Every call
//     mints a NEW image URN, so it is sent once and never retried (doRequest's 429 rule for a
//     non-idempotent POST, applied here explicitly).
//  2. PUT <uploadUrl> with the raw bytes and the bearer token ("The upload call requires a
//     valid OAuth token in the 'Authorization' header" — the Assets API's image upload, which
//     the Images API page links for this step). Never retried.
//  3. GET /rest/images/{urn} until status AVAILABLE. The Images API has no synchronous upload
//     ("SYNCHRONOUS_UPLOAD is not supported in Images API"), and LinkedIn warns that a post
//     created before its image finishes processing is not shown to members, so the URN is
//     referenced only once AVAILABLE, within a bounded wait.
//
// Scopes: /rest/images is permitted to rw_ads, w_member_social, w_organization_social and
// w_power_creators; an organization-owned image additionally needs the member to hold ADMIN or
// DSC permission on that page. The dark post this image decorates already requires
// w_organization_social and that same page role, so the upload adds no credential requirement
// the create path did not already have.

// MaxImageBytes is the largest stored asset this package uploads. LinkedIn's single image ad
// specification caps the file at "5 MB" (https://www.linkedin.com/help/lms/answer/a426534);
// the DECIMAL reading is used so that no file LinkedIn could count as over 5 MB is ever sent.
const MaxImageBytes = 5_000_000

// maxImagePixels is the Images API's pixel ceiling ("Images with less than 36,152,320 pixels").
const maxImagePixels = 36_152_320

// Single image ad dimension bounds per orientation (LinkedIn help a426534): horizontal
// 640x360 .. 7680x4320, square 360x360 .. 4320x4320, vertical 360x640 .. 2430x4320.
const (
	minLandscapeW, minLandscapeH = 640, 360
	maxLandscapeW, maxLandscapeH = 7680, 4320
	minSquareSide, maxSquareSide = 360, 4320
	minPortraitW, minPortraitH   = 360, 640
	maxPortraitW, maxPortraitH   = 2430, 4320
)

// Defaults for the availability poll. The whole dispatch is bounded by the orchestrator's
// two-minute provider call, so the wait is SHARED by every image in one CreateCampaign rather
// than granted per image.
const (
	defaultImagePollInterval     = 2 * time.Second
	defaultImageAvailabilityWait = 45 * time.Second
)

// ErrImageInvalid marks a stored image LinkedIn cannot accept. It is decided locally, before
// anything is sent.
var ErrImageInvalid = errors.New("linkedin: image not accepted")

// ImageUpload is one stored image to upload: its bytes and the verified MIME type the asset
// store recorded for them.
type ImageUpload struct {
	Data     []byte
	MimeType string
}

// WithImagePolling overrides the availability poll interval and the total wait shared by all
// images of one CreateCampaign. Non-positive values keep the defaults. Tests use it to keep the
// timeout path fast.
func WithImagePolling(interval, wait time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.imagePollInterval = interval
		}
		if wait > 0 {
			c.imageAvailabilityWait = wait
		}
	}
}

// ValidateImage reports whether in can be uploaded as a single image ad thumbnail. It decodes
// only the image HEADER, never the pixels. The bytes are never quoted in the error.
func ValidateImage(in ImageUpload) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the image has no bytes", ErrImageInvalid)
	}
	if len(in.Data) > MaxImageBytes {
		return fmt.Errorf("%w: the image is %d bytes; LinkedIn single image ads accept at most %d", ErrImageInvalid, len(in.Data), MaxImageBytes)
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
		return fmt.Errorf("%w: the image bytes are %s, not the recorded %s", ErrImageInvalid, format, in.MimeType)
	}
	w, h := cfg.Width, cfg.Height
	if int64(w)*int64(h) >= maxImagePixels {
		return fmt.Errorf("%w: the image is %dx%d; LinkedIn accepts fewer than %d pixels", ErrImageInvalid, w, h, maxImagePixels)
	}
	var minW, minH, maxW, maxH int
	var shape string
	switch {
	case w > h:
		shape, minW, minH, maxW, maxH = "horizontal", minLandscapeW, minLandscapeH, maxLandscapeW, maxLandscapeH
	case w == h:
		shape, minW, minH, maxW, maxH = "square", minSquareSide, minSquareSide, maxSquareSide, maxSquareSide
	default:
		shape, minW, minH, maxW, maxH = "vertical", minPortraitW, minPortraitH, maxPortraitW, maxPortraitH
	}
	if w < minW || h < minH {
		return fmt.Errorf("%w: the %s image is %dx%d; LinkedIn requires at least %dx%d", ErrImageInvalid, shape, w, h, minW, minH)
	}
	if w > maxW || h > maxH {
		return fmt.Errorf("%w: the %s image is %dx%d; LinkedIn accepts at most %dx%d", ErrImageInvalid, shape, w, h, maxW, maxH)
	}
	return nil
}

// imageUploadError is a failed image step. It carries the step's own classification, because
// the shared one (createOutcomeAmbiguous) answers "may this request have been applied", and for
// an image that is only the right question for the two mutating calls. unconfirmed is true when
// an image MAY exist that this run cannot name or confirm.
type imageUploadError struct {
	err         error
	unconfirmed bool
}

func (e *imageUploadError) Error() string     { return e.err.Error() }
func (e *imageUploadError) Unwrap() error     { return e.err }
func (e *imageUploadError) Unconfirmed() bool { return e.unconfirmed }

// initializeUploadResponse is the initializeUpload action's response body.
type initializeUploadResponse struct {
	Value struct {
		UploadURL string `json:"uploadUrl"`
		Image     string `json:"image"`
	} `json:"value"`
}

// imageStatusResponse is the single-image GET's response body; only status is read.
type imageStatusResponse struct {
	Status string `json:"status"`
}

// uploadVariantImages uploads each DISTINCT image the variants reference — once, however many
// variants share it — and waits until every one is AVAILABLE. It returns the asset-id -> image
// URN map for every URN this run minted (including ones that then failed, so the result can
// name them) and the Steps describing what happened.
//
// It runs AFTER the campaign is ensured and BEFORE the first dark post, deliberately:
//   - after, so that every exit from here on returns a non-nil partial result. The dispatcher
//     retains the dispatch claim on a non-nil result and the orchestrator never re-dispatches a
//     retained row, so a second dispatch can never mint a second URN for the same asset;
//   - before, so that an image failure leaves no dark post or creative behind — the campaign
//     exists, PAUSED, with zero creatives.
func (c *Client) uploadVariantImages(ctx context.Context, accountID string, in CampaignInput) (map[string]string, []string, error) {
	var order []string
	seen := map[string]bool{}
	for _, v := range in.Variants {
		if v.ImageAssetID == "" || seen[v.ImageAssetID] {
			continue
		}
		seen[v.ImageAssetID] = true
		order = append(order, v.ImageAssetID)
	}
	if len(order) == 0 {
		return nil, nil, nil
	}

	owner, err := c.orgURN(accountID)
	if err != nil {
		return nil, nil, &imageUploadError{err: fmt.Errorf("image upload: %w", err)}
	}

	urns := make(map[string]string, len(order))
	var steps []string
	for _, assetID := range order {
		urn, uerr := c.uploadImage(ctx, owner, in.Images[assetID])
		if urn != "" {
			urns[assetID] = urn
		}
		if uerr != nil {
			var ie *imageUploadError
			unconfirmed := errors.As(uerr, &ie) && ie.unconfirmed
			if unconfirmed {
				steps = append(steps, fmt.Sprintf("Image for asset %s UNCONFIRMED (an unreferenced image may exist in LinkedIn; it serves nothing): %s", assetID, uerr.Error()))
			} else {
				steps = append(steps, fmt.Sprintf("Image for asset %s NOT uploaded: %s", assetID, uerr.Error()))
			}
			return urns, steps, uerr
		}
		steps = append(steps, fmt.Sprintf("Image uploaded for asset %s: %s", assetID, urn))
	}

	if err := c.awaitImagesAvailable(ctx, order, urns); err != nil {
		steps = append(steps, fmt.Sprintf("Images NOT used (no dark post was created): %s", err.Error()))
		return urns, steps, err
	}
	steps = append(steps, fmt.Sprintf("Images AVAILABLE: %d", len(order)))
	return urns, steps, nil
}

// uploadImage runs initializeUpload and the PUT for one image, returning the minted URN (empty
// when none is known). Neither call is retried.
func (c *Client) uploadImage(ctx context.Context, owner string, img ImageUpload) (string, error) {
	if err := ValidateImage(img); err != nil {
		return "", &imageUploadError{err: err}
	}
	const initPath = "images?action=initializeUpload"
	body, err := json.Marshal(map[string]any{"initializeUploadRequest": map[string]any{"owner": owner}})
	if err != nil {
		return "", &imageUploadError{err: fmt.Errorf("marshal initializeUpload: %w", err)}
	}
	raw, err := c.imageRequest(ctx, http.MethodPost, c.baseURL+"/"+initPath, initPath, body, "application/json", true)
	if err != nil {
		// A mutating call: a 5xx, a mutating 429, a transport failure or a mutating 401 may have
		// registered an image this run cannot name. It is unreferenced, so it serves nothing.
		return "", &imageUploadError{err: fmt.Errorf("image upload: initializeUpload: %w", err), unconfirmed: createOutcomeAmbiguous(err)}
	}
	var resp initializeUploadResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil || !imageURNRE.MatchString(resp.Value.Image) {
		// A 2xx we cannot read: an image was most likely registered, but under no URN we know.
		return "", &imageUploadError{err: errors.New("image upload: initializeUpload returned no usable image URN"), unconfirmed: true}
	}
	urn := resp.Value.Image
	if !c.trustedUploadURL(resp.Value.UploadURL) {
		// The bearer token travels with the PUT, so it is sent only to LinkedIn's own hosts.
		// Nothing was uploaded: the minted image stays empty (WAITING_UPLOAD) and unreferenced.
		return urn, &imageUploadError{err: fmt.Errorf("image upload: initializeUpload returned an upload URL outside LinkedIn for %s; nothing was uploaded", urn)}
	}
	if _, err := c.imageRequest(ctx, http.MethodPut, resp.Value.UploadURL, "image upload URL", img.Data, img.MimeType, false); err != nil {
		// Not retried: the URN is known, and a second PUT of a partially-received body is not
		// something LinkedIn documents as safe. The image is NOT referenced either way.
		return urn, &imageUploadError{err: fmt.Errorf("image upload: upload of %s failed: %w", urn, err), unconfirmed: createOutcomeAmbiguous(err)}
	}
	return urn, nil
}

// awaitImagesAvailable polls each URN until AVAILABLE, all within ONE shared deadline. A poll
// that fails transiently (429, 5xx, transport) is retried at the next tick; a definite answer
// (PROCESSING_FAILED, a non-429 4xx, an expired credential) stops at once. Only GETs are sent,
// so a failure here is never outcome-ambiguous: no post references the image.
func (c *Client) awaitImagesAvailable(ctx context.Context, order []string, urns map[string]string) error {
	// ONE context bounds the whole phase — every status GET and every sleep — so the shared wait
	// is a real ceiling. A deadline checked only between polls let a slow GET (each has its own
	// 30 s attempt timeout) or an AVAILABLE reply for an earlier image carry the phase past it.
	pollCtx, cancel := context.WithTimeout(ctx, c.imageAvailabilityWait)
	defer cancel()
	for _, assetID := range order {
		urn := urns[assetID]
		path := "images/" + encodeURNForPath(urn)
		last := "unknown"
		for {
			if pollCtx.Err() != nil {
				return c.availabilityWaitError(ctx, urn, assetID, last)
			}
			raw, err := c.imageRequest(pollCtx, http.MethodGet, c.baseURL+"/"+path, "images/{urn}", nil, "", true)
			if err == nil {
				var st imageStatusResponse
				if json.Unmarshal(raw, &st) == nil {
					last = knownImageStatus(st.Status)
				}
				if last == "AVAILABLE" {
					break
				}
				if last == "PROCESSING_FAILED" {
					return &imageUploadError{err: fmt.Errorf("image %s for asset %s failed LinkedIn processing (PROCESSING_FAILED)", urn, assetID)}
				}
			} else if pollCtx.Err() != nil {
				// The request was cut off by the shared wait (or the caller): report the wait,
				// not the truncated request.
				return c.availabilityWaitError(ctx, urn, assetID, last)
			} else if !imagePollTransient(err) {
				return &imageUploadError{err: fmt.Errorf("image %s for asset %s: status check failed: %w", urn, assetID, err)}
			}
			if sleepCtx(pollCtx, c.imagePollInterval) != nil {
				return c.availabilityWaitError(ctx, urn, assetID, last)
			}
		}
	}
	return nil
}

// availabilityWaitError reports an image that did not become AVAILABLE before the shared wait
// (or the caller's context) ended. last is always a value from knownImageStatus.
func (c *Client) availabilityWaitError(ctx context.Context, urn, assetID, last string) error {
	if err := ctx.Err(); err != nil {
		return &imageUploadError{err: fmt.Errorf("image %s for asset %s: waiting for availability: %w", urn, assetID, err)}
	}
	return &imageUploadError{err: fmt.Errorf("image %s for asset %s was not AVAILABLE within %s (last status %s)", urn, assetID, c.imageAvailabilityWait, last)}
}

// knownImageStatus returns s when it is one of the Images API's documented MediaStatus values,
// and "unknown" otherwise. The status reaches Steps and logs, so an unexpected upstream string
// is never echoed — the same status-only policy as every other error here.
func knownImageStatus(s string) string {
	switch s {
	case "AVAILABLE", "PROCESSING", "PROCESSING_FAILED", "WAITING_UPLOAD":
		return s
	default:
		return "unknown"
	}
}

// preSendError is a request that never left this process: DNS failure, connection refused, no
// route. It is redacted (no URL) and typed so the status poll can retry it, while
// createOutcomeAmbiguous — which does not recognise it — keeps a pre-send POST or PUT definite.
type preSendError struct {
	Method, Path string
}

func (e *preSendError) Error() string {
	return fmt.Sprintf("linkedin %s %s: connection failed before sending", e.Method, e.Path)
}

// imagePollTransient reports whether a status GET failure may succeed on a later tick.
func imagePollTransient(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	var pe *preSendError
	if errors.As(err, &pe) {
		return true
	}
	var te *transportError
	return errors.As(err, &te)
}

// trustedUploadURL reports whether the bearer token may be sent to raw: https on linkedin.com
// or a subdomain, or exactly the scheme and host this client already sends the token to (the
// API base URL, which tests point at a local server).
func (c *Client) trustedUploadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if base, berr := url.Parse(c.baseURL); berr == nil && u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "linkedin.com" || strings.HasSuffix(host, ".linkedin.com"))
}

// imageRequest sends one image call and returns the 2xx body. It never retries. label names the
// call in errors in place of the URL, because the upload URL is signed and must not reach a
// Step, a log line or a client. api adds the Marketing API version headers, which the upload
// URL does not take. The error types match doRequest's, so createOutcomeAmbiguous classifies
// them the same way; no response body is ever rendered.
func (c *Client) imageRequest(ctx context.Context, method, rawURL, label string, body []byte, contentType string, api bool) ([]byte, error) {
	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	defer cancel()
	if err != nil {
		return nil, err
	}
	var rdr io.Reader = http.NoBody
	if body != nil {
		rdr = bytes.NewReader(body) // a view over the caller's slice, not a copy
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, rawURL, rdr)
	if err != nil {
		return nil, fmt.Errorf("linkedin %s %s: build request failed", method, label)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if api {
		req.Header.Set("LinkedIn-Version", c.apiVersion)
		req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isPreSendDialError(err) {
			return nil, &preSendError{Method: method, Path: label}
		}
		return nil, &transportError{Method: method, Path: label, Err: errors.New("request failed in flight")}
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, rerr := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := ""
		if rerr == nil {
			text = buf.String()
		}
		if ce := c.expiredCredentialsError(resp.StatusCode, text, method); ce != nil {
			return nil, ce
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: label}
	}
	if rerr != nil || int64(buf.Len()) > maxResponseBytes {
		return nil, &transportError{Method: method, Path: label, Err: errors.New("response body could not be read")}
	}
	return buf.Bytes(), nil
}
