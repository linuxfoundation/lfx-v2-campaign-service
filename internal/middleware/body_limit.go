// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
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
//     read once the handler has consumed one byte past the cap.
//
// What the second arm bounds is the READ — the handler cannot obtain more than the cap,
// so memory is bounded whatever the client sends. It does NOT close the connection.
// MaxBytesReader can request that only by type-asserting its ResponseWriter to net/http's
// unexported requestTooLarger interface, which solely the server's own *http.response
// satisfies; this middleware is mounted inside the request-ID and OTel wrappers (see
// buildHandler), so by read time the writer is a wrapper and that assertion fails
// silently. Moving MaxBodyBytes outermost would restore the signal but cost every 413 its
// request id and its trace span, which is the wrong trade for an operator diagnosing one.
// The consequence of the missing signal is that net/http may drain the remaining body
// before reusing the connection; the bytes are discarded rather than buffered, so the
// memory bound this middleware exists to provide still holds.
//
// The second arm is why this is not merely a Content-Length check: Content-Length is
// caller-supplied and a chunked request carries none at all.
//
// WHAT THE SECOND ARM DOES NOT DETECT, and why that is not a hole. Because it fires at the READ,
// it can only fire on bytes someone reads. A caller may therefore send a valid JSON value
// followed by unbounded trailing bytes: json.Decoder stops at the end of the first complete
// value, never reads far enough to trip MaxBytesReader, and the request is answered normally
// with exceeded() still false. The cap did not fire on an over-cap transmission.
//
// That is harmless, and the reason is the difference between what is SENT and what is CONSUMED.
// The bound this middleware provides is on bytes read into this process. Bytes the decoder never
// requests are never pulled off the socket: they remain in the kernel receive buffer, the
// handler returns, and net/http closes the connection instead of draining them (the same missing
// requestTooLarger signal described above means there is no drain to perform). Nothing in this
// process allocates them, so the memory bound holds no matter how many are offered. Measured:
// against a 4 KiB cap, trailing payloads of 1 MiB through 512 MiB all left consumption flat at a
// few hundred KiB of socket buffer, with peak heap unchanged.
//
// The shape that WOULD matter is oversized bytes INSIDE the value, since the decoder must read
// every one of them to produce it — and there this arm fires exactly as designed, refusing with
// 413 having decoded nothing. Both cases are pinned by tests
// (TestMaxBodyBytes_TrailingBytesAfterAValidValueAreNeverConsumed and
// TestMaxBodyBytes_OversizeInsideTheValueStillTrips413), because the distinction is the kind a
// later reader is likely to mistake for a bypass.
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

			// Buffer the response so a 400 written after a truncated read can be replaced.
			//
			// The buffering is DEFERRED, not unconditional. Holding every response in memory
			// to rewrite the rare one would tax all traffic — including bodyless GETs, whose
			// http.NoBody is non-nil and would otherwise qualify — for a rewrite that fires
			// only when the cap is actually hit. So the writer starts in pass-through mode
			// and begins buffering at the moment the limit trips: bytes written BEFORE that
			// moment cannot describe a truncation, since the handler had not yet failed a
			// read, and by the time it writes a status the read has already failed.
			rec := &deferredBufferWriter{ResponseWriter: w, buffering: tracked.exceeded}
			next.ServeHTTP(rec, r)

			if tracked.exceeded() && !rec.committed() {
				// The handler's response describes a truncated body, so it is wrong whatever
				// it says. Discard the buffered version and answer the real reason.
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

// deferredBufferWriter passes writes straight through until the body limit trips, and buffers
// from that point on so the middleware can discard a response that describes a truncation.
//
// The two modes matter for cost: unconditional buffering would put every response on every
// route through a bytes.Buffer to serve one rewrite on one arm. Deferring means ordinary traffic
// pays nothing and only a request that actually overran is held.
//
// committed reports whether any bytes reached the client before buffering began. If they did,
// the response is already partly on the wire and CANNOT be replaced by a 413 — the status line
// is long gone — so the middleware flushes what remains rather than corrupting the exchange.
type deferredBufferWriter struct {
	http.ResponseWriter
	// buffering reports whether the limit has tripped. It is the tracker's own predicate, read
	// at write time rather than snapshotted, so the mode switches the instant the read fails.
	buffering   func() bool
	buf         bytes.Buffer
	status      int
	wroteHeader bool
	passedThru  bool
	held        bool
}

func (w *deferredBufferWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if w.buffering() {
		w.held = true
		return
	}
	w.passedThru = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *deferredBufferWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.held {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// committed reports whether any part of the response already reached the client.
func (w *deferredBufferWriter) committed() bool { return w.passedThru }

// flush replays anything held back. Headers were written through the embedded ResponseWriter's
// own map, so only the status and buffered body remain.
func (w *deferredBufferWriter) flush() {
	if !w.held {
		return
	}
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

// Unwrap exposes the underlying ResponseWriter so net/http's ResponseController can find
// capabilities this wrapper does not itself implement. Without it, wrapping every request in
// deferredBufferWriter silently strips whatever the base writer offered from any in-mux handler
// that reaches for it — a failure with no error and no log line.
//
// Unwrap is the general escape hatch; Flush and Hijack below are still implemented explicitly
// because both need the BUFFERING state respected, which an unwrapped writer cannot know about.
func (w *deferredBufferWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush implements http.Flusher.
//
// It deliberately does NOT flush while the response is being held. Flushing a buffered response
// would push a body describing a truncated request onto the wire, which is precisely what the
// buffering half of this writer exists to prevent — and once flushed it could no longer be
// replaced by the 413. While held, the call is a no-op: the data stays in the buffer and is
// either discarded (limit tripped) or replayed by flush().
//
// A pass-through response flushes normally, so an under-limit streaming handler is unaffected.
// If the base writer is not a Flusher, this is a no-op rather than a panic.
func (w *deferredBufferWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.held {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so a websocket-upgrade or other connection-taking handler
// mounted in the mux keeps working.
//
// Hijacking takes the raw connection, after which this middleware can no longer write anything
// to it — so a hijacked response can never be rewritten to a 413. That is correct rather than a
// gap: the handler owns the connection from that point, and marking the response committed keeps
// the middleware from trying to write a status onto a connection it no longer controls.
func (w *deferredBufferWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := h.Hijack()
	if err == nil {
		// The connection is gone from this middleware's control: never buffer or rewrite.
		w.passedThru = true
		w.held = false
	}
	return c, rw, err
}
