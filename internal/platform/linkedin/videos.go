// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// LinkedIn video Sponsored Content (#586).
//
// A variant may carry a stored MP4 (CreativeVariant.VideoAssetID, resolved by the dispatcher into
// CampaignInput.Videos). Its bytes reach LinkedIn through the Videos API, and the resulting
// urn:li:video is the dark post's content.media.id. A campaign whose variants carry videos is
// created with format SINGLE_VIDEO ("Dynamic, Carousel, and Video Ad Campaigns must have their
// format set during creation"; "SINGLE_VIDEO can only have creatives with SPONSORED_VIDEO type"),
// so a campaign is all-video or all-image, never mixed — CreateCampaign refuses a mix up front.
//
// The upload is the Videos API's multi-part flow
// (https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/videos-api):
//
//  1. POST /rest/videos?action=initializeUpload with {"initializeUploadRequest":{"owner":
//     <organization URN>,"fileSizeBytes":N,"uploadCaptions":false,"uploadThumbnail":false}} ->
//     {"value":{"video","uploadToken","uploadInstructions":[{"uploadUrl","firstByte",
//     "lastByte"}]}}. Every call mints a NEW video URN, so it is sent once and never retried.
//  2. PUT each part's byte range [firstByte, lastByte] to its uploadUrl as
//     application/octet-stream, keeping the ETag response header of each ("Ensure that you save
//     eTag for future use"). The instructions are checked to tile the file exactly before any
//     byte is sent, and every uploadUrl must be a LinkedIn host. Never retried.
//  3. POST /rest/videos?action=finalizeUpload with {"finalizeUploadRequest":{"video",
//     "uploadToken","uploadedPartIds":[ETag...]}}, the part ids "in the same order as the parts
//     in the upload instructions". Never retried.
//  4. GET /rest/videos/{urn} until status AVAILABLE, within one bounded wait shared by every
//     video of the CreateCampaign — the same poll, classification and shared-deadline rules as
//     the Images API's (awaitMediaAvailable).
//
// IDEMPOTENCY is the image path's, by the same construction: uploads run AFTER the campaign is
// ensured and BEFORE the first dark post, so every exit from the first initializeUpload on
// returns a non-nil partial result. The dispatcher retains the claim on a non-nil result and the
// orchestrator never re-dispatches a retained row, so a retry can never upload a video twice or
// post a second dark post for it. Every minted URN is recorded on the result (VideoURNs).
//
// Scopes: the same as the dark post this video decorates (rw_ads / w_organization_social, ADMIN
// or DSC on the organization page — "For videos with company URN owners, the caller needs to
// have ADMIN or DSC permissions for the company page").

// Video ad file bounds (LinkedIn help a424737: "Between 75 KB and 500 MB", "MP4"). The minimum
// takes the BINARY reading of 75 KB so that no file LinkedIn could count as under 75 KB is sent;
// the maximum takes the DECIMAL reading of 500 MB for the same reason. The creative-asset store
// caps a stored file at 30 MiB, so in practice that is the ceiling a dispatch meets first.
const (
	MinVideoBytes = 75 * 1024
	MaxVideoBytes = 500_000_000
)

// Defaults for the video availability poll. A video takes longer than an image to process, so
// its shared wait is longer — but the whole dispatch is still bounded by the orchestrator's
// two-minute provider call, so it cannot be unbounded either. A video that is still PROCESSING
// when the wait ends is reported (the campaign is left with zero creatives and the minted URN on
// the result); see the file comment.
//
// Do NOT lengthen the wait: it shares the provider call with the uploads, finalize and the
// post/creative loop. Live-verification blocker for LINKEDIN_ADS_VIDEO_ENABLED: measure real
// processing time for a 20-30 MiB MP4 first; if it is routinely slow, decouple the upload from
// create (pre-upload at asset-store time or an earlier job phase, then reference the AVAILABLE URN).
const (
	defaultVideoPollInterval     = 3 * time.Second
	defaultVideoAvailabilityWait = 60 * time.Second
)

// ctaRegister is the dark post's contentCallToActionLabel. Every campaign this service creates
// drives event registrations, and a video post has no article card to carry the destination,
// so the post names its landing page and the REGISTER label explicitly.
const ctaRegister = "REGISTER"

// videoURNRE matches a Videos API URN. The id is LinkedIn's opaque base64url-like token.
var videoURNRE = regexp.MustCompile(`^urn:li:video:[A-Za-z0-9_-]+$`)

// ErrVideoInvalid marks a stored video LinkedIn cannot accept, decided locally before anything
// is sent.
var ErrVideoInvalid = errors.New("linkedin: video not accepted")

// VideoUpload is one stored video to upload: its bytes and the verified MIME type the asset
// store recorded for them.
type VideoUpload struct {
	Data     []byte
	MimeType string
}

// WithVideoPolling overrides the video availability poll interval and the total wait shared by
// all videos of one CreateCampaign. Non-positive values keep the defaults.
func WithVideoPolling(interval, wait time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.videoPollInterval = interval
		}
		if wait > 0 {
			c.videoAvailabilityWait = wait
		}
	}
}

// ValidateVideo reports whether in can be uploaded as a single video ad. It checks the recorded
// type, the size bounds and the MP4 ftyp signature; codec, duration and dimensions are
// LinkedIn's to judge (a video it cannot use ends PROCESSING_FAILED, before any post references
// it). The bytes are never quoted in the error.
func ValidateVideo(in VideoUpload) error {
	if len(in.Data) == 0 {
		return fmt.Errorf("%w: the video has no bytes", ErrVideoInvalid)
	}
	if in.MimeType != "video/mp4" {
		return fmt.Errorf("%w: only MP4 videos are supported", ErrVideoInvalid)
	}
	if len(in.Data) < MinVideoBytes {
		return fmt.Errorf("%w: the video is %d bytes; LinkedIn video ads require at least %d", ErrVideoInvalid, len(in.Data), MinVideoBytes)
	}
	if len(in.Data) > MaxVideoBytes {
		return fmt.Errorf("%w: the video is %d bytes; LinkedIn video ads accept at most %d", ErrVideoInvalid, len(in.Data), MaxVideoBytes)
	}
	if len(in.Data) < 12 || string(in.Data[4:8]) != "ftyp" {
		return fmt.Errorf("%w: the video bytes are not an MP4 file", ErrVideoInvalid)
	}
	return nil
}

// isVideoInput reports whether in's variants carry videos. CreateCampaign has already refused a
// mix, so the first variant decides.
func isVideoInput(in CampaignInput) bool {
	return len(in.Variants) > 0 && in.Variants[0].VideoAssetID != ""
}

// videoInitResponse is the initializeUpload action's response body.
type videoInitResponse struct {
	Value struct {
		Video              string `json:"video"`
		UploadToken        string `json:"uploadToken"`
		UploadInstructions []struct {
			UploadURL string `json:"uploadUrl"`
			FirstByte int64  `json:"firstByte"`
			LastByte  int64  `json:"lastByte"`
		} `json:"uploadInstructions"`
	} `json:"value"`
}

// uploadVariantVideos uploads each DISTINCT video the variants reference — once, however many
// variants share it — and waits until every one is AVAILABLE. It mirrors uploadVariantImages:
// the returned map names every URN this run minted (including ones that then failed), and the
// Steps describe what happened. It runs in the same window, for the same reasons.
func (c *Client) uploadVariantVideos(ctx context.Context, accountID string, in CampaignInput) (map[string]string, []string, error) {
	var order []string
	seen := map[string]bool{}
	for _, v := range in.Variants {
		if v.VideoAssetID == "" || seen[v.VideoAssetID] {
			continue
		}
		seen[v.VideoAssetID] = true
		order = append(order, v.VideoAssetID)
	}
	if len(order) == 0 {
		return nil, nil, nil
	}

	owner, err := c.orgURN(accountID)
	if err != nil {
		return nil, nil, &imageUploadError{err: fmt.Errorf("video upload: %w", err)}
	}

	urns := make(map[string]string, len(order))
	var steps []string
	for _, assetID := range order {
		urn, uerr := c.uploadVideo(ctx, owner, in.Videos[assetID])
		if urn != "" {
			urns[assetID] = urn
		}
		if uerr != nil {
			var ie *imageUploadError
			if errors.As(uerr, &ie) && ie.unconfirmed {
				steps = append(steps, fmt.Sprintf("Video for asset %s UNCONFIRMED (an unreferenced video may exist in LinkedIn; it serves nothing): %s", assetID, uerr.Error()))
			} else {
				steps = append(steps, fmt.Sprintf("Video for asset %s NOT uploaded: %s", assetID, uerr.Error()))
			}
			return urns, steps, uerr
		}
		steps = append(steps, fmt.Sprintf("Video uploaded for asset %s: %s", assetID, urn))
	}

	if err := c.awaitMediaAvailable(ctx, videoMedia, c.videoPollInterval, c.videoAvailabilityWait, order, urns); err != nil {
		steps = append(steps, fmt.Sprintf("Videos NOT used (no dark post was created): %s", err.Error()))
		return urns, steps, err
	}
	steps = append(steps, fmt.Sprintf("Videos AVAILABLE: %d", len(order)))
	return urns, steps, nil
}

// uploadVideo runs initializeUpload, the part PUTs and finalizeUpload for one video, returning
// the minted URN (empty when none is known). No call is retried.
func (c *Client) uploadVideo(ctx context.Context, owner string, v VideoUpload) (string, error) {
	if err := ValidateVideo(v); err != nil {
		return "", &imageUploadError{err: err}
	}
	size := int64(len(v.Data))
	const initPath = "videos?action=initializeUpload"
	body, err := json.Marshal(map[string]any{"initializeUploadRequest": map[string]any{
		"owner":           owner,
		"fileSizeBytes":   size,
		"uploadCaptions":  false,
		"uploadThumbnail": false,
	}})
	if err != nil {
		return "", &imageUploadError{err: fmt.Errorf("marshal initializeUpload: %w", err)}
	}
	raw, _, err := c.mediaRequest(ctx, http.MethodPost, c.baseURL+"/"+initPath, initPath, body, "application/json", true)
	if err != nil {
		return "", &imageUploadError{err: fmt.Errorf("video upload: initializeUpload: %w", err), unconfirmed: createOutcomeAmbiguous(err)}
	}
	var resp videoInitResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil || !videoURNRE.MatchString(resp.Value.Video) {
		return "", &imageUploadError{err: errors.New("video upload: initializeUpload returned no usable video URN"), unconfirmed: true}
	}
	urn := resp.Value.Video

	// Check EVERY instruction before sending ANY byte: a plan that does not tile the file, or
	// that would carry the bearer token off LinkedIn, uploads nothing. The minted video stays
	// WAITING_UPLOAD and unreferenced.
	parts := resp.Value.UploadInstructions
	if len(parts) == 0 {
		return urn, &imageUploadError{err: fmt.Errorf("video upload: initializeUpload returned no upload instructions for %s; nothing was uploaded", urn)}
	}
	var next int64
	for i, p := range parts {
		if p.FirstByte != next || p.LastByte < p.FirstByte || p.LastByte >= size {
			return urn, &imageUploadError{err: fmt.Errorf("video upload: upload instruction %d for %s does not continue the byte range; nothing was uploaded", i+1, urn)}
		}
		if !c.trustedUploadURL(p.UploadURL) {
			return urn, &imageUploadError{err: fmt.Errorf("video upload: initializeUpload returned an upload URL outside LinkedIn for %s; nothing was uploaded", urn)}
		}
		next = p.LastByte + 1
	}
	if next != size {
		return urn, &imageUploadError{err: fmt.Errorf("video upload: upload instructions for %s cover %d of %d bytes; nothing was uploaded", urn, next, size)}
	}

	etags := make([]string, 0, len(parts))
	for i, p := range parts {
		label := fmt.Sprintf("video upload URL (part %d of %d)", i+1, len(parts))
		_, hdr, perr := c.mediaRequest(ctx, http.MethodPut, p.UploadURL, label, v.Data[p.FirstByte:p.LastByte+1], "application/octet-stream", false)
		if perr != nil {
			// The video is not finalized, so it can never be AVAILABLE or referenced.
			return urn, &imageUploadError{err: fmt.Errorf("video upload: part %d of %d of %s failed: %w", i+1, len(parts), urn, perr)}
		}
		etag := strings.TrimSpace(hdr.Get("ETag"))
		if etag == "" {
			return urn, &imageUploadError{err: fmt.Errorf("video upload: part %d of %d of %s returned no ETag; the upload cannot be finalized", i+1, len(parts), urn)}
		}
		etags = append(etags, etag)
	}

	const finPath = "videos?action=finalizeUpload"
	fin, err := json.Marshal(map[string]any{"finalizeUploadRequest": map[string]any{
		"video":           urn,
		"uploadToken":     resp.Value.UploadToken,
		"uploadedPartIds": etags,
	}})
	if err != nil {
		return urn, &imageUploadError{err: fmt.Errorf("marshal finalizeUpload: %w", err)}
	}
	if _, _, err := c.mediaRequest(ctx, http.MethodPost, c.baseURL+"/"+finPath, finPath, fin, "application/json", true); err != nil {
		// The URN is known either way; whether LinkedIn finalized it is what may be unknown.
		return urn, &imageUploadError{err: fmt.Errorf("video upload: finalizeUpload of %s failed: %w", urn, err), unconfirmed: createOutcomeAmbiguous(err)}
	}
	return urn, nil
}

// createVideoDarkPost creates a dark post (feedDistribution NONE) whose content is the uploaded
// video, with the destination as its landing page, and returns its post URN. The envelope is
// createDarkPost's; only the content differs (Create and Manage Video Ads, "Create a Video Ad with
// a New Post": "content": {"media": {"title": ..., "id": "urn:li:video:..."}}).
func (c *Client) createVideoDarkPost(ctx context.Context, accountID, introText, headline, destURL, videoURN string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}
	intro := normalizeCreativeText(introText)
	if len([]rune(intro)) > 600 {
		intro = truncateRunes(intro, 600)
	}
	head := normalizeCreativeText(headline)
	if len([]rune(head)) > 200 {
		head = truncateRunes(head, 200)
	}
	body := darkPostEnvelope(author, intro, accountID)
	body["content"] = map[string]any{"media": map[string]any{"title": head, "id": videoURN}}
	body["contentLandingPage"] = destURL
	body["contentCallToActionLabel"] = ctaRegister
	return c.postDarkPost(ctx, body)
}
