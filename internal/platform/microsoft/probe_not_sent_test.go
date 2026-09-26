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

// TestProbeNotSent_TokenEndpointDialFailure covers this client's OTHER leg, and it is a
// separate test rather than a case in the one above because the two legs fail through entirely
// different machinery.
//
// A fresh connection probe refreshes before it reads, so the token endpoint is the FIRST host
// this client dials and the first one that can be unreachable. A dial failure there comes back
// as a tokenTransportError, which renders only safeCause but whose Unwrap preserves the cause —
// so isPreSendDialError sees through it, while errRequestNotSent is never attached, because the
// REST request arm is what attaches that marker and the probe never reached it.
//
// Reading only the marker therefore answered false here, and Orchestrator.ProbeConnection booked
// an upstream-call sample against Microsoft for a probe that never left this deployment: an
// unresolvable token host inflated Microsoft's error rate on
// campaign_upstream_call_duration_seconds, which is the one thing this predicate exists to stop.
func TestProbeNotSent_TokenEndpointDialFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dial  error
		doesn string
	}{
		{
			name:  "token host does not resolve",
			dial:  &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "token.invalid", IsNotFound: true}},
			doesn: "a resolver failure on the token endpoint",
		},
		{
			name:  "token endpoint refuses the connection",
			dial:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			doesn: "a refused connection to the token endpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := rtFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Host, "token.invalid") {
					return nil, tc.dial
				}
				return http.DefaultTransport.RoundTrip(r)
			})

			c := NewClient(testCreds(), testAccount(),
				WithTokenURL("http://token.invalid/oauth2/v2.0/token"),
				WithBaseURL("http://ms.invalid"),
				WithHTTPClient(&http.Client{Transport: transport}),
				WithClock(fixedClock()))

			_, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true)
			if err == nil {
				t.Fatal("doRequest succeeded with a token endpoint that cannot be dialled")
			}
			if !ProbeNotSent(err) {
				t.Errorf("ProbeNotSent(%v) = false; %s proves nothing left this process, and without that fact the probe's metrics arm books an upstream call against Microsoft for something Microsoft never saw", err, tc.doesn)
			}
			if !ProbeInconclusive(err) {
				t.Errorf("ProbeInconclusive(%v) = false; a token request that was never sent taught us nothing about the credential", err)
			}
			if ProbeCredentialRejected(err) {
				t.Errorf("ProbeCredentialRejected(%v) = true; nothing evaluated the credential", err)
			}
			// The token request's body carried client_secret and refresh_token. Whatever this
			// predicate change did, it must not have put any of that on the error.
			for _, leaked := range []string{testCreds().ClientSecret, testCreds().RefreshToken} {
				if leaked != "" && strings.Contains(err.Error(), leaked) {
					t.Errorf("error %q carries token-request credential material", err)
				}
			}
		})
	}
}

// TestProbeNotSent_TokenEndpointMidFlightFailureIsNotClaimed keeps the token leg honest in the
// same direction its sibling keeps the request leg: a failure AFTER the bytes were written is
// ambiguous, so claiming it never-sent would delete a real Microsoft failure from the upstream
// series.
func TestProbeNotSent_TokenEndpointMidFlightFailureIsNotClaimed(t *testing.T) {
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "token.invalid") {
			// No dial op: the connection was established and the failure came later.
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		}
		return http.DefaultTransport.RoundTrip(r)
	})

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL("http://token.invalid/oauth2/v2.0/token"),
		WithBaseURL("http://ms.invalid"),
		WithHTTPClient(&http.Client{Transport: transport}),
		WithClock(fixedClock()))

	_, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true)
	if err == nil {
		t.Fatal("doRequest succeeded against a transport that always fails")
	}
	if ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = true for a mid-flight reset on the token endpoint; the token request may have been received, and dropping its sample hides a real Microsoft failure", err)
	}
	if !ProbeInconclusive(err) {
		t.Errorf("ProbeInconclusive(%v) = false; a token transport failure proves nothing about the credential", err)
	}
}
