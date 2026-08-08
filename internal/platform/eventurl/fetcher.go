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
//
// Go's predicates are narrower than their names suggest, and the gaps are the entries
// below rather than obvious omissions: IsUnspecified matches ONLY 0.0.0.0, leaving the
// rest of 0/8 — which a Linux host treats as "this network" and routes locally — and
// IsLinkLocalUnicast is fe80::/10 alone, so deprecated site-local fec0::/10 passes every
// check. Both are the addresses an SSRF probe reaches for once the obvious ones are shut.
//
// The 6to4 pair (2002::/16 and its relay anycast 192.88.99.0/24, both deprecated by
// RFC 7526) is here for a sharper reason: a 6to4 address EMBEDS its IPv4 destination in
// bits 16-47, so 2002:7f00:1:: is a spelling of 127.0.0.1 that no IPv4-shaped range test
// and no net.IP predicate looks at. Decoding the embedded address and re-testing it would
// work too; refusing the whole deprecated range is simpler and loses nothing reachable.
var forbiddenNets = []net.IPNet{
	{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(8, 32)},         // RFC 1122 "this network"
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},     // RFC 6598 CGNAT
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},      // RFC 6890 IETF protocol assignments
	{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)},      // RFC 5737 TEST-NET-1
	{IP: net.IPv4(192, 88, 99, 0), Mask: net.CIDRMask(24, 32)},    // RFC 7526 deprecated 6to4 relay anycast
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},     // RFC 2544 benchmarking
	{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)},   // RFC 5737 TEST-NET-2
	{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)},    // RFC 5737 TEST-NET-3
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},       // RFC 1112 reserved, incl. 255.255.255.255
	{IP: net.ParseIP("fec0::"), Mask: net.CIDRMask(10, 128)},      // RFC 3879 deprecated site-local
	{IP: net.ParseIP("100::"), Mask: net.CIDRMask(64, 128)},       // RFC 6666 discard-only
	{IP: net.ParseIP("2001::"), Mask: net.CIDRMask(23, 128)},      // RFC 2928 IETF protocol assignments
	{IP: net.ParseIP("2001:db8::"), Mask: net.CIDRMask(32, 128)},  // RFC 3849 documentation
	{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)},      // RFC 7526 deprecated 6to4
	{IP: net.ParseIP("3fff::"), Mask: net.CIDRMask(20, 128)},      // RFC 9637 documentation
	{IP: net.ParseIP("5f00::"), Mask: net.CIDRMask(16, 128)},      // RFC 9602 SRv6 SIDs
	{IP: net.ParseIP("64:ff9b:1::"), Mask: net.CIDRMask(48, 128)}, // RFC 8215 local NAT64
}

// isForbiddenIP reports whether ip is an address this service must not connect to.
//
// This is a DENYLIST, and calling it default-deny would be a comfortable lie: a
// special-use range nobody enumerated stays reachable. A true default-deny needs a
// public-address policy or a destination allowlist, which is the right shape once the
// set of legitimate event hosts is known — until then the honest description of this
// guard is "every special-use range IANA has registered", enumerated above and pinned
// by TestIsForbiddenIP so a new one is added by editing a table, not by reasoning.
//
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

// fetchError renders a URL-FREE message while keeping its cause reachable, so a caller
// can still ask errors.Is(err, context.Canceled) after the text has been stripped.
//
// Unwrap returns both the sentinel and the cause (Go 1.20 multi-unwrap), which is what
// lets one value answer to ErrEventURLFetchFailed and to context.Canceled at once. The
// cause is UNEXPORTED on purpose: a *url.Error carries the full request URL — userinfo
// and every query value the caller supplied — in an EXPORTED field, so reflection-based
// logging or a JSON marshal of the error would print what Error() withheld.
type fetchError struct {
	sentinel error
	detail   string
	cause    error
}

func (e *fetchError) Error() string { return fmt.Sprintf("%v: %s", e.sentinel, e.detail) }

func (e *fetchError) Unwrap() []error { return []error{e.sentinel, e.cause} }

// safeCause maps a transport error onto a fixed vocabulary of URL-free descriptions.
//
// Peeling the *url.Error wrapper would NOT be enough. The URL appears in the wrapper's
// %v, but nothing stops a nested error from embedding it too, and the set of error types
// net/http can hand back is open. So this never echoes the cause's text: recognized
// outcomes get a string this package owns, and everything else collapses to a generic
// one. That default-deny is the whole point — an unrecognized error is the case where
// its text is least vouched for.
func safeCause(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Rebuilt from the boolean bits alone — dnsErr.Name is the caller's hostname.
		if dnsErr.IsNotFound {
			return "host not found"
		}
		return "dns failure"
	}
	return "transport failure"
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
				// The idle pool is bounded because the hostnames are CALLER-chosen.
				// http.Transport's zero values here are "unlimited" and "never expire",
				// so a stream of distinct event URLs would accumulate one permanent idle
				// connection per origin — a file-descriptor leak driven by request input.
				// These are http.DefaultTransport's numbers; the point is stating them,
				// not the values. MaxIdleConnsPerHost stays at its default of 2, which
				// already bounds a single origin.
				MaxIdleConns:    100,
				IdleConnTimeout: 90 * time.Second,
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
		// The cause is dropped, not formatted: url.Parse fails with a *url.Error whose
		// text repeats the whole input URL, and this error is rendered to the caller
		// and to logs. The same reasoning applies to every error below that could
		// carry the URL — see safeCause.
		return nil, fmt.Errorf("%w: malformed URL", ErrEventURLInvalid)
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
		return nil, fmt.Errorf("%w: invalid request", ErrEventURLInvalid)
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
		// The cause is CARRIED but not RENDERED. Keeping it reachable is required: a
		// cancelled or timed-out request must stay errors.Is-able as
		// context.Canceled/DeadlineExceeded, or a caller that distinguishes "we gave
		// up" from "the page did not answer" cannot. Rendering it is not — client.Do
		// fails with a *url.Error whose text is the caller's URL, userinfo and query
		// values included, and this error reaches logs.
		return nil, &fetchError{sentinel: ErrEventURLFetchFailed, detail: safeCause(err), cause: err}
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
		// Same treatment as the Do error above: a body read fails with whatever the
		// transport hands back, and http2 stream errors do quote the request.
		return nil, &fetchError{sentinel: ErrEventURLFetchFailed, detail: safeCause(err), cause: err}
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds size limit", ErrEventURLFetchFailed)
	}
	return body, nil
}
