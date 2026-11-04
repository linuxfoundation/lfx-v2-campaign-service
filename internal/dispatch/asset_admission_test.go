// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"testing"
	"time"
)

// Waits for test AssetReservers. Pick by what the test expects of the reservation.
//
// AssetReserver.reserve wraps the caller's context in context.WithTimeout(wait) and then calls
// semaphore.Weighted.Acquire, which checks ctx.Done() BEFORE it looks at capacity and refuses a
// context that is already done even when the whole budget is free. So any wait is also the
// longest the goroutine may stall between those two calls before an uncontended grant turns
// into a capacity refusal. With a 1 ms wait, that made
// TestDispatch_LinkedInInvalidStoredImageReachesTheJobResult fail on a loaded CI runner under
// -race with the "exceed the memory concurrently available" refusal instead of the stored-image
// one.
//
// membudget.Budget does not share the problem: it admits from free capacity under its lock
// before it starts a timer, so its tests' short waits only ever bound a real wait.
const (
	// testAssetReserveWait is for a reserver whose reservations the test expects to be
	// GRANTED, which is every reservation on a free budget, including the full-budget
	// reserve a leak test takes at the end. A stall cannot reach a second, and a leak still
	// fails, in one second, instead of hanging until the test binary's timeout as a zero wait
	// would.
	testAssetReserveWait = time.Second

	// testAssetContentionWait bounds a reservation the test expects to be REFUSED because the
	// budget is held: as the reserver's own wait, or as the deadline of the caller's context
	// when the same reserver must also grant. The refusal is certain whatever the wait, so only
	// the test's run time depends on it. Take the hold with holdWholeAssetBudget, so the hold
	// is not itself a grant racing this short timer.
	testAssetContentionWait = 20 * time.Millisecond
)

// holdWholeAssetBudget takes r's entire budget for the rest of the test, standing in for another
// dispatch that is already running. It acquires the semaphore directly on a background context,
// which on a fresh reserver cannot fail, rather than through reserve, whose wait would race the
// grant (see testAssetContentionWait).
func holdWholeAssetBudget(t *testing.T, r *AssetReserver) {
	t.Helper()
	if err := r.sem.Acquire(context.Background(), r.budget); err != nil {
		t.Fatalf("could not hold the whole asset budget: %v", err)
	}
	t.Cleanup(func() { r.sem.Release(r.budget) })
}
