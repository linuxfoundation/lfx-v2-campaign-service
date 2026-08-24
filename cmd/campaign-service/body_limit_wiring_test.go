// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/middleware"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// realChain builds the mounted mux wrapped in the ACTUAL middleware chain the server runs. A
// test against a hand-assembled chain would prove the middleware works but not that it is wired,
// which is the failure mode that matters: the cap existing and the cap being applied are
// different facts, and only the second one protects the service.
func realChain(t *testing.T) http.Handler {
	t.Helper()

	endpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))

	mux, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, audienceEndpoints, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	return buildHandler(mux, &config.Config{}, middleware.NewInflightTracker())
}

const uploadRoute = "/projects/cncf/briefs/6ba7b810-9dad-11d1-80b4-00c04fd430c8/creative-assets"

// TestUploadRoute_RejectsOversizeBodyBeforeDecoding is the test for the gap this PR closes.
//
// design/brief.go declares MaxLength(41943040) on the upload's `bytes` attribute, which reads
// like a size limit but does not bound the wire: the generated validator tests len() on the
// DECODED slice, and it only sees that slice after goahttp.RequestDecoder's json.Decoder has
// read the entire body off the socket and base64-decoded it. Before the cap, a caller could
// stream a body of any size and the server would buffer and decode all of it, then report the
// declared limit on whatever it had built. There was no http.MaxBytesReader anywhere in cmd/ or
// internal/ — every LimitReader in the tree caps an OUTBOUND response.
//
// The request here declares a Content-Length past the cap, so the refusal must come before any
// decoding. Asserting 413 specifically (not merely "an error") is the point: a 400 would mean
// the body was read and the JSON decoder rejected it, i.e. the buffering already happened.
func TestUploadRoute_RejectsOversizeBodyBeforeDecoding(t *testing.T) {
	h := realChain(t)

	// A body one byte past the cap. Streamed rather than materialised: allocating 42 MiB in a
	// -race test to prove a size limit would be its own small denial of service.
	oversize := constants.MaxRequestBodyBytes + 1
	req := httptest.NewRequest(http.MethodPost, uploadRoute, repeatReader('a', oversize))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = oversize

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d — an oversize upload must be refused as too large, not decoded", rec.Code, http.StatusRequestEntityTooLarge)
	}

	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("413 body is not the service's JSON error shape: %v (%q)", err, rec.Body.String())
	}
	if body.Code != "413" {
		t.Errorf("code = %q, want %q", body.Code, "413")
	}
	// Never reflect the rejected payload.
	if strings.Contains(body.Message, "aaaa") {
		t.Errorf("413 message echoes request bytes: %q", body.Message)
	}
}

// TestUploadRoute_RejectsOversizeChunkedBody covers the arm a Content-Length check cannot: a
// request that declares NO length. Content-Length is caller-supplied, so an attacker simply
// omits it; if the cap depended on it alone, this request would stream unbounded into the
// decoder.
//
// The FIXTURE is the whole test. An earlier version streamed a run of 'a' bytes, which is
// invalid JSON at byte 1 — json.Decoder aborted after ~512 bytes and answered 400 whether or
// not the cap was installed, so the test passed against a completely uncapped server and
// proved nothing. That is the shape where the fixture's own error absorbs the mutation.
//
// So the body here is WELL-FORMED and streams: a real JSON envelope whose base64 field is an
// unbounded run of 'A' (a valid base64 character, so the decoder keeps consuming rather than
// erroring). The decoder therefore has to read past the cap, which is the only condition under
// which the cap can be the thing that stops it — and 413 is then asserted UNCONDITIONALLY,
// with no early return and no t.Logf escape hatch.
func TestUploadRoute_RejectsOversizeChunkedBody(t *testing.T) {
	h := realChain(t)

	body := io.MultiReader(
		strings.NewReader(`{"content_type":"image/png","bytes":"`),
		repeatReader('A', constants.MaxRequestBodyBytes+1),
		strings.NewReader(`"}`),
	)
	req := httptest.NewRequest(http.MethodPost, uploadRoute, body)
	req.Header.Set("Content-Type", "application/json")
	// No declared length — the up-front arm cannot fire, only the read cap can.
	req.ContentLength = -1

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d — an undeclared (chunked) oversize body must be refused by the read cap, not decoded",
			rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestUploadRoute_AdmitsMaximumLegalUpload is the half that stops the cap from being "reject
// everything". It builds the LARGEST body the contract admits — a full 30 MiB image, base64'd
// into the real JSON envelope — and asserts the cap does not refuse it.
//
// This is also what validates the constant's derivation. Base64 expands by exactly 4/3, so 30
// MiB of image is 41,943,040 characters — 40 MiB to the byte — before the envelope is added. A
// cap of 40 MiB would therefore reject every maximum-size upload, and no test that only sends
// oversize bodies would ever notice. The assertion is specifically "not 413": the request is
// then refused further in for an unrelated reason (no repo bound in this no-database wiring, and
// no bearer token), which is fine — what matters is that the SIZE gate let it through.
//
// BOTH enum values are driven, and that is the point rather than thoroughness for its own sake.
// content_type is part of the body, so the envelope's size depends on which value it carries:
// "image/png" yields 41,943,079 bytes and "image/jpeg" — one character longer — yields
// 41,943,080, which is the TRUE worst legal body and the number pkg/constants/http.go derives.
// A fixture that sent only PNG could not distinguish a correct cap from one set to 41,943,079:
// that cap passes a PNG-only test while refusing every maximum-size JPEG, which the contract
// admits equally (design/brief.go's Enum). The subtest asserting the largest case is what makes
// the cap's last byte load-bearing.
func TestUploadRoute_AdmitsMaximumLegalUpload(t *testing.T) {
	const maxImage = 31457280 // the 30 MiB DECODED ceiling (maxCreativeStoredBytes); the
	// design's MaxLength(41943040) is this value base64-encoded, and it is the encoded form
	// that travels on the wire and that this test sizes the body from.
	// Encoded once: both subtests differ only in the enum value, and this is 40 MiB of work.
	encoded := base64.StdEncoding.EncodeToString(make([]byte, maxImage))

	// Ordered shortest-envelope first, so the failure message names the smaller case when the
	// cap is far too low and the larger one when it is off by only the enum's difference.
	for _, tc := range []struct {
		contentType string
		wantSize    int64 // the exact wire size, pinned so a change in encoding is visible here
	}{
		{model.MimeTypePNG, 41943079},
		{model.MimeTypeJPEG, 41943080}, // the worst case the contract admits
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			// The real wire shape: the image base64'd into the JSON body Goa decodes.
			body := fmt.Sprintf(`{"content_type":%q,"bytes":%q}`, tc.contentType, encoded)

			// Pin the premise rather than trusting the arithmetic: these are the numbers the
			// constant must clear, and if Goa ever changed encodings — or a third field were
			// added to the body — these assertions are what would say so.
			if int64(len(body)) != tc.wantSize {
				t.Fatalf("fixture body is %d bytes, want exactly %d: the envelope changed, so the "+
					"worst case pkg/constants/http.go derives is no longer the one being tested", len(body), tc.wantSize)
			}
			if int64(len(body)) <= 40<<20 {
				t.Fatalf("fixture body is %d bytes; expected a max-size upload to exceed 40 MiB after base64", len(body))
			}
			if int64(len(body)) > constants.MaxRequestBodyBytes {
				t.Fatalf("MaxRequestBodyBytes (%d) is below the largest LEGAL upload (%d bytes, content_type %q): "+
					"every maximum-size image of that type would be refused with 413",
					constants.MaxRequestBodyBytes, len(body), tc.contentType)
			}

			h := realChain(t)
			req := httptest.NewRequest(http.MethodPost, uploadRoute, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("a maximum-size legal upload (%d bytes on the wire, content_type %q) was refused with 413; the cap is too small",
					len(body), tc.contentType)
			}
		})
	}
}

// repeatReader streams n copies of c without allocating n bytes.
func repeatReader(c byte, n int64) io.Reader {
	return io.LimitReader(byteStream(c), n)
}

type byteStream byte

func (b byteStream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// TestUploadRoute_413IsDeclaredInTheContract binds the CLIENT half of the body cap.
//
// The 413 is emitted by middleware.MaxBodyBytes, which sits outside the mux, so it never passes
// through a Goa encoder — which is exactly why it is easy to ship undeclared. When the design
// omits it, the generated client has no decode case for the status and reports an ordinary
// oversized upload as ErrInvalidResponse (an unknown-status failure), while the OpenAPI documents
// omit the behaviour entirely so a consumer generating from the spec cannot know it can happen.
//
// The middleware tests prove the server SENDS 413; this proves the contract DECLARES it. Those
// are independent failures: every other test on this PR passes with the declaration missing.
func TestUploadRoute_413IsDeclaredInTheContract(t *testing.T) {
	// The generated endpoint-level error name, produced from the design's
	// Error("PayloadTooLarge", ...). Its existence is the compile-time half of the assertion:
	// if the design drops the declaration, this file stops building.
	var err error = &briefsvc.PayloadTooLargeError{Code: "413", Message: "too large"}

	var tooLarge *briefsvc.PayloadTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %T, want *briefsvc.PayloadTooLargeError", err)
	}
	if tooLarge.Code != "413" {
		t.Errorf("code = %q, want %q", tooLarge.Code, "413")
	}

	// And the runtime half: the status must be mapped onto the upload route in the OpenAPI
	// document the service serves from kodata, not merely defined as a type.
	raw, readErr := os.ReadFile(filepath.Join("kodata", "gen", "http", "openapi3.json"))
	if readErr != nil {
		t.Fatalf("read openapi3.json: %v", readErr)
	}
	var doc struct {
		Paths map[string]struct {
			Post struct {
				Responses map[string]any `json:"responses"`
			} `json:"post"`
		} `json:"paths"`
	}
	if jsonErr := json.Unmarshal(raw, &doc); jsonErr != nil {
		t.Fatalf("parse openapi3.json: %v", jsonErr)
	}

	const route = "/projects/{project_id}/briefs/{brief_id}/creative-assets"
	spec, ok := doc.Paths[route]
	if !ok {
		t.Fatalf("route %s is absent from the OpenAPI document", route)
	}
	if _, ok := spec.Post.Responses["413"]; !ok {
		got := make([]string, 0, len(spec.Post.Responses))
		for code := range spec.Post.Responses {
			got = append(got, code)
		}
		sort.Strings(got)
		t.Errorf("413 is not declared on %s; documented responses are %v — an oversized upload would decode as ErrInvalidResponse", route, got)
	}
}
