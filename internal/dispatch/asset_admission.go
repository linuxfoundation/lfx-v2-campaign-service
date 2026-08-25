// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"time"

	"golang.org/x/sync/semaphore"
)

// AssetReserver bounds the creative-asset bytes that CONCURRENT dispatches may hold at once.
//
// # Why maxVariantAssetBytes alone was not a bound
//
// maxVariantAssetBytes caps ONE dispatch. Nothing capped how many dispatches run together, and
// the orchestrator's semaphore is process-wide across ALL jobs with NO per-provider partition
// (internal/service/orchestrator.go: `sem: make(chan struct{}, maxParallelDispatch)`), so all
// five slots can be Meta at the same moment. Five dispatches each holding the per-dispatch peak
// — 240 MiB retained plus the one asset already materialised when the ceiling trips — is
// 5 x 270 MiB = 1.32 GiB against a 512 MiB pod, a 2.6x overshoot. It does not need the worst
// case either: TWO asset-heavy dispatches already exceed the limit before multipart copies and
// ordinary process memory.
//
// A per-request ceiling and an aggregate ceiling are different bounds, and only the per-request
// one existed. This is the aggregate one.
//
// # Why the budget equals maxVariantAssetBytes rather than something smaller
//
// The budget is deliberately NOT sized to make the five-way arithmetic fit. Any budget below
// maxVariantAssetBytes would mean a single dispatch carrying eight maximum-size assets could
// never acquire, so it would be refused — and a bound that satisfies its arithmetic by rejecting
// work the contract accepts is not a fix, it is the same shape as pricing an upload so cheaply
// that nothing legal fits. Equal to the per-dispatch cap is the SMALLEST value that refuses no
// legal config: exactly one full-size dispatch fits, and every smaller one shares the remainder,
// which is the same "priced for the worst legal input, shared by everything smaller" shape as
// the upload and decode budgets.
//
// What it removes is the MULTIPLIER. Dispatch-side worst case goes from 5 x 270 MiB = 1.32 GiB
// to 240 MiB + one materialised asset = 270 MiB, about 53% of the pod, a 5x reduction.
//
// # Why the charge is taken after the read
//
// The reservation is charged from the bytes actually resolved, which is the same point
// maxVariantAssetBytes is charged, and for the same reason: GetAsset loads the whole BYTEA, and
// there is no size-only read on the repository to reserve against beforehand. The residual is
// therefore the same single-asset overshoot the per-dispatch cap already documents — the asset
// that trips a ceiling is resident when it is counted. Closing it needs a byte_size-only read
// added to the port; that is a larger change than this bound and is noted where the overshoot is
// described rather than silently rounded away here.
//
// # Lifetime
//
// The reservation is held for the whole dispatch, not just the resolve loop, because the bytes
// are held that long: resolveVariantAssets returns them in the variant slice and the Meta client
// POSTs them to /adimages later in the same call. Releasing at the end of the resolve would free
// budget that is still occupied, which is the mistake DecodeReserver's comment records in the
// other direction (holding a pixel reservation across a database insert that no longer needs it).
//
// A nil *AssetReserver reserves nothing, so every construction that does not wire one — every
// test dispatcher, and the no-database mode — keeps working unchanged.
type AssetReserver struct {
	sem    *semaphore.Weighted
	budget int64
	wait   time.Duration
}

// NewAssetReserver returns a reserver bounding concurrent dispatch asset memory to budgetBytes.
// A non-positive budget returns nil, which reserves nothing.
func NewAssetReserver(budgetBytes int64, wait time.Duration) *AssetReserver {
	if budgetBytes <= 0 {
		return nil
	}
	return &AssetReserver{sem: semaphore.NewWeighted(budgetBytes), budget: budgetBytes, wait: wait}
}

// reserve waits — boundedly — for want bytes and reports whether it got them.
//
// It returns a release func rather than the weight, so the amount released cannot drift from the
// amount acquired; releasing a different weight than was taken silently corrupts a weighted
// semaphore's accounting rather than failing.
//
// A request priced above the entire budget is refused immediately rather than blocking until its
// context expires: it could never be admitted, so waiting only delays the same answer. That case
// is unreachable through resolveVariantAssets, which refuses at maxVariantAssetBytes first, but
// it is handled here so this type is correct independently of that caller.
//
// The wait is bounded so a dispatch cannot queue indefinitely behind another one's assets. A
// dispatch that cannot get budget fails as a retryable dispatch error rather than hanging: it is
// a background job with no caller waiting on a socket, and the orchestrator's own
// providerCallTimeout would otherwise be the only thing to end it.
func (a *AssetReserver) reserve(ctx context.Context, want int64) (func(), bool) {
	if a == nil {
		return func() {}, true
	}
	if want <= 0 || want > a.budget {
		return func() {}, false
	}
	if a.wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.wait)
		defer cancel()
	}
	if err := a.sem.Acquire(ctx, want); err != nil {
		return func() {}, false
	}
	return func() { a.sem.Release(want) }, true
}
