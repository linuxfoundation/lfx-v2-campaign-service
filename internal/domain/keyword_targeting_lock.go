// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"errors"
)

// ErrKeywordTargetingWriteInProgress indicates another LFX request — on this replica or another —
// is writing the same ad group's targeting, and the bounded wait for it to finish ran out. Nothing
// was read for the write and nothing was sent. Mapped to a retryable 409: the caller's revision may
// be perfectly current, but the write in flight will most likely change the targeting, so the
// retry should read it again first.
var ErrKeywordTargetingWriteInProgress = errors.New("another keyword targeting write to this ad group is in progress")

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
