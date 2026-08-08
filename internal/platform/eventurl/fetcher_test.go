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
	"testing"
)

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
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1::1"} {
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

	body, err := newFetcher(true).Fetch(context.Background(), strings.Replace(srv.URL, "http://", "HTTP://", 1))
	if err != nil || string(body) != "ok" {
		t.Fatalf("Fetch(uppercase scheme) = %q, %v; want \"ok\", nil", body, err)
	}
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer srv.Close()

	// allowPrivate is on, so the redirect would succeed if it were followed. That is
	// the point: the assertion is about the redirect policy, not about the address
	// guard incidentally blocking the second hop.
	_, err := newFetcher(true).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Errorf("Fetch(redirect) error = %v, want ErrEventURLFetchFailed", err)
	}
	if reached {
		t.Error("the redirect was followed; the target server was contacted")
	}
}

func TestFetchRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newFetcher(true).Fetch(context.Background(), srv.URL)
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

	_, err := newFetcher(true).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrEventURLFetchFailed) || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("Fetch(oversized) error = %v, want a size-limit ErrEventURLFetchFailed", err)
	}
}
