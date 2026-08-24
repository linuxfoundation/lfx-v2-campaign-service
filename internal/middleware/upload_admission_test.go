// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const testUploadPath = "/projects/p1/briefs/b1/creative-assets"

func uploadReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, testUploadPath, strings.NewReader(`{}`))
}

// flat prices every request the same, whatever it declares. The tests that predate
// size-proportional pricing assert the semaphore's own arithmetic (budget/weight admits N), and
// a flat price is what keeps that arithmetic the thing under test rather than the pricing rule.
func flat(weight int64) func(int64) int64 {
	return func(int64) int64 { return weight }
}

// TestUploadAdmission_BoundsConcurrentUploads is the core proof: the bound BINDS under
// simultaneous load. It does not assert that a constant exists — it drives real concurrent
// requests through the middleware and measures the peak number in flight at once.
//
// The handler blocks until released, so every admitted request is genuinely concurrent rather
// than serialised by chance of scheduling. The expectation (2) is derived from budget/weight,
// which are the middleware's INPUTS here, not from any production constant: the test supplies
// its own budget and weight so it cannot silently agree with a mutated constant.
// noBodyCap disables the declared-oversize arm for tests that are about the SEMAPHORE rather
// than the body cap. Those cases use small synthetic budgets whose weights bear no relation to
// MaxRequestBodyBytes, so applying a real cap to them would refuse requests for a reason the
// test is not making a claim about. The 413 arm has its own coverage against the real chain in
// cmd/campaign-service.
const noBodyCap int64 = 0

func TestUploadAdmission_BoundsConcurrentUploads(t *testing.T) {
	const (
		budget   = 100
		weight   = 40 // -> floor(100/40) = 2 concurrent
		wantPeak = 2
		callers  = 8
	)

	release := make(chan struct{})
	var inFlight, peak int64

	h := UploadAdmission(budget, noBodyCap, flat(weight), 50*time.Millisecond)(
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

	h := UploadAdmission(budget, noBodyCap, flat(weight), 20*time.Millisecond)(
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
	h := UploadAdmission(budget, noBodyCap, flat(weight), 20*time.Millisecond)(
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
	h := UploadAdmission(budget, noBodyCap, flat(weight), 20*time.Millisecond)(
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
	h := UploadAdmission(budget, noBodyCap, flat(weight), 2*time.Second)(
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

// TestUploadAdmission_ProductionConstantsAdmitConcurrentUploads is the regression guard for the
// defect that flat pricing introduced: with UploadAdmissionBudgetBytes == UploadAdmissionWeightBytes,
// effective upload concurrency was exactly ONE.
//
// Every other test in this file supplies its own budget and weight, which is what kept the
// regression invisible: they proved the SEMAPHORE's arithmetic and never the arithmetic the
// service actually ships. This one wires the real production constants and the real pricing
// function, so it fails if that pair ever again admits a single upload at a time.
//
// The proof is a RENDEZVOUS, not a timing measurement. Each admitted handler decrements a
// WaitGroup and then blocks on it, so no handler can leave until all wantConcurrent have arrived
// together. If admission only ever lets one through, the barrier is never satisfied, the
// handlers never return, and the test fails by timeout rather than passing on a lucky sleep.
// There is no elapsed-time threshold anywhere in the assertion.
func TestUploadAdmission_ProductionConstantsAdmitConcurrentUploads(t *testing.T) {
	// A realistic small creative: a logo, far below the worst-case upload the weight ceiling is
	// priced for. Declared via Content-Length, which is what the pricing function reads.
	const smallUpload = 256 << 10 // 256 KiB

	wantConcurrent := int(constants.UploadAdmissionBudgetBytes /
		constants.UploadAdmissionWeightFor(smallUpload))
	if wantConcurrent < 2 {
		t.Fatalf("production constants price a %d-byte upload at %d against a %d budget, "+
			"admitting %d at a time: uploads cannot proceed concurrently",
			smallUpload, constants.UploadAdmissionWeightFor(smallUpload),
			constants.UploadAdmissionBudgetBytes, wantConcurrent)
	}

	var arrived sync.WaitGroup
	arrived.Add(wantConcurrent)
	var admitted atomic.Int64

	h := UploadAdmission(
		constants.UploadAdmissionBudgetBytes,
		constants.MaxRequestBodyBytes,
		constants.UploadAdmissionWeightFor,
		constants.UploadAdmissionWait,
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		admitted.Add(1)
		// Announce arrival, then block until every other admitted handler has arrived. This
		// is the concurrency proof: it can only be satisfied by wantConcurrent handlers
		// being inside the middleware AT THE SAME MOMENT.
		arrived.Done()
		arrived.Wait()
		w.WriteHeader(http.StatusOK)
	}))

	done := make(chan int, wantConcurrent)
	for range wantConcurrent {
		go func() {
			req := httptest.NewRequest(http.MethodPost, testUploadPath,
				strings.NewReader(strings.Repeat("x", smallUpload)))
			req.ContentLength = smallUpload
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			done <- rec.Code
		}()
	}

	for i := range wantConcurrent {
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Errorf("request %d: status = %d, want %d; it was shed rather than admitted "+
					"alongside the others", i, code, http.StatusOK)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d uploads were admitted concurrently: the rendezvous never "+
				"completed, so the budget does not admit %d at once",
				admitted.Load(), wantConcurrent, wantConcurrent)
		}
	}

	if got := admitted.Load(); got != int64(wantConcurrent) {
		t.Errorf("admitted %d uploads, want %d", got, wantConcurrent)
	}
	t.Logf("production constants admitted %d concurrent %d KiB uploads (weight %d each, budget %d)",
		wantConcurrent, smallUpload>>10,
		constants.UploadAdmissionWeightFor(smallUpload), constants.UploadAdmissionBudgetBytes)
}

// TestUploadAdmission_SlowBodyDoesNotPinTheWholeBudget states the availability property in the
// terms the failure actually took: a client that dribbles its body holds a permit for as long as
// the read lasts, so the question is whether that hold excludes everyone else.
//
// Under flat pricing it did — one slow upload took the entire budget and every concurrent upload
// shed with 503. The slow request here never completes during the test; a second, ordinary
// upload must still be admitted and must still return 200 while the slow one is parked inside
// the middleware holding its permit.
func TestUploadAdmission_SlowBodyDoesNotPinTheWholeBudget(t *testing.T) {
	const smallUpload = 256 << 10

	parked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	var slow atomic.Bool
	h := UploadAdmission(
		constants.UploadAdmissionBudgetBytes,
		constants.MaxRequestBodyBytes,
		constants.UploadAdmissionWeightFor,
		constants.UploadAdmissionWait,
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Slow") == "1" {
			slow.Store(true)
			close(parked)
			<-release // hold the permit for the rest of the test
		}
		w.WriteHeader(http.StatusOK)
	}))

	go func() {
		req := httptest.NewRequest(http.MethodPost, testUploadPath,
			strings.NewReader(strings.Repeat("x", smallUpload)))
		req.ContentLength = smallUpload
		req.Header.Set("X-Slow", "1")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Wait for the slow request to be INSIDE the handler holding its permit. No sleep: the
	// channel close is the happens-before edge.
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow upload was never admitted; the test proved nothing")
	}
	if !slow.Load() {
		t.Fatal("slow upload did not register as parked")
	}

	req := httptest.NewRequest(http.MethodPost, testUploadPath,
		strings.NewReader(strings.Repeat("x", smallUpload)))
	req.ContentLength = smallUpload
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("second upload status = %d, want %d: a single slow body still pins the whole "+
			"upload budget, so one client can deny the route to every other", rec.Code, http.StatusOK)
	}
}

// TestUploadAdmissionWeightFor_PricesWithoutUnderCharging binds the pricing rule's contract.
//
// The property that matters is one-directional: no input may be priced BELOW what the same
// bytes could cost, and nothing may exceed the worst-case ceiling. Each expectation is written
// as an independent claim about the input, not derived from the constant under test, so a
// mutated constant cannot make the test agree with it.
func TestUploadAdmissionWeightFor_PricesWithoutUnderCharging(t *testing.T) {
	ceiling := constants.UploadAdmissionWeightBytes
	floor := constants.UploadAdmissionMinWeightBytes

	t.Run("unknown length is charged the ceiling", func(t *testing.T) {
		// Chunked or absent: nothing is known, so it must be the most expensive case or it
		// becomes the cheapest way to buy a permit.
		if got := constants.UploadAdmissionWeightFor(-1); got != ceiling {
			t.Errorf("weight for unknown length = %d, want the worst-case ceiling %d", got, ceiling)
		}
	})

	t.Run("a tiny body is charged the floor, never nothing", func(t *testing.T) {
		for _, n := range []int64{0, 1, 1024} {
			if got := constants.UploadAdmissionWeightFor(n); got != floor {
				t.Errorf("weight for %d bytes = %d, want the floor %d: an unfloored charge lets "+
					"unboundedly many tiny uploads share one budget", n, got, floor)
			}
		}
	})

	t.Run("a maximum-size body is charged the ceiling", func(t *testing.T) {
		if got := constants.UploadAdmissionWeightFor(constants.MaxRequestBodyBytes); got != ceiling {
			t.Errorf("weight for a max-size body = %d, want the ceiling %d", got, ceiling)
		}
	})

	t.Run("nothing is ever priced above the ceiling", func(t *testing.T) {
		// Including absurd and overflow-adjacent inputs: this multiplies a caller-supplied
		// number, so the guard must hold for values no honest client would send.
		for _, n := range []int64{
			constants.MaxRequestBodyBytes * 10,
			1 << 40,
			math.MaxInt64,
		} {
			if got := constants.UploadAdmissionWeightFor(n); got != ceiling {
				t.Errorf("weight for %d = %d, want the ceiling %d (no overflow, no wraparound)",
					n, got, ceiling)
			}
		}
	})

	t.Run("price rises with size", func(t *testing.T) {
		small := constants.UploadAdmissionWeightFor(4 << 20)
		large := constants.UploadAdmissionWeightFor(16 << 20)
		if small >= large {
			t.Errorf("weight(4 MiB)=%d weight(16 MiB)=%d: pricing is not proportional, so a "+
				"small upload is charged like a large one", small, large)
		}
	})

	t.Run("a priced request never exceeds the budget", func(t *testing.T) {
		// If any legal request priced above the budget, it could never be admitted at all.
		if ceiling > constants.UploadAdmissionBudgetBytes {
			t.Errorf("ceiling %d exceeds budget %d: a maximum-size upload could never be admitted",
				ceiling, constants.UploadAdmissionBudgetBytes)
		}
	})
}
