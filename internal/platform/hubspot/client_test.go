// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testCreds() Credentials {
	return Credentials{PrivateAppToken: "pat-test-token"}
}

func testAccount() AccountConfig {
	return AccountConfig{PortalID: "8112310", Label: "Test"}
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

	caller := &http.Client{} // CheckRedirect nil => would follow
	c := NewClient(testCreds(), testAccount(), WithBaseURL(srv.URL), WithHTTPClient(caller))

	if _, err := c.doRequest(context.Background(), http.MethodPost, "/crm/v3/lists/", map[string]string{"x": "y"}, false); err == nil {
		t.Fatal("expected a 3xx to surface as an error with the injected client, got nil")
	}
	if followed {
		t.Error("injected client followed the redirect — NewClient must override CheckRedirect")
	}
	if caller.CheckRedirect != nil {
		t.Error("caller's *http.Client CheckRedirect was mutated — the override must build a fresh client")
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
		t.Fatalf("mutating 429 must be a definite apiError (no retry), got %T: %v", err, err)
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
	// an idempotent read it landed no mutation, so it's safely retryable — not.
	mut := &transportError{Method: http.MethodPost, Path: "/x", Err: io.ErrUnexpectedEOF, Mutating: true}
	if !IsUnconfirmed(mut) {
		t.Error("a mutating transportError must be UNCONFIRMED")
	}
	read := &transportError{Method: http.MethodGet, Path: "/x", Err: io.ErrUnexpectedEOF, Mutating: false}
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
		Err: &url.Error{Op: "Post", URL: secretURL, Err: &net.DNSError{Err: "no such host"}},
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
		Err:    &url.Error{Op: "Post", URL: secretURL, Err: io.ErrUnexpectedEOF},
	}
	got := te.Error()
	if strings.Contains(got, "SECRET-abc123") || strings.Contains(got, secretURL) || strings.Contains(got, "hapikey=") {
		t.Errorf("transportError.Error() leaked the request URL: %q", got)
	}
	if !strings.Contains(got, io.ErrUnexpectedEOF.Error()) {
		t.Errorf("transportError.Error() should surface the underlying cause: %q", got)
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
