// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
