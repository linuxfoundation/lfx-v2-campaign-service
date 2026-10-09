// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// linkPortalServer answers token-info with hubID (or a 500 when hubID is ""), counting calls.
// Each test gets its own server, so its URL — part of the cache key — isolates it from every
// other test's cached answers without touching the package-level cache.
func linkPortalServer(t *testing.T, hubID string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenInfoPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		calls.Add(1)
		if hubID == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"hubId": %q}`, hubID)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func linkPortalClient(srv *httptest.Server, portalID string, opts ...Option) *Client {
	return NewClient(Credentials{PrivateAppToken: "test-token"}, AccountConfig{PortalID: portalID},
		append([]Option{WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond)}, opts...)...)
}

// The regression: a connection with no portal_id built every link blank, and the BFF 500'd
// compose and attach-existing on the blank master link.
func TestWithLinkPortalFallback_BlankPortalBuildsLinksFromTheToken(t *testing.T) {
	srv, _ := linkPortalServer(t, "8112310")
	c := linkPortalClient(srv, "")
	if got := c.ListURL("29084"); got != "" {
		t.Fatalf("precondition: a client with no portal builds %q, want a blank link", got)
	}

	got := c.WithLinkPortalFallback(context.Background())

	if want := AppBaseURL + "/contacts/8112310/objectLists/29084/filters"; got.ListURL("29084") != want {
		t.Errorf("ListURL = %q, want %q", got.ListURL("29084"), want)
	}
	if want := AppBaseURL + "/email/8112310/details/77/performance"; got.EmailDetailsURL("77") != want {
		t.Errorf("EmailDetailsURL = %q, want %q", got.EmailDetailsURL("77"), want)
	}
	if want := AppBaseURL + "/email/8112310/edit/77/settings"; got.emailEditURL("77") != want {
		t.Errorf("emailEditURL = %q, want %q", got.emailEditURL("77"), want)
	}
	// The receiver is never mutated: a *Client is shared across goroutines.
	if c.ListURL("29084") != "" {
		t.Error("the receiver was mutated; the resolved portal must go on a copy")
	}
}

// A configured portal_id is the operator's statement and is never overridden — not even
// looked up, so the fallback adds no HubSpot call for a correctly configured connection.
func TestWithLinkPortalFallback_ConfiguredPortalWinsWithoutALookup(t *testing.T) {
	srv, calls := linkPortalServer(t, "999")
	c := linkPortalClient(srv, "8112310")

	got := c.WithLinkPortalFallback(context.Background())

	if got != c {
		t.Error("a client with a configured portal must be returned as is")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("token-info called %d times, want 0", n)
	}
}

// Clients are built per request, so the answer must be cached across clients for the same
// token, or every list and email response would cost an extra HubSpot round trip.
func TestWithLinkPortalFallback_CachesAcrossClients(t *testing.T) {
	srv, calls := linkPortalServer(t, "8112310")

	for i := 0; i < 3; i++ {
		got := linkPortalClient(srv, "").WithLinkPortalFallback(context.Background())
		if got.ListURL("1") == "" {
			t.Fatalf("call %d built a blank link", i)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("token-info called %d times for one token, want 1", n)
	}
}

// A failed lookup degrades to the pre-fallback behaviour (blank links) without failing, and
// is remembered briefly so an outage does not cost a lookup per request; once the failure
// TTL passes it is asked again.
func TestWithLinkPortalFallback_FailureIsBlankAndBrieflyCached(t *testing.T) {
	srv, calls := linkPortalServer(t, "")
	now := time.Now()
	clock := func() time.Time { return now }

	c := linkPortalClient(srv, "", withClock(clock))
	if got := c.WithLinkPortalFallback(context.Background()); got.ListURL("1") != "" {
		t.Errorf("a failed lookup built %q, want a blank link", got.ListURL("1"))
	}
	first := calls.Load()
	if first == 0 {
		t.Fatal("precondition: the lookup was never attempted")
	}

	_ = linkPortalClient(srv, "", withClock(clock)).WithLinkPortalFallback(context.Background())
	if n := calls.Load(); n != first {
		t.Errorf("a cached failure was retried within its TTL (%d calls, want %d)", n, first)
	}

	now = now.Add(linkPortalFailureTTL + time.Second)
	_ = linkPortalClient(srv, "", withClock(clock)).WithLinkPortalFallback(context.Background())
	if n := calls.Load(); n == first {
		t.Error("an expired failure was not retried")
	}
}

// The cache must never hold the token itself.
func TestLinkPortalKey_HoldsNoCredential(t *testing.T) {
	k := linkPortalKey("https://api.hubapi.com", "pat-na1-secret")
	if len(k) != 64 {
		t.Errorf("key %q is not a sha256 hex digest", k)
	}
	for _, frag := range []string{"pat-na1", "secret"} {
		if strings.Contains(k, frag) {
			t.Errorf("key %q contains %q", k, frag)
		}
	}
}
