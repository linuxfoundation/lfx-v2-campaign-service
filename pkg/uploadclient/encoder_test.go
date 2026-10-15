// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package uploadclient

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	goahttp "goa.design/goa/v3/http"
)

// payload exercises characters encoding/json escapes (HTML-sensitive runes), so a drift from
// goa's marshalling choice would change the bytes and show up here.
type payload struct {
	ContentType string `json:"content_type"`
	Bytes       string `json:"bytes"`
	Note        string `json:"note"`
}

var sample = payload{ContentType: "image/png", Bytes: "iVBORw0KGgo=", Note: "<a&b>"}

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	// Built exactly as the generated BuildUploadCreativeAssetRequest builds it: a nil body.
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/creative-assets", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func readBody(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRequestEncoder_IsADropInThatDeclaresItsLength pins the two halves of the contract: the wire
// bytes are exactly goa's, and the length is declared where goa's is not.
func TestRequestEncoder_IsADropInThatDeclaresItsLength(t *testing.T) {
	stock := newRequest(t)
	if err := goahttp.RequestEncoder(stock).Encode(&sample); err != nil {
		t.Fatal(err)
	}
	want := readBody(t, stock.Body)

	req := newRequest(t)
	if err := RequestEncoder(req).Encode(&sample); err != nil {
		t.Fatal(err)
	}
	got := readBody(t, req.Body)

	if !bytes.Equal(got, want) {
		t.Errorf("body = %q, want goa's encoding %q: this must be a drop-in for goahttp.RequestEncoder", got, want)
	}
	if req.ContentLength != int64(len(want)) {
		t.Errorf("ContentLength = %d, want %d: an undeclared upload is charged the whole admission budget",
			req.ContentLength, len(want))
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// GetBody must replay the same bytes, more than once, as goa's encoder allows.
	for i := range 2 {
		rc, err := req.GetBody()
		if err != nil {
			t.Fatalf("GetBody #%d: %v", i+1, err)
		}
		if replay := readBody(t, rc); !bytes.Equal(replay, want) {
			t.Errorf("GetBody #%d = %q, want %q", i+1, replay, want)
		}
	}
}

// TestRequestEncoder_KeepsACallerContentType matches goa's encoder, which only defaults the header.
func TestRequestEncoder_KeepsACallerContentType(t *testing.T) {
	req := newRequest(t)
	req.Header.Set("Content-Type", "application/vnd.example+json")
	if err := RequestEncoder(req).Encode(&sample); err != nil {
		t.Fatal(err)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/vnd.example+json" {
		t.Errorf("Content-Type = %q, want the caller's value preserved", ct)
	}
}

// TestRequestEncoder_LeavesTheRequestUntouchedOnError: a marshal failure must not leave a body
// installed with a length that describes nothing.
func TestRequestEncoder_LeavesTheRequestUntouchedOnError(t *testing.T) {
	req := newRequest(t)
	if err := RequestEncoder(req).Encode(make(chan int)); err == nil {
		t.Fatal("Encode(chan) succeeded, want a marshal error")
	}
	if req.Body != nil || req.ContentLength != 0 || req.GetBody != nil {
		t.Errorf("request modified on a failed encode: body=%v length=%d getBody=%v",
			req.Body != nil, req.ContentLength, req.GetBody != nil)
	}
}
