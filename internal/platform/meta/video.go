// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Meta video ads (#550 gap 9).
//
// A variant may carry a stored MP4 (AdVariant.VideoAssetID, resolved by the dispatcher into
// VideoBytes/VideoMIME). Its ad is a video link ad: object_story_spec.video_data in place of
// link_data (Ad Creative Video Data: video_id, image_url, message, title, link_description,
// call_to_action). The flow, after the ad set exists and before the first creative:
//
//  1. POST /act_<id>/advideos with the bytes as the multipart `source` file part -> {"id"}
//     (Ad Account Advideos, non-resumable upload; the store caps a file at 30 MiB, far under the
//     non-resumable ceiling). Every call creates a NEW video object — unlike /adimages it is not
//     content-addressed — so it is sent ONCE per distinct video and never retried, not even on a
//     throttle. Variants naming the same bytes share one upload.
//  2. GET /{video_id}?fields=status until status.video_status is "ready", within ONE bounded
//     wait shared by every video of the CreateCampaign (default 60 s, polled every 3 s). The wait
//     runs inside the orchestrator's two-minute provider call, so it must not be lengthened; a
//     video still processing when it ends fails its variants (the video id is kept on the result).
//  3. GET /{video_id}/thumbnails?fields=uri,is_preferred and use the preferred auto-generated
//     thumbnail (else the first) as video_data.image_url: Meta requires a thumbnail on a video
//     creative and generates candidates while it encodes, so no image is uploaded by this service.
//  4. POST /act_<id>/adcreatives with video_data, then POST /act_<id>/ads, exactly as an image ad.
//
// RETRY SAFETY. Uploads run AFTER the ad set exists, so every failure from the first upload on is
// either a non-nil partial result (a cancelled caller) or a degraded success (a per-variant
// failure recorded in Steps). The dispatcher retains the claim on a non-nil partial and persists a
// shortfall as created_degraded, which the orchestrator reuses as-is, so a retry never uploads a
// video twice or creates a second ad for it. Every minted video id is recorded on the result
// (CampaignResult.VideoIDs) whether or not its ad was created.

// MimeTypeVideoMP4 is the only video type a variant may carry (the creative-asset store's
// model.MimeTypeMP4).
const MimeTypeVideoMP4 = "video/mp4"

// Defaults for the video readiness poll; see the file comment for why the wait is bounded.
// Live-verification blocker for META_VIDEO_ENABLED: measure Meta's real encoding time for a
// 20-30 MiB MP4 against this wait before enabling; if it is routinely slower, decouple the upload
// from create rather than lengthening the wait.
const (
	defaultVideoPollInterval = 3 * time.Second
	defaultVideoReadyWait    = 60 * time.Second
)

// Meta's video_status values (Video `status` field).
const (
	videoStatusReady = "ready"
	videoStatusError = "error"
)

// WithVideoPolling overrides the video readiness poll interval and the total wait shared by all
// videos of one CreateCampaign. Non-positive values keep the defaults.
func WithVideoPolling(interval, wait time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.videoPollInterval = interval
		}
		if wait > 0 {
			c.videoReadyWait = wait
		}
	}
}

// variantHasVideo reports whether v asked for a video ad: by its asset reference (what the caller
// sent) or by resolved bytes (what a direct client caller may send).
func variantHasVideo(v AdVariant) bool {
	return strings.TrimSpace(v.VideoAssetID) != "" || len(v.VideoBytes) > 0
}

// videoVariantInvalid refuses a video variant before any request. It wraps
// domain.ErrCreateConfigInvalid (400 on the synchronous path) and is a domain.PreflightRefusal.
func videoVariantInvalid(i int, format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: metaConfig.variants[%d].videoAssetId: %s", domain.ErrCreateConfigInvalid, i, fmt.Sprintf(format, args...)))
}

// ValidateVideoVariant is the client's pre-spend check of one variant's video (index i, 0-based as
// the config's array): a video variant carries no image (Meta picks its thumbnail), a referenced
// asset must have been resolved to bytes, and the bytes must be an MP4. A variant without a video
// passes.
func ValidateVideoVariant(i int, v AdVariant) error {
	if !variantHasVideo(v) {
		return nil
	}
	if strings.TrimSpace(v.ImageURL) != "" || strings.TrimSpace(v.ImageAssetID) != "" || len(v.ImageBytes) > 0 {
		return videoVariantInvalid(i, "a video variant carries no imageUrl or imageAssetId; Meta uses one of the video's auto-generated thumbnails")
	}
	if len(v.VideoBytes) == 0 {
		return videoVariantInvalid(i, "the video asset was not resolved to stored bytes")
	}
	if v.VideoMIME != MimeTypeVideoMP4 {
		return videoVariantInvalid(i, "only %s videos are supported", MimeTypeVideoMP4)
	}
	return nil
}

// preparedVideo is one variant's video, ready for its creative: the uploaded video id and the
// thumbnail URL, or the error that stopped it.
type preparedVideo struct {
	videoID   string
	thumbnail string
	err       error
}

// videoStageError marks a variant whose video never became usable, so no creative or ad request
// was sent for it. ambiguous reports an upload whose outcome is unknown (a video may exist in the
// ad account library under no id this service knows).
type videoStageError struct {
	msg       string
	ambiguous bool
}

func (e *videoStageError) Error() string { return e.msg }

// pendingVideo is one distinct video of a CreateCampaign and the variants (0-based) it serves.
type pendingVideo struct {
	data     []byte
	mime     string
	label    string
	variants []int
	videoID  string
	status   string
	readErr  error
	err      error
}

// videoLabel names a video on the result: its asset id when the dispatcher carried one, else the
// first variant it serves.
func videoLabel(v AdVariant, i int) string {
	if id := strings.TrimSpace(v.VideoAssetID); id != "" {
		return id
	}
	return "variant-" + strconv.Itoa(i+1)
}

// prepareVideos uploads each distinct video of variants once, waits for all of them within one
// shared bounded wait, and reads each ready video's thumbnail. It returns the per-variant outcome
// (only for variants carrying a video) and appends a Steps line per uploaded video; every minted
// id is recorded in videoIDs. A non-nil error means the CALLER's context ended: the caller returns
// its partial result.
func (c *Client) prepareVideos(ctx context.Context, variants []AdVariant, steps *[]string, videoIDs map[string]string) (map[int]preparedVideo, error) {
	byKey := map[[sha256.Size]byte]*pendingVideo{}
	var order []*pendingVideo
	for i, v := range variants {
		if len(v.VideoBytes) == 0 {
			continue
		}
		key := sha256.Sum256(v.VideoBytes)
		p, ok := byKey[key]
		if !ok {
			p = &pendingVideo{data: v.VideoBytes, mime: v.VideoMIME, label: videoLabel(v, i)}
			byKey[key] = p
			order = append(order, p)
		}
		p.variants = append(p.variants, i)
	}
	out := map[int]preparedVideo{}
	if len(order) == 0 {
		return out, nil
	}

	// 1. One upload per distinct video, never retried.
	for _, p := range order {
		id, err := c.uploadVideo(ctx, p.data, p.mime)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("meta video upload cut short (caller context done): %w", ctx.Err())
			}
			if createOutcomeAmbiguous(err) {
				p.err = &videoStageError{ambiguous: true, msg: fmt.Sprintf("video upload outcome UNCONFIRMED (an unused video may exist in the ad account library): %s", truncateErr(err, 300))}
			} else {
				p.err = &videoStageError{msg: fmt.Sprintf("video NOT uploaded: %s", truncateErr(err, 300))}
			}
			continue
		}
		p.videoID = id
		videoIDs[p.label] = id
		*steps = append(*steps, fmt.Sprintf("Video uploaded: %s (%s)", id, variantList(p.variants)))
	}

	// 2. One shared, bounded readiness wait.
	polls := int(c.videoReadyWait / c.videoPollInterval)
	if polls < 1 {
		polls = 1
	}
	for round := 0; ; round++ {
		waiting := 0
		for _, p := range order {
			if p.err != nil || p.status == videoStatusReady {
				continue
			}
			status, err := c.videoStatus(ctx, p.videoID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("meta video readiness poll cut short (caller context done): %w", ctx.Err())
				}
				// A failed read is not a verdict: keep polling until the wait ends.
				p.readErr = err
				waiting++
				continue
			}
			p.readErr = nil
			switch status {
			case videoStatusReady:
				p.status = status
			case videoStatusError:
				p.err = &videoStageError{msg: fmt.Sprintf("video %s failed Meta's processing (status error)", p.videoID)}
			default:
				p.status = status
				waiting++
			}
		}
		if waiting == 0 {
			break
		}
		if round >= polls {
			for _, p := range order {
				if p.err != nil || p.status == videoStatusReady {
					continue
				}
				detail := "status " + domain.CallerValue(p.status)
				if p.readErr != nil {
					detail = "last status read failed: " + truncateErr(p.readErr, 200)
				}
				p.err = &videoStageError{msg: fmt.Sprintf("video %s was not ready within %s (%s); it is kept in the ad account library and can be attached in Meta Ads Manager once ready", p.videoID, c.videoReadyWait, detail)}
			}
			break
		}
		if err := c.sleepFn(ctx, c.videoPollInterval); err != nil {
			return nil, fmt.Errorf("meta video readiness wait cut short (caller context done): %w", err)
		}
	}

	// 3. The auto-generated thumbnail of each ready video.
	for _, p := range order {
		if p.err == nil {
			thumb, err := c.videoThumbnail(ctx, p.videoID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("meta video thumbnail read cut short (caller context done): %w", ctx.Err())
				}
				p.err = &videoStageError{msg: fmt.Sprintf("video %s has no usable auto-generated thumbnail: %s", p.videoID, truncateErr(err, 300))}
			} else {
				for _, i := range p.variants {
					out[i] = preparedVideo{videoID: p.videoID, thumbnail: thumb}
				}
				continue
			}
		}
		for _, i := range p.variants {
			out[i] = preparedVideo{videoID: p.videoID, err: p.err}
		}
	}
	return out, nil
}

// variantList renders 0-based variant indexes as "variant 1, 2".
func variantList(idx []int) string {
	s := make([]string, len(idx))
	for k, i := range idx {
		s[k] = strconv.Itoa(i + 1)
	}
	if len(idx) == 1 {
		return "variant " + s[0]
	}
	return "variants " + strings.Join(s, ", ")
}

// videoStatusResponse mirrors GET /{video_id}?fields=status.
type videoStatusResponse struct {
	ID     string `json:"id"`
	Status struct {
		VideoStatus string `json:"video_status"`
	} `json:"status"`
}

// videoStatus reads a video's processing status (ready, processing or error).
func (c *Client) videoStatus(ctx context.Context, videoID string) (string, error) {
	var out videoStatusResponse
	if err := c.doRequest(ctx, http.MethodGet, "/"+videoID+"?fields=status", nil, &out); err != nil {
		return "", err
	}
	s := strings.ToLower(strings.TrimSpace(out.Status.VideoStatus))
	if s == "" {
		return "", fmt.Errorf("video %s status read named no video_status", videoID)
	}
	return s, nil
}

// videoThumbnailsResponse mirrors GET /{video_id}/thumbnails?fields=uri,is_preferred.
type videoThumbnailsResponse struct {
	Data []struct {
		URI         string `json:"uri"`
		IsPreferred bool   `json:"is_preferred"`
	} `json:"data"`
}

// videoThumbnail returns the preferred auto-generated thumbnail's https URL, else the first usable
// one. No usable thumbnail is an error: Meta refuses a video creative without one.
func (c *Client) videoThumbnail(ctx context.Context, videoID string) (string, error) {
	var out videoThumbnailsResponse
	if err := c.doRequest(ctx, http.MethodGet, "/"+videoID+"/thumbnails?fields=uri,is_preferred", nil, &out); err != nil {
		return "", err
	}
	usable := make([]string, 0, len(out.Data))
	preferred := ""
	for _, t := range out.Data {
		u, err := url.Parse(strings.TrimSpace(t.URI))
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			continue
		}
		usable = append(usable, u.String())
		if t.IsPreferred && preferred == "" {
			preferred = u.String()
		}
	}
	if preferred != "" {
		return preferred, nil
	}
	if len(usable) > 0 {
		return usable[0], nil
	}
	return "", fmt.Errorf("no https thumbnail was returned for video %s", videoID)
}

// videoStorySpec builds a video ad's object_story_spec: video_data in place of link_data, with the
// same copy, button and UTM-tagged destination an image ad carries.
func videoStorySpec(pageID, utmURL string, variant AdVariant, videoID, thumbnail, callToAction string) map[string]any {
	if callToAction == "" {
		callToAction = DefaultCallToAction
	}
	videoData := map[string]any{
		"video_id":  videoID,
		"image_url": thumbnail,
		"message":   variant.PrimaryText,
		"title":     variant.Headline,
		"call_to_action": map[string]any{
			"type":  callToAction,
			"value": map[string]any{"link": utmURL},
		},
	}
	if variant.Description != "" {
		videoData["link_description"] = variant.Description
	}
	return map[string]any{
		"page_id":    pageID,
		"video_data": videoData,
	}
}

// uploadVideo uploads one MP4 to /act_<id>/advideos and returns the new video id. ONE attempt:
// each accepted upload creates a new video object, so a repeat could leave a second copy. The
// outcome vocabulary is uploadImageAttempt's: a pre-send dial failure is a plain error, a non-2xx
// is an *APIError (the body is never quoted unless it is a Graph envelope, since the request's
// first part is the caller's video), and an unreadable 2xx or one naming no usable id is an
// ambiguous *transportError. The bytes are referenced in place, never copied.
func (c *Client) uploadVideo(ctx context.Context, video []byte, contentType string) (string, error) {
	if c.creds.AccessToken == "" {
		return "", fmt.Errorf("meta access token is not configured")
	}
	if len(video) == 0 {
		return "", fmt.Errorf("meta video upload called with no bytes")
	}
	prefix, suffix, contentTypeHeader, err := multipartFramingNamed(contentType, "creative.mp4")
	if err != nil {
		return "", err
	}
	path := "/" + c.account.AccountID + "/advideos"
	body := io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(video), bytes.NewReader(suffix))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = int64(len(prefix)) + int64(len(video)) + int64(len(suffix))
	req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
	req.Header.Set("Content-Type", contentTypeHeader)

	resp, derr := c.httpClient.Do(req)
	if derr != nil {
		if isPreSendDialError(derr) {
			return "", fmt.Errorf("meta API %s %s: %w", http.MethodPost, path, derr)
		}
		return "", &transportError{Method: http.MethodPost, Path: path, Err: derr}
	}
	status := resp.StatusCode
	raw, readErr := readCappedBody(resp.Body)
	if status < 200 || status >= 300 {
		apiErr := &APIError{StatusCode: status, Method: http.MethodPost, Path: path}
		var env graphErrorEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			apiErr.copyEnvelope(env.Error)
			apiErr.Message = env.Error.Message
			if readErr != nil {
				apiErr.EnvelopeUnreadable = true
			}
		} else {
			apiErr.EnvelopeUnreadable = true
			if readErr != nil {
				apiErr.Message = fmt.Sprintf("read response body: %v", readErr)
			}
		}
		return "", apiErr
	}
	if readErr != nil {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("read response body: %w", readErr)}
	}
	if int64(len(raw)) > maxResponseBody {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBody)}
	}
	var out createResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("decode response: %w", err)}
	}
	id := strings.TrimSpace(out.ID)
	if !numericIDRE.MatchString(id) {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: errors.New("video upload returned no usable video id")}
	}
	return id, nil
}
