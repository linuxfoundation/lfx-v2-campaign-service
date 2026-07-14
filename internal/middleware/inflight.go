// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"net/http"
	"sync/atomic"
	"time"
)

// InflightTracker counts in-flight HTTP handler goroutines so a graceful
// shutdown can WAIT for stragglers before the database pool is closed.
//
// http.Server.Shutdown drains most handlers, but http.Server.Close (the forced
// fallback after the drain deadline) returns WITHOUT waiting for handler
// goroutines to finish — a straggler can still be executing (and touching the
// pool) after Close returns. Wrapping each request so it increments a counter
// on entry and decrements on exit lets the shutdown path wait (bounded) for
// those goroutines to actually return before the pool closes, closing the
// pool-use-after-close window that Shutdown/Close alone leave open.
type InflightTracker struct {
	cnt atomic.Int64 // current in-flight handler count
}

// NewInflightTracker returns a ready-to-use tracker.
func NewInflightTracker() *InflightTracker { return &InflightTracker{} }

// Middleware wraps next so each in-flight request is tracked.
func (t *InflightTracker) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.cnt.Add(1)
			defer t.cnt.Add(-1)
			next.ServeHTTP(w, r)
		})
	}
}

// inflightPollInterval is how often Wait re-checks the in-flight counter while
// bounded-waiting for stragglers to drain.
const inflightPollInterval = 5 * time.Millisecond

// Wait blocks until all in-flight handlers have returned or the timeout elapses,
// reporting whether every handler drained (true) or the wait timed out with
// stragglers still running (false). The bounded wait guarantees a hung handler
// can never wedge shutdown past its budget; on a clean shutdown (Shutdown already
// drained the handlers) it returns immediately. A non-positive timeout never
// blocks: it reports drained iff nothing is in flight at the moment of the call.
//
// Implemented by POLLING the atomic counter rather than a WaitGroup + goroutine:
// a done-channel goroutine blocked on wg.Wait() would LEAK past a timeout (it
// stays parked until every handler finishes), and wg.Add racing wg.Wait across
// repeated calls is unsafe. Polling has no such goroutine and is safe to call
// multiple times.
func (t *InflightTracker) Wait(timeout time.Duration) bool {
	if t.cnt.Load() == 0 {
		return true
	}
	if timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(inflightPollInterval)
	defer ticker.Stop()
	for {
		<-ticker.C
		if t.cnt.Load() == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return t.cnt.Load() == 0
		}
	}
}
