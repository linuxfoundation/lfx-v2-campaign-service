// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// DecodeReserver bounds the PIXEL BUFFERS that concurrent image decodes may allocate.
//
// # Why the wire-priced admission upstream cannot do this
//
// middleware.UploadAdmission prices a permit from the request's declared Content-Length, which
// is the right basis for the bytes that arrive on the SOCKET but is not a bound on what decoding
// them costs. Image compression breaks the proportionality: a flat 4000x4000 PNG is ~68 KiB on
// the wire and 61 MiB decoded, an amplification of over 900x, and it is entirely legal — the
// dimension gate admits it deliberately. Priced on wire size it takes the minimum permit, so a
// budget that admits sixteen of them concurrently admits ~976 MiB of pixel buffers against a
// 512 MiB pod. The aggregate memory bound the upstream middleware exists to provide would hold
// for the socket and be defeated at the decode.
//
// The two bounds are therefore not redundant and neither subsumes the other. Wire bytes and
// decoded bytes are different quantities with different worst cases, and each needs its own
// budget:
//
//   - UploadAdmission bounds the PRE-AUTH read. It must run before the body is touched, so the
//     only thing it can know is Content-Length.
//   - This bounds the DECODE. It runs after DecodeConfig has read the header, which is the
//     earliest moment the pixel cost is knowable at all — and still before image.Decode has
//     allocated anything.
//
// # Why this one may sit in the service method
//
// The placement argument that forces UploadAdmission outside the mux does not apply here, and
// the reason is worth stating so the two are not "fixed" into one. That middleware guards an
// allocation made on behalf of an UNAUTHENTICATED caller: Goa decodes the body before the
// endpoint runs authJWTFn, so a guard inside the handler would arrive too late to bound it. The
// pixel buffer is different — it is allocated by this method, after auth, from bytes already in
// memory. The earliest point it can be bounded is exactly where the size becomes knowable, and
// that point is here.
//
// # Reserving the estimate, not the allocation
//
// The weight is the DECLARED cost computed from the header (dimensions x the colour model's
// bytes per pixel), taken before image.Decode runs and released as soon as it returns. It is the
// same figure dimensionsWithinLimits already computes to decide whether ONE image is admissible;
// this reserves it against a budget so N of them are bounded too. A per-image ceiling and an
// aggregate ceiling are different bounds, and the per-image one was already in place.
//
// A nil *DecodeReserver reserves nothing, so every construction that does not wire one keeps
// working unchanged.
type DecodeReserver struct {
	sem    *semaphore.Weighted
	budget int64
}

// NewDecodeReserver returns a reserver bounding concurrent decodes to budgetBytes in total.
func NewDecodeReserver(budgetBytes int64) *DecodeReserver {
	if budgetBytes <= 0 {
		return nil
	}
	return &DecodeReserver{sem: semaphore.NewWeighted(budgetBytes), budget: budgetBytes}
}

// reserve blocks until want bytes of decode budget are available, or ctx ends.
//
// It returns a release func rather than requiring the caller to remember the weight, so the
// amount released cannot drift from the amount acquired — releasing a different weight than was
// taken silently corrupts a weighted semaphore's accounting rather than failing.
//
// A nil reserver is a no-op with a no-op release, and a request priced above the entire budget
// is refused immediately rather than blocking until its context expires: it could never be
// admitted, so waiting only delays the same answer.
func (d *DecodeReserver) reserve(ctx context.Context, want int64) (func(), bool) {
	if d == nil {
		return func() {}, true
	}
	if want <= 0 || want > d.budget {
		return func() {}, false
	}
	if err := d.sem.Acquire(ctx, want); err != nil {
		return func() {}, false
	}
	return func() { d.sem.Release(want) }, true
}

// decodeReserver snapshots the reserver under the read lock, matching creativeAssetRepo and the
// other late-bound collaborators. A nil result reserves nothing.
func (s *BriefService) decodeReserver() *DecodeReserver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.decodeReserve
}

// SetDecodeReserver late-binds the decode budget, mirroring SetCreativeAssetRepo. Guarded by mu
// against concurrent handler reads.
func (s *BriefService) SetDecodeReserver(d *DecodeReserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decodeReserve = d
}

// DecodeReserverIsSet reports whether an aggregate decode budget is bound.
//
// Exported for the same reason CreativeAssetRepoIsSet is: the container's wiring test needs to
// observe that the shared live-wiring helper actually bound it. Deleting the bind compiles and
// keeps every other test green — an unused constant is legal Go — so the only thing that holds
// the wiring is an assertion on the bound state itself.
func (s *BriefService) DecodeReserverIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.decodeReserve != nil
}
