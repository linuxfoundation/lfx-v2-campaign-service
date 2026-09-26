// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// TestProbeNotSent_RealDialFailureThroughTheClient is this package's share of the provenance
// coverage internal/dispatch carries for its five siblings.
//
// Those five can be driven from outside their packages, because they return the dial error
// itself and isPreSendDialError still sees through it. This client cannot: its pre-send arm
// renders the cause through safeCause into a plain string so a custom RoundTripper's error text
// can never reach a persisted campaign step, which also erases the *net.OpError a classifier
// would match on. The fact therefore travels on an unexported marker, and the only honest way to
// pin it is to make a real request fail on the real code path — a stub returning the marker
// would test nothing but the stub.
//
// The token endpoint is a live test server here on purpose: a client that cannot get a token
// fails in the token layer and never reaches the request arm under test.
func TestProbeNotSent_RealDialFailureThroughTheClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dial  error
		doesn string
	}{
		{
			name:  "host does not resolve",
			dial:  &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "ms.invalid", IsNotFound: true}},
			doesn: "a resolver failure",
		},
		{
			name:  "connection refused",
			dial:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			doesn: "a refused connection",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
			t.Cleanup(tok.Close)

			transport := rtFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Host, "ms.invalid") {
					return nil, tc.dial
				}
				return http.DefaultTransport.RoundTrip(r)
			})

			c := NewClient(testCreds(), testAccount(),
				WithTokenURL(tok.URL), WithBaseURL("http://ms.invalid"),
				WithHTTPClient(&http.Client{Transport: transport}),
				WithClock(fixedClock()))

			_, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true)
			if err == nil {
				t.Fatal("doRequest succeeded against a host that cannot be dialled")
			}
			if !ProbeNotSent(err) {
				t.Errorf("ProbeNotSent(%v) = false; %s proves the request never left this process, and without that fact the probe's metrics arm books an upstream call against Microsoft for something Microsoft never saw", err, tc.doesn)
			}
			if !ProbeInconclusive(err) {
				t.Errorf("ProbeInconclusive(%v) = false; a request that was never sent taught us nothing about the credential", err)
			}
			if ProbeCredentialRejected(err) {
				t.Errorf("ProbeCredentialRejected(%v) = true; nothing evaluated the credential", err)
			}
			// The marker must not have reopened the leak the rendering exists to close.
			if strings.Contains(err.Error(), "ms.invalid/") {
				t.Errorf("error %q carries the request URL", err)
			}
		})
	}
}

// TestProbeNotSent_MidFlightFailureIsNotClaimed is the other half, and the one that keeps the
// predicate honest.
//
// A failure after the bytes were written is AMBIGUOUS — Microsoft may well have received and
// processed the request — so claiming it as never-sent would delete a real platform failure from
// the upstream series, which is the expensive direction to be wrong in.
func TestProbeNotSent_MidFlightFailureIsNotClaimed(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tok.Close)

	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "ms.invalid") {
			// No dial op: the connection was made and the failure came later.
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		}
		return http.DefaultTransport.RoundTrip(r)
	})

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tok.URL), WithBaseURL("http://ms.invalid"),
		WithHTTPClient(&http.Client{Transport: transport}),
		WithClock(fixedClock()))

	_, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true)
	if err == nil {
		t.Fatal("doRequest succeeded against a transport that always fails")
	}
	if ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = true for a mid-flight reset; the request may have been received, and dropping its sample hides a real Microsoft failure", err)
	}
	if !ProbeInconclusive(err) {
		t.Errorf("ProbeInconclusive(%v) = false; a transport failure proves nothing about the credential", err)
	}
}
