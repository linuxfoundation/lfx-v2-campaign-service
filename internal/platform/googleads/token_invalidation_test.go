// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
