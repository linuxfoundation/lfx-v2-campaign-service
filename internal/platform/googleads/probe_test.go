// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"errors"
	"fmt"
	"testing"
)

// TestProbePredicates pins the classification the connection test depends on (LFXV2-2665).
//
// The case that matters most is the token-endpoint 4xx. That is the failure this service has
// already hit in production — a revoked refresh token — and before the split in fetchToken it
// was indistinguishable from a transient outage, so it fell to ProbeInconclusive's default and
// the connection test answered OK: true for a permanently broken connection.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
	}{
		{
			name:         "token endpoint refused the refresh (revoked token)",
			err:          fmt.Errorf("%w: google ads token refresh -> 400", ErrTokenRequestRejected),
			wantRejected: true,
			// Deliberately ALSO true: ProbeInconclusive's default is true, and this case is
			// exactly why the dispatcher consults the rejection predicate FIRST. If the
			// evaluation order in probeSubject.probeClass ever flips, this connection is
			// reported healthy again.
			wantInconclusiv: true,
		},
		{
			name:            "token endpoint unavailable (5xx)",
			err:             fmt.Errorf("%w: google ads token refresh -> 503", errTokenEndpointUnavailable),
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "api 401",
			err:             &apiError{StatusCode: 401, Method: "POST", Path: "/customers:listAccessibleCustomers"},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "api 403",
			err:             &apiError{StatusCode: 403},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			// A rate limit is the platform declining to answer, not answering. Calling it a
			// rejection would report a working connection as broken every time Google throttles.
			name:            "api 429",
			err:             &apiError{StatusCode: 429},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "api 500",
			err:             &apiError{StatusCode: 500},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// Neither predicate: Google received the request and refused it on grounds that are
			// not about the credential. The dispatcher turns this into a service defect, NOT a
			// failed test — nothing the operator owns is wrong and no field they can edit
			// repairs it.
			name:            "api 400 is neither a rejection nor inconclusive",
			err:             &apiError{StatusCode: 400},
			wantRejected:    false,
			wantInconclusiv: false,
		},
		{
			name:            "mid-flight transport failure",
			err:             &transportError{Method: "POST", Path: "/x", Err: errors.New("connection reset by peer")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// An error from neither the round trip nor the token exchange — a completeness
			// guard, a malformed response this client refused to trust. It proves nothing
			// about the credential.
			name:            "unrecognised error defaults to inconclusive",
			err:             errors.New("decoding result row: unexpected end of JSON input"),
			wantRejected:    false,
			wantInconclusiv: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProbeCredentialRejected(tc.err); got != tc.wantRejected {
				t.Errorf("ProbeCredentialRejected = %v, want %v", got, tc.wantRejected)
			}
			if got := ProbeInconclusive(tc.err); got != tc.wantInconclusiv {
				t.Errorf("ProbeInconclusive = %v, want %v", got, tc.wantInconclusiv)
			}
		})
	}
}

// TestProbeCredentialRejected_SeesThroughWrapping pins that the sentinel survives the wrapping
// the real call path applies. fetchToken's error travels up through accessTokenValue and
// doRequest, each of which returns it bare, but a caller adding context with %w must not break
// the classification either — and an %v would, silently, by turning the connection test's
// verdict back into "inconclusive" for a dead credential.
func TestProbeCredentialRejected_SeesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("listing accessible customers: %w",
		fmt.Errorf("%w: google ads token refresh -> 401", ErrTokenRequestRejected))
	if !ProbeCredentialRejected(err) {
		t.Fatal("ProbeCredentialRejected = false through a wrapped chain; a revoked refresh token would be reported as a healthy connection")
	}
}
