// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package eventurl provides event URL fetching and parsing with SSRF protections.
package eventurl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

const (
	// maxResponseBytes bounds a fetched body. An unbounded fetch is a memory-exhaustion
	// vector, and 10MiB is far larger than any real event page.
	maxResponseBytes = 10 << 20 // 10 MiB
	// fetchTimeout is the deadline for a complete fetch (DNS + connect + read).
	fetchTimeout = 15 * time.Second
	// connectTimeout bounds DNS resolution plus the TCP handshake for one address.
	connectTimeout = 10 * time.Second
)

// forbiddenNets are ranges that must never be reachable through a caller-supplied URL
// but that no net.IP predicate covers. The predicates handle the rest (see isForbiddenIP).
var forbiddenNets = []net.IPNet{
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}, // RFC 6598 CGNAT
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},  // RFC 6890 IETF protocol assignments
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}, // RFC 2544 benchmarking
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},   // RFC 1112 reserved, incl. 255.255.255.255
}

// isForbiddenIP reports whether ip is an address this service must not connect to.
//
// It default-DENIES: the caller only proceeds for an address that fails every check
// here, so a range nobody thought of stays reachable only if it is genuinely public.
// The 4-in-6 form is normalized first, because a mapped address like ::ffff:169.254.169.254
// is the same host as its IPv4 spelling and must not slip past an IPv4-shaped range test.
func isForbiddenIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for i := range forbiddenNets {
		if forbiddenNets[i].Contains(ip) {
			return true
		}
	}
	return false
}

// noFollow keeps the client from following redirects. Following one would re-run the
// whole address decision on a location the caller never supplied, so the redirect
// response is returned as-is and rejected by the non-2xx check.
func noFollow(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// Fetcher wraps an HTTP client with SSRF guards for event URL fetching.
type Fetcher struct {
	client *http.Client
}

// guardDialAddress is the net.Dialer.Control hook that refuses a non-public address.
//
// The check lives HERE, and not next to a prior LookupIPAddr, because Control runs after
// resolution and immediately before connect, on the very address the kernel is about to
// use. Resolving separately and checking the result is a TOCTOU: http.Transport resolves
// the hostname again, so a DNS record with a short TTL can answer a public address for
// the check and 127.0.0.1 for the connection. Checking here, that second answer IS the
// one inspected — and every address a multi-address host offers is inspected, not just
// the first.
func guardDialAddress(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable dial address %q", ErrEventURLForbidden, address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: unparsable dial address %q", ErrEventURLForbidden, address)
	}
	if isForbiddenIP(ip) {
		return fmt.Errorf("%w: %s", ErrEventURLForbidden, ip)
	}
	return nil
}

// NewFetcher constructs a Fetcher with the standard SSRF-safe defaults. It is the only
// constructor in non-test code, so no production path can obtain an unguarded fetcher.
func NewFetcher() *Fetcher {
	dialer := &net.Dialer{Timeout: connectTimeout, Control: guardDialAddress}
	return &Fetcher{
		client: &http.Client{
			Timeout:       fetchTimeout,
			CheckRedirect: noFollow,
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
				// Proxy is nil ON PURPOSE, and the zero value is not enough of a
				// statement to leave implicit. net/http uses no proxy when Proxy is
				// nil, but http.DefaultTransport sets ProxyFromEnvironment — so
				// "simplifying" this to DefaultTransport.Clone() would route every
				// fetch through a cluster HTTP_PROXY, and the dialer would then only
				// ever see the PROXY's address. The guard above would pass while the
				// proxy fetched 169.254.169.254 on our behalf. Keep this direct.
				Proxy: nil,
			},
		},
	}
}

// Fetch retrieves the body of eventURL with SSRF protections. It rejects:
//   - non-http/https schemes
//   - any URL whose connection would land on a non-public address (see isForbiddenIP)
//   - redirects, and any non-2xx response
//   - bodies exceeding maxResponseBytes
//
// Errors wrap ErrEventURLInvalid (the caller's URL is unusable), ErrEventURLForbidden
// (the address is off limits), or ErrEventURLFetchFailed (the origin did not answer
// usefully). The body is never echoed back in an error.
func (f *Fetcher) Fetch(ctx context.Context, eventURL string) ([]byte, error) {
	parsed, err := url.Parse(eventURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEventURLInvalid, err)
	}
	// url.Parse lower-cases the scheme (RFC 3986 §3.1), so this comparison already
	// covers "HTTPS://". Host and path are deliberately left alone.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrEventURLInvalid, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w: missing hostname", ErrEventURLInvalid)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid request: %v", ErrEventURLInvalid, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LFX-CampaignService/1.0)")

	resp, err := f.client.Do(req)
	if err != nil {
		// A refusal from the dial Control hook arrives here wrapped in *url.Error and
		// *net.OpError; both unwrap, so the sentinel survives. It must be re-checked
		// BEFORE the generic branch, or a forbidden address would be reported as a
		// transient fetch failure and answered 503 — inviting a retry of a request
		// that must never succeed.
		if errors.Is(err, ErrEventURLForbidden) {
			return nil, fmt.Errorf("%w", ErrEventURLForbidden)
		}
		return nil, fmt.Errorf("%w: fetch failed: %v", ErrEventURLFetchFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded amount before closing: net/http only returns a connection to
		// the idle pool once its body has reached EOF and been closed.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%w: HTTP %d", ErrEventURLFetchFailed, resp.StatusCode)
	}

	// Read one byte past the limit so an oversized body is detected rather than
	// silently truncated into a parse of half a page.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read response: %v", ErrEventURLFetchFailed, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds size limit", ErrEventURLFetchFailed)
	}
	return body, nil
}
