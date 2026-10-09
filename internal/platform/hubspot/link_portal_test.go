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

// linkPortalServer answers token-info with hubID, or with failStatus when hubID is "",
// counting calls.
// Each test gets its own server, so its URL — part of the cache key — isolates it from every
// other test's cached answers without touching the package-level cache.
func linkPortalServer(t *testing.T, hubID string, failStatus ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenInfoPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		calls.Add(1)
		if hubID == "" {
			status := http.StatusForbidden
			if len(failStatus) > 0 {
				status = failStatus[0]
			}
			w.WriteHeader(status)
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

// A DEFINITIVE failure (the token cannot read its own portal) degrades to the pre-fallback
// behaviour (blank links) without failing, and is remembered briefly so a revoked token does
// not cost a lookup per request; once the failure TTL passes it is asked again.
func TestWithLinkPortalFallback_DefinitiveFailureIsBlankAndBrieflyCached(t *testing.T) {
	srv, calls := linkPortalServer(t, "", http.StatusForbidden)
	now := time.Now()
	clock := func() time.Time { return now }

	c := linkPortalClient(srv, "", withClock(clock))
	if got := c.WithLinkPortalFallback(context.Background()); got.ListURL("1") != "" {
		t.Errorf("a failed lookup built %q, want a blank link", got.ListURL("1"))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a 403 is not retried; token-info called %d times, want 1", n)
	}

	_ = linkPortalClient(srv, "", withClock(clock)).WithLinkPortalFallback(context.Background())
	if n := calls.Load(); n != 1 {
		t.Errorf("a cached failure was retried within its TTL (%d calls, want 1)", n)
	}

	now = now.Add(linkPortalFailureTTL + time.Second)
	_ = linkPortalClient(srv, "", withClock(clock)).WithLinkPortalFallback(context.Background())
	if n := calls.Load(); n != 2 {
		t.Errorf("an expired failure was not retried (%d calls, want 2)", n)
	}
}

// A TRANSIENT failure (5xx, 429, transport) is never cached: the BFF refuses a master list with
// a blank link, so caching one HubSpot blip would be a minute of the 500s this fixes.
func TestWithLinkPortalFallback_TransientFailureIsNotCached(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, calls := linkPortalServer(t, "", status)
			_ = linkPortalClient(srv, "").WithLinkPortalFallback(context.Background())
			first := calls.Load()
			_ = linkPortalClient(srv, "").WithLinkPortalFallback(context.Background())
			if n := calls.Load(); n == first {
				t.Errorf("a %d was cached; the next request did not ask again", status)
			}
		})
	}
}

// The answer is shared by every caller of the token, so ONE caller's cancelled request must not
// decide it: the lookup runs detached from the caller's cancellation and its answer is cached.
func TestWithLinkPortalFallback_CallerCancellationDoesNotDecideTheAnswer(t *testing.T) {
	srv, calls := linkPortalServer(t, "8112310")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := linkPortalClient(srv, "").WithLinkPortalFallback(ctx); got.ListURL("1") == "" {
		t.Error("a cancelled caller context left the link blank")
	}
	if got := linkPortalClient(srv, "").WithLinkPortalFallback(context.Background()); got.ListURL("1") == "" {
		t.Error("the next caller got a blank link")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("token-info called %d times, want 1", n)
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
