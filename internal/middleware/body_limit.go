// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
)

// MaxBodyBytes caps how much of a request body any handler can read.
//
// It closes a gap the design-level size bounds cannot: Goa's declared MaxLength on a
// Bytes attribute is checked by the GENERATED VALIDATOR against the already-decoded
// slice, which means goahttp.RequestDecoder's json.Decoder has already read the whole
// body off the socket and base64-decoded it by the time any limit is consulted. A
// caller that streams an oversized body therefore gets it buffered and decoded first,
// and the declared limit only reports afterwards on whatever survived. This middleware
// is the bound that applies BEFORE the body is read, so an oversized request costs a
// fixed, bounded amount of memory no matter what it declares.
//
// Two arms, because a body can exceed the cap in two different ways:
//
//   - Content-Length declares more than the cap. Rejected up front, without reading a
//     byte. This is the common, honest case (a real client uploading a too-large file)
//     and it is cheapest to answer immediately.
//   - The declared length is absent, understated, or the body is chunked. Then only
//     reading can reveal the size, so http.MaxBytesReader wraps the body and fails the
//     read once the handler has consumed one byte past the cap. MaxBytesReader also
//     signals the ResponseWriter to stop reading further, so the connection is not left
//     draining an attacker's stream.
//
// The second arm is why this is not merely a Content-Length check: Content-Length is
// caller-supplied and a chunked request carries none at all.
//
// Both arms answer 413 with the service's JSON error shape rather than letting the
// overflow surface as a decode failure. A truncated body reaching the Goa decoder
// produces "invalid character ... looking for beginning of value" with a 400 — which
// tells an operator the client sent malformed JSON when in fact the server refused to
// read it, and misdirects a legitimate client into re-encoding a payload that was
// merely too big.
//
// Nothing about the body is logged or echoed. The response names the limit and the
// nature of the failure only; the rejected bytes are never read into a message, and on
// the Content-Length arm they are never read at all.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A GET/DELETE with no body carries ContentLength 0 (or -1 when unknown) and
			// is unaffected by either arm.
			if r.ContentLength > limit {
				writeRequestTooLarge(w)
				return
			}
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}
			// Wrap the body AND remember whether the limit was actually hit. MaxBytesReader
			// reports the overflow to whoever performs the read — which is the Goa decoder,
			// deep behind the mux — and that decoder does not distinguish "the body was cut
			// off" from "the body was malformed": it turns both into a generic 400 about
			// invalid JSON. So observing the error at the READ is the only place the
			// distinction still exists.
			tracked := &limitTrackingBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit)}
			r.Body = tracked

			// Buffer the response so the 400 the decoder is about to write can be replaced.
			// Nothing is forwarded to the client until the handler returns, so there is no
			// risk of a half-written body: either the recorder is flushed verbatim, or it is
			// discarded and a 413 is written instead.
			rec := &bufferedResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			if tracked.exceeded() {
				// The handler's response describes a truncated body, so it is wrong whatever
				// it says. Discard it and answer the real reason.
				writeRequestTooLarge(w)
				return
			}
			rec.flush()
		})
	}
}

// limitTrackingBody records whether a read ever tripped the MaxBytesReader limit. The flag is
// what lets the middleware tell a body that was CUT OFF from one that was merely malformed —
// a distinction the Goa decoder erases by reporting both as a JSON syntax error.
type limitTrackingBody struct {
	io.ReadCloser
	hit atomic.Bool
}

func (b *limitTrackingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var maxErr *http.MaxBytesError
	if err != nil && errors.As(err, &maxErr) {
		b.hit.Store(true)
	}
	return n, err
}

func (b *limitTrackingBody) exceeded() bool { return b.hit.Load() }

// bufferedResponseWriter holds the handler's response until the middleware has decided whether
// the request was truncated. Only headers, status and body bytes are captured; the middleware
// either replays them verbatim or drops them in favour of a 413.
type bufferedResponseWriter struct {
	http.ResponseWriter
	buf         bytes.Buffer
	status      int
	wroteHeader bool
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.buf.Write(p)
}

// flush replays the captured response onto the real ResponseWriter. Headers were written
// straight through (the embedded Header map is the real one), so only status and body remain.
func (w *bufferedResponseWriter) flush() {
	w.ResponseWriter.WriteHeader(w.status)
	// Best-effort: the status is already committed, so a failed write has no recovery path.
	_, _ = w.ResponseWriter.Write(w.buf.Bytes())
}

// requestTooLargeBody mirrors the {code, message} shape the Goa error responses use, so
// a client parsing service errors does not need a special case for this one.
type requestTooLargeBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeRequestTooLarge emits the 413. The message is fixed: it states neither the cap
// nor anything about the request — no byte counts derived from the body, no content, no
// caller-supplied values.
func writeRequestTooLarge(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	// Best-effort: the status and headers are already committed, so a write failure here
	// (client hung up) leaves nothing to recover and nothing worth logging.
	_ = json.NewEncoder(w).Encode(requestTooLargeBody{
		Code:    "413",
		Message: "request body exceeds the maximum allowed size",
	})
}
