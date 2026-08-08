// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newUnguardedFetcher builds a Fetcher WITHOUT the dial address guard, because httptest
// servers bind to 127.0.0.1 — precisely the address production refuses — so no end-to-end
// test of redirects, status handling or the size bound can run through NewFetcher.
//
// It lives in the test file rather than as a parameter on the production constructor so
// that no production code path can construct an unguarded fetcher, even by mistake. The
// address decision itself is covered separately by TestIsForbiddenIP and
// TestFetchRejectsForbiddenAddressAtDial, which DO use NewFetcher.
func newUnguardedFetcher() *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: fetchTimeout, CheckRedirect: noFollow}}
}

// TestIsForbiddenIP pins the address decision itself. It is a pure function, so every
// range is covered without a network — including the ones no live test could reach.
func TestIsForbiddenIP(t *testing.T) {
	forbidden := []string{
		"127.0.0.1", "::1", // loopback
		"0.0.0.0", "::", // unspecified — 0.0.0.0 routes to localhost on Linux
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fd00::1", // private
		"169.254.169.254", "fe80::1", // link-local, incl. the cloud metadata address
		"224.0.0.1", "ff02::1", // multicast
		"100.64.0.1",      // CGNAT
		"192.0.0.1",       // IETF protocol assignments
		"198.18.0.1",      // benchmarking
		"240.0.0.1",       // reserved
		"255.255.255.255", // broadcast
		// The ranges Go's own predicates do NOT cover, each reachable before it was
		// listed: IsUnspecified matches ONLY 0.0.0.0, so the rest of 0/8 passed, and
		// IsLinkLocalUnicast is fe80::/10, so deprecated site-local fec0::/10 passed.
		"0.0.0.1", "0.255.255.255", // RFC 1122 "this network", past the unspecified address
		"fec0::1",                                  // RFC 3879 site-local
		"192.0.2.1", "198.51.100.1", "203.0.113.1", // RFC 5737 TEST-NET-1/2/3
		"100::1",       // RFC 6666 discard-only
		"2001::1",      // RFC 2928 IETF protocol assignments
		"2001:db8::1",  // RFC 3849 documentation — outside 2001::/23, despite the prefix
		"3fff::1",      // RFC 9637 documentation
		"5f00::1",      // RFC 9602 SRv6 SIDs
		"64:ff9b:1::1", // RFC 8215 local-use NAT64
		// The 4-in-6 spellings of two of the above: same host, different notation.
		"::ffff:127.0.0.1", "::ffff:169.254.169.254",
	}
	for _, s := range forbidden {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: %q is not an IP", s)
		}
		if !isForbiddenIP(ip) {
			t.Errorf("isForbiddenIP(%s) = false, want true", s)
		}
	}
	// Over-rejection is a false absence too: a public page refused here is reported to the
	// caller as a page with no event metadata, which is a different and wrong answer.
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:2800:220:1::1"} {
		if isForbiddenIP(net.ParseIP(s)) {
			t.Errorf("isForbiddenIP(%s) = true, want false", s)
		}
	}
}

// TestFetchRejectsForbiddenAddressAtDial proves the guard is WIRED, not merely defined.
// The URL is a literal loopback address, so no DNS is involved and no network is needed.
// Reverting the Control hook makes this fail with a connection error instead of the
// sentinel — the disposition matters, because ErrEventURLFetchFailed is answered 503
// and invites a retry of a request that must never succeed.
func TestFetchRejectsForbiddenAddressAtDial(t *testing.T) {
	_, err := NewFetcher().Fetch(context.Background(), "http://127.0.0.1:9/page.html")
	if !errors.Is(err, ErrEventURLForbidden) {
		t.Fatalf("Fetch(loopback) error = %v, want ErrEventURLForbidden", err)
	}
	// The error must not carry the dial detail, which would confirm reachability.
	if strings.Contains(err.Error(), "connect") {
		t.Errorf("forbidden error leaked transport detail: %v", err)
	}
}

func TestFetchRejectsBadScheme(t *testing.T) {
	for _, u := range []string{"ftp://example.com", "file:///etc/passwd", "gopher://example.com", "example.com", ""} {
		_, err := NewFetcher().Fetch(context.Background(), u)
		if !errors.Is(err, ErrEventURLInvalid) {
			t.Errorf("Fetch(%q) error = %v, want ErrEventURLInvalid", u, err)
		}
	}
}

// TestFetchAcceptsUppercaseScheme guards the RFC 3986 §3.1 case-folding assumption:
// url.Parse lower-cases the scheme, so the comparison must not be defeated by "HTTP://".
func TestFetchAcceptsUppercaseScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, err := newUnguardedFetcher().Fetch(context.Background(), strings.Replace(srv.URL, "http://", "HTTP://", 1))
	if err != nil || string(body) != "ok" {
		t.Fatalf("Fetch(uppercase scheme) = %q, %v; want \"ok\", nil", body, err)
	}
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	// atomic, not a plain bool: the handler runs on the test server's goroutine and the
	// assertion reads from the test's, and a redirect the client never follows leaves NO
	// happens-before edge between the two — the very case this test exists to detect is
	// the one with no synchronisation to borrow.
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer srv.Close()

	// The guard is off, so the redirect WOULD succeed if it were followed. That is the
	// point: the assertion is about the redirect policy, not about the address guard
	// incidentally blocking the second hop.
	_, err := newUnguardedFetcher().Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Errorf("Fetch(redirect) error = %v, want ErrEventURLFetchFailed", err)
	}
	if reached.Load() {
		t.Error("the redirect was followed; the target server was contacted")
	}
}

func TestFetchRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newUnguardedFetcher().Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Errorf("Fetch(500) error = %v, want ErrEventURLFetchFailed", err)
	}
	if strings.Contains(err.Error(), "nope") {
		t.Errorf("error echoed the response body: %v", err)
	}
}

// TestFetchRejectsOversizedBody covers the +1 read: a body one byte over the limit must
// be REFUSED, not truncated and handed to the parser as if it were a whole page.
func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxResponseBytes+1))
	}))
	defer srv.Close()

	_, err := newUnguardedFetcher().Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("Fetch(oversized) error = %v, want a size-limit ErrEventURLFetchFailed", err)
	}
}

// TestFetchPreservesContextCancellation pins the %w on the transport cause. A cancelled
// fetch is not an upstream failure the caller should retry — it is this service giving up —
// and only errors.Is can tell the two apart once both wear ErrEventURLFetchFailed. With %v
// the cause is flattened into the message and the identity is gone.
func TestFetchPreservesContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newUnguardedFetcher().Fetch(ctx, srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Fatalf("Fetch err = %v, want ErrEventURLFetchFailed", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fetch err = %v, want it to unwrap to context.Canceled", err)
	}
}
