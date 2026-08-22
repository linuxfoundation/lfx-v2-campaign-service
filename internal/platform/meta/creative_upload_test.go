// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
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
// the single image part. A body a real multipart reader cannot walk fails the test
// here rather than silently producing an empty capture.
func captureUpload(t *testing.T, r *http.Request) uploadedPart {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("upload Content-Type %q is not parseable: %v", ct, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("upload Content-Type = %q, want multipart/form-data", mediaType)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var got uploadedPart
	for {
		p, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			t.Fatalf("walking multipart body: %v", perr)
		}
		b, rerr := io.ReadAll(p)
		if rerr != nil {
			t.Fatalf("reading multipart part: %v", rerr)
		}
		got.partCount++
		got.fieldName = p.FormName()
		got.filename = p.FileName()
		got.contentType = p.Header.Get("Content-Type")
		got.body = b
	}
	return got
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
	var got uploadedPart
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/adimages") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got = captureUpload(t, r)
		w.Header().Set("Content-Type", "application/json")
		// Meta keys the response map by the BASENAME of the filename we sent (the PHP
		// SDK reads images[basename(filename)]), so the fake echoes that contract.
		_, _ = io.WriteString(w, `{"images":{"`+path.Base(got.filename)+`":{"hash":"HASH_OK","url":"https://x/y.png"}}}`)
	}))
	defer srv.Close()

	hash, err := imageTestClient(srv.URL).uploadImage(context.Background(), []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("uploadImage error: %v", err)
	}
	if hash != "HASH_OK" {
		t.Fatalf("hash = %q, want HASH_OK", hash)
	}

	// Exactly one part: one image per call is the invariant the response guard below
	// depends on.
	if got.partCount != 1 {
		t.Errorf("part count = %d, want exactly 1 — one image per call", got.partCount)
	}
	if strings.TrimSpace(got.fieldName) == "" {
		t.Errorf("multipart part carries no field name")
	}
	// A filename must be present. NOTE ON THE EXTENSION: no Meta documentation states
	// that the filename must carry a real extension, and no authoritative source was
	// found either way — Meta sniffs image content for format and validation. This
	// asserts only PRESENCE, deliberately: asserting an extension rule would pin a
	// claim that is undocumented in both directions.
	if strings.TrimSpace(got.filename) == "" {
		t.Errorf("multipart part carries no filename; Graph needs one to treat the part as a file upload")
	}
	if got.contentType != "image/png" {
		t.Errorf("part Content-Type = %q, want image/png", got.contentType)
	}
	// The bytes must arrive VERBATIM. `bytes` is documented as a base64 string, but a
	// multipart file part carries raw content and Graph decodes the part itself;
	// double-encoding would corrupt every image.
	if string(got.body) != "PNGBYTES" {
		t.Errorf("uploaded bytes = %q, want the caller's bytes verbatim", got.body)
	}
	if enc := base64.StdEncoding.EncodeToString([]byte("PNGBYTES")); string(got.body) == enc {
		t.Errorf("bytes were base64-encoded into the file part (%q); a multipart file part carries raw content", got.body)
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
