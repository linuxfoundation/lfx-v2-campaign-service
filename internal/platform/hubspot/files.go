// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
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

	data, contentType, filename, err := c.downloadImage(ctx, imageURL)
	if err != nil {
		return "", err
	}

	return c.uploadFileBytes(ctx, data, contentType, filename)
}

// imageAcceptHeader is what the download advertises. Go sends no Accept header of its own, which
// leaves a content-negotiating origin free to answer with a format this service cannot decode. It
// can only decode (and so only re-host) the three formats named here, so ask for those first. This
// alone does not fix a CDN that has already cached the wrong format under the plain URL; that is
// what the retry in downloadImage is for.
const imageAcceptHeader = "image/jpeg,image/png,image/gif;q=0.9,*/*;q=0.1"

// formatRetryParam is the query parameter appended on the one retry downloadImage makes when an
// origin hands back a modern format. It exists only to give the request a fresh shared-cache key.
// The value is fixed rather than random so the retry is reproducible and itself cacheable.
const formatRetryParam = "lfx_fmt=1"

// downloadImage fetches imageURL, validates that it actually served an image, and returns bytes
// this service is willing to re-host, mirroring the prototype's upload_image_to_hubspot: a page
// returning an HTML error page (wrong URL, expired link) must be rejected before we waste a
// HubSpot upload on it.
//
// A source answering with AVIF or WebP gets exactly one retry. The failure this exists for: an
// origin behind a CDN that ignores `Vary: Accept` caches whichever format the first visitor was
// served, then hands it to every client regardless of the Accept they send — so the plain
// "hero.jpg" URL returns AVIF to us while the origin still holds the JPEG. A new cache key (see
// formatRetryParam) reaches the origin, which negotiates honestly on imageAcceptHeader. The retry
// is bounded to one attempt and only for those two declared types, so HTML claiming to be a PNG is
// still refused after a single fetch, and every fetch goes through the same guarded client.
//
// Failure reports the FIRST response, not the retry's: the retry is a best-effort guess at a
// different URL, and its failure (a signed URL rejecting the extra parameter, say) says nothing
// about why the original was refused.
func (c *Client) downloadImage(ctx context.Context, imageURL string) (data []byte, contentType, filename string, err error) {
	body, declaredType, ferr := c.fetchImage(ctx, imageURL)
	if ferr != nil {
		return nil, "", "", ferr
	}

	ext, mime, sniffErr := sniffImageFormat(body)
	if sniffErr == nil {
		return body, mime, deriveImageFilename(imageURL, ext, body), nil
	}
	if !isModernImageType(declaredType) {
		return nil, "", "", sniffErr
	}

	if retryURL, ok := withFormatRetryParam(imageURL); ok {
		if retryBody, _, rerr := c.fetchImage(ctx, retryURL); rerr == nil {
			if rext, rmime, rsniffErr := sniffImageFormat(retryBody); rsniffErr == nil {
				return retryBody, rmime, deriveImageFilename(imageURL, rext, retryBody), nil
			}
		}
	}
	// declaredType is one of the two constants isModernImageType accepts, so naming it cannot echo
	// attacker-chosen header text into the error.
	return nil, "", "", fmt.Errorf("%w (the source served %s; this service re-hosts only jpeg, png and gif)",
		sniffErr, declaredType)
}

// isModernImageType reports whether a declared Content-Type is a format that real browsers
// receive from content-negotiating origins but this service cannot decode.
func isModernImageType(declaredType string) bool {
	return declaredType == "image/avif" || declaredType == "image/webp"
}

// withFormatRetryParam returns imageURL with formatRetryParam appended, or false when the URL is
// unparsable or already carries it (so a retry URL is never itself retried). The existing query is
// kept byte-for-byte rather than re-encoded, so a signed URL is changed by exactly one parameter.
func withFormatRetryParam(imageURL string) (string, bool) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return "", false
	}
	for _, kv := range strings.Split(u.RawQuery, "&") {
		if kv == formatRetryParam {
			return "", false
		}
	}
	if u.RawQuery == "" {
		u.RawQuery = formatRetryParam
	} else {
		u.RawQuery += "&" + formatRetryParam
	}
	return u.String(), true
}

// fetchImage performs ONE guarded GET of imageURL and validates that it served an image-typed
// response within the size cap, returning the body and its normalised (lower-case, parameter-free)
// declared Content-Type. It does not decide whether the BYTES are a format this service re-hosts —
// that is sniffImageFormat's job, applied by downloadImage.
//
// imageURL is CALLER-SUPPLIED (a hero/sponsor image scraped from an operator-named
// event page), so this fetch is an SSRF sink and goes through c.downloadClient, which
// carries eventurl's dial-time address guard. Two properties of THIS sink make the
// guard load-bearing rather than defensive:
//
//   - the response body is re-hosted in the LF portal as a PUBLIC_INDEXABLE file and
//     its CDN URL returned, so anything fetched becomes publicly readable. An
//     unguarded fetch of 169.254.169.254 or a cluster-internal service would not just
//     leak to this process, it would publish.
//   - the Content-Type check below is NOT a second line of defence. The server that
//     answers is the attacker's to choose, so it can claim image/png for any bytes.
//
// The scheme is checked here rather than left to the dialer because a non-http(s)
// scheme never reaches a dial at all: file:// and similar are refused by Transport
// with an error that reads like a network failure, and the guard would never run.
func (c *Client) fetchImage(ctx context.Context, imageURL string) (body []byte, declaredType string, err error) {
	parsed, perr := url.Parse(imageURL)
	if perr != nil {
		// NOT %w: url.Parse returns a *url.Error whose text embeds the COMPLETE input, so
		// wrapping it copies a signed query into every string this error reaches. The parse
		// failed, so there is nothing safe to name — the reason alone is what is actionable.
		reason := "invalid URL"
		var uerr *url.Error
		if errors.As(perr, &uerr) && uerr.Err != nil {
			reason = uerr.Err.Error()
		}
		return nil, "", fmt.Errorf("hubspot: image URL is not parsable: %s", reason)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("hubspot: image URL scheme %q is not http(s)", parsed.Scheme)
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if rerr != nil {
		// Same hazard as the parse above: this error renders the request URL verbatim.
		return nil, "", fmt.Errorf("hubspot: build image download request for %s: %s",
			redact.URLUserinfo(imageURL), errors.Unwrap(rerr))
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", imageAcceptHeader)

	client := c.downloadClient
	if client == nil {
		// Only reachable via a zero-value Client built outside NewClient. Falling back
		// to an unguarded &http.Client{} here would make the guard depend on how the
		// struct was constructed, which is exactly the bug this defends against.
		client = eventurl.NewGuardedRedirectClient(imageDownloadTimeout)
	}
	resp, derr := client.Do(req)
	if derr != nil {
		// NOT %w on derr: http.Client.Do returns a *url.Error whose Error() renders the full
		// request URL, and a hero URL is frequently signed — so wrapping it verbatim copies any
		// credential in the query string into every error string this bubbles into, including
		// logs. Unwrap to the cause, which carries the network reason without the URL, and name
		// the target with its query removed.
		cause := derr
		var uerr *url.Error
		if errors.As(derr, &uerr) && uerr.Err != nil {
			cause = uerr.Err
		}
		return nil, "", fmt.Errorf("hubspot: download image from %s: %w", redact.URLUserinfo(imageURL), cause)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("hubspot: download image returned status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("hubspot: source URL did not return an image (content-type %q)", ct)
	}

	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxImageDownloadBytes+1))
	if rerr != nil {
		return nil, "", fmt.Errorf("hubspot: read downloaded image: %w", rerr)
	}
	if len(data) > maxImageDownloadBytes {
		return nil, "", fmt.Errorf("hubspot: downloaded image exceeds %d bytes", maxImageDownloadBytes)
	}

	// The returned type is the DECLARED one and is NOT trusted: the responding server chooses
	// Content-Type, so it establishes intent, not content — an attacker can serve HTML as
	// image/png. downloadImage decides what the bytes are by decoding them (sniffImageFormat),
	// which matters here beyond the usual because the bytes are re-hosted in the LF portal as a
	// PUBLIC_INDEXABLE file: an unvalidated payload becomes publicly served content under an LF
	// domain. The declared type is used only to decide whether a retry is worth making.
	return data, ct, nil
}

// allowedImageFormats are the formats this service will re-host, keyed by the name
// image.DecodeConfig reports. GIF is included because event pages use animated banners; SVG is
// absent deliberately — it is a document format that can carry script, and re-hosting one as a
// public LF-served asset is exactly the thing the sniff exists to prevent.
var allowedImageFormats = map[string]struct{ ext, mime string }{
	// The extension and the MIME type are NOT the same string: image/jpeg is the registered
	// type while .jpg is the conventional extension, so deriving one from the other produces
	// the invalid "image/jpg" that some consumers reject.
	"jpeg": {ext: "jpg", mime: "image/jpeg"},
	"png":  {ext: "png", mime: "image/png"},
	"gif":  {ext: "gif", mime: "image/gif"},
}

// WebP and AVIF are absent because the standard library has no decoder for either and this
// service takes no image-codec dependency for one. The consequence is a REFUSAL, not a bypass: such
// a hero is rejected rather than silently re-hosted unvalidated. downloadImage first tries to get a
// hostable rendition of the same URL (an Accept header, then one retry under a fresh cache key);
// transcoding would be the only way to accept an origin that serves nothing else.

// sniffImageFormat decodes just the image header and returns the canonical extension and the
// registered MIME type for the format the BYTES are in.
func sniffImageFormat(body []byte) (ext, mime string, err error) {
	_, format, derr := image.DecodeConfig(bytes.NewReader(body))
	if derr != nil {
		return "", "", fmt.Errorf("hubspot: source URL did not return a decodable image: %w", derr)
	}
	allowed, ok := allowedImageFormats[format]
	if !ok {
		return "", "", fmt.Errorf("hubspot: image format %q is not one this service re-hosts", format)
	}
	return allowed.ext, allowed.mime, nil
}

// deriveImageFilename keeps the source URL's basename for recognisability, but the EXTENSION
// always comes from the sniffed format rather than the URL.
//
// The URL's own extension is caller-controlled and the upload is PUBLIC_INDEXABLE, so carrying
// it through is what would let `payload.html` be re-hosted under an LF domain with an extension
// that invites a browser to render it. The bytes decoded as an image; the name must say so too.
func deriveImageFilename(imageURL, ext string, body []byte) string {
	stem := "email_img"
	if u, perr := url.Parse(imageURL); perr == nil {
		base := path.Base(u.Path)
		// A trailing slash makes path.Base return the last DIRECTORY, which is not a filename
		// and would name every image after its folder. The original required a dot for the same
		// reason; that test has to happen BEFORE the extension is stripped.
		if strings.HasSuffix(u.Path, "/") {
			base = ""
		}
		if base != "" && base != "/" && base != "." {
			if i := strings.LastIndex(base, "."); i > 0 {
				base = base[:i]
			}
			// Path separators cannot appear (path.Base strips them), but a name is still
			// caller-supplied text going into a multipart filename field.
			base = strings.Map(func(r rune) rune {
				if r == '"' || r == '\\' || r == '/' || r < ' ' {
					return -1
				}
				return r
			}, base)
			// RUNES, not bytes: len() counts bytes and base[:80] can cut mid-character, putting
			// an invalid UTF-8 sequence into the multipart filename field. A scraped basename is
			// frequently non-ASCII (an event name in Japanese or German), so this is ordinary
			// input rather than a contrived one.
			if runes := []rune(base); len(runes) > 80 {
				base = string(runes[:80])
			}
			if base != "" {
				stem = base
			}
		}
	}
	// A content hash disambiguates the shared /email-staging folder, which uploads with
	// overwrite:true. Source basenames collide constantly -- "hero.png" is the common case, not
	// the exotic one -- so without this, two campaigns uploading different images under the same
	// name silently overwrite each other and both emails render whichever landed last.
	//
	// Keyed on the BYTES, so the same image re-uploaded keeps one file (the overwrite is then
	// harmless and idempotent) while different bytes can never share a name.
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%s-%x.%s", stem, sum[:6], ext)
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
	// The +1 above exists to DETECT overflow; reading it and not acting on it is the bug. A body
	// at the cap is a truncated read, so the JSON that parses out of it describes only part of
	// what the server sent -- and this is a non-idempotent create, so accepting it would confirm
	// an upload whose real outcome is unknown. Ambiguous, not failed: the file may well exist.
	if len(raw) > maxResponseBody {
		return "", &apiError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodPost,
			Path:       filesPath,
			Ambiguous:  true,
		}
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
