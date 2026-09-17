// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"time"
)

const filesPath = "/files/v3/files"

// maxImageDownloadBytes caps how large a scraped hero/sponsor image we'll re-host can
// be. A marketing email image has no legitimate reason to exceed this.
const maxImageDownloadBytes = 25 << 20 // 25 MiB

const imageDownloadTimeout = 15 * time.Second

// UploadImage downloads the image at imageURL and re-hosts it in HubSpot's file
// manager under /email-staging, returning the HubSpot-hosted CDN URL. MUTATING
// (creates a file in the portal).
//
// Re-hosting rather than referencing imageURL directly in the email is deliberate:
// scraped event-page images often sit behind hotlink protection or short-lived
// signed URLs that would break once the email is actually sent, so the sent email
// must reference a HubSpot-hosted copy.
func (c *Client) UploadImage(ctx context.Context, imageURL string) (string, error) {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return "", fmt.Errorf("hubspot: UploadImage requires a non-empty image URL")
	}
	if c.creds.PrivateAppToken == "" {
		return "", fmt.Errorf("hubspot: missing private-app token")
	}

	data, contentType, filename, err := downloadImage(ctx, imageURL)
	if err != nil {
		return "", err
	}

	return c.uploadFileBytes(ctx, data, contentType, filename)
}

// downloadImage fetches imageURL and validates that it actually served an image,
// mirroring the prototype's upload_image_to_hubspot: a page returning an HTML error
// page (wrong URL, expired link) must be rejected before we waste a HubSpot upload
// on it.
func downloadImage(ctx context.Context, imageURL string) (data []byte, contentType, filename string, err error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if rerr != nil {
		return nil, "", "", fmt.Errorf("hubspot: build image download request: %w", rerr)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	client := &http.Client{Timeout: imageDownloadTimeout}
	resp, derr := client.Do(req)
	if derr != nil {
		return nil, "", "", fmt.Errorf("hubspot: download image: %w", derr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("hubspot: download image returned status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", "", fmt.Errorf("hubspot: source URL did not return an image (content-type %q)", ct)
	}

	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxImageDownloadBytes+1))
	if rerr != nil {
		return nil, "", "", fmt.Errorf("hubspot: read downloaded image: %w", rerr)
	}
	if len(body) > maxImageDownloadBytes {
		return nil, "", "", fmt.Errorf("hubspot: downloaded image exceeds %d bytes", maxImageDownloadBytes)
	}

	return body, ct, deriveImageFilename(imageURL, ct), nil
}

// deriveImageFilename mirrors the prototype's filename derivation: use the source
// URL's last path segment when it looks like a real filename, otherwise synthesize
// one from the content type.
func deriveImageFilename(imageURL, contentType string) string {
	if u, perr := url.Parse(imageURL); perr == nil {
		base := path.Base(u.Path)
		if base != "" && base != "/" && base != "." && strings.Contains(base, ".") {
			if len(base) > 80 {
				base = base[:80]
			}
			return base
		}
	}
	ext := strings.TrimPrefix(contentType, "image/")
	if ext == "jpeg" {
		ext = "jpg"
	}
	if ext == "" {
		ext = "jpg"
	}
	return "email_img." + ext
}

type fileUploadResponse struct {
	URL string `json:"url"`
}

// uploadFileBytes POSTs a multipart/form-data body to HubSpot's Files API. This
// cannot reuse doRequest (JSON-only), so it builds and sends the HTTP request
// directly, following this package's apiError/transportError/unconfirmed
// conventions for a mutating, non-idempotent call.
func (c *Client) uploadFileBytes(ctx context.Context, data []byte, contentType, filename string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fileHeader := textproto.MIMEHeader{}
	fileHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if contentType != "" {
		fileHeader.Set("Content-Type", contentType)
	}
	part, perr := mw.CreatePart(fileHeader)
	if perr != nil {
		return "", fmt.Errorf("hubspot: build file upload body: %w", perr)
	}
	if _, werr := part.Write(data); werr != nil {
		return "", fmt.Errorf("hubspot: build file upload body: %w", werr)
	}
	if werr := mw.WriteField("options", `{"access":"PUBLIC_INDEXABLE","overwrite":true}`); werr != nil {
		return "", fmt.Errorf("hubspot: build file upload body: %w", werr)
	}
	if werr := mw.WriteField("folderPath", "/email-staging"); werr != nil {
		return "", fmt.Errorf("hubspot: build file upload body: %w", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		return "", fmt.Errorf("hubspot: build file upload body: %w", cerr)
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+filesPath, &buf)
	if rerr != nil {
		return "", fmt.Errorf("hubspot: build file upload request: %w", rerr)
	}
	req.Header.Set("Authorization", "Bearer "+c.creds.PrivateAppToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, derr := c.httpClient.Do(req)
	if derr != nil {
		return "", &transportError{Method: http.MethodPost, Path: filesPath, err: derr, Mutating: true}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if rerr != nil {
		return "", &transportError{Method: http.MethodPost, Path: filesPath, err: rerr, Mutating: true}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &apiError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodPost,
			Path:       filesPath,
			Ambiguous:  isAmbiguousMutatingStatus(resp.StatusCode),
		}
	}

	var out fileUploadResponse
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return "", unconfirmed("hubspot: file upload UNCONFIRMED (2xx with an undecodable body)", uerr)
	}
	if out.URL == "" {
		return "", unconfirmed("hubspot: file upload UNCONFIRMED (2xx with no url; a file may have been created)", nil)
	}
	return out.URL, nil
}
