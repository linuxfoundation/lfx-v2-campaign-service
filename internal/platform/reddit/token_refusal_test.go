// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestClassifyTokenRefusal pins the split added in LFXV2-2665: a non-2xx token-endpoint
// response that is neither a 5xx nor a 429 is not automatically Reddit evaluating the stored
// credential.
//
// Before the split every such status carried the credential-rejection sentinel, so a redirect
// that was deliberately not followed, a moved endpoint, and RFC 6749's three REQUEST-shaped
// error codes all told the operator "reddit rejected your stored credential" about a
// credential Reddit never looked at — sending them to replace a working one while the actual
// defect, in a request only this service composes, went unreported.
func TestClassifyTokenRefusal(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"invalid_grant is the revoked or expired refresh token", http.StatusBadRequest, `{"error":"invalid_grant"}`, ErrCredentialRejected},
		{"invalid_client is application credentials that no longer match", http.StatusUnauthorized, `{"error":"invalid_client"}`, ErrCredentialRejected},
		{"unauthorized_client is the application registration", http.StatusBadRequest, `{"error":"unauthorized_client"}`, ErrCredentialRejected},
		{"invalid_request describes the request, not the credential", http.StatusBadRequest, `{"error":"invalid_request"}`, ErrTokenRequestRejected},
		{"unsupported_grant_type describes the request, not the credential", http.StatusBadRequest, `{"error":"unsupported_grant_type"}`, ErrTokenRequestRejected},
		{"invalid_scope describes the request, not the credential", http.StatusBadRequest, `{"error":"invalid_scope"}`, ErrTokenRequestRejected},
		// Redirects are not followed, so a 3xx surfaces here as a status rather than being
		// chased; 404 and 405 mean the endpoint moved. No token endpoint answers a
		// well-formed refresh with any of them.
		{"a 302 is a redirect this client refused to follow", http.StatusFound, "", ErrTokenRequestRejected},
		{"a 404 means the token endpoint moved", http.StatusNotFound, "not found", ErrTokenRequestRejected},
		{"a 405 means the token endpoint moved", http.StatusMethodNotAllowed, "", ErrTokenRequestRejected},
		// The status GATES the body. An OAuth-shaped body can arrive with a status no token
		// endpoint answers a refresh with — from a proxy, a gateway error page, or whatever
		// now answers at the moved address — and reading it first would report a credential
		// the platform never evaluated as refused.
		{"a 302 carrying an OAuth body is still the request, not the credential", http.StatusFound, `{"error":"invalid_grant"}`, ErrTokenRequestRejected},
		{"a 404 carrying an OAuth body is still the request, not the credential", http.StatusNotFound, `{"error":"invalid_grant"}`, ErrTokenRequestRejected},
		{"a 405 carrying an OAuth body is still the request, not the credential", http.StatusMethodNotAllowed, `{"error":"invalid_client"}`, ErrTokenRequestRejected},
		// The conservative fallback. An unrecognised body on a status a token endpoint DOES
		// use to refuse a credential stays a credential verdict: promoting it would turn the
		// ordinary revoked-token case — the failure this endpoint exists to catch — into a
		// 500 that pages us instead of an answer the operator can act on.
		{"an unrecognised body on a 400 stays a credential verdict", http.StatusBadRequest, "<html>gateway</html>", ErrCredentialRejected},
		{"an empty body on a 401 stays a credential verdict", http.StatusUnauthorized, "", ErrCredentialRejected},
		{"a non-OAuth JSON body on a 403 stays a credential verdict", http.StatusForbidden, `{"message":"forbidden"}`, ErrCredentialRejected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTokenRefusal(tc.status, []byte(tc.body))
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyTokenRefusal(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestTokenRefusalSentinelsRouteToOppositeOutcomes pins that the two sentinels classifyTokenRefusal
// returns do not merely differ in text — they reach different arms of probeClass.
//
// ErrCredentialRejected must match ProbeCredentialRejected, the only arm whose verdict reaches the
// operator as a confirmed failed test. ErrTokenRequestRejected must match NEITHER predicate, which
// is what routes it to domain.ErrServiceDefect. The inconclusive half is the one that has to be
// asserted explicitly: ProbeInconclusive answers true for any error it does not recognise, so
// without its dedicated arm this sentinel would report the connection as OK with an advisory.
func TestTokenRefusalSentinelsRouteToOppositeOutcomes(t *testing.T) {
	credential := fmt.Errorf("%w: reddit token refresh -> %d", ErrCredentialRejected, http.StatusBadRequest)
	if !ProbeCredentialRejected(credential) {
		t.Error("a refused credential did not match ProbeCredentialRejected, so a revoked refresh " +
			"token would not be reported as a failed connection test")
	}

	request := fmt.Errorf("%w: reddit token refresh -> %d", ErrTokenRequestRejected, http.StatusNotFound)
	if ProbeCredentialRejected(request) {
		t.Error("a request this service built wrong matched ProbeCredentialRejected, so the " +
			"operator would be told to replace a credential the platform never evaluated")
	}
	if ProbeInconclusive(request) {
		t.Error("a request this service built wrong matched ProbeInconclusive, so probeClass " +
			"would report OK with an advisory instead of raising the service defect it is")
	}
}
