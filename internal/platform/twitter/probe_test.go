// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"errors"
	"testing"
)

// TestProbePredicates pins X's connection-probe classification (LFXV2-2665).
//
// X authenticates with an OAuth 1.0a four-tuple and runs no token exchange, so unlike Google,
// Reddit and Microsoft there is no refresh arm to classify: every verdict here comes from the
// account request itself. That probe addresses the configured account root directly, which is
// why a 404 belongs with the rejections — the same reasoning as Reddit's.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
	}{
		{
			// Decidable without contacting X at all — which is why neither predicate claims
			// it. It IS a confirmed verdict, but one the dispatcher authors
			// (noAccountConfigured); calling it a credential rejection here would tell an
			// operator to re-authorise credentials X never looked at.
			name:         "no account configured is not a credential rejection",
			err:          ErrAccountNotConfigured,
			wantRejected: false,
			// Still inconclusive by default, which is why the dispatcher intercepts this
			// sentinel before either predicate is consulted at all.
			wantInconclusiv: true,
		},
		{
			name:            "401",
			err:             &apiError{StatusCode: 401, Method: "GET", Path: "/accounts/18ce54d4x5t"},
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
			name:            "404 on the configured account root",
			err:             &apiError{StatusCode: 404},
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
			name:            "pre-send failure",
			err:             &preSendError{Method: "GET", Path: "/accounts/x", Err: errors.New("dial tcp: i/o timeout")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "mid-flight transport failure",
			err:             &transportError{Method: "GET", Path: "/accounts/x", Err: errors.New("connection reset by peer")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "unrecognised error defaults to inconclusive",
			err:             errors.New("signing request: missing consumer secret"),
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
