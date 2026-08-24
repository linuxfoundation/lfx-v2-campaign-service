// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestRequest_401InvalidatesTheCachedToken pins that an auth rejection does not leave the
// client serving the rejected token for the rest of its cached lifetime.
//
// This is a property of a CACHED client, which is what makes it worth a test now: a client
// rebuilt per operation started with an empty cache, so a revoked token cost one failure
// and the next operation re-minted. Once the dispatcher caches the client across operations
// (internal/dispatch/reddit.go), the same client — and the same cachedToken — serves every
// later call, so a token the platform has already rejected keeps being presented until its
// ADVERTISED expiry, which for a revoked token never usefully arrives.
//
// The assertion is on the token endpoint HIT COUNT, not on the error: the error is the same
// either way, and only re-minting distinguishes a client that recovered from one that is
// stuck. The fixture hands out a distinct token per mint so the second attempt's identity is
// observable, and asserts the second request actually PRESENTED the new one.
func TestRequest_401InvalidatesTheCachedToken(t *testing.T) {
	var tokenHits int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&tokenHits, 1)
		// A long expires_in is the whole point: expiry must NOT be what rescues this.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok_%d", n), "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	var presented []string
	var apiHits int64
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		presented = append(presented, r.Header.Get("Authorization"))
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
	)

	// Two operations on ONE client, the shape dispatch-level caching creates.
	for i := range 2 {
		if _, err := c.request(context.Background(), http.MethodGet, "/thing", nil); err == nil {
			t.Fatalf("request %d: got nil error, want the 401 to surface", i+1)
		}
	}

	// The clock is fixed, so no token expired: a second mint can only come from the 401
	// clearing the cache.
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
	c := NewClient(testCreds, testAccount)

	c.mu.Lock()
	c.cachedToken = "tok_current"
	c.mu.Unlock()

	// A late 401 answering an OLDER token must be a no-op.
	c.invalidateAccessToken("tok_stale")
	if got := c.cachedToken; got != "tok_current" {
		t.Errorf("cached token = %q, want it untouched: a 401 naming an older token must not "+
			"evict the newer one the cache now holds", got)
	}

	// The token the 401 actually named IS dropped.
	c.invalidateAccessToken("tok_current")
	if got := c.cachedToken; got != "" {
		t.Errorf("cached token = %q, want it cleared: the token the 401 named must be dropped", got)
	}
}

// TestRequest_401InvalidatesBeforeTheBodyIsRead pins the ORDERING, not the outcome.
//
// readResponseBody can block until the per-attempt deadline on a slow or truncated 401, and
// for that whole window every concurrent caller on this shared client keeps taking the
// rejected token from refreshToken's fast path — so a guard placed after the read makes the
// "next operation re-mints" guarantee false exactly under the load that makes it matter.
//
// The fixture flushes the 401 status and then stalls before writing the body, and asserts the
// cache is ALREADY empty at that instant. The outcome-only tests cannot see this: they pass
// with the guard in either position.
func TestRequest_401InvalidatesBeforeTheBodyIsRead(t *testing.T) {
	bodyGate := make(chan struct{})
	statusFlushed := make(chan struct{})
	observed := make(chan string, 1)
	presented := make(chan string, 1)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok_1", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// The status line is on the wire and the body is deliberately withheld. Signal
		// HERE, from inside the handler, rather than letting the sampler guess with a
		// timer: a fixed sleep is not synchronised with either the token mint or this
		// flush, so on a loaded runner it can sample a cache that was simply never
		// populated and record a FALSE PASS.
		close(statusFlushed)
		<-bodyGate
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL),
		WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	go func() {
		<-statusFlushed
		// The 401 status is out; the client clears a moment later, as Do returns. Poll the
		// cache until it does, instead of sampling once (which races the client) or
		// sleeping (a wall-clock guess that goes flaky under load). This cannot pass
		// vacuously: the BODY is still withheld below, so a guard placed after
		// readResponseBody stays blocked on bodyGate -- which only this goroutine releases
		// -- for as long as the loop runs. Bounded so a real regression fails rather than
		// hangs.
		deadline := time.Now().Add(10 * time.Second)
		for {
			c.mu.Lock()
			got := c.cachedToken
			c.mu.Unlock()
			if got == "" || time.Now().After(deadline) {
				observed <- got
				break
			}
			runtime.Gosched()
		}
		close(bodyGate)
	}()

	_, _ = c.request(context.Background(), http.MethodGet, "/thing", nil)

	// Positive control. Without this the assertion below passes just as happily against a
	// cache that was never populated at all -- the exact false pass a fixed sleep invites.
	// Proving a token was minted AND presented makes the empty read below mean eviction.
	if got := <-presented; got != "Bearer tok_1" {
		t.Fatalf("Authorization = %q, want Bearer tok_1: the assertion below is only "+
			"meaningful if a token was actually minted and presented", got)
	}

	if got := <-observed; got != "" {
		t.Errorf("cached token = %q while the 401 body was still in flight, want it already "+
			"cleared: invalidation must happen on the status line, not after readResponseBody", got)
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
// cannot pass by scheduling luck. The assertion goes through refreshToken -- the real waiter
// path a joining caller takes -- not the field, so it fails if the value leaks by any route.
func TestInvalidate_DoesNotLeakThroughAPublishedFlight(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok_fresh", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	// A flight that has ALREADY completed with tok_1 and is still published. This state is
	// CONSTRUCTED to exercise the poison arm's contract -- it is not a state the leader path
	// can leave behind, since publication and teardown share one critical section (see the
	// doc comment above). Seeding it directly is what makes the test deterministic.
	done := make(chan struct{})
	close(done)
	c.mu.Lock()
	c.cachedToken = "tok_1"
	c.tokenExpireAt = c.now().Add(time.Hour)
	c.inflight = &tokenRefresh{done: done, token: "tok_1"}
	c.mu.Unlock()

	// The 401 for tok_1 arrives.
	c.invalidateAccessToken("tok_1")

	// A caller that missed the cache now joins. It must not be handed tok_1.
	got, err := c.refreshToken(context.Background())
	if err != nil {
		t.Fatalf("refreshToken after invalidation: %v", err)
	}
	if got == "tok_1" {
		t.Errorf("refreshToken returned the rejected token %q: an in-flight refresh "+
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
	c := NewClient(testCreds, testAccount, WithNowFunc(fixedRedditClock()))

	done := make(chan struct{})
	close(done)
	c.mu.Lock()
	c.cachedToken = "tok_1"
	c.tokenExpireAt = c.now().Add(time.Hour)
	c.inflight = &tokenRefresh{done: done, token: "tok_2"}
	c.mu.Unlock()

	c.invalidateAccessToken("tok_1")

	// tok_2 was never rejected, so a caller joining that flight must still receive it --
	// with no upstream token server configured, a re-exchange would fail outright.
	got, err := c.refreshToken(context.Background())
	if err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if got != "tok_2" {
		t.Errorf("refreshToken = %q, want tok_2: a flight carrying a token no 401 named "+
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
// The test drives the ACTUAL leader (refreshToken -> fetchToken -> the teardown goroutine)
// rather than replaying the teardown inline: a test that re-implements the branch it is
// checking agrees with itself and survives the branch being deleted. The token server is held
// until the flight has been unpublished, so the leader tears down into a client that has
// genuinely moved on.
func TestInvalidate_DoesNotStrandANewerFlight(t *testing.T) {
	release := make(chan struct{})
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok_stale", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	// A real leader starts and blocks in fetchToken, with its flight published.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.refreshToken(context.Background())
	}()

	stale := waitForFlight(t, c, nil)

	// Retract that flight the way invalidateAccessToken does, then publish a newer one in
	// its place -- the state a 401 plus a following caller produces.
	newer := &tokenRefresh{done: make(chan struct{})}
	c.mu.Lock()
	c.inflight = newer
	c.mu.Unlock()

	// Let the stale leader finish and run its teardown against the moved-on client.
	close(release)
	<-leaderDone

	c.mu.Lock()
	published := c.inflight
	c.mu.Unlock()

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
		c.mu.Lock()
		got := c.inflight
		c.mu.Unlock()
		if got != nil && got != prev {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a refresh flight to be published")
		}
		runtime.Gosched()
	}
}
