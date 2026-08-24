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
// 30 MiB MaxLength, maxCreativeDecodedBytes at 80 MiB) each bound ONE request and say nothing
// about how many run at once. Against a fixed pod memory limit that is the whole problem: N
// concurrent, entirely LEGAL uploads multiply an unbounded-in-aggregate per-request allocation
// until the pod is OOM-killed, which denies service to every tenant on it and restarts the
// process. No amount of tightening a per-request number fixes an aggregate.
//
// The placement is the security property, and it is why this is not a semaphore wrapped around
// image.Decode in the service method. Goa's generated handler calls decodeRequest(r) and only
// THEN endpoint(...), and the generated endpoint calls authJWTFn as its first statement — so the
// request body is read off the socket and base64-decoded BEFORE any JWT is examined. A guard
// inside UploadCreativeAsset therefore runs after roughly 72 MiB (the buffered body plus the
// decoded byte slice) has already been allocated on behalf of a caller who has not authenticated.
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
// WHAT THE WEIGHT ACCOUNTS, precisely — because a bound that silently under-counts reads as
// protection while providing less than it appears to.
//
// It accounts the INBOUND request: the buffered body, the base64-decoded slice and the pixel
// buffer image.Decode may allocate for ONE upload on this HTTP path. That is the entire memory
// cost of the route this middleware gates.
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
func UploadAdmission(budgetBytes int64, perRequestBytes int64, wait time.Duration) func(http.Handler) http.Handler {
	sem := semaphore.NewWeighted(budgetBytes)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isUploadRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			// A BOUNDED wait, not an unbounded queue. Unbounded queuing converts a memory
			// problem into a latency and goroutine problem: requests pile up holding
			// connections until WriteTimeout kills them, which is a worse failure than an
			// honest refusal. A short wait still absorbs ordinary jitter, so two uploads
			// arriving microseconds apart do not shed one for no reason.
			ctx, cancel := context.WithTimeout(r.Context(), wait)
			defer cancel()

			if err := sem.Acquire(ctx, perRequestBytes); err != nil {
				// SHED — and it must be unmistakably a failure. Returning 200, or an empty
				// body, or letting the request through unadmitted, would each turn a refusal
				// into a silent wrong answer: the caller would believe the asset was stored
				// when nothing was read. The status is 503 (transient, retryable) rather than
				// 429 (which implies a per-client quota; this is a whole-process capacity
				// bound that a well-behaved single client can hit through no fault of its own).
				writeUploadShed(w)
				return
			}
			defer sem.Release(perRequestBytes)

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
