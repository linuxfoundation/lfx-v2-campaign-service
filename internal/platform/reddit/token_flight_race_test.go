// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// steppingClock is a goroutine-safe test clock the test can move forward. The token
// cache reads it from the leader's refresh goroutine, so a plain captured variable
// would be a data race under -race.
type steppingClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSteppingClock() *steppingClock {
	return &steppingClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (s *steppingClock) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t
}

func (s *steppingClock) advance(d time.Duration) {
	s.mu.Lock()
	s.t = s.t.Add(d)
	s.mu.Unlock()
}

type tokenResult struct {
	token string
	err   error
}

// gatedTokenServer answers the token endpoint from bodies, one per hit (the last one
// repeats). The hit numbered gateHit closes entered and then blocks until release is
// closed, so a test can act while that refresh is provably on the network: the flight is
// published and inflight.token is still "".
func gatedTokenServer(t *testing.T, gateHit int64, bodies ...string) (srv *httptest.Server, hits *int64, entered <-chan struct{}, release func()) {
	t.Helper()
	var n int64
	in := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit := atomic.AddInt64(&n, 1)
		if hit == gateHit {
			close(in)
			<-gate
		}
		i := int(hit) - 1
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		_, _ = fmt.Fprint(w, bodies[i])
	}))
	rel := func() { once.Do(func() { close(gate) }) }
	// Release before Close: Close blocks on in-flight handlers, so a test that fails while
	// the gate is shut must not hang the package.
	t.Cleanup(func() { rel(); srv.Close() })
	return srv, &n, in, rel
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// TestRefresh_401DuringTheFlightIsNotRepublished reproduces #180 against the REAL leader
// path -- no seeded flight state.
//
// The interleaving, every step of it rendezvoused through the token server's handler:
//
//  1. tok_1 is minted and handed out (a request now carries it).
//  2. The clock moves inside the expiry buffer, so the next caller leads a refresh. That
//     refresh is held INSIDE the token server's handler: the flight is published and
//     inflight.token is still "".
//  3. The 401 for tok_1 lands now. The cache still holds tok_1 and is cleared; the flight
//     carries no token yet, so nothing on it matches.
//  4. The endpoint answers with tok_1 again (an authorization server may re-issue a
//     still-unexpired access token on refresh).
//
// Before the fix, the leader then wrote tok_1 back into the cache with a fresh hour of
// life and handed it to every waiter: the 401 was undone, and a cached client re-presented
// the rejected token to every later call until it expired. The assertions are on both
// routes out -- the waiter's result and the cache fast path -- plus a positive control that
// the recovery exchange actually ran.
func TestRefresh_401DuringTheFlightIsNotRepublished(t *testing.T) {
	srv, hits, entered, release := gatedTokenServer(t, 2,
		`{"access_token":"tok_1","expires_in":3600}`,
		`{"access_token":"tok_1","expires_in":3600}`, // the re-issued, already-rejected value
		`{"access_token":"tok_2","expires_in":3600}`,
	)
	clock := newSteppingClock()
	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(clock.now))
	ctx := context.Background()

	if got, err := c.refreshToken(ctx); err != nil || got != "tok_1" {
		t.Fatalf("first mint = %q, %v; want tok_1", got, err)
	}

	clock.advance(time.Hour - 30*time.Second) // inside redditTokenExpiryBuffer: forces a refresh

	waiter := make(chan tokenResult, 1)
	go func() {
		tok, err := c.refreshToken(ctx)
		waiter <- tokenResult{tok, err}
	}()
	waitFor(t, entered, "the refresh to reach the token endpoint")

	c.invalidateAccessToken("tok_1") // the 401 answering step 1's request
	release()

	w := waitFor(t, waiter, "the waiter")
	if w.err != nil {
		t.Fatalf("waiter: %v", w.err)
	}
	if w.token == "tok_1" {
		t.Errorf("waiter returned %q: a 401 that landed while the refresh was on the network "+
			"was undone when that refresh published the same, already-rejected token", w.token)
	}

	fast, err := c.refreshToken(ctx)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}
	if fast == "tok_1" {
		t.Errorf("cache fast path returned %q: the refresh re-cached a token a 401 rejected "+
			"mid-flight, so every later call re-presents it until it expires", fast)
	}
	if w.token != "tok_2" || fast != "tok_2" || atomic.LoadInt64(hits) != 3 {
		t.Errorf("waiter=%q fast=%q token hits=%d; want tok_2, tok_2, 3: the rejected result "+
			"must be dropped and ONE recovery exchange run", w.token, fast, atomic.LoadInt64(hits))
	}
}

// TestRefresh_StaleMidFlight401KeepsTheNewToken is the ABA side of the same window: a late
// 401 naming an OLDER token that lands while the refresh is on the network must not cost
// the refresh its new token. Rejection is by identity, so the fresh tok_2 is published,
// cached and served, with no extra exchange.
func TestRefresh_StaleMidFlight401KeepsTheNewToken(t *testing.T) {
	srv, hits, entered, release := gatedTokenServer(t, 1,
		`{"access_token":"tok_2","expires_in":3600}`,
	)
	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
	ctx := context.Background()

	waiter := make(chan tokenResult, 1)
	go func() {
		tok, err := c.refreshToken(ctx)
		waiter <- tokenResult{tok, err}
	}()
	waitFor(t, entered, "the refresh to reach the token endpoint")

	c.invalidateAccessToken("tok_1") // late 401 for a token this flight did not mint
	release()

	w := waitFor(t, waiter, "the waiter")
	if w.err != nil || w.token != "tok_2" {
		t.Fatalf("waiter = %q, %v; want tok_2: a 401 naming another token must not poison "+
			"this flight", w.token, w.err)
	}
	if fast, err := c.refreshToken(ctx); err != nil || fast != "tok_2" {
		t.Errorf("fast path = %q, %v; want the cached tok_2", fast, err)
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Errorf("token hits = %d, want 1: a stale 401 forced a needless re-exchange", n)
	}
}

// TestRefresh_ReissuedRecoveryIsStillClearedByTheNext401 pins the deliberate limit of the
// rejection record: it lives on the flight a 401 raced, not beyond it. If the RECOVERY
// exchange is answered with the same value again, that is the authorization server's current
// answer and it is served -- remembering rejections across flights would turn one transient
// 401 into a permanent refusal of a token the endpoint still vouches for. What must hold is
// that nothing is stuck: the next 401 for that value clears the cache, and the call after it
// exchanges again instead of being served the rejected token until it expires.
func TestRefresh_ReissuedRecoveryIsStillClearedByTheNext401(t *testing.T) {
	srv, hits, entered, release := gatedTokenServer(t, 2,
		`{"access_token":"tok_1","expires_in":3600}`,
		`{"access_token":"tok_1","expires_in":3600}`, // raced by the 401: dropped
		`{"access_token":"tok_1","expires_in":3600}`, // recovery: re-issued again
		`{"access_token":"tok_2","expires_in":3600}`,
	)
	clock := newSteppingClock()
	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(clock.now))
	ctx := context.Background()

	if got, err := c.refreshToken(ctx); err != nil || got != "tok_1" {
		t.Fatalf("first mint = %q, %v; want tok_1", got, err)
	}
	clock.advance(time.Hour - 30*time.Second)

	waiter := make(chan tokenResult, 1)
	go func() {
		tok, err := c.refreshToken(ctx)
		waiter <- tokenResult{tok, err}
	}()
	waitFor(t, entered, "the refresh to reach the token endpoint")
	c.invalidateAccessToken("tok_1")
	release()

	if w := waitFor(t, waiter, "the waiter"); w.err != nil || w.token != "tok_1" {
		t.Fatalf("waiter = %q, %v; want the recovery's re-issued tok_1", w.token, w.err)
	}

	// The waiter presents it and is refused again.
	c.invalidateAccessToken("tok_1")

	got, err := c.refreshToken(ctx)
	if err != nil {
		t.Fatalf("after the second 401: %v", err)
	}
	if got == "tok_1" {
		t.Errorf("served %q after a second 401: the cache is stuck on a rejected token", got)
	}
	if got != "tok_2" || atomic.LoadInt64(hits) != 4 {
		t.Errorf("got %q with %d token hits; want tok_2 from a 4th exchange", got, atomic.LoadInt64(hits))
	}
}

// TestFetchToken_DoesNotWriteTheCache pins WHERE the cache write lives. The leader stores
// the token in the same critical section that checks the flight's rejections, sets
// inflight.token and tears the flight down. A cache write inside fetchToken (its own lock
// hold, before that section) reopens the gap #180 describes: the token becomes visible on
// the fast path, can be presented and 401'd, and is then still published to the waiters.
func TestFetchToken_DoesNotWriteTheCache(t *testing.T) {
	srv, _, _, _ := gatedTokenServer(t, 0, `{"access_token":"tok_1","expires_in":3600}`)
	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	tok, _, err := c.fetchToken(context.Background())
	if err != nil || tok != "tok_1" {
		t.Fatalf("fetchToken = %q, %v; want tok_1", tok, err)
	}
	c.mu.Lock()
	cached := c.cachedToken
	c.mu.Unlock()
	if cached != "" {
		t.Errorf("cache = %q after a bare fetchToken; the cache must be written only by the "+
			"leader, atomically with publication", cached)
	}
}
