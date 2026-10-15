// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Creative images for an AUTHORED promoted-only tweet (LFXV2-2665).
//
// The X Ads API attaches media to a tweet it authors through the `media_keys` parameter of
// POST accounts/:account_id/tweet — "Include up to 4 images, 1 animated GIF, or 1 video. Note:
// The media asset must be in the account's Media Library" (docs.x.com/x-ads-api/creatives/
// reference). So an image takes three moves, in this order:
//
//  1. upload the bytes through the X API v2 chunked media upload (POST /2/media/upload/
//     initialize, /{id}/append, /{id}/finalize on api.x.com, media_category=tweet_image),
//     which accepts OAuth 1.0a user context — the same four-part credential every Ads API call
//     here already signs with;
//  2. add the returned media_key to the AD ACCOUNT's Media Library (POST accounts/:account_id/
//     media_library), which is what makes it usable by the account's promotable users rather
//     than only by the token's own user;
//  3. pass that media_key as media_keys on the tweet create.
//
// IDEMPOTENCY. An upload mints a new media id every time — there is no content addressing like
// Meta's image_hash — so a retried dispatch would re-upload. The library entry is therefore
// NAMED deterministically from the image's SHA-256 (mediaLibraryName) and looked up by that
// name before anything is uploaded, exactly as campaigns and line items are found-or-created
// by name. One (ad account, image) pair is uploaded once; every later dispatch of the same
// bytes to the same account reuses the recorded library entry, and the media_key is recorded
// in CampaignResult.MediaKey.
//
// ORDER. All of this runs BEFORE the campaign POST: an image that cannot be uploaded fails the
// create while nothing billable exists. A library entry left behind by a later failure is
// reused by the retry, not duplicated.

const (
	// DefaultMediaUploadBaseURL is the X API v2 origin that serves media upload. It is NOT the
	// Ads API origin (DefaultBaseURL): media upload is an X API endpoint, not an Ads API one.
	DefaultMediaUploadBaseURL = "https://api.x.com"

	// MaxImageBytes is X's documented image upload limit ("Images: 5 MB",
	// docs.x.com/x-api/media/quickstart/media-upload-chunked). The service stores assets up to
	// 30 MiB for Meta, so a stored asset can be legal here and too large for X; the dispatcher
	// refuses it on its stored size before loading it, and the client refuses it again before
	// any request.
	MaxImageBytes = 5 << 20

	// defaultMediaChunkSize is the APPEND segment size. X documents a 5 MB per-segment maximum;
	// 4 MiB stays under it with margin, so a maximum-size image is two appends.
	defaultMediaChunkSize = 4 << 20

	// mediaLibraryPageSize is the count requested on the Media Library lookup — X's documented
	// maximum for that endpoint (Max: 50). Like listPageSize it is also the denominator of the
	// short-page test, so it is one constant.
	mediaLibraryPageSize = 50

	// maxMediaStatusPolls bounds how many times a pending finalize is re-checked. Images are
	// normally processed synchronously and return no processing_info at all.
	maxMediaStatusPolls = 10
	// defaultMediaPollCap caps one wait between status polls regardless of what X asks for.
	defaultMediaPollCap = 5 * time.Second

	// mediaLibraryNamePrefix prefixes the deterministic library name. Letters and hex digits
	// only, so the whole name is a single search term for the library's `q` filter.
	mediaLibraryNamePrefix = "lfxasset"
)

// ImageInput is a creative image for the authored tweet. Bytes are read, never retained or
// mutated; MIME is the stored asset's verified type.
type ImageInput struct {
	Bytes []byte
	MIME  string
}

// mediaIDRe is the v2 upload session id shape (docs: pattern ^[0-9]{1,19}$). It is interpolated
// into the append/finalize PATH, so it is validated before use rather than trusted.
var mediaIDRe = regexp.MustCompile(`^[0-9]{1,19}$`)

// mediaKeyRe is a media_key: a numeric type prefix, an underscore, and the numeric media id
// (e.g. "3_931236738554519552"). It is sent as a query parameter and persisted, so a value of
// any other shape from a 2xx body is treated as no key at all.
var mediaKeyRe = regexp.MustCompile(`^[0-9]{1,4}_[0-9]{1,19}$`)

// WithMediaUploadBaseURL overrides the media upload origin (default DefaultMediaUploadBaseURL).
// Tests point it at the same stand-in server as WithBaseURL.
func WithMediaUploadBaseURL(u string) Option {
	return func(c *Client) { c.uploadBaseURL = strings.TrimRight(u, "/") }
}

// mediaLibraryName is the deterministic Media Library name for an image: the prefix plus the
// lowercase hex SHA-256 of its bytes (72 characters, inside the 100-character name limit).
// Computed HERE from the bytes actually uploaded, never from a caller-supplied checksum, so a
// name can never describe different bytes than the entry holds.
func mediaLibraryName(image []byte) string {
	sum := sha256.Sum256(image)
	return mediaLibraryNamePrefix + hex.EncodeToString(sum[:])
}

// validateImageInput is the up-front check CreateCampaign runs before any request.
func validateImageInput(img *ImageInput) error {
	if len(img.Bytes) == 0 {
		return fmt.Errorf("invalid creative image: no image bytes")
	}
	if len(img.Bytes) > MaxImageBytes {
		return fmt.Errorf("invalid creative image: %d bytes exceeds X's %d-byte image limit", len(img.Bytes), MaxImageBytes)
	}
	switch img.MIME {
	case "image/png", "image/jpeg":
	default:
		return fmt.Errorf("invalid creative image: type %s is not image/png or image/jpeg", domain.MIMEValue(img.MIME))
	}
	// The stored asset was sniffed at upload; this re-check costs one look at 512 bytes and
	// keeps the client correct for a caller that is not the dispatcher.
	if sniffed := http.DetectContentType(img.Bytes); sniffed != img.MIME {
		// sniffed is net/http's own fixed content-type vocabulary, not caller text, so it is
		// rendered as is; the declared MIME goes through domain.MIMEValue.
		return fmt.Errorf("invalid creative image: bytes are %s, not the declared %s", sniffed, domain.MIMEValue(img.MIME))
	}
	return nil
}

// mediaLibraryEntry is the subset of a Media Library object this client reads.
type mediaLibraryEntry struct {
	Name      *string `json:"name"`
	MediaKey  string  `json:"media_key"`
	MediaType string  `json:"media_type"`
	Deleted   bool    `json:"deleted"`
}

// findMediaByName returns the media_key of the account's live IMAGE library entry named name,
// or "" when there is none. It follows findByName's rules: a short page is conclusively last, a
// FULL page with no usable cursor is inconclusive and errors rather than reporting not-found —
// not-found is answered with an upload, so it must not rest on a truncated read.
func (c *Client) findMediaByName(ctx context.Context, name string) (string, error) {
	path := "media_library?count=" + strconv.Itoa(mediaLibraryPageSize) + "&media_type=IMAGE&q=" + url.QueryEscape(name)
	seen := map[string]struct{}{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		resp, err := c.requestPage(ctx, path, cursor)
		if err != nil {
			return "", fmt.Errorf("x media library lookup failed: %w", err)
		}
		var entries []mediaLibraryEntry
		if resp != nil && len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, &entries); err != nil {
				return "", fmt.Errorf("x media library lookup returned an undecodable page")
			}
		}
		if resp == nil || entries == nil {
			// No data field: not an empty page, an unanswered one. Refuse rather than upload
			// on the strength of it — the same rule findByName applies.
			return "", fmt.Errorf("x media library lookup returned a page with no data field, so an existing upload of this image cannot be ruled out")
		}
		for _, e := range entries {
			if e.Deleted || e.Name == nil || *e.Name != name || !strings.EqualFold(e.MediaType, "IMAGE") {
				continue
			}
			if key := strings.TrimSpace(e.MediaKey); mediaKeyRe.MatchString(key) {
				return key, nil
			}
		}
		switch cursorVerdict(resp) {
		case cursorMore:
		case cursorExhausted:
			return "", nil
		default:
			if len(entries) < mediaLibraryPageSize {
				return "", nil
			}
			return "", fmt.Errorf("x media library lookup returned a full page with no usable next_cursor, so an existing upload of this image cannot be ruled out")
		}
		if _, dup := seen[resp.NextCursor]; dup {
			return "", fmt.Errorf("x media library lookup did not terminate (repeated page cursor)")
		}
		seen[resp.NextCursor] = struct{}{}
		cursor = resp.NextCursor
	}
	return "", fmt.Errorf("x media library lookup exceeded %d pages with more results remaining", maxListPages)
}

// ensureImageMedia returns a media_key for img that is in the account's Media Library,
// uploading and adding it only when no entry for these bytes exists yet.
//
// Every error it returns precedes the campaign POST, so the caller returns (nil, err): no
// campaign, line item or tweet exists. Its messages name the stage and the outcome class, and
// never carry upstream body text (the request layer's errors render method, path and status).
func (c *Client) ensureImageMedia(ctx context.Context, img *ImageInput, steps *[]string) (string, error) {
	name := mediaLibraryName(img.Bytes)
	existing, err := c.findMediaByName(ctx, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		*steps = append(*steps, fmt.Sprintf("Reusing creative image already in the account's Media Library: %s", existing))
		return existing, nil
	}

	mediaID, mediaKey, err := c.uploadImageChunked(ctx, img)
	if err != nil {
		// Nothing reached the Media Library: an upload session that stalls expires on its own
		// (expires_after_secs), so a retry starts a fresh one and cannot duplicate a library
		// entry. Stated for both outcome classes, because only the wording differs.
		if createOutcomeAmbiguous(err) {
			return "", fmt.Errorf("x creative image upload UNCONFIRMED before any campaign was created (an unfinished upload session expires on its own; nothing was added to the Media Library): %w", err)
		}
		return "", fmt.Errorf("x creative image upload failed before any campaign was created: %w", err)
	}

	if err := c.pace(ctx); err != nil {
		return "", fmt.Errorf("x media library add aborted before any campaign was created: %w", err)
	}
	resp, err := c.createRequest(ctx, "media_library", map[string]string{
		"media_key": mediaKey,
		"name":      name,
		"file_name": name,
	}, false /* NOT idempotent: a repeat adds a second library entry */)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			// The entry MAY exist under the deterministic name; the retry's lookup finds it.
			return "", fmt.Errorf("x media library add UNCONFIRMED before any campaign was created (the image may already be in the account's Media Library; a retry reuses it by name): %w", err)
		}
		return "", fmt.Errorf("x media library add failed before any campaign was created: %w", err)
	}
	var added struct {
		MediaKey string `json:"media_key"`
	}
	if resp != nil && len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &added)
	}
	if strings.TrimSpace(added.MediaKey) != mediaKey {
		return "", fmt.Errorf("x media library add UNCONFIRMED before any campaign was created (X returned a 2xx without the uploaded media key; the image may already be in the account's Media Library; a retry reuses it by name)")
	}
	*steps = append(*steps, fmt.Sprintf("Creative image uploaded (media %s) and added to the account's Media Library: %s", mediaID, mediaKey))
	return mediaKey, nil
}

// uploadImageChunked runs initialize -> append xN -> finalize (-> status polls when X reports
// processing) and returns the media id and media_key. Every POST is paced through the account
// pacer, like every other write this client issues.
func (c *Client) uploadImageChunked(ctx context.Context, img *ImageInput) (string, string, error) {
	initBody, err := json.Marshal(map[string]any{
		"media_type":     img.MIME,
		"total_bytes":    len(img.Bytes),
		"media_category": "tweet_image",
	})
	if err != nil {
		return "", "", fmt.Errorf("encode media upload initialize: %w", err)
	}
	if err := c.pace(ctx); err != nil {
		return "", "", err
	}
	initPath := "/2/media/upload/initialize"
	resp, err := c.doBodyRequest(ctx, c.uploadBaseURL+initPath, initPath, "application/json", initBody)
	if err != nil {
		return "", "", err
	}
	var initData struct {
		ID       string `json:"id"`
		MediaKey string `json:"media_key"`
	}
	if resp != nil && len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &initData)
	}
	mediaID := strings.TrimSpace(initData.ID)
	if !mediaIDRe.MatchString(mediaID) {
		return "", "", fmt.Errorf("x media upload initialize returned no usable media id")
	}

	chunk := c.mediaChunkSize
	if chunk <= 0 {
		chunk = defaultMediaChunkSize
	}
	appendPath := "/2/media/upload/" + mediaID + "/append"
	for seg, off := 0, 0; off < len(img.Bytes); seg, off = seg+1, off+chunk {
		end := off + chunk
		if end > len(img.Bytes) {
			end = len(img.Bytes)
		}
		prefix, suffix, ctype, ferr := appendFraming(seg)
		if ferr != nil {
			return "", "", ferr
		}
		if err := c.pace(ctx); err != nil {
			return "", "", err
		}
		if _, err := c.doBodyRequest(ctx, c.uploadBaseURL+appendPath, appendPath, ctype, prefix, img.Bytes[off:end], suffix); err != nil {
			return "", "", fmt.Errorf("segment %d: %w", seg, err)
		}
	}

	if err := c.pace(ctx); err != nil {
		return "", "", err
	}
	finalizePath := "/2/media/upload/" + mediaID + "/finalize"
	resp, err = c.doBodyRequest(ctx, c.uploadBaseURL+finalizePath, finalizePath, "", nil)
	if err != nil {
		return "", "", err
	}
	fin, ok := decodeMediaState(resp)
	if !ok {
		return "", "", fmt.Errorf("x media upload finalize returned an undecodable body")
	}
	if err := c.awaitMediaProcessing(ctx, mediaID, fin); err != nil {
		return "", "", err
	}
	mediaKey := strings.TrimSpace(fin.MediaKey)
	if mediaKey == "" {
		mediaKey = strings.TrimSpace(initData.MediaKey)
	}
	if !mediaKeyRe.MatchString(mediaKey) {
		return "", "", fmt.Errorf("x media upload returned no usable media key")
	}
	return mediaID, mediaKey, nil
}

// mediaState is the finalize / status response data this client reads.
type mediaState struct {
	MediaKey       string `json:"media_key"`
	ProcessingInfo *struct {
		State          string `json:"state"`
		CheckAfterSecs int    `json:"check_after_secs"`
	} `json:"processing_info"`
}

func decodeMediaState(resp *apiResponse) (mediaState, bool) {
	var st mediaState
	if resp == nil || len(resp.Data) == 0 {
		return st, true
	}
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		return st, false
	}
	return st, true
}

// awaitMediaProcessing returns once the finalized media is usable. No processing_info — the
// normal answer for an image — or state "succeeded" is done; "failed" is a definite refusal;
// "pending"/"in_progress" is re-checked through the read-only STATUS command, a bounded number
// of times.
func (c *Client) awaitMediaProcessing(ctx context.Context, mediaID string, st mediaState) error {
	for poll := 0; ; poll++ {
		if st.ProcessingInfo == nil {
			return nil
		}
		switch strings.ToLower(st.ProcessingInfo.State) {
		case "succeeded", "":
			return nil
		case "failed":
			return fmt.Errorf("x rejected the creative image during processing")
		}
		if poll >= maxMediaStatusPolls {
			return fmt.Errorf("x did not finish processing the creative image after %d status checks", maxMediaStatusPolls)
		}
		wait := time.Duration(st.ProcessingInfo.CheckAfterSecs) * time.Second
		if wait <= 0 || wait > c.mediaPollCap {
			wait = c.mediaPollCap
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
		statusPath := "/2/media/upload"
		statusURL := c.uploadBaseURL + statusPath + "?command=STATUS&media_id=" + mediaID
		resp, err := c.doRequestAbs(ctx, http.MethodGet, statusURL, statusPath, nil, true /* idempotent: a read */)
		if err != nil {
			return fmt.Errorf("x media processing status check failed: %w", err)
		}
		next, ok := decodeMediaState(resp)
		if !ok {
			return fmt.Errorf("x media processing status returned an undecodable body")
		}
		if next.MediaKey == "" {
			next.MediaKey = st.MediaKey
		}
		st = next
	}
}

// appendFraming builds the multipart framing for one APPEND request — a `segment_index` field
// and the `media` file part's headers as the PREFIX, the closing boundary as the SUFFIX — with
// the chunk itself absent, so the request body references the caller's bytes in place instead
// of copying them into a buffer outside the dispatcher's asset reservation (the same framing
// the Meta client uses for /adimages).
func appendFraming(segment int) (prefix, suffix []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("segment_index", strconv.Itoa(segment)); err != nil {
		return nil, nil, "", fmt.Errorf("frame media append: %w", err)
	}
	if _, err := w.CreateFormFile("media", "media"); err != nil {
		return nil, nil, "", fmt.Errorf("frame media append: %w", err)
	}
	prefix = append([]byte(nil), buf.Bytes()...)
	buf.Reset()
	if err := w.Close(); err != nil {
		return nil, nil, "", fmt.Errorf("frame media append: %w", err)
	}
	return prefix, append([]byte(nil), buf.Bytes()...), w.FormDataContentType(), nil
}

// doBodyRequest issues ONE OAuth1-signed POST carrying a body, for the media upload endpoints
// (the Ads API endpoints carry no body; see doRequestAbs). It classifies outcomes exactly as
// doRequestAbs does — a pre-send dial failure is a definite preSendError, a failure after the
// request may have been sent or an unreadable 2xx is an ambiguous transportError, and a non-2xx
// is an apiError carrying status only — and it never retries: none of these writes converges
// on a repeat, so a 429 is returned at once (and classified ambiguous by
// createOutcomeAmbiguous, as every other mutating 429 here is).
//
// The body is NOT part of the OAuth 1.0a signature: RFC 5849 §3.4.1.3.1 signs entity-body
// parameters only for application/x-www-form-urlencoded, which neither JSON nor multipart is.
// The caller paces.
func (c *Client) doBodyRequest(ctx context.Context, reqURL, logPath, contentType string, parts ...[]byte) (*apiResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errRequestContextAlreadyDone, err)
	}
	method := http.MethodPost
	authHeader, err := c.buildOAuthHeader(method, reqURL, nil)
	if err != nil {
		return nil, err
	}
	readers := make([]io.Reader, 0, len(parts))
	var length int64
	for _, p := range parts {
		readers = append(readers, bytes.NewReader(p))
		length += int64(len(p))
	}
	var body io.Reader = http.NoBody
	if length > 0 {
		body = io.MultiReader(readers...)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Explicit: net/http cannot infer a length from an io.MultiReader and would otherwise send
	// the upload chunked.
	req.ContentLength = length
	req.Header.Set("Authorization", authHeader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isPreSendDialError(err) {
			return nil, &preSendError{Method: method, Path: logPath, err: err}
		}
		return nil, &transportError{Method: method, Path: logPath, err: err}
	}
	respBody, readErr := readAll(resp)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: logPath, ErrorCodes: parseErrorCodes(respBody), errorParams: parseErrorParams(respBody)}
	}
	if readErr != nil {
		return nil, &transportError{Method: method, Path: logPath, err: fmt.Errorf("read response body: %w", readErr)}
	}
	var out apiResponse
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, &transportError{Method: method, Path: logPath, err: errors.New("decode response")}
		}
	}
	out.raw = respBody
	return &out, nil
}
