// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"encoding/json"
	"errors"
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

// TestMaxBodyBytes_DoesNotBufferTheHappyPath pins the DEFERRAL, which is the whole point of
// deferredBufferWriter over an unconditional recorder.
//
// Buffering every response would tax all traffic — including bodyless GETs, whose http.NoBody is
// non-nil and so reaches this middleware — to serve a rewrite that fires only when the cap trips.
// Correctness alone cannot detect the difference: an always-buffering implementation passes every
// other test in this file, verified by mutation. So the property has to be observed directly,
// through the ResponseWriter: a pass-through write reaches the underlying writer DURING the
// handler, whereas a buffered one appears only after flush.
//
// The probe records when bytes arrive relative to the handler returning. It is the only way to
// distinguish "wrote through" from "wrote, held, and replayed", since both end with identical
// bytes at the client.
func TestMaxBodyBytes_DoesNotBufferTheHappyPath(t *testing.T) {
	probe := &writeTimingRecorder{ResponseWriter: httptest.NewRecorder()}

	var sawDuringHandler bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
		// Read the probe BEFORE returning: if the middleware passed through, the bytes are
		// already at the underlying writer; if it buffered, they are not.
		sawDuringHandler = probe.wrote()
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("small"))
	MaxBodyBytes(1024)(h).ServeHTTP(probe, req)

	if !sawDuringHandler {
		t.Error("an under-limit response was buffered; the happy path must write straight through, not accumulate in memory")
	}
}

// TestMaxBodyBytes_BodylessRequestIsNotBuffered is the same property for the case that dominates
// real traffic. http.NoBody is NOT nil, so a GET reaches the body-wrapping path and would be
// buffered by an unconditional recorder.
func TestMaxBodyBytes_BodylessRequestIsNotBuffered(t *testing.T) {
	probe := &writeTimingRecorder{ResponseWriter: httptest.NewRecorder()}

	var sawDuringHandler bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		sawDuringHandler = probe.wrote()
	})

	req := httptest.NewRequest(http.MethodGet, "/x", http.NoBody)
	if req.Body == nil {
		t.Fatal("premise: http.NoBody must be non-nil, otherwise this case never reaches the wrapper")
	}
	MaxBodyBytes(1024)(h).ServeHTTP(probe, req)

	if !sawDuringHandler {
		t.Error("a bodyless GET response was buffered; every read request would pay for a rewrite that cannot apply to it")
	}
}

// writeTimingRecorder reports whether anything has reached the underlying writer yet. It answers
// "was this written through, or held?" — a question the final bytes cannot answer, because both
// modes deliver the same bytes.
type writeTimingRecorder struct {
	http.ResponseWriter
	written bool
}

func (w *writeTimingRecorder) WriteHeader(status int) {
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *writeTimingRecorder) Write(p []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(p)
}

func (w *writeTimingRecorder) wrote() bool { return w.written }

// TestDeferredBufferWriter_ExposesFlusherHijackerUnwrap covers the interface-dropping gap:
// MaxBodyBytes wraps EVERY request with a non-nil body, so any capability the wrapper fails to
// forward is silently lost to every in-mux handler. Silently is the operative word — a handler
// type-asserting for http.Flusher simply gets ok==false and degrades, with no error anywhere.
func TestDeferredBufferWriter_ExposesFlusherHijackerUnwrap(t *testing.T) {
	var (
		sawFlusher  bool
		sawHijacker bool
		sawUnwrap   bool
	)

	h := MaxBodyBytes(1 << 20)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		_, sawHijacker = w.(http.Hijacker)
		type unwrapper interface{ Unwrap() http.ResponseWriter }
		_, sawUnwrap = w.(unwrapper)
		w.WriteHeader(http.StatusOK)
	}))

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)))

	if !sawFlusher {
		t.Error("wrapped writer does not implement http.Flusher: streaming handlers lose Flush")
	}
	if !sawHijacker {
		t.Error("wrapped writer does not implement http.Hijacker: upgrade handlers lose Hijack")
	}
	if !sawUnwrap {
		t.Error("wrapped writer exposes no Unwrap: ResponseController cannot reach the base writer")
	}
}

// TestDeferredBufferWriter_UnwrapReturnsTheBaseWriter asserts Unwrap returns the ACTUAL embedded
// writer, not merely something non-nil. A wrapper that returned itself would satisfy the
// interface check above while leaving ResponseController in an infinite unwrap loop.
func TestDeferredBufferWriter_UnwrapReturnsTheBaseWriter(t *testing.T) {
	base := httptest.NewRecorder()
	w := &deferredBufferWriter{ResponseWriter: base, buffering: func() bool { return false }}

	got := w.Unwrap()
	if got != http.ResponseWriter(base) {
		t.Errorf("Unwrap() = %#v, want the embedded base writer %#v", got, base)
	}
}

// flushRecorder counts Flush calls that actually reach the base writer.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++ }

// TestDeferredBufferWriter_FlushPassesThroughUnderLimit proves Flush is forwarded on the
// ordinary (pass-through) path, which is the case a streaming handler depends on.
func TestDeferredBufferWriter_FlushPassesThroughUnderLimit(t *testing.T) {
	base := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &deferredBufferWriter{ResponseWriter: base, buffering: func() bool { return false }}

	w.WriteHeader(http.StatusOK)
	w.Flush()

	if base.flushes != 1 {
		t.Errorf("base writer saw %d flushes, want 1: Flush was not forwarded", base.flushes)
	}
}

// TestDeferredBufferWriter_FlushDoesNotEscapeBuffering is the REGRESSION guard on the half of M2
// that is already correct. Forwarding Flush unconditionally would push a response describing a
// truncated request onto the wire and make it unrewritable — defeating the buffering the two
// write-timing tests protect. While held, Flush must be inert.
func TestDeferredBufferWriter_FlushDoesNotEscapeBuffering(t *testing.T) {
	base := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &deferredBufferWriter{ResponseWriter: base, buffering: func() bool { return true }}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("truncated-body-response"))
	w.Flush()

	if base.flushes != 0 {
		t.Errorf("base writer saw %d flushes while buffering, want 0: "+
			"Flush escaped the buffer and committed a response that must stay rewritable",
			base.flushes)
	}
	if w.committed() {
		t.Error("writer reports committed while buffering: the 413 rewrite would be skipped")
	}
	if base.Body.Len() != 0 {
		t.Errorf("buffered bytes reached the base writer: %q", base.Body.String())
	}
}

// TestDeferredBufferWriter_HijackWithoutSupportReturnsErrNotSupported asserts the degradation is
// an explicit error rather than a panic when the base writer cannot hijack.
func TestDeferredBufferWriter_HijackWithoutSupportReturnsErrNotSupported(t *testing.T) {
	w := &deferredBufferWriter{
		ResponseWriter: httptest.NewRecorder(), // does not implement http.Hijacker
		buffering:      func() bool { return false },
	}
	c, rw, err := w.Hijack()
	if err == nil {
		t.Fatal("Hijack on a non-hijackable writer returned nil error")
	}
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("err = %v, want http.ErrNotSupported", err)
	}
	if c != nil || rw != nil {
		t.Error("Hijack returned a connection despite failing")
	}
}

// TestMaxBodyBytes_TrailingBytesAfterAValidValueAreNeverConsumed pins the ONE case where
// tracked.exceeded() legitimately stays false on an over-cap request, so that the reason it is
// harmless is recorded as an assertion rather than as an argument.
//
// The shape: a VALID JSON value followed by megabytes of trailing whitespace, sent without a
// declared length. json.Decoder.Decode stops at the end of the first complete value, so it never
// reads far enough to trip MaxBytesReader — the cap does not fire, the handler succeeds, and the
// request is answered 200. Read only that far, it looks like a bypass of the byte bound.
//
// It is not one, and the distinction is WHAT WAS READ rather than what was sent. The bound this
// middleware exists to provide is on bytes the process CONSUMES, not on bytes a client is willing
// to transmit. Bytes the decoder never asks for are never pulled off the socket into this
// process: they sit in the kernel's receive buffer, the handler returns, and net/http closes the
// connection rather than draining them. Nothing allocates them, so the memory bound holds.
//
// The two assertions below are the ones that make that checkable. The first is the load-bearing
// one: consumption must NOT scale with the payload. A middleware that had actually been bypassed
// would read the trailing bytes, and reading them is what costs memory.
//
// This also settles the question for the pricing rule in constants.UploadAdmissionWeightFor,
// whose floor for undeclared bodies rests on "MaxBodyBytes bounds the real body regardless".
// That premise survives: an undeclared request still cannot get MORE bytes into this process by
// appending trailing junk, because the trailing junk is not read. The companion case — oversize
// bytes INSIDE the JSON value, which the decoder must read and which is the only shape that
// could actually store an over-cap asset — is covered by
// TestMaxBodyBytes_OversizeInsideTheValueStillTrips413 below, and there the cap DOES fire.
func TestMaxBodyBytes_TrailingBytesAfterAValidValueAreNeverConsumed(t *testing.T) {
	const limit int64 = 4096

	// A decoder that stops at the first complete value, exactly like Goa's.
	var readByHandler int64
	decoderLike := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counted := &countingReader{r: r.Body}
		var v map[string]any
		if err := json.NewDecoder(counted).Decode(&v); err != nil {
			readByHandler = counted.n
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		readByHandler = counted.n
		w.WriteHeader(http.StatusOK)
	})

	envelope := `{"name":"asset","bytes":"AAAA"}`

	// The same request shape at two payload sizes an order of magnitude apart. If the trailing
	// bytes were being consumed, consumption would grow with the payload.
	small := measureTrailing(t, limit, decoderLike, envelope, 1<<20, &readByHandler)
	large := measureTrailing(t, limit, decoderLike, envelope, 16<<20, &readByHandler)

	// THE BOUND: what the process reads does not scale with what the client sends. Both are
	// capped by the decoder stopping at the value's end, not by the payload's size.
	if large > small*2 {
		t.Errorf("bytes consumed scaled with the payload: %d for 1 MiB trailing vs %d for 16 MiB — "+
			"the trailing bytes are being read into the process, which is the bypass this test denies",
			large, small)
	}
	// And in absolute terms it stays near the envelope, nowhere near the payload.
	if large > int64(len(envelope))*64 {
		t.Errorf("handler consumed %d bytes for a %d-byte value; trailing bytes are being drained into the process",
			large, len(envelope))
	}
}

// TestMaxBodyBytes_OversizeInsideTheValueStillTrips413 is the counterweight to the test above,
// and it is the case that actually matters for what gets STORED.
//
// Trailing bytes after a valid value are harmless because nobody reads them — but bytes INSIDE
// the value are different in kind: the decoder must read every one of them to produce the value,
// so an oversized `bytes` field is read, allocated, and would be persisted. That is the shape a
// bypass of this cap would need in order to store an over-cap asset.
//
// Here the cap fires as designed: the read runs past the limit, MaxBytesReader fails it,
// exceeded() goes true, and the middleware answers 413 with nothing decoded. Without this test,
// the sibling above could be misread as saying the cap is conditional in a way that matters for
// persistence. It is not — it is conditional only on bytes nobody wanted.
func TestMaxBodyBytes_OversizeInsideTheValueStillTrips413(t *testing.T) {
	const limit int64 = 64 << 10

	var decodedLen int
	var decodeFailed bool
	decoderLike := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Bytes []byte `json:"bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			decodeFailed = true
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		decodedLen = len(p.Bytes)
		w.WriteHeader(http.StatusOK)
	})

	// A base64 payload 128x the cap, inside the value.
	body := io.MultiReader(
		strings.NewReader(`{"name":"asset","bytes":"`),
		io.LimitReader(repeatReader{c: 'A'}, int64(limit)*128),
		strings.NewReader(`"}`),
	)
	req := httptest.NewRequest(http.MethodPost, "/x", body)
	req.ContentLength = -1 // undeclared: only the read arm can catch this
	rec := httptest.NewRecorder()

	MaxBodyBytes(limit)(decoderLike).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d — an oversize value must trip the cap at the read", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !decodeFailed {
		t.Error("the decoder completed on an oversize value; the cap did not bound the read")
	}
	if decodedLen != 0 {
		t.Errorf("decoded %d bytes from an over-cap body; nothing may be produced for persistence", decodedLen)
	}
	assertCleanJSON413(t, rec)
}

// countingReader records how many bytes a handler actually pulled from the body.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// repeatReader yields an endless run of one byte, so a large payload can be built without
// allocating it — the test must not itself buffer what it claims the server does not.
type repeatReader struct{ c byte }

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.c
	}
	return len(p), nil
}

// measureTrailing drives one envelope-plus-trailing-whitespace request and returns how many
// bytes the handler consumed.
func measureTrailing(t *testing.T, limit int64, h http.Handler, envelope string, trailing int64, read *int64) int64 {
	t.Helper()
	body := io.MultiReader(
		strings.NewReader(envelope),
		io.LimitReader(repeatReader{c: ' '}, trailing),
	)
	req := httptest.NewRequest(http.MethodPost, "/x", body)
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	*read = 0
	MaxBodyBytes(limit)(h).ServeHTTP(rec, req)
	return *read
}
