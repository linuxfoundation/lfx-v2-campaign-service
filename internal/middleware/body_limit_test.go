// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoReadAll is the stand-in for a Goa decoder: it reads the WHOLE body, which is precisely
// what json.Decoder does behind the mux. Recording how many bytes it managed to read is what
// makes "the server buffered an unbounded body" observable — asserting only the status code
// would pass against a middleware that let the read complete and then reported 413 afterwards,
// which is the very behaviour being fixed.
type echoReadAll struct {
	read    int64
	readErr error
	called  bool
}

func (e *echoReadAll) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.called = true
	n, err := io.Copy(io.Discard, r.Body)
	e.read = n
	e.readErr = err
	if err != nil {
		// A handler that cannot read its body writes nothing useful; the middleware has
		// already signalled the ResponseWriter via MaxBytesReader.
		return
	}
	w.WriteHeader(http.StatusOK)
}

// TestMaxBodyBytes_RejectsDeclaredOversizeWithoutReading covers the Content-Length arm: a body
// that ANNOUNCES it is too large is refused before a single byte is read, and the handler never
// runs at all.
func TestMaxBodyBytes_RejectsDeclaredOversizeWithoutReading(t *testing.T) {
	const limit int64 = 1024

	h := &echoReadAll{}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", int(limit)+1)))
	// httptest sets ContentLength from the reader; assert the premise rather than assume it.
	if req.ContentLength <= limit {
		t.Fatalf("fixture ContentLength = %d, want > limit %d", req.ContentLength, limit)
	}
	rec := httptest.NewRecorder()

	MaxBodyBytes(limit)(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	// The point of this arm: the oversize body never reached the handler, so nothing buffered it.
	if h.called {
		t.Errorf("handler ran on a declared-oversize request; it read %d bytes", h.read)
	}
	assertCleanJSON413(t, rec)
}

// TestMaxBodyBytes_RejectsUndeclaredOversizeMidRead covers the arm Content-Length cannot: a
// request whose declared length is ABSENT (chunked, or a caller that simply lies by omission).
// Here only reading can reveal the size, so the handler does run — but its read must FAIL rather
// than deliver the full body, and it must not get more than the cap.
//
// This is the case that proves the fix is not just a Content-Length check. Content-Length is
// caller-supplied; an attacker omits it.
func TestMaxBodyBytes_RejectsUndeclaredOversizeMidRead(t *testing.T) {
	const limit int64 = 1024
	const sent = int64(64 * 1024)

	h := &echoReadAll{}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", int(sent))))
	// Exactly the attacker's shape: no declared length, so the up-front arm cannot fire.
	req.ContentLength = -1
	rec := httptest.NewRecorder()

	MaxBodyBytes(limit)(h).ServeHTTP(rec, req)

	if !h.called {
		t.Fatal("handler should run: with no declared length the size is only knowable by reading")
	}
	if h.readErr == nil {
		t.Errorf("body read succeeded (%d bytes); want the read to fail past the cap", h.read)
	}
	// The bound that matters: the server buffered a fixed amount, not the whole stream.
	if h.read > limit {
		t.Errorf("read %d bytes, want at most the cap %d", h.read, limit)
	}
	if h.read >= sent {
		t.Errorf("read the entire %d-byte body; the cap did not bound the read", sent)
	}
}

// TestMaxBodyBytes_AllowsBodyAtTheLimit is the other half of a size guard: an at-the-limit
// request must pass intact. Without it, a mutation that rejects everything (or that is off by
// enough to refuse legitimate maximum-size uploads) would look like a working cap.
func TestMaxBodyBytes_AllowsBodyAtTheLimit(t *testing.T) {
	const limit int64 = 1024

	h := &echoReadAll{}
	body := strings.Repeat("a", int(limit))
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	rec := httptest.NewRecorder()

	MaxBodyBytes(limit)(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d for a body exactly at the limit", rec.Code, http.StatusOK)
	}
	if h.readErr != nil {
		t.Errorf("read error on an at-limit body: %v", h.readErr)
	}
	if h.read != limit {
		t.Errorf("handler read %d bytes, want the full %d", h.read, limit)
	}
}

// TestMaxBodyBytes_PassesBodylessRequests pins that a GET carrying no body is untouched. A
// bodyless request has ContentLength 0 or -1, and -1 is the same value the chunked arm sees, so
// this guards against a cap that accidentally refuses (or mangles) every read request.
func TestMaxBodyBytes_PassesBodylessRequests(t *testing.T) {
	for _, tc := range []struct {
		name          string
		contentLength int64
	}{
		{"zero length", 0},
		{"unknown length", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &echoReadAll{}
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.ContentLength = tc.contentLength
			rec := httptest.NewRecorder()

			MaxBodyBytes(1024)(h).ServeHTTP(rec, req)

			if !h.called {
				t.Fatal("handler did not run for a bodyless request")
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

// assertCleanJSON413 pins the RESPONSE SHAPE, not merely the status. The gap being closed is
// partly that an oversized body otherwise surfaces as a JSON *decode* error (400, "invalid
// character..."), which misdirects the client into re-encoding a payload that was simply too
// big. It also checks the message carries nothing derived from the request.
func assertCleanJSON413(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the service's JSON error shape: %v (body %q)", err, rec.Body.String())
	}
	if body.Code != "413" {
		t.Errorf("code = %q, want %q", body.Code, "413")
	}
	if body.Message == "" {
		t.Error("message is empty; a 413 must say why the request was refused")
	}
	// Never echo the rejected content. The fixture body is a run of 'a's, so its presence in the
	// message would mean request bytes were reflected back.
	if strings.Contains(body.Message, "aaaa") {
		t.Errorf("message echoes request bytes: %q", body.Message)
	}
}

// TestMaxBodyBytes_ConvertsTruncatedReadTo413 pins the half of the read arm that a status-code
// check on the handler alone cannot see.
//
// http.MaxBytesReader reports the overflow to whoever performs the READ — in production that is
// the Goa decoder, deep behind the mux — and that decoder cannot tell "the body was cut off"
// from "the body was malformed". It reports both as a generic 400 about invalid JSON. So a
// handler that reads a truncated body and answers 400 is exactly what production does, and the
// middleware must REPLACE that response rather than forward it.
//
// The fake handler here reproduces that precisely: it reads, sees the error, and writes its own
// 400 with a decode-shaped message. The middleware must discard both the status and the body.
func TestMaxBodyBytes_ConvertsTruncatedReadTo413(t *testing.T) {
	const limit int64 = 1024

	decoderLike := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"400","message":"invalid character looking for beginning of value"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", int(limit)*8)))
	req.ContentLength = -1 // force the read arm, not the Content-Length arm
	rec := httptest.NewRecorder()

	MaxBodyBytes(limit)(decoderLike).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d — a decode 400 caused by truncation must be replaced", rec.Code, http.StatusRequestEntityTooLarge)
	}
	// The handler's misleading body must not survive alongside the corrected status.
	if strings.Contains(rec.Body.String(), "invalid character") {
		t.Errorf("the decoder's misleading message leaked into the 413 response: %q", rec.Body.String())
	}
	assertCleanJSON413(t, rec)
}

// TestMaxBodyBytes_ForwardsHandlerResponseWhenUnderLimit is the counterweight: the middleware
// buffers every response in order to be able to replace one, so it must replay an ordinary
// response — status AND body — untouched when nothing was truncated. Without this, a middleware
// that swallowed or rewrote all responses would still pass every test above.
func TestMaxBodyBytes_ForwardsHandlerResponseWhenUnderLimit(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Custom", "kept")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"asset-1"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("small"))
	rec := httptest.NewRecorder()

	MaxBodyBytes(1024)(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d passed through untouched", rec.Code, http.StatusCreated)
	}
	if got := rec.Body.String(); got != `{"id":"asset-1"}` {
		t.Errorf("body = %q, want the handler's response replayed verbatim", got)
	}
	if got := rec.Header().Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom = %q, want the handler's header preserved", got)
	}
}
