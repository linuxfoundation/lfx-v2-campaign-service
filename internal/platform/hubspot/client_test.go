// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCreds() Credentials {
	return Credentials{PrivateAppToken: "pat-test-token"}
}

func testAccount() AccountConfig {
	return AccountConfig{PortalID: "8112310"}
}

// newTestClient wires a client against an httptest server whose handler is
// supplied per-test, with the 429 backoff shrunk to near-zero.
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithBaseURL(srv.URL), withRetryBaseDelay(time.Millisecond))
	return c, srv
}

func TestDoRequest_HappyPathReturnsBodyAndSetsBearer(t *testing.T) {
	var gotAuth, gotAccept, gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	raw, err := c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/123", nil, true)
	if err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("body = %q", raw)
	}
	if gotAuth != "Bearer pat-test-token" {
		t.Errorf("Authorization = %q, want Bearer pat-test-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotPath != "/crm/v3/lists/123" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestDoRequest_MissingTokenFailsPreSend(t *testing.T) {
	c := NewClient(Credentials{}, testAccount(), WithBaseURL("http://127.0.0.1:1"))
	_, err := c.doRequest(context.Background(), http.MethodGet, "/x", nil, true)
	if err == nil || !strings.Contains(err.Error(), "missing private-app token") {
		t.Errorf("expected a missing-token error, got: %v", err)
	}
}

func TestDoRequest_AlreadyCancelledCtxIsPreSendNotUnconfirmed(t *testing.T) {
	// If the caller's context is already done BEFORE we send, nothing was sent. For a
	// MUTATING call this must be a clean PRE-SEND error (definitely-not-committed), NOT
	// an ambiguous transportError → UNCONFIRMED (which would wrongly tell the caller the
	// mutation MIGHT have landed and to verify-before-retry). Also: the server must
	// never be hit.
	var hits int32
	c, srv := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{}`)
	})
	_ = srv
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the call

	_, err := c.doRequest(ctx, http.MethodPost, "/crm/v3/lists", map[string]string{"x": "y"}, false)
	if err == nil {
		t.Fatal("a cancelled ctx must produce an error")
	}
	var pe *preSendError
	if !errors.As(err, &pe) {
		t.Fatalf("an already-cancelled ctx must be a preSendError (definitely not sent), got %T: %v", err, err)
	}
	if IsUnconfirmed(err) {
		t.Error("a pre-send (never-sent) mutating request must NOT be UNCONFIRMED")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the pre-send error must wrap the ctx cause for errors.Is, got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("the server must never be hit when ctx is already done, got %d hits", n)
	}
}

func TestNewClient_NormalizesTokenAndPortalID(t *testing.T) {
	// A whitespace-only token must be treated as missing (not sent as "Bearer   "),
	// and a padded portal id must be trimmed before it builds app URLs.
	cWs := NewClient(Credentials{PrivateAppToken: "   "}, testAccount(), WithBaseURL("http://127.0.0.1:1"))
	_, err := cWs.doRequest(context.Background(), http.MethodGet, "/x", nil, true)
	if err == nil || !strings.Contains(err.Error(), "missing private-app token") {
		t.Errorf("a whitespace-only token must be treated as missing, got: %v", err)
	}

	var gotAuth string
	_, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	})
	c2 := NewClient(Credentials{PrivateAppToken: "  pat-x  "}, AccountConfig{PortalID: "  8112310  "},
		WithBaseURL(srv.URL), withRetryBaseDelay(0))
	if _, err := c2.doRequest(context.Background(), http.MethodGet, "/x", nil, true); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotAuth != "Bearer pat-x" {
		t.Errorf("token must be trimmed in the Authorization header, got %q", gotAuth)
	}
	if c2.account.PortalID != "8112310" {
		t.Errorf("portal id must be trimmed, got %q", c2.account.PortalID)
	}
}

func TestDoRequest_JSONBodySetsContentType(t *testing.T) {
	var gotCT, gotBody string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{}`)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"name": "L"}, false)
	if err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody != `{"name":"L"}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestDoRequest_Non2xxIsApiErrorWithoutBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"SECRET-detail token=abc"}`)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"x": "y"}, false)
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apiError, got %T: %v", err, err)
	}
	if ae.StatusCode != 400 {
		t.Errorf("status = %d", ae.StatusCode)
	}
	// The body must NEVER be surfaced by Error() (it can quote request material).
	if strings.Contains(ae.Error(), "SECRET-detail") || strings.Contains(ae.Error(), "token=abc") {
		t.Errorf("apiError.Error() leaked the response body: %q", ae.Error())
	}
	if !strings.Contains(ae.Error(), "400") {
		t.Errorf("apiError.Error() should carry the status: %q", ae.Error())
	}
}

func TestDoRequest_ErrorPathStripsQueryString(t *testing.T) {
	// A paginated request path carries `?after=<cursor>`; the query (a cursor or any
	// future secret) must NOT leak into the error's rendered path.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	_, err := c.doRequest(context.Background(), http.MethodGet, "/marketing/v3/emails?limit=100&after=SECRETCURSOR", nil, true)
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apiError, got %T: %v", err, err)
	}
	if strings.Contains(ae.Path, "?") || strings.Contains(ae.Path, "SECRETCURSOR") {
		t.Errorf("apiError.Path must not carry the query string, got %q", ae.Path)
	}
	if strings.Contains(ae.Error(), "SECRETCURSOR") {
		t.Errorf("apiError.Error() leaked the cursor: %q", ae.Error())
	}
	if ae.Path != "/marketing/v3/emails" {
		t.Errorf("apiError.Path = %q, want the query-free path", ae.Path)
	}
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var followed bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			followed = true
			_, _ = io.WriteString(w, `{}`)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"x": "y"}, false)
	if err == nil {
		t.Fatal("expected a 3xx to surface as an error, got nil")
	}
	if followed {
		t.Error("client followed the redirect — it must hand the 3xx back instead")
	}
	// A 3xx on a MUTATING request is AMBIGUOUS (the target may have been created before
	// the redirect), so it must be UNCONFIRMED — assert the ambiguity, not just the error.
	if !IsUnconfirmed(err) {
		t.Errorf("a mutating 3xx must be UNCONFIRMED (it may have committed), got %T: %v", err, err)
	}
}

func TestDoRequest_ConnectionRefusedIsPreSendNotUnconfirmed(t *testing.T) {
	// A connection-refused dial failure is a DEFINITE pre-send error
	// (isPreSendDialError: dial + ECONNREFUSED) — the request never reached HubSpot, so
	// even a MUTATING call is NOT UNCONFIRMED (no mutation could have landed). Port 1 on
	// loopback refuses immediately.
	c := NewClient(testCreds(), testAccount(), WithBaseURL("http://127.0.0.1:1"), withRetryBaseDelay(0))
	_, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists", map[string]string{"x": "y"}, false)
	if err == nil {
		t.Fatal("expected a connection-refused dial failure to error")
	}
	var pe *preSendError
	if !errors.As(err, &pe) {
		t.Fatalf("a connection-refused dial failure must be a preSendError (definitely not sent), got %T: %v", err, err)
	}
	if IsUnconfirmed(err) {
		t.Errorf("a pre-send (dial-refused) mutating failure must NOT be UNCONFIRMED, got: %v", err)
	}
}

func TestDoRequest_PerAttemptTimeoutEnforcedWithZeroTimeoutClient(t *testing.T) {
	// A caller may inject an *http.Client whose Timeout is 0 (WithHTTPClient) AND pass
	// context.Background() (no deadline). Without a per-attempt context deadline, a
	// stalled HubSpot connection would hang indefinitely. This test proves the
	// per-attempt WithTimeout enforces the bound INDEPENDENTLY of the injected client:
	// the handler blocks until the test ends, the injected client has Timeout:0, the
	// caller ctx has no deadline, yet doRequest returns quickly.
	block := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-block // never respond within the per-attempt window
		_, _ = io.WriteString(w, `{}`)
	}))
	// Cleanup runs LIFO: unblock the stuck handler FIRST, THEN Close — otherwise
	// srv.Close() would block forever waiting on the handler still parked on <-block.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	c := NewClient(testCreds(), testAccount(),
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{}), // Timeout: 0 — must NOT be the thing that saves us
		withRequestTimeout(60*time.Millisecond),
		withRetryBaseDelay(0),
	)

	done := make(chan error, 1)
	go func() {
		// Idempotent GET so a mid-flight timeout is a retryable transportError, not a
		// mutating-ambiguous one; the point here is the bound, not the classification.
		_, err := c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/1", nil, true)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a per-attempt timeout error, got nil")
		}
		// The per-attempt deadline fires mid-flight → *url.Error wrapping
		// context.DeadlineExceeded, surfaced as a transportError (NOT a preSendError:
		// the request WAS sent). errors.Is reaches the deadline cause via Unwrap.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected the wrapped cause to be context.DeadlineExceeded, got: %v", err)
		}
		var te *transportError
		if !errors.As(err, &te) {
			t.Errorf("a mid-flight per-attempt timeout should be a transportError, got %T: %v", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doRequest did not return within 5s — the per-attempt timeout is NOT enforced " +
			"(a zero-Timeout injected client + background ctx hung indefinitely)")
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Error("the server was never reached — the request failed pre-send, not mid-flight")
	}
}

// A caller-supplied *http.Client that WOULD follow redirects must be force-overridden
// to no-follow WITHOUT mutating the caller's client.
func TestClient_OverridesInjectedCheckRedirectWithoutMutatingCaller(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			followed = true
			_, _ = io.WriteString(w, `{}`)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	defer srv.Close()

	// A NON-nil caller follow-policy: NewClient must override it (so the rebuilt
	// client never invokes it) AND leave it intact on the caller's own client.
	var callerPolicyInvoked bool
	callerPolicy := func(_ *http.Request, _ []*http.Request) error {
		callerPolicyInvoked = true
		return nil // would follow
	}
	caller := &http.Client{CheckRedirect: callerPolicy}
	c := NewClient(testCreds(), testAccount(), WithBaseURL(srv.URL), WithHTTPClient(caller))

	if _, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"x": "y"}, false); err == nil {
		t.Fatal("expected a 3xx to surface as an error with the injected client, got nil")
	}
	if followed || callerPolicyInvoked {
		t.Error("the rebuilt client used the caller's follow-policy — NewClient must override CheckRedirect")
	}
	// The caller's OWN client must be untouched: its non-nil policy still present.
	if caller.CheckRedirect == nil {
		t.Error("caller's *http.Client CheckRedirect was cleared — the override must build a fresh client, not mutate the caller")
	}
	if c.httpClient == caller {
		t.Error("NewClient must use a fresh client, not the caller's pointer")
	}
}

func TestDoRequest_Idempotent429RetriesThenSucceeds(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	raw, err := c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/1", nil, true)
	if err != nil {
		t.Fatalf("idempotent 429 should retry then succeed, got: %v", err)
	}
	if string(raw) != `{"ok":true}` || calls != 2 {
		t.Errorf("calls = %d, body = %q", calls, raw)
	}
}

func TestDoRequest_Mutating429IsNotRetried(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"x": "y"}, false)
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("mutating 429 must be an apiError (no retry), got %T: %v", err, err)
	}
	// A mutating 429 is AMBIGUOUS — the throttled request may already have committed —
	// so it must be UNCONFIRMED, not a clean definite failure. This is the load-bearing
	// contract (client.go sets Ambiguous: !idempotent); assert it so a regression to
	// Ambiguous=false is caught, not just the type/status.
	if !IsUnconfirmed(err) {
		t.Error("a mutating 429 must be UNCONFIRMED because it may have committed")
	}
	if calls != 1 {
		t.Errorf("mutating 429 must NOT be retried, got %d calls", calls)
	}
}

func TestDoRequest_429OverCapRetryAfterAborts(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "99999")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.doRequest(context.Background(), http.MethodGet, "/crm/v3/lists/1", nil, true)
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("an over-cap Retry-After must abort with the 429, got %T: %v", err, err)
	}
	if calls != 1 {
		t.Errorf("over-cap Retry-After must abort without sleeping/retrying, got %d calls", calls)
	}
}

func TestDoRequest_Mutating5xxIsUnconfirmed(t *testing.T) {
	// A mutating 5xx may have committed server-side → the apiError must be Ambiguous
	// and IsUnconfirmed(err) must be true, so callers verify instead of assuming a
	// clean failure. A definite 4xx must be the opposite.
	c5, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c5.doRequest(context.Background(), http.MethodPost, "/marketing/v3/emails/clone", map[string]string{"x": "y"}, false)
	if !IsUnconfirmed(err) {
		t.Errorf("a mutating 5xx must be UNCONFIRMED, got %T: %v", err, err)
	}
	c4, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	_, err = c4.doRequest(context.Background(), http.MethodPost, "/marketing/v3/emails/clone", map[string]string{"x": "y"}, false)
	if IsUnconfirmed(err) {
		t.Errorf("a definite 4xx must NOT be UNCONFIRMED (it cleanly did nothing): %v", err)
	}
}

func TestIsUnconfirmed_TransportErrorOnlyWhenMutating(t *testing.T) {
	// A transport failure on a MUTATING request is UNCONFIRMED (may have landed); on
	// an idempotent read it landed no mutation, so it's safely retryable and NOT
	// reported as unconfirmed.
	mut := &transportError{Method: http.MethodPost, Path: "/x", err: io.ErrUnexpectedEOF, Mutating: true}
	if !IsUnconfirmed(mut) {
		t.Error("a mutating transportError must be UNCONFIRMED")
	}
	read := &transportError{Method: http.MethodGet, Path: "/x", err: io.ErrUnexpectedEOF, Mutating: false}
	if IsUnconfirmed(read) {
		t.Error("an idempotent-read transportError must NOT be UNCONFIRMED (safely retryable)")
	}
}

func TestPreSendError_UnwrapsAndHidesURL(t *testing.T) {
	// The pre-send error must preserve the cause for errors.Is/As AND never leak the
	// request URL.
	secretURL := "https://api.hubapi.com/x?hapikey=SECRET"
	pe := &preSendError{
		Method: http.MethodPost, Path: "/x",
		err: &url.Error{Op: "Post", URL: secretURL, Err: &net.DNSError{Err: "no such host"}},
	}
	if IsUnconfirmed(pe) {
		t.Error("a pre-send error is a DEFINITE failure — not UNCONFIRMED")
	}
	var dnsErr *net.DNSError
	if !errors.As(pe, &dnsErr) {
		t.Error("pre-send error must Unwrap to the underlying dial cause for errors.As")
	}
	if strings.Contains(pe.Error(), "SECRET") || strings.Contains(pe.Error(), "hapikey") {
		t.Errorf("pre-send error leaked the request URL: %q", pe.Error())
	}
	// The cause is UNEXPORTED, so JSON/reflection serialization of the error (a
	// structured logger, error middleware) cannot walk into the nested *url.Error.URL
	// and leak the query/cursor — even though Error() already strips it.
	if b, _ := json.Marshal(pe); strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "hapikey") || strings.Contains(string(b), "hubapi.com") {
		t.Errorf("json.Marshal(preSendError) leaked the request URL: %s", b)
	}
}

func TestParseRetryAfter_SecondsAndHTTPDate(t *testing.T) {
	// Fixed "now" so an HTTP-date delay is deterministic.
	now := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	c := NewClient(testCreds(), testAccount(), withClock(func() time.Time { return now }))

	cases := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"seconds", "120", 120 * time.Second, true},
		{"http-date future", "Wed, 21 Oct 2026 07:28:00 GMT", 28 * time.Minute, true},
		{"http-date past", "Wed, 21 Oct 2026 06:00:00 GMT", 0, false},
		{"http-date now", "Wed, 21 Oct 2026 07:00:00 GMT", 0, false},
		{"empty", "", 0, false},
		{"garbage", "soon", 0, false},
		{"zero seconds", "0", 0, false},
		// A huge value must saturate to a positive over-cap duration, NOT overflow to
		// a non-positive one (which would bypass the over-cap abort).
		{"overflow-huge", "999999999999999999999999", time.Duration(overCapSeconds) * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := c.parseRetryAfter(tc.header)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tc.header, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestTransportError_DoesNotLeakURL(t *testing.T) {
	secretURL := "https://api.hubapi.com/crm/v3/lists/?hapikey=SECRET-abc123"
	te := &transportError{
		Method: http.MethodPost,
		Path:   "/crm/v3/lists/",
		err:    &url.Error{Op: "Post", URL: secretURL, Err: io.ErrUnexpectedEOF},
	}
	got := te.Error()
	if strings.Contains(got, "SECRET-abc123") || strings.Contains(got, secretURL) || strings.Contains(got, "hapikey=") {
		t.Errorf("transportError.Error() leaked the request URL: %q", got)
	}
	// safeCause maps io.ErrUnexpectedEOF (through the *url.Error wrapper) to a fixed
	// URL-free description rather than echoing the raw cause text.
	if !strings.Contains(got, "connection closed") {
		t.Errorf("transportError.Error() should surface a safe cause description: %q", got)
	}
	// JSON/reflection serialization must not leak the URL either (unexported cause).
	if b, _ := json.Marshal(te); strings.Contains(string(b), "SECRET-abc123") || strings.Contains(string(b), "hapikey") || strings.Contains(string(b), "hubapi.com") {
		t.Errorf("json.Marshal(transportError) leaked the request URL: %s", b)
	}
}

// leakyTransport is a custom RoundTripper whose error TEXT itself embeds the request
// URL — the exact vector a WithHTTPClient caller could introduce. safeCause must NOT
// echo this text.
type leakyTransport struct{}

func (leakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The inner error text carries the full URL incl. ?after=<cursor>. http.Client
	// wraps this in a *url.Error; peeling that wrapper still exposes this text.
	return nil, fmt.Errorf("request %s failed", req.URL.String())
}

func TestSafeCause_DoesNotEchoCustomTransportErrorText(t *testing.T) {
	// A custom transport (injectable via WithHTTPClient) can return an error whose text
	// embeds the URL. safeCause must default-deny: render a generic, URL-free message,
	// never the transport's arbitrary text.
	c := NewClient(testCreds(), testAccount(),
		WithBaseURL("https://api.hubapi.com"),
		WithHTTPClient(&http.Client{Transport: leakyTransport{}}),
		withRetryBaseDelay(0),
	)
	_, err := c.doRequest(context.Background(), http.MethodGet,
		"/marketing/v3/emails?limit=100&after=SECRETCURSOR", nil, true)
	if err == nil {
		t.Fatal("expected the leaky transport to surface an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "SECRETCURSOR") || strings.Contains(msg, "after=") || strings.Contains(msg, "hubapi.com") {
		t.Errorf("safeCause echoed the custom-transport error text, leaking the URL/cursor: %q", msg)
	}
	if !strings.Contains(msg, "transport failure") {
		t.Errorf("an unrecognized transport error should collapse to a generic description, got: %q", msg)
	}
}

func TestSafeCause_NamesKnownSafeCauses(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want string
	}{
		{"canceled", &url.Error{Op: "Get", URL: "https://api.hubapi.com/x?after=S", Err: context.Canceled}, "context canceled"},
		{"deadline", &url.Error{Op: "Get", URL: "https://api.hubapi.com/x?after=S", Err: context.DeadlineExceeded}, "context deadline exceeded"},
		{"eof", &url.Error{Op: "Get", URL: "https://api.hubapi.com/x?after=S", Err: io.ErrUnexpectedEOF}, "connection closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := safeCause(tc.in)
			if got != tc.want {
				t.Errorf("safeCause = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "after=") || strings.Contains(got, "hubapi.com") {
				t.Errorf("safeCause leaked the URL: %q", got)
			}
		})
	}
}

func TestParsePositiveInt(t *testing.T) {
	cases := map[string]int{"5": 5, " 12 ": 12, "0": 0}
	for in, want := range cases {
		if got, err := parsePositiveInt(in); err != nil || got != want {
			t.Errorf("parsePositiveInt(%q) = (%d,%v), want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-3", "1.5", "abc", "10s"} {
		if _, err := parsePositiveInt(bad); err == nil {
			t.Errorf("parsePositiveInt(%q) should error", bad)
		}
	}
}

func TestDoRequest_ResponseBodyCapBoundary(t *testing.T) {
	// The 10 MiB response-safety guard is load-bearing (bounds memory + retained
	// paging `after` cursor strings). Exercise the boundary: a body AT the limit succeeds, a
	// body at limit+1 is a transportError, and for a MUTATING call that oversized
	// body is UNCONFIRMED (the write may have committed).

	// AT the limit: read succeeds and returns the full body.
	atLimit := bytes.Repeat([]byte("a"), maxResponseBody)
	cOK, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(atLimit)
	})
	raw, err := cOK.doRequest(context.Background(), http.MethodGet, "/x", nil, true)
	if err != nil {
		t.Fatalf("a body exactly at the %d-byte cap must succeed, got %v", maxResponseBody, err)
	}
	if len(raw) != maxResponseBody {
		t.Errorf("read length = %d, want %d", len(raw), maxResponseBody)
	}

	// limit+1 on an IDEMPOTENT read: transportError, and NOT unconfirmed (a read
	// commits nothing, so it's safely retryable).
	overLimit := bytes.Repeat([]byte("a"), maxResponseBody+1)
	cOver, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(overLimit)
	})
	_, err = cOver.doRequest(context.Background(), http.MethodGet, "/x", nil, true)
	var te *transportError
	if !errors.As(err, &te) {
		t.Fatalf("a body over the cap must be a transportError, got %T: %v", err, err)
	}
	if IsUnconfirmed(err) {
		t.Error("an over-cap IDEMPOTENT read must NOT be UNCONFIRMED (nothing committed)")
	}

	// limit+1 on a MUTATING call: still a transportError, but UNCONFIRMED (the
	// mutation may have landed even though we couldn't accept the reply).
	cMut, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(overLimit)
	})
	_, err = cMut.doRequest(context.Background(), http.MethodPost, "/marketing/v3/emails/clone", map[string]string{"x": "y"}, false)
	if !IsUnconfirmed(err) {
		t.Errorf("an over-cap MUTATING call must be UNCONFIRMED, got %T: %v", err, err)
	}
}
