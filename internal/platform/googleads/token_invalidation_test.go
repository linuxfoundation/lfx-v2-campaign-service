// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoRequest_401InvalidatesTheCachedToken pins the same property as its reddit and
// microsoft siblings, on the third client the dispatcher caches.
//
// This client was not named in the review that found the other two, which is the reason to
// cover it: the token cache here is the same design (fast path on accessToken until
// tokenExpiry), and internal/dispatch/googleads.go resolves it through the same clientCache,
// so it has the same hole for the same reason. Fixing only the two reported lines would leave
// the class open on a path that spends money.
func TestDoRequest_401InvalidatesTheCachedToken(t *testing.T) {
	var tokenHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&tokenHits, 1)
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"access_token":"tok_%d","expires_in":3600}`, n))
	}))
	defer tokenSrv.Close()

	var presented []string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = append(presented, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	for i := range 2 {
		if _, err := c.doRequest(context.Background(), http.MethodGet, "/customers/1/campaigns", nil, true); err == nil {
			t.Fatalf("doRequest %d: got nil error, want the 401 to surface", i+1)
		}
	}

	if n := atomic.LoadInt64(&tokenHits); n != 2 {
		t.Errorf("token endpoint hit %d times across 2 operations, want 2: a 401 must "+
			"invalidate the cached token so the next operation re-mints rather than "+
			"re-presenting a token the platform already rejected", n)
	}
	if len(presented) == 2 && presented[0] == presented[1] {
		t.Errorf("both requests presented %s: the second re-sent the rejected token",
			presented[0])
	}
}

// TestDoRequest_UnreadableBody401AlsoInvalidates covers the arm a body-dependent guard would
// miss: a 401 whose body cannot be read.
//
// The server declares a Content-Length it does not satisfy and closes early, so the body read
// fails while the STATUS LINE is intact. That combination is not exotic — a proxy or gateway
// terminating an auth failure produces it — and it is the case where a guard written on the
// parsed error body silently stops invalidating. The assertion is the same token-mint count as
// the readable case, because an ambiguous 401 must be handled identically to a readable one.
func TestDoRequest_UnreadableBody401AlsoInvalidates(t *testing.T) {
	var tokenHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&tokenHits, 1)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"access_token":"tok_%d","expires_in":3600}`, n))
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Promise more bytes than are sent, then hijack and close: the status reaches the
		// client, the body read does not complete.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusUnauthorized)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Write SOME of the promised body, then cut the connection. Hijacking without
		// writing anything can race the flush: the client may then see a complete
		// zero-length body -- a perfectly READABLE 401 -- and the test silently stops
		// exercising the unreadable arm it exists for. Sending a partial body first makes
		// the truncation unambiguous, so the read always fails.
		_, _ = io.WriteString(w, `{"error":{"code":401,`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	for i := range 2 {
		if _, err := c.doRequest(context.Background(), http.MethodGet, "/customers/1/campaigns", nil, true); err == nil {
			t.Fatalf("doRequest %d: got nil error, want a failure", i+1)
		}
	}

	if n := atomic.LoadInt64(&tokenHits); n != 2 {
		t.Errorf("token endpoint hit %d times across 2 operations, want 2: an UNREADABLE 401 "+
			"is still a 401 and must invalidate the cached token", n)
	}
}

// TestABA_LateUnauthorizedDoesNotEvictANewerToken pins the compare-and-clear property.
//
// The invalidator is called from a 401 that describes ONE specific token, but a shared client
// may have moved on by the time that response lands: request A leaves carrying tok_1, request B
// refreshes and caches tok_2, and A's late 401 then arrives naming tok_1. Clearing
// unconditionally would evict tok_2 — a token nothing rejected — and a burst of late responses
// would drive serial re-exchanges, defeating the single-flight coalescing.
//
// The assertion is on the CACHE CONTENT after a stale invalidation, not on a mint count,
// because the mint count cannot distinguish "kept the good token" from "never had one".
func TestABA_LateUnauthorizedDoesNotEvictANewerToken(t *testing.T) {
	c := NewClient(testCreds(), testAccount())

	c.tokenMu.Lock()
	c.accessToken = "tok_current"
	c.tokenMu.Unlock()

	// A late 401 answering an OLDER token must be a no-op.
	c.invalidateAccessToken("tok_stale")
	if got := c.accessToken; got != "tok_current" {
		t.Errorf("cached token = %q, want it untouched: a 401 naming an older token must not "+
			"evict the newer one the cache now holds", got)
	}

	// The token the 401 actually named IS dropped.
	c.invalidateAccessToken("tok_current")
	if got := c.accessToken; got != "" {
		t.Errorf("cached token = %q, want it cleared: the token the 401 named must be dropped", got)
	}
}

// TestDoRequest_401InvalidatesBeforeTheBodyIsRead pins the ORDERING, which the two tests
// above do not.
//
// TestDoRequest_401InvalidatesTheCachedToken and TestDoRequest_UnreadableBody401AlsoInvalidates
// are outcome-only: they count token-endpoint hits across two completed operations, so they
// prove only EVENTUAL invalidation. A guard moved down to just before the return statements
// would satisfy both while leaving every concurrent caller on this shared client taking the
// rejected token from the fast path for as long as the body takes to arrive -- which, on a
// slow or truncated 401, is the whole per-attempt deadline, exactly the load under which the
// guarantee matters.
//
// This test holds the 401 BODY open and asserts the cache is ALREADY empty at that moment.
// The rendezvous is a channel closed from inside the handler, not a sleep: a fixed timer is
// not synchronised with the token mint or the flush, so under load it can sample a cache that
// was never populated and record a false pass. The presented-header control rules that out.
func TestDoRequest_401InvalidatesBeforeTheBodyIsRead(t *testing.T) {
	bodyGate := make(chan struct{})
	statusFlushed := make(chan struct{})
	observed := make(chan string, 1)
	presented := make(chan string, 1)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok_1","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// The status line is on the wire and the body is deliberately withheld.
		close(statusFlushed)
		<-bodyGate
		_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	go func() {
		<-statusFlushed
		// Poll until the client clears, rather than sampling once (which races it) or
		// sleeping (a wall-clock guess). This cannot pass vacuously: the BODY is withheld
		// below, so a guard placed after the ReadFrom stays blocked on bodyGate -- which
		// only this goroutine releases -- for as long as the loop runs.
		deadline := time.Now().Add(10 * time.Second)
		for {
			c.tokenMu.Lock()
			got := c.accessToken
			c.tokenMu.Unlock()
			if got == "" || time.Now().After(deadline) {
				observed <- got
				break
			}
			runtime.Gosched()
		}
		close(bodyGate)
	}()

	_, _ = c.doRequest(context.Background(), http.MethodGet, "/customers/1/campaigns", nil, true)

	// Positive control: without it the assertion below passes just as happily against a
	// cache that was never populated at all.
	if got := <-presented; got != "Bearer tok_1" {
		t.Fatalf("Authorization = %q, want Bearer tok_1: the assertion below is only "+
			"meaningful if a token was actually minted and presented", got)
	}

	if got := <-observed; got != "" {
		t.Errorf("cached token = %q while the 401 body was still in flight, want it already "+
			"cleared: invalidation must happen on the status line, not after the body read", got)
	}
}

// TestInvalidate_DoesNotLeakThroughAPublishedFlight pins the flight-poison arm of
// invalidateAccessToken: a PUBLISHED flight whose token a 401 named must not hand that token
// back through the waiter path.
//
// Read what this test does and does not prove, because the seeded state is CONSTRUCTED, not
// reproduced.
//
// The state it seeds -- c.inflight non-nil while inflight.token is already "tok_1" -- is not
// a state the current leader path can produce. The leader sets inflight.token, retracts
// c.inflight and closes done inside ONE unbroken critical section, so a published flight is
// never observable carrying a non-empty token. A racing probe over ~7M observations of a
// published flight saw this state zero times; it appeared ~74k times only once that critical
// section was split in two. So this test does NOT reproduce a production interleaving, and it
// does NOT cover the residual pre-publication window tracked by #180 (a 401 landing before
// inflight.token is set matches nothing and the leader publishes the rejected token anyway).
//
// What it DOES prove is still worth having: it is the executable specification of the poison
// arm's contract. Neutering that arm in a provider fails this test in that provider and only
// that provider, so the arm is enforced behaviour rather than dead weight. Its value is
// defence-in-depth -- the arm's unreachability is a property of today's lock discipline, and
// the single Unlock/Lock split that makes it reachable is exactly the shape an attempted #180
// fix would introduce. This test is what would catch such a fix regressing the guard.
//
// The state is reconstructed directly rather than raced for, so the test is deterministic and
// cannot pass by scheduling luck. The assertion goes through accessTokenValue -- the real
// waiter path a joining caller takes -- not the field, so it fails if the value leaks by any
// route.
func TestInvalidate_DoesNotLeakThroughAPublishedFlight(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"access_token":"tok_fresh","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	// A flight that has ALREADY completed with tok_1 and is still published. This state is
	// CONSTRUCTED to exercise the poison arm's contract -- it is not a state the leader path
	// can leave behind, since publication and teardown share one critical section (see the
	// doc comment above). Seeding it directly is what makes the test deterministic.
	done := make(chan struct{})
	close(done)
	c.tokenMu.Lock()
	c.accessToken = "tok_1"
	c.tokenExpiry = c.now().Add(time.Hour)
	c.inflight = &tokenRefresh{done: done, token: "tok_1"}
	c.tokenMu.Unlock()

	// The 401 for tok_1 arrives.
	c.invalidateAccessToken("tok_1")

	// A caller that missed the cache now joins. It must not be handed tok_1.
	got, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("accessTokenValue after invalidation: %v", err)
	}
	if got == "tok_1" {
		t.Errorf("accessTokenValue returned the rejected token %q: an in-flight refresh "+
			"republished the value the 401 just invalidated", got)
	}
}

// TestInvalidate_LeavesAnUnrelatedFlightAlone is the OPPOSED failure mode, and the reason the
// fix cannot simply be "clear more".
//
// The compare-and-clear exists because an unconditional clear evicted a NEWER token that no
// 401 had rejected (see TestABA_LateUnauthorizedDoesNotEvictANewerToken). Poisoning the
// in-flight result must be exactly as selective: a flight carrying a DIFFERENT token was
// never rejected, and blanking it would force a needless re-exchange and reintroduce the very
// over-invalidation the guard was written to prevent.
func TestInvalidate_LeavesAnUnrelatedFlightAlone(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))

	done := make(chan struct{})
	close(done)
	c.tokenMu.Lock()
	c.accessToken = "tok_1"
	c.tokenExpiry = c.now().Add(time.Hour)
	c.inflight = &tokenRefresh{done: done, token: "tok_2"}
	c.tokenMu.Unlock()

	c.invalidateAccessToken("tok_1")

	// tok_2 was never rejected, so a caller joining that flight must still receive it --
	// with no upstream token server configured, a re-exchange would fail outright.
	got, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("accessTokenValue: %v", err)
	}
	if got != "tok_2" {
		t.Errorf("accessTokenValue = %q, want tok_2: a flight carrying a token no 401 named "+
			"must survive, or the fix reintroduces the ABA over-invalidation", got)
	}
}

// TestInvalidate_DoesNotStrandANewerFlight pins the consequence of unpublishing.
//
// Because invalidateAccessToken now retracts a poisoned flight from c.inflight, a REAL leader
// goroutine can still be running against a flight that is no longer the published one. If
// that leader retracted c.inflight unconditionally it would erase the newer flight, and every
// caller waiting on it would block forever on a result nobody completes.
//
// The test drives the ACTUAL leader (accessTokenValue -> fetchToken -> the teardown
// goroutine) rather than replaying the teardown inline: a test that re-implements the branch
// it is checking agrees with itself and survives the branch being deleted. The token server
// is held until the flight has been unpublished, so the leader tears down into a client that
// has genuinely moved on.
func TestInvalidate_DoesNotStrandANewerFlight(t *testing.T) {
	release := make(chan struct{})
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = fmt.Fprint(w, `{"access_token":"tok_stale","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	// A real leader starts and blocks in fetchToken, with its flight published.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.accessTokenValue(context.Background())
	}()

	stale := waitForFlight(t, c, nil)

	// Retract that flight the way invalidateAccessToken does, then publish a newer one in
	// its place -- the state a 401 plus a following caller produces.
	newer := &tokenRefresh{done: make(chan struct{})}
	c.tokenMu.Lock()
	c.inflight = newer
	c.tokenMu.Unlock()

	// Let the stale leader finish and run its teardown against the moved-on client.
	close(release)
	<-leaderDone

	c.tokenMu.Lock()
	published := c.inflight
	c.tokenMu.Unlock()

	if published != newer {
		t.Errorf("published flight = %p, want the newer flight %p (stale was %p): the stale "+
			"leader retracted a flight it did not own, stranding every caller waiting on it",
			published, newer, stale)
	}
}

// waitForFlight blocks until c.inflight is non-nil and different from prev, returning it.
// It rendezvouses on the client's own state rather than sleeping, so it observes the
// publication itself instead of guessing at how long one takes.
func waitForFlight(t *testing.T, c *Client, prev *tokenRefresh) *tokenRefresh {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.tokenMu.Lock()
		got := c.inflight
		c.tokenMu.Unlock()
		if got != nil && got != prev {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a refresh flight to be published")
		}
		runtime.Gosched()
	}
}
