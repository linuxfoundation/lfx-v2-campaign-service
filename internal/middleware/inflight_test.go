// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Wait must block until an in-flight handler returns (so shutdown awaits a
// straggler before the pool closes), and must return true once it does.
func TestInflightTracker_WaitsForInflightHandler(t *testing.T) {
	tr := NewInflightTracker()
	release := make(chan struct{})
	entered := make(chan struct{})
	h := tr.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release // hold the handler in-flight
	}))

	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered // the handler is now in-flight

	// With the handler still running, a bounded Wait must time out (report false).
	if tr.Wait(30 * time.Millisecond) {
		t.Fatal("Wait returned true while a handler was still in-flight")
	}

	close(release) // let the handler finish
	if !tr.Wait(time.Second) {
		t.Fatal("Wait did not observe the handler returning within the budget")
	}
}

// Wait returns immediately (true) when nothing is in flight — the clean-shutdown
// case where Shutdown already drained the handlers.
func TestInflightTracker_WaitImmediateWhenIdle(t *testing.T) {
	tr := NewInflightTracker()
	start := time.Now()
	if !tr.Wait(time.Second) {
		t.Fatal("Wait reported stragglers when idle")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("idle Wait took %v; should return immediately", elapsed)
	}
}

// A non-positive timeout must never block: it reports drained if idle, else
// false at once.
func TestInflightTracker_NonPositiveTimeoutDoesNotBlock(t *testing.T) {
	tr := NewInflightTracker()
	if !tr.Wait(0) {
		t.Error("Wait(0) when idle should report drained")
	}

	release := make(chan struct{})
	entered := make(chan struct{})
	h := tr.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered
	start := time.Now()
	if tr.Wait(0) {
		t.Error("Wait(0) with a handler in-flight should report not-drained")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Wait(0) blocked for %v; must return at once", elapsed)
	}
	close(release)
}
