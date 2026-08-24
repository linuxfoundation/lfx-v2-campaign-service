// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestDoRequest_401InvalidatesTheCachedToken pins that an auth rejection does not leave the
// client serving the rejected token for the rest of its cached lifetime.
//
// See the reddit sibling for the full reasoning; the mechanism is identical because the two
// token caches are the same design. In short: a client rebuilt per operation started with an
// empty cache, so a revoked token cost one failure. Once the dispatcher caches the client
// across operations (internal/dispatch/microsoft.go), the same accessToken serves every later
// call until its ADVERTISED expiry — which for a revoked token never usefully arrives.
//
// The assertion is on the token endpoint HIT COUNT rather than the error, because the error
// is the same either way and only re-minting distinguishes a recovered client from a stuck
// one. The clock is fixed so expiry cannot be what rescues it.
func TestDoRequest_401InvalidatesTheCachedToken(t *testing.T) {
	var tokenHits int64
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&tokenHits, 1)
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"access_token":"tok_%d","expires_in":3600}`, n))
	}))
	defer tok.Close()

	var presented []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = append(presented, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"Errors":[{"Code":"AuthenticationTokenExpired"}]}`)
	}))
	defer api.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tok.URL), WithBaseURL(api.URL), WithClock(fixedClock()))

	// Two operations on ONE client, the shape dispatch-level caching creates.
	for i := range 2 {
		if _, err := c.doRequest(context.Background(), http.MethodGet, "/Campaigns", nil, true); err == nil {
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
		if _, err := c.doRequest(context.Background(), http.MethodGet, "/Campaigns", nil, true); err == nil {
			t.Fatalf("doRequest %d: got nil error, want a failure", i+1)
		}
	}

	if n := atomic.LoadInt64(&tokenHits); n != 2 {
		t.Errorf("token endpoint hit %d times across 2 operations, want 2: an UNREADABLE 401 "+
			"is still a 401 and must invalidate the cached token", n)
	}
}
