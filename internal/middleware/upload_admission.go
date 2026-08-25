// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"
)

// UploadAdmission bounds the TOTAL bytes that concurrent upload requests may cause this process
// to allocate, admitting each request against a weighted semaphore before the body is read.
//
// # Why a middleware and not a guard inside the service method
//
// The per-request budgets already in place (constants.MaxRequestBodyBytes at 42 MiB, the design's
// MaxLength(41943040) encoded ceiling, the handler's 30 MiB maxCreativeStoredBytes,
// maxCreativeDecodedBytes at 80 MiB) each bound ONE request and say nothing
// about how many run at once. Against a fixed pod memory limit that is the whole problem: N
// concurrent, entirely LEGAL uploads multiply an unbounded-in-aggregate per-request allocation
// until the pod is OOM-killed, which denies service to every tenant on it and restarts the
// process. No amount of tightening a per-request number fixes an aggregate.
//
// The placement is the security property, and it is why this is not a semaphore wrapped around
// image.Decode in the service method. Goa's generated handler calls decodeRequest(r) and only
// THEN endpoint(...), and the generated endpoint calls authJWTFn as its first statement — so the
// request body is read off the socket and unmarshalled BEFORE any JWT is examined. A guard
// inside UploadCreativeAsset therefore runs after roughly 82 MiB (the buffered body plus the
// ~40 MiB base64 STRING the decoder materialises) has already been allocated on behalf of a
// caller who has not authenticated. The base64 DECODE now happens in the service method, after
// auth — but that moves ~30 MiB to the authenticated side, it does not shrink the pre-auth half,
// because the encoded string is larger than the bytes it encodes.
// It would bound the 80 MiB decode tail and leave the pre-auth half entirely unguarded, while
// reading — to a maintainer, and to a reviewer — as though the finding had been fixed. Sitting
// outside the mux, this middleware takes the permit before the decoder ever touches the body.
//
// What this does NOT do, stated plainly: it does not authenticate, and it does not stop an
// unauthenticated body from being read at all. Auth-before-body-read belongs at the gateway, not
// in this service. What it does own is that such a read cannot happen without first taking a
// permit from a budget derived from the pod's real memory limit — so the unauthenticated arm is
// bounded even though it is not eliminated.
//
// Non-upload routes are never gated. They carry bodies orders of magnitude smaller, and making
// them queue behind an upload would turn a memory control into an availability regression.
//
// WHAT THE PERMIT COSTS, and why it is not a flat charge. The weight is priced from the
// request's DECLARED body size (see constants.UploadAdmissionWeightFor), so a small upload takes
// a small share of the budget and a maximum-size one takes the ceiling. Charging every upload
// the worst-case weight is not merely pessimistic, it is a functional regression: with a budget
// equal to that weight the semaphore admits exactly ONE upload at a time regardless of size.
//
// THE HOLD IS THE WHOLE HANDLER, deliberately, and that is the cost of this placement. The
// permit is taken before next.ServeHTTP and released after it, so the socket read is INSIDE it —
// which is the point (the body must not be read unadmitted) and also the exposure: a client that
// dribbles its body holds its share of the budget for as long as the read takes. That is bounded
// by constants.DefaultReadTimeout rather than left open, and it is why the read deadline is a
// load-bearing part of this control rather than an unrelated tuning knob. Proportional pricing
// is what keeps a single slow client from pinning the ENTIRE budget: it now holds only what its
// declared size bought, so other uploads continue to be admitted alongside it.
//
// WHAT THE WEIGHT ACCOUNTS, precisely — because a bound that silently under-counts reads as
// protection while providing less than it appears to.
//
// It accounts the WIRE side of the inbound request: the buffered body and the base64 STRING the
// JSON decoder materialises for ONE upload on this HTTP path. (The base64-DECODED slice is
// allocated later, in the service method after auth; the weight covers it too, via the
// amplification ratio, but it is not part of the pre-auth half.)
//
// It does NOT account the decoded PIXEL buffer, and that is a split, not a gap. The weight is
// priced from Content-Length, and compression severs the link between wire bytes and decoded
// bytes: a flat 4000x4000 PNG is ~68 KiB on the wire and 61 MiB decoded, so it takes the
// minimum permit here while allocating a buffer this permit never paid for. Pixels are admitted
// against their own budget by service.DecodeReserver, at the only point their cost is knowable
// — after the header is read and before image.Decode allocates. The two bound different
// quantities with different worst cases; treating this middleware as covering both would make
// that reservation read as redundant.
//
// It does NOT account bodies sent to OTHER routes. isUploadRequest gates this middleware to the
// creative-asset POST, so every other body-bearing endpoint decodes without taking a permit,
// bounded only by MaxBodyBytes' per-request 42 MiB cap. That cap is per request, not aggregate,
// and Goa decodes before endpoint authentication on those routes too, so concurrent oversized
// JSON to any other POST can allocate pre-auth without touching this semaphore. The retention is
// real rather than theoretical: encoding/json materialises a string field in full before the
// generated MaxLength validator ever runs, so a route whose largest declared field is 8000
// characters can still hold ~42 MiB from one request.
//
// That is a deliberate scope boundary, not an oversight, and it is stated so the "pre-auth
// memory protection" claim above is not read as covering the whole server. This control exists
// for the one route whose CONTRACT admits a body three orders of magnitude larger than any
// other; extending it to every route means route-specific caps and weights, and a far smaller
// global body cap is the cheaper lever for the rest. Anyone tightening this should change
// MaxRequestBodyBytes (or make it per-route) rather than widening this middleware, because the
// exposure on other routes is the CAP being generous, not the permit being absent.
//
// It does NOT account memory that an unrelated code path later allocates from bytes that were
// stored earlier. Outbound dispatch reads assets back out of the database, but that happens in a
// dispatch worker, on bytes fetched from Postgres, long after the HTTP request that stored them
// has returned and released its permit. It is a different lifetime and a different code path,
// not an undercount of this one: no upload request gated here can multiply its own resident
// bytes through variant fan-out, because fan-out reads from storage rather than from the
// request. Dispatch-side memory is bounded by its own control on that path — the Meta dispatcher
// resolves each distinct asset once per dispatch and caps the total distinct bytes one dispatch
// may hold — so this middleware neither provides nor needs to claim that bound.
func UploadAdmission(budgetBytes int64, maxBodyBytes int64, weightFor func(contentLength int64) int64, wait time.Duration) func(http.Handler) http.Handler {
	sem := semaphore.NewWeighted(budgetBytes)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isUploadRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			// A body that is ALREADY too large by its own declaration is refused here, before
			// any permit is sought, and the reason is which of two answers the caller gets.
			//
			// 413 is permanent and client-fixable; the 503 this middleware sheds with is
			// transient and carries Retry-After. Seeking the permit first inverted them
			// whenever the budget was busy: weightFor prices anything above the amplification
			// threshold at the full worst-case weight, which equals the whole budget, so an
			// over-cap request waited the full shed timeout and was told to retry a request
			// that could never succeed at any size of budget.
			//
			// This does not weaken the pre-auth placement that the rest of this middleware
			// exists for. The check reads Content-Length only — a header already parsed before
			// this handler ran — so it consumes no body and admits nothing unadmitted. An
			// UNDECLARED or chunked body is not covered here and must not be: its size is
			// unknowable without reading it, so it still goes through admission and meets
			// MaxBodyBytes' MaxBytesReader arm downstream, which is where that case belongs.
			if maxBodyBytes > 0 && r.ContentLength > maxBodyBytes {
				writeRequestTooLarge(w)
				return
			}

			// Priced from the DECLARED size, not charged flat. A flat charge equal to the
			// budget made effective concurrency exactly 1: every upload, however small, took
			// the entire budget, so one slow body pinned the only permit for as long as the
			// read deadline allowed and every concurrent upload shed with 503. Pricing per
			// request keeps the aggregate bound identical — the semaphore still cannot issue
			// more than budgetBytes — while letting small uploads share it.
			weight := weightFor(r.ContentLength)
			if weight > budgetBytes {
				// A single request priced above the whole budget could never be admitted, and
				// semaphore.Acquire would block until its context expired and then shed. Answer
				// immediately instead: the outcome is the same 503, reached without holding the
				// caller for the wait.
				writeUploadShed(w)
				return
			}

			// A BOUNDED wait, not an unbounded queue. Unbounded queuing converts a memory
			// problem into a latency and goroutine problem: requests pile up holding
			// connections until WriteTimeout kills them, which is a worse failure than an
			// honest refusal. A short wait still absorbs ordinary jitter, so two uploads
			// arriving microseconds apart do not shed one for no reason.
			ctx, cancel := context.WithTimeout(r.Context(), wait)
			defer cancel()

			if err := sem.Acquire(ctx, weight); err != nil {
				// SHED — and it must be unmistakably a failure. Returning 200, or an empty
				// body, or letting the request through unadmitted, would each turn a refusal
				// into a silent wrong answer: the caller would believe the asset was stored
				// when nothing was read. The status is 503 (transient, retryable) rather than
				// 429 (which implies a per-client quota; this is a whole-process capacity
				// bound that a well-behaved single client can hit through no fault of its own).
				writeUploadShed(w)
				return
			}
			defer sem.Release(weight)

			next.ServeHTTP(w, r)
		})
	}
}

// isUploadRequest reports whether this request is the creative-asset upload.
//
// Matched on method plus path suffix rather than a compiled route pattern: this middleware sits
// OUTSIDE the mux, so the Goa router has not run and no route pattern is available on the
// request yet. That ordering is deliberate (see UploadAdmission), so the match has to be made
// from the raw URL.
//
// It errs toward gating: a path that ends in the upload segment is admitted through the
// semaphore even if it would later 404. Gating a request that turns out not to exist costs one
// permit briefly; failing to gate a real upload costs the bound.
func isUploadRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(r.URL.Path, "/creative-assets")
}

// uploadShedBody mirrors the {code, message} shape used by the Goa error responses and by
// writeRequestTooLarge, so a client parsing service errors needs no special case for this one.
type uploadShedBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeUploadShed emits the 503. Like writeRequestTooLarge, the message is fixed: it states
// neither the budget nor anything derived from the request, so the response leaks nothing about
// capacity or about other tenants' traffic to an unauthenticated caller.
func writeUploadShed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Retry-After is the actionable half: this is transient by construction — permits are
	// released as in-flight uploads finish — so a client that backs off briefly will succeed.
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	// Best-effort: status and headers are committed, so a write failure (client hung up)
	// leaves nothing to recover and nothing worth logging.
	_ = json.NewEncoder(w).Encode(uploadShedBody{
		Code:    "503",
		Message: "the service is at upload capacity; retry shortly",
	})
}
