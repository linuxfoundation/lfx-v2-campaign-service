// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testUploadPath = "/projects/p1/briefs/b1/creative-assets"

func uploadReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, testUploadPath, strings.NewReader(`{}`))
}

// TestUploadAdmission_BoundsConcurrentUploads is the core proof: the bound BINDS under
// simultaneous load. It does not assert that a constant exists — it drives real concurrent
// requests through the middleware and measures the peak number in flight at once.
//
// The handler blocks until released, so every admitted request is genuinely concurrent rather
// than serialised by chance of scheduling. The expectation (2) is derived from budget/weight,
// which are the middleware's INPUTS here, not from any production constant: the test supplies
// its own budget and weight so it cannot silently agree with a mutated constant.
func TestUploadAdmission_BoundsConcurrentUploads(t *testing.T) {
	const (
		budget   = 100
		weight   = 40 // -> floor(100/40) = 2 concurrent
		wantPeak = 2
		callers  = 8
	)

	release := make(chan struct{})
	var inFlight, peak int64

	h := UploadAdmission(budget, weight, 50*time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt64(&inFlight, -1)
			w.WriteHeader(http.StatusOK)
		}))

	var wg sync.WaitGroup
	codes := make([]int, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, uploadReq())
			codes[i] = rec.Code
		}()
	}
	close(start)

	// Let the admitted set fill and the rest exhaust their wait, then release.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > wantPeak {
		t.Errorf("peak concurrent uploads = %d, want at most %d: the admission bound did not bind",
			got, wantPeak)
	}
	if got := atomic.LoadInt64(&peak); got == 0 {
		t.Fatal("no upload was ever admitted; the test proved nothing")
	}

	var ok, shed int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			shed++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if shed == 0 {
		t.Errorf("no request was shed out of %d against a budget admitting %d; "+
			"the bound is not binding under simultaneous load", callers, wantPeak)
	}
	if ok+shed != callers {
		t.Errorf("accounted %d of %d requests", ok+shed, callers)
	}
	t.Logf("peak=%d admitted=%d shed=%d", peak, ok, shed)
}

// TestUploadAdmission_ShedRequestIsNotASuccess is the defect-class guard: a rejected admission
// must never reach the client as a success or as an empty result. If it did, a caller would
// believe an asset was stored when the body was never even read.
func TestUploadAdmission_ShedRequestIsNotASuccess(t *testing.T) {
	const budget, weight = 10, 10

	entered := make(chan struct{})
	release := make(chan struct{})
	var handlerCalls int64

	h := UploadAdmission(budget, weight, 20*time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&handlerCalls, 1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"stored"}`))
		}))

	go func() { h.ServeHTTP(httptest.NewRecorder(), uploadReq()) }()
	<-entered // the only permit is now held

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uploadReq())
	close(release)

	if rec.Code == http.StatusOK {
		t.Fatalf("shed request returned 200: a refusal surfaced as a success")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Body.Len() == 0 {
		t.Error("shed request returned an EMPTY body: a refusal must say so explicitly")
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("shed body is not the service error shape: %v (%q)", err, rec.Body.String())
	}
	if body["code"] != "503" {
		t.Errorf(`body code = %q, want "503"`, body["code"])
	}
	if body["message"] == "" {
		t.Error("shed body carries no message")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("shed response has no Retry-After; the caller cannot know it is transient")
	}
	// The shed request must not have reached the handler at all — that is the whole point:
	// the body is never read, so nothing is allocated for it.
	if got := atomic.LoadInt64(&handlerCalls); got != 1 {
		t.Errorf("handler ran %d times, want 1: the shed request reached the handler anyway", got)
	}
}

// TestUploadAdmission_PermitIsReleased proves permits are returned, so saturation is transient
// rather than permanent. Without release, the first N uploads would brick the endpoint forever.
func TestUploadAdmission_PermitIsReleased(t *testing.T) {
	const budget, weight = 10, 10
	h := UploadAdmission(budget, weight, 20*time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for i := range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, uploadReq())
		if rec.Code != http.StatusOK {
			t.Fatalf("sequential upload %d got %d, want 200: a permit was not released", i, rec.Code)
		}
	}
}

// TestUploadAdmission_NonUploadRoutesAreNotGated guards the availability property: making
// ordinary routes queue behind an upload would turn a memory control into an outage. The single
// permit is held for the whole test, so a gated route would shed.
func TestUploadAdmission_NonUploadRoutesAreNotGated(t *testing.T) {
	const budget, weight = 10, 10

	entered := make(chan struct{})
	release := make(chan struct{})
	h := UploadAdmission(budget, weight, 20*time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only the ONE permit-holding POST parks here. Matching on the path alone would
			// also park the GET probe below, which shares the path — deadlocking the test on
			// its own fixture rather than on anything the middleware does.
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/creative-assets") {
				select {
				case entered <- struct{}{}:
					<-release
				default:
				}
			}
			w.WriteHeader(http.StatusOK)
		}))

	go func() { h.ServeHTTP(httptest.NewRecorder(), uploadReq()) }()
	<-entered
	defer close(release)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"get brief", http.MethodGet, "/projects/p1/briefs/b1"},
		{"list campaigns", http.MethodGet, "/projects/p1/campaigns"},
		{"create audience", http.MethodPost, "/projects/p1/briefs/b1/audiences"},
		{"readyz", http.MethodGet, "/readyz"},
		// Same path, non-POST: only the upload verb is gated.
		{"get creative assets", http.MethodGet, testUploadPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s %s got %d, want 200: a non-upload route was gated by upload admission",
					tc.method, tc.path, rec.Code)
			}
		})
	}
}

// TestUploadAdmission_WaitsBrieflyRatherThanShedingInstantly proves the bounded wait is real:
// a request arriving while a permit is momentarily held should succeed once it frees, not shed
// on contact.
func TestUploadAdmission_WaitsBrieflyRatherThanShedingInstantly(t *testing.T) {
	const budget, weight = 10, 10

	entered := make(chan struct{})
	release := make(chan struct{})
	h := UploadAdmission(budget, weight, 2*time.Second)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case entered <- struct{}{}:
				<-release
			default:
			}
			w.WriteHeader(http.StatusOK)
		}))

	go func() { h.ServeHTTP(httptest.NewRecorder(), uploadReq()) }()
	<-entered
	go func() {
		time.Sleep(80 * time.Millisecond)
		close(release)
	}()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uploadReq())
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the bounded wait did not absorb a brief contention",
			rec.Code)
	}
}
