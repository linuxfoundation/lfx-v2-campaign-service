// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/middleware"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestUploadAdmission_IsWiredIntoTheRealChain asserts the admission bound is APPLIED, not merely
// implemented.
//
// This is a distinct fact from anything internal/middleware can prove. Its own tests drive
// UploadAdmission directly, so they pass whether or not buildHandler ever calls it -- deleting
// the wiring line compiles, keeps every middleware test green, and silently removes the control
// in production. Only a test that drives the real chain closes that gap, which is exactly why
// buildHandler is a seam.
//
// The proof is behavioural rather than structural: no reflection, no counting of wrappers. It
// saturates the budget by parking requests inside the chain, then asserts that a further upload
// is SHED with the admission middleware's own 503 -- a response no other layer in the chain
// produces, and one that can only appear if admission is wired.
//
// Only the probe's response is inspected. The parked requests are never allowed to complete
// during the assertion, which also keeps this test clear of a pre-existing upstream data race in
// goa v3.25.3's ErrorEncoder (http/encoding.go:265-266 writes a shared `formatter` closure
// variable): that race fires when two error responses encode concurrently and reproduces through
// the bare mux with no admission middleware present, so it is not this PR's to fix here.
func TestUploadAdmission_IsWiredIntoTheRealChain(t *testing.T) {
	// buildHandler is exercised with a SENTINEL inner handler in place of the mux. The subject
	// under test is buildHandler's own composition -- whether it installs UploadAdmission --
	// and the sentinel keeps the parked requests out of goa's generated error encoder, which
	// carries a pre-existing upstream data race (see the doc comment above). The chain being
	// measured is the real one; only the thing it wraps is substituted.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(http.StatusOK)
	})
	chain := buildHandler(inner, &config.Config{}, middleware.NewInflightTracker())

	// The parked requests below stream a body of UNKNOWN length (blockingBody carries no
	// Content-Length), which constants.UploadAdmissionWeightFor prices at the FLOOR — the
	// same charge as the smallest declared upload, because omitting the header cannot buy
	// more bytes than declaring them (MaxBodyBytes bounds the body, the decode reservation
	// bounds the pixel buffer, and neither consults the declared length).
	//
	// The saturation count is therefore computed from that same pricing function rather than
	// from the raw weight constant, so it tracks the rule the chain actually applies instead
	// of restating an arithmetic the middleware no longer performs. That matters more now
	// than it did when unknown length priced at the ceiling: saturating the budget takes as
	// many parked requests as the floor admits, not one.
	admits := int(constants.UploadAdmissionBudgetBytes / constants.UploadAdmissionWeightFor(-1))

	release := make(chan struct{})
	var reading int64
	var wg sync.WaitGroup

	// Occupy every permit. Each parked request holds its permit until the test ends.
	for range admits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, uploadRoute,
				&blockingBody{release: release, reading: &reading})
			req.Header.Set("Content-Type", "application/json")
			chain.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt64(&reading) < int64(admits) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&reading); got != int64(admits) {
		close(release)
		wg.Wait()
		t.Fatalf("only %d of %d permits were taken; cannot test saturation", got, admits)
	}

	// The budget is now fully occupied. One more upload must be shed by admission.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, uploadRoute, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	chain.ServeHTTP(rec, req)

	close(release)
	wg.Wait()

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("saturated chain answered %d, want %d: UploadAdmission is not wired into "+
			"buildHandler (budget %d MiB, weight %d MiB, %d permits held)",
			rec.Code, http.StatusServiceUnavailable,
			constants.UploadAdmissionBudgetBytes>>20,
			constants.UploadAdmissionWeightBytes>>20, admits)
	}
	if !strings.Contains(rec.Body.String(), "upload capacity") {
		t.Errorf("shed body = %q, want the admission middleware's own message; "+
			"a 503 from another layer would not prove admission is wired", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the shed response from the real chain")
	}
}

// blockingBody is a request body whose first Read parks until released. It makes an admitted
// request hold its permit for a controlled window, so saturation is deterministic rather than a
// race against how quickly the handler rejects the payload.
type blockingBody struct {
	release chan struct{}
	reading *int64
	done    bool
}

func (b *blockingBody) Read(_ []byte) (int, error) {
	if !b.done {
		atomic.AddInt64(b.reading, 1)
		<-b.release
		b.done = true
	}
	return 0, io.EOF
}

// TestReadTimeoutIsSetOnTheServer guards the slowloris bound on the REAL server object.
//
// It reads buildServer's output rather than restating the timeouts in a literal of its own. An
// earlier version of this test built its own http.Server from the same constants and passed
// even with the ReadTimeout field deleted from the server -- it was asserting that a constant
// equals itself. The bug it must catch is a missing FIELD, so it has to inspect the struct the
// service actually runs.
func TestReadTimeoutIsSetOnTheServer(t *testing.T) {
	srv := buildServer(&config.Config{}, http.NotFoundHandler())

	if srv.ReadTimeout == 0 {
		t.Fatal("ReadTimeout is unset on the server: a slow body can hold an admission permit " +
			"indefinitely, exhausting the upload budget with requests that never complete")
	}
	if srv.ReadTimeout <= srv.ReadHeaderTimeout {
		t.Errorf("ReadTimeout %v <= ReadHeaderTimeout %v: it covers the body as well as the headers",
			srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	// A legitimate maximum-size upload must fit inside the deadline.
	if srv.ReadTimeout < 60*time.Second {
		t.Errorf("ReadTimeout %v is too short for a %d MiB upload",
			srv.ReadTimeout, constants.MaxRequestBodyBytes>>20)
	}
	if srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Error("buildServer dropped WriteTimeout or IdleTimeout")
	}

	// The binding constraint: net/http installs the WRITE deadline when the request headers
	// are read, and it keeps expiring while the handler reads the body. A read budget longer
	// than the write budget lets a slow upload satisfy the read deadline and then have nothing
	// left to answer with, so the caller sees a dropped connection instead of a response.
	if srv.ReadTimeout > srv.WriteTimeout {
		t.Errorf("ReadTimeout %v exceeds WriteTimeout %v: an upload can finish reading with no "+
			"write budget left and fail to send any response",
			srv.ReadTimeout, srv.WriteTimeout)
	}

	// The header deadline must leave room for the body inside the total read budget.
	if srv.ReadHeaderTimeout >= srv.ReadTimeout {
		t.Errorf("ReadHeaderTimeout %v leaves no room for the body within ReadTimeout %v",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}

// TestServerTimeoutsReserveHandlerHeadroom pins the inequality the three deadlines must satisfy.
//
// Equality is NOT sufficient: because the write deadline keeps expiring while the body is read
// and while the handler runs, ReadTimeout == WriteTimeout lets a slow body consume the entire
// write budget and leave nothing for image.Decode, the insert, and the response — the same
// dropped-connection failure as a read deadline that exceeds the write deadline, reached from
// the other side. The read budget must be strictly smaller, by at least the reserved headroom.
func TestServerTimeoutsReserveHandlerHeadroom(t *testing.T) {
	srv := buildServer(&config.Config{}, http.NotFoundHandler())

	if srv.ReadTimeout+constants.UploadHandlerHeadroom > srv.WriteTimeout {
		t.Errorf("ReadTimeout %v + headroom %v exceeds WriteTimeout %v: a slow body can consume "+
			"the write budget and leave nothing to decode, persist and answer with",
			srv.ReadTimeout, constants.UploadHandlerHeadroom, srv.WriteTimeout)
	}
	if srv.ReadTimeout >= srv.WriteTimeout {
		t.Errorf("ReadTimeout %v is not strictly below WriteTimeout %v: equal budgets reserve "+
			"no response headroom at all", srv.ReadTimeout, srv.WriteTimeout)
	}
	if constants.UploadHandlerHeadroom <= 0 {
		t.Error("UploadHandlerHeadroom must be positive to reserve any response budget")
	}
}

// TestDeclaredOversizeUploadGets413NotShed pins which of two refusals a client receives when its
// declared body already exceeds the cap AND the upload budget is occupied.
//
// The two controls answer different questions and the ORDER between them decides the status. An
// oversized body is a permanent, client-fixable error (413, documented in docs/api-catalog.md);
// a full budget is a transient capacity condition (503 + Retry-After). With admission outermost,
// a request whose Content-Length is already over the cap still had to buy an admission permit
// first — and because UploadAdmissionWeightFor prices anything above the amplification threshold
// at the full worst-case weight, that permit equals the ENTIRE budget. So whenever any other
// upload holds a permit, a plainly-too-large request waits its 250ms and is shed with 503,
// telling the caller to retry a request that can never succeed however long it waits.
//
// The size is known from the headers alone, without reading a byte, so the 413 costs nothing and
// belongs first.
func TestDeclaredOversizeUploadGets413NotShed(t *testing.T) {
	handlerReached := make(chan struct{})
	blockUntil := make(chan struct{})
	chain := buildHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-Park") == "1" {
			close(handlerReached)
			<-blockUntil
		}
		w.WriteHeader(http.StatusOK)
	}), &config.Config{}, middleware.NewInflightTracker())

	// Park a legitimate upload inside the chain so it holds an admission permit.
	var parked sync.WaitGroup
	parked.Add(1)
	go func() {
		defer parked.Done()
		req := httptest.NewRequest(http.MethodPost, uploadRoute, strings.NewReader("x"))
		req.Header.Set("X-Park", "1")
		req.ContentLength = 1
		chain.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-handlerReached

	// Now a request that is plainly too large by its DECLARED size alone.
	over := constants.MaxRequestBodyBytes + 1
	req := httptest.NewRequest(http.MethodPost, uploadRoute, strings.NewReader("y"))
	req.ContentLength = over
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	close(blockUntil)
	parked.Wait()

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d for a body whose DECLARED size (%d) already exceeds the "+
			"cap (%d): its size is known from the headers, so it must be refused as too large "+
			"rather than shed as a capacity problem it can never retry past",
			rec.Code, http.StatusRequestEntityTooLarge, over, constants.MaxRequestBodyBytes)
	}
}
