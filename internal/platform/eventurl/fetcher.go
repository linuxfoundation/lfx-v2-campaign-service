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
//
// The IPv4-compatible form ::/96 is the same trick with a shorter prefix — ::a9fe:a9fe names
// 169.254.169.254 and To4 does NOT normalise it (To4 requires bytes 10-11 to be 0xffff, which
// this form leaves zero). RFC 4291 deprecated it, so it gets 6to4's treatment rather than the
// decoding below. The three /96s are disjoint despite looking alike: IPv4-compatible is twelve
// zero bytes, IPv4-mapped ::ffff:0:0/96 sets bytes 10-11, and IPv4-translated ::ffff:0:0:0/96
// sets bytes 8-9. Neither :: nor ::1 loses anything by the deny — both are already refused by
// IsUnspecified and IsLoopback.
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
	{IP: net.ParseIP("100:0:0:1::"), Mask: net.CIDRMask(64, 128)}, // RFC 9780 dummy prefix
	{IP: net.ParseIP("2001::"), Mask: net.CIDRMask(23, 128)},      // RFC 2928 IETF protocol assignments
	{IP: net.ParseIP("2001:db8::"), Mask: net.CIDRMask(32, 128)},  // RFC 3849 documentation
	{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)},      // RFC 7526 deprecated 6to4
	{IP: net.ParseIP("3fff::"), Mask: net.CIDRMask(20, 128)},      // RFC 9637 documentation
	{IP: net.ParseIP("5f00::"), Mask: net.CIDRMask(16, 128)},      // RFC 9602 SRv6 SIDs
	{IP: net.ParseIP("64:ff9b:1::"), Mask: net.CIDRMask(48, 128)}, // RFC 8215 local NAT64
	{IP: net.ParseIP("::"), Mask: net.CIDRMask(96, 128)},          // RFC 4291 deprecated IPv4-compatible
}

// ipv4EmbeddingNets are prefixes whose LOW 32 BITS are a literal IPv4 destination, to be
// decoded and re-tested rather than denied wholesale.
//
// They differ from 6to4 above in what a blanket deny would cost. 2002::/16 is deprecated, so
// refusing all of it loses nothing reachable. These two are not: on a NAT64/SIIT network
// 64:ff9b::/96 is how a v6-only host reaches the ORDINARY IPv4 internet, so denying the prefix
// would refuse every legitimate IPv4 event host at once. Decoding is therefore the only option
// that is both safe and correct — and it is needed, because net.IP.To4 normalises exactly one
// embedding (::ffff:0:0/96, IPv4-mapped) and neither of these. 64:ff9b::a9fe:a9fe survives
// every predicate and every IPv4-shaped range test above while naming 169.254.169.254.
var ipv4EmbeddingNets = []net.IPNet{
	{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)},    // RFC 6052 well-known NAT64 prefix
	{IP: net.ParseIP("::ffff:0:0:0"), Mask: net.CIDRMask(96, 128)}, // RFC 2765 IPv4-translated
}

// rfc6052PrefixLens are the only prefix lengths RFC 6052 §2.2 defines. A NAT64 prefix of any
// other length is not a valid translation prefix and is rejected at configuration time.
var rfc6052PrefixLens = map[int]bool{32: true, 40: true, 48: true, 56: true, 64: true, 96: true}

// embeddedIPv4 extracts the IPv4 destination an RFC 6052 §2.2 address carries, for a prefix of
// the given length. It is NOT simply "the low 32 bits": only the /96 layout puts the address
// there. The shorter layouts split it around bits 64-71, the octet the RFC reserves.
//
// NOTHING the RFC merely requires to be zero is checked — not the reserved octet, not the
// trailing suffix bits. This is a security guard, and every such check is a way to FAIL OPEN:
// an address that violates the MUST would decode to nil, and nil means "no embedded address
// here", which means the dial proceeds. An attacker who wants to reach 169.254.169.254 through
// a translated prefix would simply set the reserved octet nonzero. Whether a translator then
// honours the address is the translator's business — this guard must not bet on it.
//
// Nor is the octet needed to identify the layout, because this function is only ever called
// after an address matched a prefix whose length is KNOWN, from configuration or from the
// well-known /96. The length is given, not inferred, so there is nothing to self-describe and
// no over-rejection to trade against: within a matched translation prefix, every address is
// bound for a translator by construction.
//
//	/32  bytes 4-7           /40  bytes 5-7 + 9      /48  bytes 6-7 + 9-10
//	/56  byte 7 + 9-11       /64  bytes 9-12         /96  bytes 12-15
//
// Returns nil only when ip is not a 16-byte address or prefixLen is not an RFC 6052 length.
func embeddedIPv4(ip net.IP, prefixLen int) net.IP {
	if len(ip) != net.IPv6len || !rfc6052PrefixLens[prefixLen] {
		return nil
	}
	start := prefixLen / 8
	out := make(net.IP, 0, net.IPv4len)
	for i := start; len(out) < net.IPv4len; i++ {
		if i == 8 {
			continue // the reserved octet is not part of the address
		}
		out = append(out, ip[i])
	}
	return net.IPv4(out[0], out[1], out[2], out[3])
}

// nat64Prefix is a configured RFC 6052 translation prefix: the network AND the length, because
// the length alone determines where in the address the IPv4 destination sits.
type nat64Prefix struct {
	net    net.IPNet
	length int
}

// wellKnownNAT64 is the prefix RFC 6052 §2.1 assigns for general use, and the only one that can
// be known without configuration. Every Fetcher starts with it.
var wellKnownNAT64 = nat64Prefix{
	net:    net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)},
	length: 96,
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
// To4 covers only that one embedding; ipv4EmbeddingNets handles the two it does not.
func isForbiddenIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// An IPv4-embedding address is judged by the IPv4 it names. The recursion terminates:
	// the value handed back is 4 bytes after To4, so it cannot match a /96 v6 prefix.
	if len(ip) == net.IPv6len {
		for i := range ipv4EmbeddingNets {
			if ipv4EmbeddingNets[i].Contains(ip) {
				return isForbiddenIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
			}
		}
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
// Unwrap returns both the sentinel and the identity (Go 1.20 multi-unwrap), which is what
// lets one value answer to ErrEventURLFetchFailed and to context.Canceled at once.
//
// identity is NOT the transport's error. Keeping the original here — even unexported —
// would leak: Unwrap hands it to anyone walking the chain, so errors.As(err, &urlErr)
// recovers the *url.Error and its EXPORTED URL field, userinfo and query included, and
// chain-walking telemetry or middleware would print exactly what Error() withheld. An
// unexported field is not a boundary when the type publishes an Unwrap.
//
// So the chain carries only values THIS package can vouch for: canonical sentinels from
// safeIdentity, never anything net/http constructed. Default-deny — an unrecognized cause
// contributes nothing at all, because it is precisely the case whose contents are least
// vouched for.
type fetchError struct {
	sentinel error
	detail   string
	identity error
}

func (e *fetchError) Error() string { return fmt.Sprintf("%v: %s", e.sentinel, e.detail) }

func (e *fetchError) Unwrap() []error {
	if e.identity == nil {
		return []error{e.sentinel}
	}
	return []error{e.sentinel, e.identity}
}

// safeIdentity maps a transport error onto the canonical sentinels a caller legitimately
// branches on, and nothing else. It is the errors.Is half of what safeCause does for the
// message: same recognized set, same default-deny, but returning a value the chain may
// safely expose rather than a string.
//
// Only the context sentinels and io's EOFs qualify: they are package-level singletons that
// carry no fields, so exposing one reveals nothing about the request. A *net.DNSError or a
// *url.Error would have to be REBUILT to be safe (their Name/URL fields are the caller's
// input), and no caller needs to branch on those — safeCause already describes them in the
// message.
func safeIdentity(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.ErrUnexpectedEOF):
		return io.ErrUnexpectedEOF
	case errors.Is(err, io.EOF):
		return io.EOF
	}
	return nil
}

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

// Option configures a Fetcher at construction. There is no setter: the guard must be fixed
// before the client can dial, so configuration that arrives later would be configuration that
// arrives too late.
type Option func(*fetcherConfig)

type fetcherConfig struct {
	nat64 []nat64Prefix
}

// WithNAT64Prefixes declares the deployment's NETWORK-SPECIFIC RFC 6052 translation prefixes,
// in addition to the well-known 64:ff9b::/96 that is always applied.
//
// This cannot be discovered in-process and must not be guessed. A network-specific prefix is
// ordinary global unicast space the operator was assigned, so it is indistinguishable from any
// other public prefix by inspection — and speculatively decoding every address at all six RFC
// 6052 layouts would over-reject: roughly one global address in 256 has a zero octet at bits
// 64-71 and bytes that read as 10.0.0.0/8 at the /64 layout, and refusing a legitimate event
// page is a real cost, not a conservative default.
//
// On a cluster that HAS such a prefix, an unlisted one is a live SSRF hole: the translator, not
// this process, makes the IPv4 connection, so an encoded 169.254.169.254 passes every check
// here. Where the prefix cannot be enumerated, the destination policy belongs at an egress
// boundary instead — this option is the in-process half, not a substitute for it.
//
// Each cidr must be a valid RFC 6052 length (/32, /40, /48, /56, /64, /96); anything else is a
// misconfiguration that would silently decode at the wrong offset. It panics rather than
// returning an error because the only caller is the composition root: a NAT64 prefix typed
// wrong is a deployment that must not start, not a request that fails.
func WithNAT64Prefixes(cidrs ...string) Option {
	return func(c *fetcherConfig) {
		for _, cidr := range cidrs {
			_, n, err := net.ParseCIDR(cidr)
			if err != nil || n.IP.To4() != nil {
				panic(fmt.Sprintf("eventurl: %q is not an IPv6 CIDR", cidr))
			}
			ones, _ := n.Mask.Size()
			if !rfc6052PrefixLens[ones] {
				panic(fmt.Sprintf("eventurl: /%d is not an RFC 6052 prefix length (32, 40, 48, 56, 64, 96)", ones))
			}
			c.nat64 = append(c.nat64, nat64Prefix{net: *n, length: ones})
		}
	}
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
func guardDialAddress(nat64 []nat64Prefix) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
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
		// Configured translation prefixes are judged by the IPv4 they name, exactly as the
		// well-known one is inside isForbiddenIP. Kept out of that function because the set
		// is per-Fetcher: it is deployment configuration, not a property of the IANA registry.
		for _, p := range nat64 {
			if !p.net.Contains(ip) {
				continue
			}
			v4 := embeddedIPv4(ip, p.length)
			if v4 != nil && isForbiddenIP(v4) {
				return fmt.Errorf("%w: %s names %s through a nat64 prefix", ErrEventURLForbidden, ip, v4)
			}
		}
		return nil
	}
}

// NewFetcher constructs a Fetcher with the standard SSRF-safe defaults. It is the only
// constructor in non-test code, so no production path can obtain an unguarded fetcher.
func NewFetcher(opts ...Option) *Fetcher {
	cfg := fetcherConfig{nat64: []nat64Prefix{wellKnownNAT64}}
	for _, opt := range opts {
		opt(&cfg)
	}
	dialer := &net.Dialer{Timeout: connectTimeout, Control: guardDialAddress(cfg.nat64)}
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
		// The scheme is NOT echoed, even though it looks like the one harmless piece of
		// the URL to quote. A scheme is ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ), so
		// "s3cr3t-token://host" parses with the token as a perfectly valid scheme and
		// the invariant here is that no part of the caller's input reaches the message.
		return nil, fmt.Errorf("%w: scheme is not http or https", ErrEventURLInvalid)
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
		// Neither the cause nor its text survives. A cancelled or timed-out request must
		// stay errors.Is-able as context.Canceled/DeadlineExceeded — a caller that
		// distinguishes "we gave up" from "the page did not answer" needs that — but the
		// value satisfying it is the canonical sentinel, not what client.Do returned. Do
		// fails with a *url.Error whose text AND exported URL field are the caller's URL,
		// userinfo and query values included, and this error reaches logs.
		return nil, &fetchError{sentinel: ErrEventURLFetchFailed, detail: safeCause(err), identity: safeIdentity(err)}
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
		return nil, &fetchError{sentinel: ErrEventURLFetchFailed, detail: safeCause(err), identity: safeIdentity(err)}
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds size limit", ErrEventURLFetchFailed)
	}
	return body, nil
}
