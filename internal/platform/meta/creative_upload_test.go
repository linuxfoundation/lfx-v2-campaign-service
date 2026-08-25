// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// uploadedPart is what the fake /adimages endpoint observed about the request
// body: the file part's field name, filename, declared type and content. It is
// captured by PARSING the body the way a server does, not by matching raw bytes —
// a test that string-matches the wire would pass on a body no multipart parser
// accepts.
type uploadedPart struct {
	fieldName   string
	filename    string
	contentType string
	body        []byte
	partCount   int
}

// captureUpload parses an /adimages request the way Meta's server does and records
// the single image part. A body a real multipart reader cannot walk is reported as an
// ERROR rather than silently producing an empty capture.
//
// It returns an error instead of calling t.Fatalf because both call sites run it INSIDE an
// httptest.Server handler goroutine. t.Fatalf calls FailNow, which the testing package
// requires to run on the test goroutine only: from a handler it exits just that goroutine
// without failing the test, leaving the client blocked on a response that is never written
// and the test hanging until the suite timeout — a timeout naming the wrong test. Handlers
// pair this with t.Errorf (which IS safe off-goroutine) and an HTTP error response, so a
// malformed body fails loudly and promptly. Mirrors decodeRequest in the googleads client
// tests; see docs/reviews/knowledge-base/test-hygiene.md:httptest-handler-state-needs-synchronized-handoff.
func captureUpload(r *http.Request) (uploadedPart, error) {
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return uploadedPart{}, fmt.Errorf("upload Content-Type %q is not parseable: %w", ct, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return uploadedPart{}, fmt.Errorf("upload Content-Type = %q, want multipart/form-data", mediaType)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var got uploadedPart
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return uploadedPart{}, fmt.Errorf("walking multipart body: %w", perr)
		}
		b, rerr := io.ReadAll(p)
		if rerr != nil {
			return uploadedPart{}, fmt.Errorf("reading multipart part: %w", rerr)
		}
		got.partCount++
		got.fieldName = p.FormName()
		got.filename = p.FileName()
		got.contentType = p.Header.Get("Content-Type")
		got.body = b
	}
	return got, nil
}

// TestUploadImageSendsOneNamedFilePart pins the SHAPE of the /adimages upload
// without over-claiming a field name Meta does not document.
//
// WHAT META ACTUALLY DOCUMENTS, since this was contested in review and the answer
// is "not what either side assumed": the Marketing API reference for POST
// /act_<id>/adimages documents exactly TWO create parameters — `bytes` ("Image
// file", type "Base64 UTF-8 string") and `copy_from`. It documents NO multipart
// file field and describes no multipart upload at all.
//
// Multipart nevertheless works, and the two OFFICIAL SDKs disagree about the part's
// name, which is the load-bearing fact: the Python SDK's FacebookRequest.add_file
// builds `file_key = 'source' + str(self._file_counter)` and so uploads under
// `source0`, while the PHP SDK's AdImage.php sets AdImageFields::FILENAME and so
// uploads under `filename`. Two vendor SDKs, two different part names, both in
// production — so the endpoint is LENIENT about the part name and no particular
// name is the contract. The reviewers' "it must be `bytes`" is therefore not
// supported, and `source` is not evidence of a bug. The name is left as-is.
//
// This test consequently asserts only what IS load-bearing: exactly one file part,
// carrying a non-empty field name and a non-empty filename (a filename is what makes
// Graph treat the part as a file upload rather than a scalar field), with the caller's
// bytes verbatim.
func TestUploadImageSendsOneNamedFilePart(t *testing.T) {
	// Guarded by mu: the handler writes `got` on the server goroutine and the assertions
	// read it on the test goroutine (see the test-hygiene KB entry cited on captureUpload).
	var mu sync.Mutex
	var got uploadedPart
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/adimages") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		part, cerr := captureUpload(r)
		if cerr != nil {
			// t.Errorf is goroutine-safe where t.Fatalf is not; answering 400 also unblocks
			// the client so the test reports this failure instead of hanging on it.
			t.Errorf("capturing upload: %v", cerr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = part
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Meta keys the response map by the BASENAME of the filename we sent (the PHP
		// SDK reads images[basename(filename)]), so the fake echoes that contract.
		_, _ = io.WriteString(w, `{"images":{"`+path.Base(part.filename)+`":{"hash":"HASH_OK","url":"https://x/y.png"}}}`)
	}))
	defer srv.Close()

	hash, err := imageTestClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("uploadImage error: %v", err)
	}
	if hash != "HASH_OK" {
		t.Fatalf("hash = %q, want HASH_OK", hash)
	}

	// Snapshot the capture under the lock ONCE, then assert on the copy. uploadImage has
	// returned, so the handler has finished, but taking the lock is what establishes the
	// happens-before edge the race detector needs rather than relying on that ordering.
	mu.Lock()
	part := got
	mu.Unlock()

	// Exactly one part: one image per call is the invariant the response guard below
	// depends on.
	if part.partCount != 1 {
		t.Errorf("part count = %d, want exactly 1 — one image per call", part.partCount)
	}
	if strings.TrimSpace(part.fieldName) == "" {
		t.Errorf("multipart part carries no field name")
	}
	// The part must NOT be named `bytes`, and that exclusion is the assertion worth
	// making even though Meta documents no multipart field name at all.
	//
	// `bytes` is the documented SCALAR parameter, typed "Base64 UTF-8 string". Naming a
	// raw multipart file part after it is the specific wrong move this endpoint invites,
	// because the reference lists `bytes` and a reader reasonably concludes the part
	// should carry that name — two reviewers on this PR concluded exactly that. Sending
	// raw bytes under the base64 scalar's name is not the documented transport and not
	// the SDK transport; it is neither.
	//
	// This deliberately does not assert `source` as the ONLY acceptable name: Meta's own
	// SDKs disagree (Python uploads under `source0`, PHP under a filename param), so
	// pinning one literal would encode a choice Meta has not published. Excluding the
	// name that is known-wrong is the claim the evidence actually supports.
	if part.fieldName == "bytes" {
		t.Errorf("multipart file part is named %q — that is the DOCUMENTED SCALAR parameter "+
			"(Base64 UTF-8 string), not a multipart file field. A raw file part under that "+
			"name is neither the documented transport nor the one the official SDKs use",
			part.fieldName)
	}
	// A filename must be present. NOTE ON THE EXTENSION: no Meta documentation states
	// that the filename must carry a real extension, and no authoritative source was
	// found either way — Meta sniffs image content for format and validation. This
	// asserts only PRESENCE, deliberately: asserting an extension rule would pin a
	// claim that is undocumented in both directions.
	if strings.TrimSpace(part.filename) == "" {
		t.Errorf("multipart part carries no filename; Graph needs one to treat the part as a file upload")
	}
	if part.contentType != "image/png" {
		t.Errorf("part Content-Type = %q, want image/png", part.contentType)
	}
	// The bytes must arrive VERBATIM. `bytes` is documented as a base64 string, but a
	// multipart file part carries raw content and Graph decodes the part itself;
	// double-encoding would corrupt every image.
	if string(part.body) != "PNGBYTES" {
		t.Errorf("uploaded bytes = %q, want the caller's bytes verbatim", part.body)
	}
	if enc := base64.StdEncoding.EncodeToString([]byte("PNGBYTES")); string(part.body) == enc {
		t.Errorf("bytes were base64-encoded into the file part (%q); a multipart file part carries raw content", part.body)
	}
}

// TestUploadImageRejectsMultiEntryResponse is the wrong-creative-on-live-spend
// guard, and it is DETERMINISTIC BY CONSTRUCTION.
//
// The old code read `for _, img := range out.Images { if hash != "" { return hash } }`.
// That is worse than it looks. The documented response is
// `Map { string: Map { string: Struct { hash, url, ... } } }` under `images`, and the
// key is the BASENAME of the file we uploaded (the PHP SDK reads
// images[basename(filename)]) — a key this client never derives. Iterating and taking
// the first non-empty hash is a workaround for never computing that key, and because
// Go RANDOMIZES map iteration order, a response carrying more than one entry yields an
// ARBITRARY hash — which then becomes link_data.image_hash on a creative that spends
// money.
//
// The fix is not to guess a different entry or to hardcode the key, but to make the
// one-upload/one-entry invariant EXPLICIT and fail closed. A test asserting "we got
// hash A" would pass only ~1/N of the time by luck, so this asserts the one outcome
// no iteration order can produce: a multi-entry response is REFUSED.
//
// The fixture carries THREE distinct non-empty hashes. Under the old code EVERY
// iteration order returns some hash and none returns an error, so the old code fails
// this test on 100% of runs rather than 2/3 of them — the assertion is on the refusal,
// not on which hash won. Verified with -count=20.
func TestUploadImageRejectsMultiEntryResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Three entries, all with non-empty hashes: no iteration order yields a
		// defensible answer, so the only correct behaviour is to refuse.
		_, _ = io.WriteString(w, `{"images":{
			"a":{"hash":"HASH_A"},
			"b":{"hash":"HASH_B"},
			"c":{"hash":"HASH_C"}}}`)
	}))
	defer srv.Close()

	hash, err := imageTestClient(srv.URL).uploadImage(context.Background(), []byte("B"), "image/png")
	if err == nil {
		t.Fatalf("uploadImage returned hash %q and no error; a multi-entry response must be REFUSED — one upload yields one entry, and picking one of three under randomized map iteration attaches an arbitrary image to a paid ad", hash)
	}
	if hash != "" {
		t.Errorf("hash = %q on the refusal path, want empty — an empty hash must never reach a creative", hash)
	}
	// Ambiguity, not a caller error: the upload may have landed but cannot be named.
	var te *transportError
	if !errors.As(err, &te) {
		t.Errorf("error = %T (%v), want *transportError so the variant fails rather than attaching a guessed hash", err, err)
	}
	// The candidate hashes must not leak into the error either — none was chosen, and
	// naming one would suggest it had been.
	for _, h := range []string{"HASH_A", "HASH_B", "HASH_C"} {
		if strings.Contains(err.Error(), h) {
			t.Errorf("error names candidate hash %q: %v", h, err)
		}
	}
}

// TestUploadImageRejectsEmptyHashEntry closes the other half of the same guard: a
// single, well-formed-looking entry whose hash is empty (or whitespace) must be
// refused rather than returned. An empty image_hash on a creative is not a harmless
// no-op — it is a creative Meta rejects AFTER the campaign and ad set already exist,
// i.e. after the spend commitment.
func TestUploadImageRejectsEmptyHashEntry(t *testing.T) {
	// wantCount distinguishes the COUNT arm from the no-hash arm. Asserting only "an
	// error happened" would let the count guard be weakened from `!= 1` to `> 1` with
	// every test still green: a zero-entry response would fall through to the
	// pre-existing "returned no hash" error, so both arms error and a coarse assertion
	// cannot tell them apart. That mutation SURVIVED until this assertion was added.
	cases := []struct {
		body      string
		wantCount bool // refused by the exactly-one-entry guard, not by the hash check
	}{
		{`{"images":{"a":{"hash":""}}}`, false},
		{`{"images":{"a":{"hash":"   "}}}`, false},
		{`{"images":{"a":{"url":"https://x/y.png"}}}`, false},
		// Zero entries is a COUNT violation: one upload must yield exactly one entry, so
		// this is refused before the hash is ever inspected.
		{`{"images":{}}`, true},
		{`{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			hash, err := imageTestClient(srv.URL).uploadImage(context.Background(), []byte("B"), "image/png")
			if err == nil {
				t.Fatalf("uploadImage returned %q and no error for %s; an empty hash must be refused", hash, tc.body)
			}
			if hash != "" {
				t.Errorf("hash = %q, want empty", hash)
			}
			var te *transportError
			if !errors.As(err, &te) {
				t.Fatalf("error = %T (%v), want *transportError", err, err)
			}
			// The REASON must match the arm that should have fired.
			gotCount := strings.Contains(err.Error(), "want exactly 1")
			if gotCount != tc.wantCount {
				t.Errorf("error = %q; want the %s arm to fire",
					err, map[bool]string{true: "exactly-one-entry", false: "no-hash"}[tc.wantCount])
			}
		})
	}
}

// TestUploadImageAcceptsSingleEntryUnderAnyKey pins what the count guard must NOT
// break: exactly one entry is the normal case, and the key Meta chooses for it is not
// something this client should depend on. Meta keys the map by the basename of the
// uploaded filename, so the key IS predictable — but reading the single entry by
// VALUE keeps the client correct without deriving a key whose contract is documented
// only in SDK source. The guard is on the COUNT, not the name.
func TestUploadImageAcceptsSingleEntryUnderAnyKey(t *testing.T) {
	for _, key := range []string{"creative", "creative.png", "some_other_key"} {
		t.Run(key, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"images":{"`+key+`":{"hash":"THE_HASH"}}}`)
			}))
			defer srv.Close()

			hash, err := imageTestClient(srv.URL).uploadImage(context.Background(), []byte("B"), "image/png")
			if err != nil {
				t.Fatalf("uploadImage error for key %q: %v", key, err)
			}
			if hash != "THE_HASH" {
				t.Errorf("hash = %q, want THE_HASH", hash)
			}
		})
	}
}

// metaUploadCacheServer is a full campaign fake that counts /adimages calls and records
// the image_hash each creative received. Every other endpoint answers the minimum the
// campaign flow needs, so the only variable under test is upload behaviour.
func metaUploadCacheServer(t *testing.T, uploads *int32, hashes chan<- string, uploadStatus func(n int32) (int, string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adimages"):
			n := atomic.AddInt32(uploads, 1)
			// Drain the body so the client's write always completes.
			_, _ = io.Copy(io.Discard, r.Body)
			status, body := uploadStatus(n)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			body := decodeBody(t, r)
			ld := creativeLinkData(t, body)
			h, _ := ld["image_hash"].(string)
			hashes <- h
			_, _ = io.WriteString(w, `{"id":"creative_x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_x"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestUploadIsDedupedAcrossVariantsSharingOneAsset is the behavioural lock for the
// per-campaign upload cache. resolveVariantAssets already makes N variants naming ONE
// asset share ONE buffer, so at this layer they arrive as identical bytes. Uploading
// them once is the point: five variants naming one 30 MiB asset previously pushed five
// identical transfers inside CreateCampaign's single deadline.
//
// The assertion is on OBSERVED REQUESTS and on the hash each creative received — not on
// the cache's internals — so it fails if the dedupe is removed and equally if the dedupe
// is implemented by handing a variant the WRONG hash.
func TestUploadIsDedupedAcrossVariantsSharingOneAsset(t *testing.T) {
	var uploads int32
	hashes := make(chan string, 8)
	srv := metaUploadCacheServer(t, &uploads, hashes, func(int32) (int, string) {
		return http.StatusOK, `{"images":{"creative":{"hash":"SHARED_HASH"}}}`
	})
	defer srv.Close()

	shared := []byte("IDENTICAL-ASSET-BYTES")
	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "One", Headline: "H1", ImageBytes: shared, ImageMIME: "image/png"},
		AdVariant{PrimaryText: "Two", Headline: "H2", ImageBytes: shared, ImageMIME: "image/png"},
		AdVariant{PrimaryText: "Three", Headline: "H3", ImageBytes: shared, ImageMIME: "image/png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if res.AdCount != 3 {
		t.Fatalf("ad count = %d, want 3", res.AdCount)
	}
	if n := atomic.LoadInt32(&uploads); n != 1 {
		t.Errorf("/adimages called %d times for 3 variants naming ONE asset, want exactly 1", n)
	}
	close(hashes)
	got := 0
	for h := range hashes {
		got++
		if h != "SHARED_HASH" {
			t.Errorf("creative received image_hash %q, want SHARED_HASH", h)
		}
	}
	if got != 3 {
		t.Fatalf("creatives = %d, want 3", got)
	}
}

// TestDistinctAssetsAreUploadedSeparately is the counterweight: the cache must key on
// CONTENT, so two variants naming DIFFERENT assets must still upload twice and must each
// receive their own hash. Without this, a cache keyed on something coarser (or a blanket
// "upload once per campaign") would pass the dedupe test above while attaching one
// variant's image to another variant's paid creative.
func TestDistinctAssetsAreUploadedSeparately(t *testing.T) {
	var uploads int32
	hashes := make(chan string, 8)
	srv := metaUploadCacheServer(t, &uploads, hashes, func(n int32) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"images":{"creative":{"hash":"HASH_%d"}}}`, n)
	})
	defer srv.Close()

	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		// SAME LENGTH, different content. Equal length is deliberate: it makes the test
		// sensitive to a cache keyed on anything coarser than the bytes themselves (a
		// length, a size class), which would collide here and upload only once —
		// attaching variant 1's image to variant 2's paid creative.
		AdVariant{PrimaryText: "One", Headline: "H1", ImageBytes: []byte("ASSET-ALPHA"), ImageMIME: "image/png"},
		AdVariant{PrimaryText: "Two", Headline: "H2", ImageBytes: []byte("ASSET-BRAVO"), ImageMIME: "image/png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if res.AdCount != 2 {
		t.Fatalf("ad count = %d, want 2", res.AdCount)
	}
	if n := atomic.LoadInt32(&uploads); n != 2 {
		t.Errorf("/adimages called %d times for 2 DISTINCT assets, want 2", n)
	}
	close(hashes)
	seen := map[string]bool{}
	for h := range hashes {
		if seen[h] {
			t.Errorf("two creatives received the SAME image_hash %q; distinct assets must not share a hash", h)
		}
		seen[h] = true
	}
	if len(seen) != 2 {
		t.Fatalf("distinct hashes = %d, want 2", len(seen))
	}
}

// TestFailedUploadIsNotCachedAcrossVariants is the load-bearing negative: a FAILED upload
// must never be memoized. If it were, one bad transfer would become a campaign-wide
// failure — every later variant naming that asset would fail without ever retrying, even
// though the failure may have been transient.
//
// The fake fails the FIRST /adimages with a 500 (an ambiguous outcome — the upload may
// have landed upstream, which is exactly the case that must not be cached in either
// direction) and succeeds afterwards. The second variant must therefore RETRY and succeed.
func TestFailedUploadIsNotCachedAcrossVariants(t *testing.T) {
	var uploads int32
	hashes := make(chan string, 8)
	srv := metaUploadCacheServer(t, &uploads, hashes, func(n int32) (int, string) {
		if n == 1 {
			return http.StatusInternalServerError, `{"error":{"message":"transient","code":2}}`
		}
		return http.StatusOK, `{"images":{"creative":{"hash":"RECOVERED_HASH"}}}`
	})
	defer srv.Close()

	shared := []byte("IDENTICAL-ASSET-BYTES")
	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "One", Headline: "H1", ImageBytes: shared, ImageMIME: "image/png"},
		AdVariant{PrimaryText: "Two", Headline: "H2", ImageBytes: shared, ImageMIME: "image/png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	// Variant 1 fails its upload (non-fatal, recorded in Steps); variant 2 must retry the
	// upload rather than inherit variant 1's failure, so exactly one ad is created.
	if res.AdCount != 1 {
		t.Fatalf("ad count = %d, want 1 (variant 1's upload failed, variant 2 must still succeed)", res.AdCount)
	}
	if n := atomic.LoadInt32(&uploads); n != 2 {
		t.Errorf("/adimages called %d times, want 2 — a FAILED upload must not be cached, so variant 2 must retry", n)
	}
	close(hashes)
	got := []string{}
	for h := range hashes {
		got = append(got, h)
	}
	if len(got) != 1 || got[0] != "RECOVERED_HASH" {
		t.Errorf("creative hashes = %v, want exactly [RECOVERED_HASH]", got)
	}
}

// TestAmbiguousUploadStepDoesNotPromiseRedispatchRecovery locks the WORDING of the
// operator-facing Step for an ambiguous (5xx/transport) image upload failure.
//
// The recovery this Step used to promise does not exist. An upload shortfall leaves
// CampaignResult.AdCount below the requested variant count; MetaDispatcher.Dispatch
// persists that as `created_degraded`; and service.isReusableCampaign treats
// `created_degraded` as a terminal, REUSABLE row — so a later dispatch returns the
// existing campaign without re-running any ad step, the upload included. Telling the
// operator that "re-dispatching reuses it" reads as "retry and the ad appears", so they
// would wait for an ad that nothing will ever create.
//
// What remains true is that the image itself needs no cleanup (it is content-addressed).
// This test pins both halves: the no-cleanup fact stays, the false retry promise is gone
// and is replaced by an explicit statement that re-dispatch will NOT recreate the ad.
func TestAmbiguousUploadStepDoesNotPromiseRedispatchRecovery(t *testing.T) {
	var uploads int32
	hashes := make(chan string, 4)
	srv := metaUploadCacheServer(t, &uploads, hashes, func(int32) (int, string) {
		// A 5xx: ambiguous — the upload may have landed, but no hash came back.
		return http.StatusInternalServerError, `{"error":{"message":"upstream unavailable","code":2}}`
	})
	defer srv.Close()

	res, err := imageTestClient(srv.URL).CreateCampaign(context.Background(), imageTestInput(
		AdVariant{PrimaryText: "One", Headline: "H1", ImageBytes: []byte("SOME-ASSET"), ImageMIME: "image/png"},
	))
	if err != nil {
		t.Fatalf("CreateCampaign error = %v, want nil (a per-variant upload failure is non-fatal)", err)
	}
	if res.AdCount != 0 {
		t.Fatalf("ad count = %d, want 0 — the only variant's upload failed", res.AdCount)
	}

	var step string
	for _, s := range res.Steps {
		if strings.Contains(s, "image upload did not complete") {
			step = s
			break
		}
	}
	if step == "" {
		t.Fatalf("no upload-failure step was recorded; steps = %v", res.Steps)
	}
	// The ambiguity must be reported at all — otherwise the assertions below are vacuous.
	if !strings.Contains(step, "may have landed") {
		t.Fatalf("an ambiguous upload must be reported as possibly landed, got %q", step)
	}
	// The FALSE promise must be gone. This is the substance of the fix: re-dispatch does
	// not re-run the upload, so the step must not say it reuses the image on re-dispatch.
	if strings.Contains(step, "re-dispatching reuses it") {
		t.Errorf("step still promises a recovery that cannot happen — a created_degraded "+
			"campaign is reused as-is and no ad step re-runs: %q", step)
	}
	// And it must say what DOES happen, so the operator reconciles instead of retrying.
	if !strings.Contains(step, "will NOT recreate this ad") {
		t.Errorf("step must state that re-dispatch will not recreate the ad, got %q", step)
	}
	if !strings.Contains(step, "created_degraded") {
		t.Errorf("step must name the status the campaign persists as, got %q", step)
	}
	// The still-true half must survive: the image needs no cleanup.
	if !strings.Contains(step, "no cleanup") {
		t.Errorf("step must still tell the operator the image needs no cleanup, got %q", step)
	}
}

// uploadRetryClient is imageTestClient with a negligible backoff base so a retry test
// does not spend real seconds sleeping. The DELAY is not what is under test — the number
// of attempts and the final outcome are — so shrinking it changes nothing being asserted.
func uploadRetryClient(srvURL string) *Client {
	return NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srvURL),
		WithClock(fixedMetaClock()),
		withRetryBaseDelay(time.Millisecond),
	)
}

// TestUploadImageRetriesThrottleAndSucceeds proves the throttle retry exists and that a
// variant survives a transient rate limit.
//
// This is the case the retry is FOR: by the time uploadImage runs, the campaign and ad
// set already exist, so dropping the variant on a 429 leaves a created_degraded campaign
// that no re-dispatch repairs. Retrying is sound because the endpoint is
// content-addressed — repeating the upload creates nothing.
//
// Asserted on observed attempts and the returned hash, never on elapsed time.
func TestUploadImageRetriesThrottleAndSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		// A plain 429.
		{"http 429", http.StatusTooManyRequests, `{"error":{"message":"slow down","code":613}}`},
		// The COMMON shape: Meta reports rate limiting as a 400 carrying a Graph
		// rate-limit code far more often than as a 429. A status-only test would miss it.
		{"http 400 with graph rate-limit code", http.StatusBadRequest, `{"error":{"message":"limit","code":4}}`},
		// A 429 with no parseable Graph envelope — the status alone is decisive.
		{"http 429 with no envelope", http.StatusTooManyRequests, `not json at all`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := atomic.AddInt32(&attempts, 1)
				// Every attempt must carry the FULL multipart body. A replayed request
				// that reused a consumed reader would arrive empty, so parsing it here is
				// what catches that.
				got, cerr := captureUpload(r)
				if cerr != nil {
					// t.Errorf, not t.Fatalf: this runs on the server goroutine. Answering
					// 400 unblocks the client so the failure is reported, not hung on.
					t.Errorf("attempt %d: capturing upload: %v", n, cerr)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(got.body) == 0 {
					t.Errorf("attempt %d carried an EMPTY image part; the multipart body must be replayed per attempt", n)
				}
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_, _ = io.WriteString(w, `{"images":{"creative":{"hash":"AFTER_RETRY"}}}`)
			}))
			defer srv.Close()

			hash, err := uploadRetryClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
			if err != nil {
				t.Fatalf("uploadImage error after a retryable throttle: %v", err)
			}
			if hash != "AFTER_RETRY" {
				t.Errorf("hash = %q, want AFTER_RETRY", hash)
			}
			if n := atomic.LoadInt32(&attempts); n != 2 {
				t.Errorf("attempts = %d, want 2 (one throttled, one successful retry)", n)
			}
		})
	}
}

// TestUploadImageDoesNotRetryNonThrottleFailures is the counterweight: the retry must be
// scoped to throttles ONLY. A 400 that is a genuine semantic rejection (a corrupt image)
// must fail on the FIRST attempt — retrying it would spend the deadline re-sending bytes
// Meta has already refused on their merits.
func TestUploadImageDoesNotRetryNonThrottleFailures(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// code 100 is a plain invalid-parameter error, NOT a rate-limit code.
		_, _ = io.WriteString(w, `{"error":{"message":"invalid image","code":100}}`)
	}))
	defer srv.Close()

	_, err := uploadRetryClient(srv.URL).uploadImage(context.Background(), []byte("BAD"), "image/png")
	if err == nil {
		t.Fatal("a 400 invalid-image must fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 — a non-throttle rejection must not be retried", n)
	}
}

// TestUploadImageStopsAfterRetryMaxThrottles bounds the retry: a server that throttles
// forever must not loop indefinitely inside the campaign's deadline, and the error the
// caller finally sees must still be the throttle (so it classifies as a rate limit, not
// as some generic failure).
func TestUploadImageStopsAfterRetryMaxThrottles(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613}}`)
	}))
	defer srv.Close()

	_, err := uploadRetryClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err == nil {
		t.Fatal("an endlessly throttled upload must fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want the throttle *APIError", err, err)
	}
	if apiErr.Code != 613 {
		t.Errorf("final error code = %d, want the throttle code 613 preserved", apiErr.Code)
	}
	// retryMax retries after the first attempt.
	if n := atomic.LoadInt32(&attempts); n != retryMax+1 {
		t.Errorf("attempts = %d, want %d (initial + retryMax)", n, retryMax+1)
	}
}

// TestUploadImageAbortsWhenRetryAfterExceedsCap mirrors do()'s policy: when the server
// DECLARES a reset longer than maxRetryWait, sleeping only the cap would retry while Meta
// is still throttling — burning attempts and stalling a synchronous flow — so it aborts
// instead of clamping, carrying the Graph diagnostics.
func TestUploadImageAbortsWhenRetryAfterExceedsCap(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613,"type":"OAuthException"}}`)
	}))
	defer srv.Close()

	_, err := uploadRetryClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err == nil {
		t.Fatal("an over-cap Retry-After must abort")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if !strings.Contains(apiErr.Message, "exceeds max wait") {
		t.Errorf("message = %q, want the over-cap abort wording", apiErr.Message)
	}
	// The RAW header is what needs debugging upstream, so it must be reported verbatim.
	if !strings.Contains(apiErr.Message, `"600"`) {
		t.Errorf("message = %q, want the raw Retry-After value 600", apiErr.Message)
	}
	if apiErr.Code != 613 {
		t.Errorf("abort dropped the Graph code: got %d, want 613", apiErr.Code)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 — an over-cap reset must abort, not retry", n)
	}
}

// NOTE ON A TEST THAT IS NOT HERE. An earlier revision added
// TestUploadImageCancelledThrottleSleepStaysAmbiguous, which drove uploadImage with an
// ALREADY-cancelled context and asserted the outcome stayed ambiguous. It was vacuous:
// with the context dead before the call, the round trip fails on attempt 1 and the error
// comes from the TRANSPORT arm, so the retry wait never ran. Because both arms return
// *transportError, every assertion in it passed — including against a build where the
// sleep arm returned a bare ctx.Err(). It was removed rather than repaired: the test
// below covers the same behaviour and actually fails when the wrap is reverted, and two
// tests where only one can fail is worse than one that can.

// TestThrottleWaitInterruptionIsWrappedAmbiguous pins the SLEEP ARM directly, because the
// end-to-end test above cannot reliably distinguish it from the transport arm: once the
// context is cancelled, the HTTP round trip may fail first, and BOTH arms return a
// *transportError — so the ambiguity assertion alone cannot say which one ran. Several
// earlier formulations of this test passed against broken code for exactly that reason,
// which is why the wrapper assertion at the end is the discriminator.
//
// ORDERING IS STRUCTURAL, not timed. The server answers with a throttle whose Retry-After
// is an HTTP-DATE, and parseRetryAfter resolves that form by calling the client's clock —
// which happens only AFTER the response has been fully read and classified. Cancelling
// from inside that clock hook therefore places the cancellation provably after the
// transport arm has succeeded and immediately before the wait, with no reliance on
// scheduling, sleeps, or elapsed time.
//
// Everything else (cancelling in the handler, on connection-idle, or from a goroutine
// racing the response) lands while the client is still reading, and produces a
// transport-arm error instead.
func TestThrottleWaitInterruptionIsWrappedAmbiguous(t *testing.T) {
	var attempts int32
	// A far-future HTTP-date reset: selects parseRetryAfter's date branch (so the clock is
	// consulted) and yields a wait far longer than the test, so it can never elapse.
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The clock is consulted by parseRetryAfter's HTTP-date branch, i.e. after the
	// response is read and before the wait begins. Cancelling here is what makes the
	// wait — and only the wait — observe the cancellation.
	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		WithClock(func() time.Time {
			cancel()
			return time.Now()
		}),
		withRetryBaseDelay(time.Millisecond),
	)

	_, err := c.uploadImage(ctx, []byte("PNGBYTES"), "image/png")
	if err == nil {
		t.Fatal("a cancelled throttle wait must fail")
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("attempts = %d, want 1 — the wait must be interrupted, not elapsed", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	// THE POINT: the outcome must stay AMBIGUOUS, not read as a clean failure.
	if !createOutcomeAmbiguous(err) {
		t.Errorf("a cancelled throttle wait classified as a DEFINITE failure (%v); "+
			"a shed throttle on a mutating call may have been accepted upstream", err)
	}
	var te *transportError
	if !errors.As(err, &te) {
		t.Errorf("error = %v (%T), want *transportError so the ambiguity survives", err, err)
	}
	// THE DISCRIMINATOR: only the sleep arm adds this wrapper.
	if !strings.Contains(err.Error(), "throttled upload retry interrupted") {
		t.Errorf("error = %v, want the throttle-wait wrapper", err)
	}
}

// TestUploadBackoffIsExponentialInAttempt pins the /adimages no-header backoff to the
// SAME capped exponential schedule do() uses, and does so WITHOUT sleeping.
//
// HOW THE DELAY IS MADE OBSERVABLE. throttleWait RETURNS the duration it wants the
// caller to wait; it does not sleep. So the schedule can be read directly as a value,
// per attempt, with no timer, no elapsed-time threshold and no goroutine scheduling in
// the assertion. An elapsed-time test would be both slow and unable to discriminate —
// a wait that is merely unscheduled looks identical to a wait that was never requested.
//
// WHAT IT DISCRIMINATES. The defect was that the attempt number never reached the delay
// computation, so every retry waited retryBaseDelay: 1s/1s/1s instead of 1s/2s/4s.
// Asserting the FULL per-attempt sequence (not just "some delay happened") is what makes
// a revert to the base delay fail here: attempt 0 agrees under both behaviours, so a test
// checking only one attempt would pass against the bug.
func TestUploadBackoffIsExponentialInAttempt(t *testing.T) {
	// A 429 carrying NO Retry-After: exactly the no-server-declared-reset case whose
	// fallback is the exponential schedule under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613}}`)
	}))
	defer srv.Close()

	// The PRODUCTION base delay, so the asserted numbers are the real schedule rather
	// than a shrunken test-only one.
	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
	)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("fetch throttle response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	apiErr := &APIError{StatusCode: http.StatusTooManyRequests, Code: 613}

	// The schedule do() implements: retryBaseDelay << attempt, capped at maxRetryWait.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for attempt, expect := range want {
		got, abort := c.throttleWait(context.Background(), resp, apiErr, http.StatusTooManyRequests, "/act_777/adimages", attempt)
		if abort != nil {
			t.Fatalf("attempt %d: unexpected abort: %v", attempt, abort)
		}
		if got != expect {
			t.Errorf("attempt %d: wait = %s, want %s — the /adimages fallback must be "+
				"capped exponential in the ATTEMPT NUMBER, as do() is; a constant %s on "+
				"every attempt means the attempt number never reached the computation",
				attempt, got, expect, c.retryBaseDelay)
		}
	}
}

// TestUploadBackoffMatchesDoBackoff proves the two paths share ONE schedule rather than
// two that happen to agree today: it compares the upload path's computed wait against
// the very helper do() calls, for every attempt the retry loop can reach.
//
// This is the structural half of the fix. Equal-by-construction is what a future edit
// cannot silently break — if someone reintroduces a private computation on either side,
// this fails even if that computation looks plausible.
func TestUploadBackoffMatchesDoBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613}}`)
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
	)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("fetch throttle response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	apiErr := &APIError{StatusCode: http.StatusTooManyRequests, Code: 613}

	for attempt := 0; attempt <= retryMax; attempt++ {
		got, abort := c.throttleWait(context.Background(), resp, apiErr, http.StatusTooManyRequests, "/act_777/adimages", attempt)
		if abort != nil {
			t.Fatalf("attempt %d: unexpected abort: %v", attempt, abort)
		}
		if want := c.backoffDelay(attempt); got != want {
			t.Errorf("attempt %d: upload wait = %s, do() wait = %s — both paths must "+
				"resolve to the SAME shared schedule", attempt, got, want)
		}
	}
}

// TestBackoffDelayCapsAtMaxRetryWait pins the cap arm of the shared helper, which the
// attempt-threaded shift makes reachable: without a cap, a large attempt number would
// shift the base delay past maxRetryWait.
func TestBackoffDelayCapsAtMaxRetryWait(t *testing.T) {
	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
	)
	// 1s << 10 = 1024s, far past the 60s cap.
	if got := c.backoffDelay(10); got != maxRetryWait {
		t.Errorf("backoffDelay(10) = %s, want the cap %s", got, maxRetryWait)
	}
}

// TestUploadImageLoopThreadsItsAttemptCounter binds the RETRY LOOP's threading of its own
// attempt counter, end to end through uploadImage.
//
// WHY THIS EXISTS SEPARATELY. The tests above call throttleWait directly, so they pin the
// SCHEDULE but not the wiring: a mutation replacing the loop's `attempt` argument with a
// constant 0 left every one of them green, because they never exercise the call site that
// supplies it. This test observes the delays uploadImage actually REQUESTS, so pinning the
// argument to a constant fails here.
//
// NO SLEEPING. sleepFn is replaced with a recorder that captures the requested duration
// and returns immediately, so the full 1s/2s/4s schedule is asserted as a sequence of
// VALUES in microseconds of runtime. An elapsed-time assertion could neither run at the
// production base delay nor distinguish a wait that was requested from one that was merely
// unscheduled.
func TestUploadImageLoopThreadsItsAttemptCounter(t *testing.T) {
	var attempts int32
	// Always throttle, with no Retry-After, so every retry takes the exponential
	// fallback and the loop runs to its retryMax bound.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":613}}`)
	}))
	defer srv.Close()

	var waits []time.Duration
	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		// Production base delay: the asserted schedule is the real one, and it costs
		// nothing because the wait is recorded rather than taken.
		withSleepFn(func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		}),
	)

	if _, err := c.uploadImage(context.Background(), []byte("PNGBYTES"), "image/png"); err == nil {
		t.Fatal("an always-throttled upload must fail once retries are exhausted")
	}

	// retryMax retries after the first attempt: waits are taken for attempts 0,1,2.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("recorded %d waits (%v), want %d — the loop must retry up to retryMax",
			len(waits), waits, len(want))
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %s, want %s — uploadImage must thread ITS OWN attempt "+
				"counter into the backoff; a constant here means the call site discards it",
				i, waits[i], want[i])
		}
	}
	if n := atomic.LoadInt32(&attempts); n != retryMax+1 {
		t.Errorf("attempts = %d, want %d", n, retryMax+1)
	}
}

// TestUploadImageRetriesTruncated429 pins that a 429 whose BODY CANNOT BE READ is still
// retried as the throttle it plainly is.
//
// THE FAILURE MODE. The throttle arms were once gated on `readErr == nil`, so a 429 with a
// mismatched Content-Length (io.ReadAll returns "unexpected EOF") fell through to the
// default arm and was returned as FINAL. That turns a retryable rate limit into a terminal
// `created_degraded` campaign on the strength of a body the status made unnecessary — and
// it diverges from do(), which computes isThrottle from the status BEFORE consuming readErr
// precisely so a throttle it will retry is not short-circuited by an unreadable body.
//
// HOW THE TRUNCATION IS PRODUCED. The handler declares a Content-Length larger than what it
// writes, then closes the connection. That is the real shape of the bug, not a simulated
// error value, so the test exercises the client's actual read path.
func TestUploadImageRetriesTruncated429(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// Promise 4096 bytes, send a handful, hang up: the client's io.ReadAll sees
			// an unexpected EOF, which is exactly the case the readErr gate mishandled.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":`)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, herr := hj.Hijack()
				if herr == nil {
					_ = conn.Close()
				}
			}
			return
		}
		// The retry succeeds, proving the first response was treated as retryable.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"images":{"creative":{"hash":"HASH_OK","url":"https://x/y.png"}}}`)
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		withSleepFn(func(context.Context, time.Duration) error { return nil }),
	)

	hash, err := c.uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("a truncated 429 must be RETRIED, not returned as final: %v", err)
	}
	if hash != "HASH_OK" {
		t.Errorf("hash = %q, want %q", hash, "HASH_OK")
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("attempts = %d, want 2 — the truncated 429 must cost exactly one retry", n)
	}
}

// TestUploadImageRetriesTruncatedGraphThrottle pins that a Graph-coded throttle is retried
// even when the response body is TRUNCATED.
//
// THE FAILURE MODE. Meta reports rate limiting as an HTTP 400 carrying a Graph rate-limit
// code far more often than as a 429, so the envelope — not the status — is what identifies
// the common throttle shape. The envelope arm was gated on `readErr == nil`, so a 400 whose
// body is a COMPLETE `{"error":{"code":4}}` followed by a connection closed early on a
// mismatched Content-Length skipped that arm entirely: `raw` parses fine, but readErr is
// non-nil. It then failed the status==429 arm and fell to `default`, returning `final` — a
// retryable throttle turned terminal, leaving a created_degraded campaign no re-dispatch
// repairs.
//
// This is the SAME defect the truncated-429 test covers, on the other classification arm,
// and do() already gets it right: it unmarshals the envelope on every non-2xx path before
// consuming readErr, precisely so a parseable body is not discarded for being short.
//
// HOW THE TRUNCATION IS PRODUCED. The handler writes a complete JSON envelope but declares a
// larger Content-Length and hangs up, so io.ReadAll returns the usable bytes AND an
// unexpected EOF — the real shape of the bug rather than a simulated error value.
func TestUploadImageRetriesTruncatedGraphThrottle(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// A COMPLETE, parseable rate-limit envelope — code 4 is in graphRateLimitCodes —
			// carried on a 400 and truncated by an overstated Content-Length.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":4,"type":"OAuthException","message":"rate limit"}}`)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, herr := hj.Hijack()
				if herr == nil {
					_ = conn.Close()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"images":{"creative":{"hash":"HASH_OK","url":"https://x/y.png"}}}`)
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		withSleepFn(func(context.Context, time.Duration) error { return nil }),
	)

	hash, err := c.uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("a truncated Graph-coded throttle must be RETRIED, not returned as final: %v", err)
	}
	if hash != "HASH_OK" {
		t.Errorf("hash = %q, want %q", hash, "HASH_OK")
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("attempts = %d, want 2 — the truncated Graph throttle must cost exactly one retry", n)
	}
}

// TestUploadImageTruncatedEnvelopeIsMarkedUnreadable pins the OTHER half of parsing the
// envelope without gating on readErr.
//
// Dropping that gate means a TRUNCATED body can now populate Type/Code/Message. That is
// wanted — it is what lets a code-4 throttle on a 400 be seen — but it must not make a
// short body look like a COMPLETE rejection. `raw` holds only what arrived before the
// connection closed, so fields Meta actually sent may be missing entirely; a caller that
// reads a bare parsed envelope as the full story would treat "we never finished reading"
// as "Meta said exactly this".
//
// So a truncated envelope keeps its parsed fields AND carries EnvelopeUnreadable. This
// uses code 100 — deliberately NOT in graphRateLimitCodes — so the response stays FINAL
// and the assertion is about the flag rather than about retrying. Without the flag this
// test passes silently, which is exactly the survivor it exists to kill.
func TestUploadImageTruncatedEnvelopeIsMarkedUnreadable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":100,"type":"OAuthException","message":"bad param"}}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, herr := hj.Hijack()
			if herr == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok-img"},
		AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		withSleepFn(func(context.Context, time.Duration) error { return nil }),
	)

	_, err := c.uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err == nil {
		t.Fatal("a non-throttle 400 must fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *APIError", err)
	}
	// The parsed fields survive the truncation...
	if apiErr.Code != 100 {
		t.Errorf("Code = %d, want 100 — a parseable envelope must be read even when truncated", apiErr.Code)
	}
	if apiErr.Message != "bad param" {
		t.Errorf("Message = %q, want %q — the read diagnostic must not overwrite Meta's message", apiErr.Message, "bad param")
	}
	// ...but the response is NOT presented as a complete rejection.
	if !apiErr.EnvelopeUnreadable {
		t.Error("EnvelopeUnreadable = false, want true — a truncated body may be missing fields Meta sent, so it must not read as a clean rejection")
	}
	// A non-rate-limit code is final: no retry.
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 — code 100 is not a throttle", n)
	}
}

// TestMultipartFramingMatchesBufferedEncoding is the equivalence proof the zero-copy upload
// rests on: prefix + image + suffix must be byte-for-byte what a buffered multipart.Writer
// produces for the same part.
//
// This is the assertion that makes the change safe rather than plausible. If the framing were
// off by a CRLF, a boundary, or a header, Meta would reject a body that LOOKS correct, and the
// failure would surface as an opaque upload error far from this code.
func TestMultipartFramingMatchesBufferedEncoding(t *testing.T) {
	for _, contentType := range []string{"image/png", "image/jpeg", ""} {
		t.Run("contentType="+contentType, func(t *testing.T) {
			image := []byte("\x89PNG\r\n\x1a\nNOT-A-REAL-IMAGE-BUT-EXACT-BYTES-MATTER")

			prefix, suffix, headerValue, err := multipartFraming(contentType)
			if err != nil {
				t.Fatalf("multipartFraming: %v", err)
			}

			// The reference encoding: what the code did before, on the SAME boundary. The
			// boundary is random per writer, so it is read back out of the header value and
			// pinned onto the reference writer — otherwise the two could never match and the
			// test would be comparing noise.
			_, params, perr := mime.ParseMediaType(headerValue)
			if perr != nil {
				t.Fatalf("parse Content-Type %q: %v", headerValue, perr)
			}
			boundary := params["boundary"]
			if boundary == "" {
				t.Fatal("framing returned no boundary in its Content-Type")
			}

			var ref bytes.Buffer
			mw := multipart.NewWriter(&ref)
			if berr := mw.SetBoundary(boundary); berr != nil {
				t.Fatalf("SetBoundary: %v", berr)
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="source"; filename="creative"`)
			if strings.TrimSpace(contentType) != "" {
				h.Set("Content-Type", contentType)
			}
			part, cerr := mw.CreatePart(h)
			if cerr != nil {
				t.Fatalf("CreatePart: %v", cerr)
			}
			if _, werr := part.Write(image); werr != nil {
				t.Fatalf("part.Write: %v", werr)
			}
			if clerr := mw.Close(); clerr != nil {
				t.Fatalf("Close: %v", clerr)
			}

			got := make([]byte, 0, len(prefix)+len(image)+len(suffix))
			got = append(append(append(got, prefix...), image...), suffix...)

			if !bytes.Equal(got, ref.Bytes()) {
				t.Errorf("framed body differs from the buffered encoding\n got: %q\nwant: %q",
					got, ref.Bytes())
			}
			// And it must still PARSE as multipart, which is the property Meta actually cares
			// about — equality with our own reference would not catch both sides being wrong.
			mr := multipart.NewReader(bytes.NewReader(got), boundary)
			p, nerr := mr.NextPart()
			if nerr != nil {
				t.Fatalf("framed body does not parse as multipart: %v", nerr)
			}
			readBack, rerr := io.ReadAll(p)
			if rerr != nil {
				t.Fatalf("read part: %v", rerr)
			}
			if !bytes.Equal(readBack, image) {
				t.Errorf("round-tripped part = %q, want the original image bytes", readBack)
			}
		})
	}
}

// TestUploadImageDoesNotCopyTheImage is the MEMORY assertion, and it is the one that fails on
// the previous implementation.
//
// The defect: multipart.Writer copied every image into a second buffer held for the whole retry
// sequence, outside dispatch.AssetReserver's budget. Five concurrent dispatches therefore added
// ~150 MiB the aggregate bound did not account for.
//
// MEASURED, not derived. testing.AllocsPerRun is not usable here (the upload does I/O), so this
// reads TotalAlloc across a real upload against a local server. TotalAlloc only ever grows, so it
// is cumulative bytes allocated during the call and immune to whether the GC happened to run —
// which HeapAlloc is not.
//
// The same measurement against the previous buffered shape allocates ~100% of the payload
// (33,703,328 bytes for this 32 MiB fixture); framing it allocates ~0.5% (169,560 bytes), which
// is the prefix, the suffix and the HTTP machinery. The budget below sits between the two by a
// wide margin, so ordinary allocation noise cannot trip it and a reintroduced copy cannot pass it.
func TestUploadImageDoesNotCopyTheImage(t *testing.T) {
	const imageSize = 32 << 20 // large enough that a copy is unmistakable

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the send completes, without retaining it.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("server: drain body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":{"creative":{"hash":"abc123"}}}`))
	}))
	defer srv.Close()

	c := uploadRetryClient(srv.URL)
	image := make([]byte, imageSize)

	// Settle the heap first so the baseline is not carrying warm-up garbage.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := c.uploadImage(context.Background(), image, "image/png"); err != nil {
		t.Fatalf("uploadImage: %v", err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	const budget = imageSize / 2

	if allocated > budget {
		t.Errorf("uploadImage allocated %d bytes for a %d-byte image (budget %d): the payload is "+
			"being copied into the multipart body instead of framed around, which puts a second "+
			"copy of every image outside dispatch.AssetReserver's budget",
			allocated, imageSize, budget)
	}
	t.Logf("allocated %d bytes uploading a %d-byte image (%.2f%% of payload)",
		allocated, imageSize, 100*float64(allocated)/float64(imageSize))
}

// TestUploadImageRetrySendsTheFullBodyEveryAttempt is the correctness guard for the zero-copy
// change, and it protects against a defect that would be WORSE than the memory it saves.
//
// An io.Reader is consumed by the send. If the same reader were reused across attempts, the
// retry would post an EMPTY or TRUNCATED body — and Meta would reject it for a reason that has
// nothing to do with the throttle that caused the retry, so the failure would be misdiagnosed.
// The previous implementation avoided this by keeping a full encoded COPY and wrapping a fresh
// bytes.Reader per attempt; this one builds a fresh io.MultiReader over the same three slices,
// which is the same guarantee without the copy.
//
// Asserted by capturing every attempt's body server-side and requiring them to be BYTE-IDENTICAL
// and to contain the whole image. Comparing lengths alone would miss a body that is the right
// size but framed differently on the second attempt.
func TestUploadImageRetrySendsTheFullBodyEveryAttempt(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("IMAGE-PAYLOAD-", 4096))

	var mu sync.Mutex
	var bodies [][]byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("server: read body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, got)
		n := len(bodies)
		mu.Unlock()

		if n == 1 {
			// Throttle the first attempt so the client retries.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"calls to this api have exceeded the rate limit","code":4}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":{"creative":{"hash":"deadbeef"}}}`))
	}))
	defer srv.Close()

	c := uploadRetryClient(srv.URL)
	hash, err := c.uploadImage(context.Background(), image, "image/png")
	if err != nil {
		t.Fatalf("uploadImage: %v", err)
	}
	if hash != "deadbeef" {
		t.Errorf("hash = %q, want deadbeef", hash)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("the server saw %d attempt(s), want at least 2 — the retry never happened, so "+
			"this test proves nothing about replay", len(bodies))
	}
	// The retry must be byte-identical to the first attempt: same framing, same payload.
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("attempt 2 body differs from attempt 1 (%d vs %d bytes): the body is not being "+
			"replayed faithfully, so a retry sends something other than the original image",
			len(bodies[0]), len(bodies[1]))
	}
	// And every attempt must actually carry the image, not an empty part.
	for i, body := range bodies {
		if !bytes.Contains(body, image) {
			t.Errorf("attempt %d does not contain the full image: a consumed reader was reused, "+
				"so the retry posted a truncated or empty body", i+1)
		}
	}
}

// TestUploadRetryReusesTheConnectionAfterAnOverCapThrottle pins the connection-reuse
// property of the throttle retry path, which is the whole point of draining a capped body
// before closing it.
//
// THE DEFECT IT CLOSES. The over-cap read stopped at maxResponseBody+1 and closed the body
// with the remainder unread. net/http can only return a connection to the idle pool once its
// body is read to EOF, so closing early forced a NEW TCP+TLS connection for the retry — at
// the exact moment Meta is rate-limiting the service. The sibling clients all drain first.
//
// WHY IT COUNTS CONNECTIONS RATHER THAN INSPECTING THE POOL. There is no exported way to ask
// a Transport how many idle connections it holds, so the observable that actually matters is
// counted directly: httptest.Server's ConnState fires per accepted connection, so a reused
// connection produces ONE StateNew for two requests and a non-reused one produces two.
func TestUploadRetryReusesTheConnectionAfterAnOverCapThrottle(t *testing.T) {
	var conns int32
	var attempts int32

	// A throttle body LARGER than the cap, so the first response takes the over-cap arm.
	oversized := `{"error":{"message":"` + strings.Repeat("x", int(maxResponseBody)+1024) +
		`","code":4}}`

	// NewUnstartedServer, not NewServer: ConnState must be installed BEFORE the serve loop
	// starts. Setting it on an already-started server is a write racing the accept goroutine's
	// read — which -race catches, and which is the same class of defect this round is fixing.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, oversized)
			return
		}
		_, _ = io.WriteString(w, `{"images":{"creative":{"hash":"AFTER_RETRY"}}}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	hash, err := uploadRetryClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("an over-cap 429 must be retried, not returned as final: %v", err)
	}
	if hash != "AFTER_RETRY" {
		t.Errorf("hash = %q, want AFTER_RETRY", hash)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (the premise of the reuse assertion)", got)
	}
	// The assertion: both attempts rode ONE connection. Without the bounded drain this is 2.
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Errorf("the retry opened a NEW connection (%d total) instead of reusing the first: "+
			"an over-cap body closed without draining cannot be pooled, so every retry pays a "+
			"fresh TCP/TLS handshake while Meta is already rate-limiting us", got)
	}
}
