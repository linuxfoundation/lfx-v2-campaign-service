// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"net/http"
	"sync"
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
	wg  sync.WaitGroup
	cnt atomic.Int64 // current in-flight count, so an unbounded-free Wait can read it directly
}

// NewInflightTracker returns a ready-to-use tracker.
func NewInflightTracker() *InflightTracker { return &InflightTracker{} }

// Middleware wraps next so each in-flight request is tracked.
func (t *InflightTracker) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.wg.Add(1)
			t.cnt.Add(1)
			defer func() {
				t.cnt.Add(-1)
				t.wg.Done()
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Wait blocks until all in-flight handlers have returned or the timeout elapses,
// reporting whether every handler drained (true) or the wait timed out with
// stragglers still running (false). The bounded wait guarantees a hung handler
// can never wedge shutdown past its budget; on a clean shutdown (Shutdown already
// drained the handlers) it returns immediately. A non-positive timeout never
// blocks: it reports drained iff nothing is in flight at the moment of the call.
func (t *InflightTracker) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		// No budget to wait: report drained only if nothing is in flight right now.
		// Read the atomic counter directly rather than racing a wg.Wait goroutine.
		return t.cnt.Load() == 0
	}
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
