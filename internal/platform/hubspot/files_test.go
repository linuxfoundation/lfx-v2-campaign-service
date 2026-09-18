// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadImage_HappyPathReturnsHostedURL(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG(t))
	}))
	t.Cleanup(imgSrv.Close)

	var gotFilename, gotFolderPath, gotOptions string
	var gotAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != filesPath {
			t.Errorf("unexpected upload path %s", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content-type, got %q (%v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				t.Fatalf("read multipart part: %v", perr)
			}
			switch part.FormName() {
			case "file":
				gotFilename = part.FileName()
				data, _ := io.ReadAll(part)
				if len(data) == 0 {
					t.Errorf("uploaded no bytes")
				}
			case "folderPath":
				b, _ := io.ReadAll(part)
				gotFolderPath = string(b)
			case "options":
				b, _ := io.ReadAll(part)
				gotOptions = string(b)
			}
		}
		_, _ = io.WriteString(w, `{"url":"https://hubspot.example/hubfs/hero.png"}`)
	})

	got, err := c.UploadImage(context.Background(), imgSrv.URL+"/hero-source.png")
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if got != "https://hubspot.example/hubfs/hero.png" {
		t.Errorf("UploadImage returned %q", got)
	}
	if gotAuth != "Bearer pat-test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	// The source basename survives for recognisability, with a content hash appended so two
	// campaigns uploading different "hero.png"s cannot overwrite each other in the shared folder.
	if !strings.HasPrefix(gotFilename, "hero-source-") || !strings.HasSuffix(gotFilename, ".png") {
		t.Errorf("filename = %q, want hero-source-<hash>.png", gotFilename)
	}
	if gotFolderPath != "/email-staging" {
		t.Errorf("folderPath = %q, want /email-staging", gotFolderPath)
	}
	if !strings.Contains(gotOptions, `"access":"PUBLIC_INDEXABLE"`) {
		t.Errorf("options = %q, missing PUBLIC_INDEXABLE access", gotOptions)
	}
}

func TestUploadImage_RejectsNonImageContentType(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	t.Cleanup(imgSrv.Close)

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upload call should be made when the source URL isn't an image")
	})

	_, err := c.UploadImage(context.Background(), imgSrv.URL+"/not-an-image")
	if err == nil {
		t.Fatal("expected an error for a non-image content-type")
	}
}

func TestUploadImage_RejectsEmptyURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call should be made for an empty image URL")
	})
	if _, err := c.UploadImage(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty image URL")
	}
}

func TestUploadImage_NonSuccessUploadStatusIsAnAPIError(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(tinyPNG(t))
	}))
	t.Cleanup(imgSrv.Close)

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"forbidden"}`)
	})

	_, err := c.UploadImage(context.Background(), imgSrv.URL+"/hero.jpg")
	if err == nil {
		t.Fatal("expected an error on a non-2xx upload response")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *apiError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
}

func TestDeriveImageFilename(t *testing.T) {
	body := []byte("some-image-bytes")
	cases := []struct {
		url, ext, wantPrefix, wantSuffix string
	}{
		{"https://cdn.example.com/path/hero-banner.jpg", "jpg", "hero-banner-", ".jpg"},
		{"https://cdn.example.com/path/", "png", "email_img-", ".png"},
		{"https://cdn.example.com/no-extension", "jpg", "no-extension-", ".jpg"},
		// The URL's extension never survives: the sniffed format decides it, so a payload
		// served as HTML cannot be re-hosted under a name that invites a browser to render it.
		{"https://cdn.example.com/payload.html", "png", "payload-", ".png"},
	}
	for _, tc := range cases {
		got := deriveImageFilename(tc.url, tc.ext, body)
		if !strings.HasPrefix(got, tc.wantPrefix) || !strings.HasSuffix(got, tc.wantSuffix) {
			t.Errorf("deriveImageFilename(%q, %q) = %q, want %q...%q", tc.url, tc.ext, got, tc.wantPrefix, tc.wantSuffix)
		}
	}
}

// TestDeriveImageFilename_DifferentBytesNeverShareAName pins the collision property.
//
// Uploads land in one shared /email-staging folder with overwrite:true, and source basenames
// collide constantly -- "hero.png" is the common case. Without disambiguation two campaigns
// uploading different images under the same name overwrite each other, and both emails render
// whichever landed last.
func TestDeriveImageFilename_DifferentBytesNeverShareAName(t *testing.T) {
	const url = "https://cdn.example.com/hero.png"

	a := deriveImageFilename(url, "png", []byte("campaign-a-image"))
	b := deriveImageFilename(url, "png", []byte("campaign-b-image"))
	if a == b {
		t.Fatalf("two different images share the upload name %q", a)
	}

	// The same bytes must still collapse to one file, so a re-upload is idempotent rather than
	// littering the folder with duplicates.
	again := deriveImageFilename(url, "png", []byte("campaign-a-image"))
	if again != a {
		t.Errorf("the same image produced two names: %q and %q", a, again)
	}
}

// TestDownloadImage_DefaultClientRefusesAForbiddenAddress pins the SSRF guard on the
// path that matters: a Client built the way production builds it (NewClient with no
// download-client override). The other tests in this file inject an unguarded client so
// they can reach their 127.0.0.1 httptest server, so without this one the guard could be
// deleted entirely and every test here would still pass.
//
// It asserts on the DEFAULT, not on eventurl's guard — that package pins its own address
// enumeration. What is pinned here is the wiring: that UploadImage's fetch goes through
// the guarded client rather than a bare http.Client.
func TestDownloadImage_DefaultClientRefusesAForbiddenAddress(t *testing.T) {
	// Serves on 127.0.0.1, which the guard denies. Reaching it means no guard.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG(t))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(testCreds(), testAccount())

	_, err := c.UploadImage(context.Background(), srv.URL+"/hero.png")
	if err == nil {
		t.Fatal("UploadImage fetched a loopback address: the SSRF guard is not wired in")
	}
	if !strings.Contains(err.Error(), "forbidden address") {
		t.Fatalf("expected a forbidden-address refusal, got %v", err)
	}
}

// TestDownloadImage_RefusesANonHTTPScheme covers the case the dial-time guard cannot:
// a non-http(s) scheme never reaches a dial, so without the explicit scheme check the
// request would fail with a transport error that reads like an ordinary network fault.
func TestDownloadImage_RefusesANonHTTPScheme(t *testing.T) {
	c := NewClient(testCreds(), testAccount())

	_, err := c.UploadImage(context.Background(), "file:///etc/passwd")
	if err == nil {
		t.Fatal("UploadImage accepted a file:// URL")
	}
	if !strings.Contains(err.Error(), "not http(s)") {
		t.Fatalf("expected a scheme refusal, got %v", err)
	}
}

// TestDownloadImage_FollowsARedirect pins the behaviour the SSRF guard must NOT break.
//
// Asset URLs routinely answer 302 rather than serving bytes: S3 pre-signed links, Cloudinary and
// imgix transforms, and most CDN hotlink paths. The guarded client refuses redirects by default
// (correct for fetching an event PAGE, whose content is parsed), and adopting that default here
// would have made every hero image behind a CDN fail upload -- silently, because
// applyEmailContentWithHero swallows the error as best-effort.
func TestDownloadImage_FollowsARedirect(t *testing.T) {
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect.png" {
			http.Redirect(w, r, origin.URL+"/real.png", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG(t))
	}))
	t.Cleanup(origin.Close)

	// The unguarded download client stands in for the guard, which would refuse 127.0.0.1; the
	// redirect POLICY is what is under test, and TestDownloadImage_GuardAppliesAfterARedirect
	// covers the guard's half.
	c := NewClient(testCreds(), testAccount(), withDownloadClient(&http.Client{Timeout: 5 * time.Second, CheckRedirect: boundedRedirects}))

	data, ct, _, err := c.downloadImage(context.Background(), origin.URL+"/redirect.png")
	if err != nil {
		t.Fatalf("a redirected image URL failed to download: %v", err)
	}
	if len(data) == 0 || ct != "image/png" {
		t.Errorf("followed the redirect but got %d bytes (%s)", len(data), ct)
	}
}

// TestDownloadImage_RefusesAnOverlongRedirectChain: following is bounded, so a redirector cannot
// be used as an open one.
func TestDownloadImage_RefusesAnOverlongRedirectChain(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(testCreds(), testAccount(), withDownloadClient(&http.Client{Timeout: 5 * time.Second, CheckRedirect: boundedRedirects}))

	if _, _, _, err := c.downloadImage(context.Background(), srv.URL+"/start"); err == nil {
		t.Fatal("an unbounded redirect loop was followed")
	}
}

// boundedRedirects mirrors NewGuardedRedirectClient's policy without the address guard, so the
// redirect tests can use a 127.0.0.1 httptest server. The guard's own half is covered by
// TestDownloadImage_GuardAppliesAfterARedirect.
func boundedRedirects(_ *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("redirect chain exceeded 5 hops")
	}
	return nil
}

// TestDownloadImage_GuardAppliesAfterARedirect is the half that makes following safe at all.
//
// The address guard is a Transport-level dial hook, not a per-request check, so every hop opens
// its own connection and is judged on its own resolved address. The FIRST hop must therefore be
// permitted and the SECOND refused: if the initial URL were itself forbidden, the guard would
// reject it before any Location was read, and the test would prove nothing about hops.
func TestDownloadImage_GuardAppliesAfterARedirect(t *testing.T) {
	var hops int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, r, "http://169.254.169.254/hero.png", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(testCreds(), testAccount(),
		withDownloadClient(&http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: boundedRedirects,
			Transport:     &http.Transport{DialContext: refuseAfterFirstHop(&hops)},
		}))

	_, err := c.UploadImage(context.Background(), srv.URL+"/start.png")
	if err == nil {
		t.Fatal("the redirect hop was not judged")
	}
	if atomic.LoadInt32(&hops) == 0 {
		t.Fatal("no request reached the server; the test never exercised a redirect")
	}
	if !strings.Contains(err.Error(), "forbidden address") {
		t.Fatalf("expected the hop to be refused as a forbidden address, got %v", err)
	}
}

// refuseAfterFirstHop models guardDialAddress's per-hop behaviour: the first dial stands in for
// a permitted public host, every later one is refused the way a forbidden address is.
func refuseAfterFirstHop(hops *int32) func(context.Context, string, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if atomic.LoadInt32(hops) > 0 {
			return nil, fmt.Errorf("event URL resolves to a forbidden address: %s", addr)
		}
		return d.DialContext(ctx, network, addr)
	}
}

// TestDownloadImage_DefaultClientFollowsRedirects pins the PRODUCTION wiring, not the policy.
//
// TestDownloadImage_FollowsARedirect injects its own client, so it survives NewClient being
// switched back to the no-follow NewGuardedClient -- verified by mutation. This one reads the
// default client's own CheckRedirect, which is the thing that would regress.
func TestDownloadImage_DefaultClientFollowsRedirects(t *testing.T) {
	c := NewClient(testCreds(), testAccount())

	if c.downloadClient.CheckRedirect == nil {
		t.Fatal("the default download client has no redirect policy")
	}
	// A first hop must be allowed: refusing here is the regression that silently drops every
	// hero image behind a CDN or pre-signed URL.
	if err := c.downloadClient.CheckRedirect(nil, nil); err != nil {
		t.Fatalf("the default download client refuses redirects: %v", err)
	}
	// And the chain must still be bounded.
	via := make([]*http.Request, 5)
	if err := c.downloadClient.CheckRedirect(nil, via); err == nil {
		t.Fatal("the default download client follows an unbounded redirect chain")
	}
}

// TestDownloadImage_NAT64PrefixesReachTheDownloadGuard pins that the download guard judges the
// SAME address space as the event-URL fetcher.
//
// The guard decodes the IPv4 embedded in a NAT64 address and judges THAT. It can only do so for
// prefixes it was told about: under an undeclared prefix the address is opaque, so the private
// IPv4 it encodes is never seen and the fetch proceeds — and the response is re-hosted as a
// PUBLIC_INDEXABLE file. Passing only the well-known prefix here is therefore a narrower guard
// than the fetcher's, which is the gap this covers.
func TestDownloadImage_NAT64PrefixesReachTheDownloadGuard(t *testing.T) {
	// 2a01:4f8:808:808::a9fe:a9fe decodes to 169.254.169.254 at /96 — the metadata endpoint.
	const encoded = "http://[2a01:4f8:808:808::a9fe:a9fe]/hero.png"

	guarded := NewClient(testCreds(), testAccount(), WithNAT64Prefixes("2a01:4f8:808:808::/96"))
	_, err := guarded.UploadImage(context.Background(), encoded)
	if err == nil {
		t.Fatal("a NAT64-encoded metadata address was fetched despite the prefix being configured")
	}
	if !strings.Contains(err.Error(), "forbidden address") {
		t.Fatalf("expected a forbidden-address refusal, got %v", err)
	}
	// The refusal must name the DECODED destination, which is what proves the prefix was used
	// rather than the address being rejected for some unrelated reason.
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("refused, but not by decoding the embedded IPv4: %v", err)
	}
}

// TestDownloadImage_DoesNotLeakASignedURLIntoTheError pins that a credential in the query
// string does not travel into the error text.
//
// http.Client.Do returns a *url.Error whose Error() renders the full request URL. Wrapping that
// verbatim copies any signature into every string this error bubbles into, logs included — and
// hero URLs are frequently signed (S3 pre-signed links, CDN tokens).
func TestDownloadImage_DoesNotLeakASignedURLIntoTheError(t *testing.T) {
	const secret = "SIGNATURE-THAT-MUST-NOT-APPEAR"
	// A loopback address, so the guard refuses it and we get an error without a live server.
	url := "http://127.0.0.1:1/hero.png?X-Amz-Signature=" + secret

	c := NewClient(testCreds(), testAccount())

	_, err := c.UploadImage(context.Background(), url)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the signed query string leaked into the error: %v", err)
	}
	// The host/path must survive, or the error names nothing actionable.
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error no longer names the target at all: %v", err)
	}
}

// tinyPNG is a real 1x1 PNG. The fixtures need decodable bytes now that downloadImage sniffs the
// format rather than trusting Content-Type — a literal "fake-png-bytes" string is exactly the
// payload the sniff exists to reject.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// TestDownloadImage_RefusesHTMLServedAsAnImage pins the sniff.
//
// Content-Type is chosen by the responding server, so it establishes intent, not content. That
// matters more here than usual: the bytes are re-hosted in the LF portal as a PUBLIC_INDEXABLE
// file, so an unvalidated payload becomes publicly served content under an LF domain. The
// format is therefore decided by decoding the bytes, and the stored extension comes from the
// decoded format rather than the caller's URL.
func TestDownloadImage_RefusesHTMLServedAsAnImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "<html><script>alert(1)</script></html>")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(testCreds(), testAccount(),
		withDownloadClient(&http.Client{Timeout: 5 * time.Second}))

	_, err := c.UploadImage(context.Background(), srv.URL+"/payload.html")
	if err == nil {
		t.Fatal("HTML claiming to be a PNG was accepted for public re-hosting")
	}
	if !strings.Contains(err.Error(), "decodable image") {
		t.Fatalf("expected a decode refusal, got %v", err)
	}
}

// TestSniffImageFormat_UsesRegisteredMIMETypes pins that the MIME type is not derived from the
// extension. `image/jpeg` is the registered type while `.jpg` is the conventional extension, so
// building one from the other yields the invalid `image/jpg` that some consumers reject.
func TestSniffImageFormat_UsesRegisteredMIMETypes(t *testing.T) {
	ext, mime, err := sniffImageFormat(tinyPNG(t))
	if err != nil {
		t.Fatalf("sniffImageFormat: %v", err)
	}
	if ext != "png" || mime != "image/png" {
		t.Errorf("png: ext=%q mime=%q", ext, mime)
	}

	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatalf("encode fixture jpeg: %v", err)
	}
	ext, mime, err = sniffImageFormat(jbuf.Bytes())
	if err != nil {
		t.Fatalf("sniffImageFormat(jpeg): %v", err)
	}
	if ext != "jpg" {
		t.Errorf("jpeg extension = %q, want jpg", ext)
	}
	if mime != "image/jpeg" {
		t.Errorf("jpeg mime = %q, want image/jpeg (image/jpg is not a registered type)", mime)
	}
}
