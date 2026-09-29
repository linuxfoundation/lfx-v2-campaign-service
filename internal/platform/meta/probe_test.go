// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"errors"
	"testing"
)

// TestProbePredicates pins Meta's connection-probe classification (LFXV2-2665).
//
// Meta is the platform where HTTP status alone is least informative: the Graph API reports an
// invalid or expired access token as code 190 under a 400 at least as often as under a 401, and
// reports rate limiting as a 400 too. Classifying on status alone would therefore either miss
// the revoked token entirely (reporting a broken connection healthy) or call a throttle a
// credential rejection (reporting a working connection broken). Both halves are pinned here.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
	}{
		{
			// The headline case: an expired or revoked token, which Meta reports under a 400.
			name:            "graph code 190 under a 400 is a credential rejection",
			err:             &APIError{StatusCode: 400, Code: graphCodeInvalidToken, Message: "Error validating access token"},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "graph code 200 (permission denied)",
			err:             &APIError{StatusCode: 403, Code: graphCodePermissionDenied},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "graph code 10 (application denied)",
			err:             &APIError{StatusCode: 403, Code: graphCodeApplicationDenied},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "401 with no graph code",
			err:             &APIError{StatusCode: 401},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			// A rate limit Meta dressed as a 400. Calling this a rejection would tell an
			// operator their working connection is broken every time Meta throttles.
			name:            "a rate-limit code under a 400 is inconclusive, not a rejection",
			err:             &APIError{StatusCode: 400, Code: 4},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "429",
			err:             &APIError{StatusCode: 429},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// The OTHER 4xx that is not an answer. A 408 means the endpoint, or an intermediary
			// in front of it, gave up waiting for the request: nothing evaluated the credential
			// and the same call can succeed on a retry. This package's token leg already read it
			// that way; the account leg did not, so an account-read 408 matched NEITHER predicate,
			// fell through probeClass's default arm and reached the operator as a typed 500
			// service defect — paging us for a timeout.
			name:            "api 408",
			err:             &APIError{StatusCode: 408},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "500",
			err:             &APIError{StatusCode: 500},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// The status gates the code. A shed request that happens to carry 190 evaluated
			// nothing, so calling it a rejection would tell an operator to reauthorize a
			// credential Meta never looked at — and ProbeCredentialRejected is consulted
			// BEFORE ProbeInconclusive, so without the gate this arm won.
			name:            "a 429 carrying graph code 190 is inconclusive, not a rejection",
			err:             &APIError{StatusCode: 429, Code: graphCodeInvalidToken},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "a 500 carrying graph code 190 is inconclusive, not a rejection",
			err:             &APIError{StatusCode: 500, Code: graphCodeInvalidToken},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "a 503 carrying graph code 200 is inconclusive, not a rejection",
			err:             &APIError{StatusCode: 503, Code: graphCodePermissionDenied},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// Code is absent because the envelope could not be READ, not because Meta omitted
			// it — so the 400 below must not be read as a clean semantic rejection. Same
			// reasoning the create path already applies to this field.
			name:            "an unreadable envelope is inconclusive whatever the status",
			err:             &APIError{StatusCode: 400, EnvelopeUnreadable: true},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// Neither predicate: Meta received the request and refused it on grounds that are
			// not about the credential — a moved edge, a field this build stopped sending.
			// The dispatcher turns this into a service defect, not a failed test.
			name:            "a plain 400 with no recognised code is neither",
			err:             &APIError{StatusCode: 400, Code: 100},
			wantRejected:    false,
			wantInconclusiv: false,
		},
		{
			name:            "unrecognised error defaults to inconclusive",
			err:             errors.New("decoding graph response: unexpected end of JSON input"),
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
