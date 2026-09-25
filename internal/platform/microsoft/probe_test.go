// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"errors"
	"fmt"
	"testing"
)

// TestProbePredicates pins Microsoft's connection-probe classification (LFXV2-2665).
//
// The token-endpoint split is the load-bearing part, for the same reason as Google's: a refresh
// token that Microsoft has revoked, expired, or issued against different application
// credentials fails here permanently, and before the split in fetchToken that failure was
// untyped and fell to ProbeInconclusive's default — so the connection test reported it healthy.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
	}{
		{
			name:         "token endpoint refused the refresh (revoked token)",
			err:          fmt.Errorf("%w: microsoft-ads token refresh -> 400", ErrTokenRequestRejected),
			wantRejected: true,
			// Also true: the default is true, which is why the rejection predicate is
			// consulted first.
			wantInconclusiv: true,
		},
		{
			name:            "token endpoint unavailable (5xx)",
			err:             fmt.Errorf("%w: microsoft-ads token refresh -> 503", errTokenEndpointUnavailable),
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "token refresh transport failure",
			err:             &tokenTransportError{err: errors.New("dial tcp: i/o timeout")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "401",
			err:             &apiError{StatusCode: 401, Method: "POST", Path: "/CustomerManagement/v13"},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "403",
			err:             &apiError{StatusCode: 403},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "429",
			err:             &apiError{StatusCode: 429},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "500",
			err:             &apiError{StatusCode: 500},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "400 is neither",
			err:             &apiError{StatusCode: 400},
			wantRejected:    false,
			wantInconclusiv: false,
		},
		{
			name:            "mid-flight transport failure",
			err:             &transportError{Method: "POST", Path: "/x", err: errors.New("connection reset by peer")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// ListAdAccounts treats an incomplete answer as an error rather than a short list,
			// and that guard lands here. It proves nothing about the credential.
			name:            "unrecognised error defaults to inconclusive",
			err:             errors.New("account enumeration returned an incomplete page"),
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

// TestProbeInconclusive_PreSendDialErrorIsNotClaimed documents, as an executable note, why this
// package's ProbeInconclusive omits the isPreSendDialError arm its Google, Reddit and X
// siblings carry.
//
// This client's pre-send arm renders the cause through safeCause into a plain string rather
// than wrapping it with %w — deliberately, so a custom RoundTripper's error text can never
// reach a persisted campaign step. So the dial classifier cannot see through such an error,
// and an arm calling it would assert a match that can never happen. The classification is
// unchanged either way (the default below is inconclusive too); only the claim would be false.
func TestProbeInconclusive_PreSendDialErrorIsNotClaimed(t *testing.T) {
	// The shape the pre-send arm actually produces: the cause is rendered, not wrapped.
	preSend := errors.New("microsoft-ads POST /CampaignManagement/v13: " + safeCause(errors.New("dial tcp: i/o timeout")))
	if isPreSendDialError(preSend) {
		t.Fatal("isPreSendDialError now sees through this client's pre-send error; the omission in ProbeInconclusive should be revisited and its comment corrected")
	}
	if !ProbeInconclusive(preSend) {
		t.Error("ProbeInconclusive = false for a pre-send dial failure; nothing was learned about the credential, so it must not read as a verdict")
	}
}
