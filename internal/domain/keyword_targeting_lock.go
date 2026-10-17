// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"errors"
	"sync"
	"time"
)

// KeywordTargetingLockWait bounds how long a whole-targeting write waits for another LFX write on
// the same scope before it is refused (ErrKeywordTargetingWriteInProgress). The single source for
// both the Postgres lock and the dispatcher's in-process fallback. The holder's critical section is
// three platform requests inside the mutation timeout; a waiter that outlasted this would rarely
// still fit its own write inside that timeout.
const KeywordTargetingLockWait = 10 * time.Second

// ErrKeywordTargetingWriteInProgress indicates another LFX request — on this replica or another —
// is writing the same ad group's targeting, and the bounded wait for it to finish ran out (or, on a
// saturated database pool, no connection could be had to ask for the lock within that wait).
// Nothing was read for the write and nothing was sent. Mapped to a retryable 409: the caller's revision may
// be perfectly current, but the write in flight will most likely change the targeting, so the
// retry should read it again first.
var ErrKeywordTargetingWriteInProgress = errors.New("another keyword targeting write to this ad group is in progress")

// ErrKeywordTargetingLockUnavailable indicates the keyword-targeting write lock could not be had
// for a reason other than another writer holding it past the wait — the database could not be
// reached, the lock query failed, or the request ended while waiting (its context's error stays
// wrapped) — so, as on a timeout, nothing was read for the write and nothing was sent. Distinct
// from ErrKeywordTargetingWriteInProgress because no other writer is known to hold the lock; it
// maps to the definite 503 ("could not be removed/added" — nothing was changed).
var ErrKeywordTargetingLockUnavailable = errors.New("coordination of keyword targeting writes is unavailable")

// IsKeywordTargetingLockRefusal reports whether err is this service refusing a write at its own
// write lock — a timeout or an unavailable lock — rather than anything the platform said. Such a
// refusal made no platform call, so it is not recorded as an upstream call.
func IsKeywordTargetingLockRefusal(err error) bool {
	return errors.Is(err, ErrKeywordTargetingWriteInProgress) || errors.Is(err, ErrKeywordTargetingLockUnavailable)
}

// lockWaitKey keys the per-call lock-wait accumulator in a context.
type lockWaitKey struct{}

type lockWait struct {
	mu sync.Mutex
	d  time.Duration
}

// WithLockWaitRecorder returns a context that collects the time spent waiting for this service's
// own write locks during one platform call, and the function that reads the total. The
// orchestrator uses it so upstream latency excludes LFX's own contention.
func WithLockWaitRecorder(ctx context.Context) (context.Context, func() time.Duration) {
	w := &lockWait{}
	return context.WithValue(ctx, lockWaitKey{}, w), func() time.Duration {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.d
	}
}

// RecordLockWait adds d to the context's lock-wait accumulator, if it carries one.
func RecordLockWait(ctx context.Context, d time.Duration) {
	if w, ok := ctx.Value(lockWaitKey{}).(*lockWait); ok {
		w.mu.Lock()
		w.d += d
		w.mu.Unlock()
	}
}

// KeywordTargetingWriteLock serializes a whole-object targeting write (read, compare the caller's
// revision, PATCH, re-read) per scope ACROSS REPLICAS, so two LFX requests on one ad group cannot
// both pass the revision check before either PATCH lands and write back stale targeting.
//
// It closes the LFX-vs-LFX window only: an edit made outside this service (Ads Manager, another
// API client) between the read and the PATCH is not excluded by it, because the platform offers no
// conditional write.
type KeywordTargetingWriteLock interface {
	// Lock waits a bounded time for scope's lock and returns the function that releases it
	// (idempotent; safe to defer). When the wait runs out with the lock held elsewhere it returns
	// ErrKeywordTargetingWriteInProgress. Any other error means the lock could not be asked for
	// (database unreachable, request ended) — and, like a timeout, the caller must not write.
	Lock(ctx context.Context, scope string) (unlock func(), err error)
}
