// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		// 6to4 (RFC 7526). The relay anycast address is ordinary IPv4, but the 2002::/16
		// entries are the sharp ones: bits 16-47 ARE an IPv4 address, so each of these is
		// a spelling of a host every IPv4-shaped test above already refuses, in a notation
		// none of them inspects. 2002:7f00:1:: is 127.0.0.1; 2002:a9fe:a9fe:: is the cloud
		// metadata address; 2002:0a00:0001:: is 10.0.0.1.
		"192.88.99.1",
		"2002:7f00:1::", "2002:a9fe:a9fe::", "2002:0a00:0001::",
		// The 4-in-6 spellings of two of the above: same host, different notation.
		"::ffff:127.0.0.1", "::ffff:169.254.169.254",
		// The two embeddings To4 does NOT normalise, decoded and re-tested instead of
		// denied wholesale (see ipv4EmbeddingNets). 64:ff9b::a9fe:a9fe is the cloud
		// metadata address on any NAT64 network; ::ffff:0:7f00:1 is 127.0.0.1.
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "64:ff9b::a00:1",
		"::ffff:0:a9fe:a9fe", "::ffff:0:7f00:1",
		// IPv4-compatible ::/96 (RFC 4291, deprecated) embeds IPv4 in the low 32 bits too,
		// and To4 leaves it alone because bytes 10-11 are zero rather than 0xffff. Denied
		// as a whole prefix, like 6to4 — hence ::5db8:d822, a PUBLIC address in that form,
		// is refused here and deliberately absent from the allowed list below.
		"::a9fe:a9fe", "::7f00:1", "::5db8:d822",
		// RFC 9780's dummy prefix sits outside the RFC 6666 discard block above and no
		// predicate rejects it.
		"100:0:0:1::1",
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
	// Over-rejection costs a reachable page: Fetch fails with ErrEventURLForbidden, which
	// the endpoint answers 400, so a legitimate public event URL is refused outright rather
	// than degrading to "no metadata found".
	// The last two are the reason the embedding prefixes are DECODED rather than denied:
	// on a NAT64 network they are how a v6-only host reaches an ordinary public IPv4 host,
	// so denying 64:ff9b::/96 outright would refuse every legitimate IPv4 event host.
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:2800:220:1::1",
		"64:ff9b::5db8:d822", "::ffff:0:808:808"} {
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

// TestFetchErrorsDoNotLeakTheURL pins the redaction. The URL a caller hands this service
// is not a public identifier: it can carry userinfo and query values that are the whole
// credential (a signed preview link, a one-time token). The transport's own error text
// repeats it verbatim — *url.Error's Error() is "Get \"<the URL>\": <cause>" — and this
// error is rendered into the API response and the logs.
//
// The assertion is by SUBSTRING on the secrets rather than by equality on the message,
// because the leak can appear at any layer: what must hold is that no part of the input
// survives. Reverting either fmt.Errorf back to formatting the cause makes this fail.
func TestFetchErrorsDoNotLeakTheURL(t *testing.T) {
	const (
		user   = "s3cr3t-user"
		pass   = "s3cr3t-pass"
		token  = "s3cr3t-token"
		secret = "s3cr3t"
	)
	// Port 9 (discard) with nothing listening: Do fails at connect, so the *url.Error
	// carrying the URL is produced without a server and without DNS.
	target := "http://" + user + ":" + pass + "@127.0.0.1:9/e?token=" + token

	// Through the unguarded client the failure is a transport error; through NewFetcher
	// the same URL is refused at dial. Both paths render an error, so both are checked.
	for name, f := range map[string]*Fetcher{"transport": newUnguardedFetcher(), "guarded": NewFetcher()} {
		_, err := f.Fetch(context.Background(), target)
		if err == nil {
			t.Fatalf("%s: Fetch(unreachable) error = nil, want an error", name)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: error leaked the caller's URL: %v", name, err)
		}
	}

	// A malformed URL takes the url.Parse branch, whose *url.Error also repeats the input.
	if _, err := NewFetcher().Fetch(context.Background(), "http://exa mple.com/?token="+token); err == nil {
		t.Error("Fetch(malformed) error = nil, want an error")
	} else if strings.Contains(err.Error(), secret) {
		t.Errorf("parse error leaked the caller's URL: %v", err)
	}

	// The scheme branch is the least obvious leak, because quoting the scheme looks
	// harmless: a scheme is ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ), so the whole
	// secret parses as one.
	if _, err := NewFetcher().Fetch(context.Background(), secret+"-scheme://example.com/"); err == nil {
		t.Error("Fetch(bad scheme) error = nil, want an error")
	} else if strings.Contains(err.Error(), secret) {
		t.Errorf("scheme error leaked the caller's input: %v", err)
	}
}

// TestSafeCauseNeverEchoesUnknownText is the default-deny half of the redaction: the set
// of error types net/http can return is open, so an unrecognized cause must collapse to a
// fixed string rather than have its text rendered. A mapping that fell back to err.Error()
// would pass every case above (none of those causes quote the URL) and still leak here.
func TestSafeCauseNeverEchoesUnknownText(t *testing.T) {
	if got := safeCause(errors.New("Bearer s3cr3t: dial https://host/?token=s3cr3t")); got != "transport failure" {
		t.Errorf("safeCause(unknown) = %q, want %q", got, "transport failure")
	}
	// A DNS failure is named, but rebuilt from the boolean bits — net.DNSError.Name is
	// the caller's hostname and must not be echoed either.
	got := safeCause(&net.DNSError{Err: "no such host", Name: "secret-preview.example.com", IsNotFound: true})
	if got != "host not found" {
		t.Errorf("safeCause(DNSError) = %q, want %q", got, "host not found")
	}
}

// TestFetchErrorsDoNotExposeTheTransportErrorThroughUnwrap is the other half of the
// redaction, and the half a message-only assertion cannot reach. fetchError publishes an
// Unwrap, so every value in its chain is recoverable by anything doing errors.As — logging
// middleware, telemetry, a generic error renderer. A *url.Error recovered that way hands
// over its EXPORTED URL field: the caller's userinfo and query, i.e. exactly the secret the
// message strips. Restoring `cause: err` at either construction site makes this fail.
func TestFetchErrorsDoNotExposeTheTransportErrorThroughUnwrap(t *testing.T) {
	const secret = "s3cr3t"
	target := "http://" + secret + ":" + secret + "@127.0.0.1:9/e?token=" + secret

	_, err := newUnguardedFetcher().Fetch(context.Background(), target)
	if err == nil {
		t.Fatal("Fetch(unreachable) error = nil, want an error")
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Fatalf("the *url.Error is reachable through the chain, carrying URL %q", urlErr.URL)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		t.Fatalf("the transport error is reachable through the chain: %v", opErr)
	}
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Errorf("err = %v, want it to stay errors.Is-able as ErrEventURLFetchFailed", err)
	}
}

// leakyBody fails its Read with a secret-bearing *url.Error. http2 stream errors really do
// quote the request, so this is the shape the body-read branch has to survive — not a
// hypothetical.
type leakyBody struct{ readErr error }

func (b leakyBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b leakyBody) Close() error             { return nil }

// leakyTransport answers 200 with a body that fails on read, so the failure lands PAST the
// client.Do branch the other redaction test covers.
type leakyTransport struct{ err error }

func (t leakyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: leakyBody{readErr: t.err}, Header: http.Header{}}, nil
}

// TestBodyReadErrorsAreRedactedToo covers the SECOND construction site. The Do-path test
// would stay green if only this branch regressed to carrying the raw error, so redaction
// tested at one site proves nothing about the other — the leak is per-branch, and so is the
// test. Both assertions matter: the message must not quote the URL, and errors.As must not
// be able to walk back to the *url.Error the message was derived from.
func TestBodyReadErrorsAreRedactedToo(t *testing.T) {
	const secret = "s3cr3t"
	leak := &url.Error{Op: "Get", URL: "http://u:" + secret + "@example.test/e?token=" + secret, Err: errors.New("stream error")}
	f := &Fetcher{client: &http.Client{Transport: leakyTransport{err: leak}}}

	_, err := f.Fetch(context.Background(), "http://example.test/e")
	if err == nil {
		t.Fatal("Fetch with a failing body read returned no error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the rendered error quotes the secret: %v", err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Fatalf("the *url.Error is reachable through the chain, carrying URL %q", urlErr.URL)
	}
	if !errors.Is(err, ErrEventURLFetchFailed) {
		t.Errorf("err = %v, want it to stay errors.Is-able as ErrEventURLFetchFailed", err)
	}
}

// TestSafeIdentityKeepsOnlyCanonicalSentinels: the identity exists so a caller can tell "we
// gave up" from "the page did not answer", which needs exactly the context sentinels — and
// the value satisfying errors.Is must be the canonical singleton, not the wrapper that
// happened to contain it. Anything unrecognized contributes nothing, the same default-deny
// safeCause applies to the message.
func TestSafeIdentityKeepsOnlyCanonicalSentinels(t *testing.T) {
	wrapped := &url.Error{Op: "Get", URL: "http://u:p@host/?token=s3cr3t", Err: context.DeadlineExceeded}
	got := safeIdentity(wrapped)
	if got != context.DeadlineExceeded {
		t.Fatalf("safeIdentity(wrapped deadline) = %#v, want the canonical context.DeadlineExceeded", got)
	}
	if safeIdentity(&url.Error{Op: "Get", URL: "http://u:p@host/", Err: errors.New("boom")}) != nil {
		t.Error("safeIdentity kept an unrecognized cause; it must contribute nothing to the chain")
	}
	if safeIdentity(context.Canceled) != context.Canceled {
		t.Error("safeIdentity dropped context.Canceled")
	}
}

// TestEmbeddedIPv4DecodesEveryRFC6052Layout: the IPv4 destination is at a DIFFERENT offset for
// every prefix length, and only /96 puts it in the low 32 bits. A decoder that assumed the /96
// layout would read four unrelated bytes for the other five and answer "not forbidden" about an
// address naming 169.254.169.254 — the exact failure this exists to prevent. Each case below
// encodes 169.254.169.254 (a9 fe a9 fe) at its length's layout.
func TestEmbeddedIPv4DecodesEveryRFC6052Layout(t *testing.T) {
	want := net.IPv4(169, 254, 169, 254)
	cases := map[int]string{
		32: "2001:db8:a9fe:a9fe::",
		40: "2001:db8:aa9:fea9:00fe::",
		48: "2001:db8:0:a9fe:a9:fe00::",
		56: "2001:db8:0:a9:fe:a9fe::",
		64: "2001:db8:a9fe:a9fe:00a9:fea9:fe00::",
		96: "2001:db8::a9fe:a9fe",
	}
	for length, s := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("/%d: fixture %q does not parse", length, s)
		}
		got := embeddedIPv4(ip, length)
		if !got.Equal(want) {
			t.Errorf("embeddedIPv4(%s, /%d) = %v, want %v", s, length, got, want)
		}
	}

	// A nonzero reserved octet must STILL decode. RFC 6052 requires bits 64-71 to be zero,
	// but refusing to decode when they are not would fail OPEN: nil means "no embedded
	// address", which means the dial proceeds. An attacker reaching for 169.254.169.254
	// through a translated prefix would just set this octet. The length is known from the
	// matched prefix, so the octet identifies nothing and buys nothing.
	if got := embeddedIPv4(net.ParseIP("2001:db8:a9fe:a9fe:01a9:fea9:fe00::"), 64); !got.Equal(want) {
		t.Errorf("embeddedIPv4 with a nonzero reserved octet = %v, want %v — a MUST-violating address must not fail open", got, want)
	}
	// A length RFC 6052 does not define must never be guessed at.
	if got := embeddedIPv4(net.ParseIP("2001:db8::a9fe:a9fe"), 80); got != nil {
		t.Errorf("embeddedIPv4 decoded at an undefined prefix length: %v", got)
	}
}

// TestConfiguredNAT64PrefixIsGuarded: on a cluster with a network-specific translation prefix,
// the TRANSLATOR makes the IPv4 connection, so an encoded 169.254.169.254 reaches the metadata
// service while every check in isForbiddenIP sees an ordinary global IPv6 address. The prefix
// cannot be discovered in-process — it is the operator's own global unicast space — so it is
// configured, and the guard has to decode it.
//
// The third assertion is the half that makes this a fix rather than a blanket deny: an address
// in the SAME configured prefix naming a public IPv4 must still be allowed through the guard.
func TestConfiguredNAT64PrefixIsGuarded(t *testing.T) {
	// A globally routable prefix, not 2001:db8::/32: the documentation range is already in
	// forbiddenNets, so using it would let this test pass on the wrong rejection.
	guard := guardDialAddress([]nat64Prefix{{
		net:    net.IPNet{IP: net.ParseIP("2a01:4f8:1::"), Mask: net.CIDRMask(48, 128)},
		length: 48,
	}})

	// 2a01:4f8:1:: + a9fe/a9fe at the /48 layout (bytes 6-7, reserved octet, bytes 9-10).
	if err := guard("tcp6", "[2a01:4f8:1:a9fe:a9:fe00::]:443", nil); err == nil {
		t.Error("a configured nat64 prefix naming 169.254.169.254 was allowed")
	} else if !errors.Is(err, ErrEventURLForbidden) {
		t.Errorf("err = %v, want ErrEventURLForbidden", err)
	}

	// Unconfigured, the same address is invisible: this is what makes the option load-bearing
	// rather than decorative, and what the knowledge doc records as the residual risk.
	if err := guardDialAddress(nil)("tcp6", "[2a01:4f8:1:a9fe:a9:fe00::]:443", nil); err == nil {
		t.Log("unconfigured: the encoded address is not detectable in-process, as documented")
	}

	// 8.8.8.8 (08 08 08 08) at the same layout: public, and must stay reachable.
	if err := guard("tcp6", "[2a01:4f8:1:808:8:800::]:443", nil); err != nil {
		t.Errorf("a configured prefix naming a public IPv4 was refused: %v", err)
	}

	// The same metadata address with the RESERVED octet set to 01 instead of 00. RFC 6052
	// says that octet MUST be zero, and the temptation is to treat a violation as "not an
	// embedding" — but this guard only ever decides between "refuse" and "dial", so a
	// refusal to decode IS a dial. That makes the reserved octet a one-byte bypass of the
	// entire NAT64 check, reachable by anyone who can name a host.
	if err := guard("tcp6", "[2a01:4f8:1:a9fe:1a9:fe00::]:443", nil); err == nil {
		t.Error("a nonzero reserved octet bypassed the nat64 guard — the check fails open")
	} else if !errors.Is(err, ErrEventURLForbidden) {
		t.Errorf("err = %v, want ErrEventURLForbidden", err)
	}
}
