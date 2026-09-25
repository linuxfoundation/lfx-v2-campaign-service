// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"errors"
	"testing"
)

// TestProbePredicates pins HubSpot's connection-probe classification (LFXV2-2665).
//
// HubSpot has no token-refresh arm to classify, unlike Google, Reddit and Microsoft: private
// apps authenticate with a long-lived token and this client runs no OAuth exchange, so the
// whole class of "the refresh was refused" cannot occur. That is a property of the integration,
// not an omission — which is why this table has no sentinel cases and why 403 sits with the
// rejections: a private app's scopes are chosen when the token is issued, so a scope refusal is
// a fact about the stored credential.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
	}{
		{
			name:            "401 (revoked or mistyped token)",
			err:             &apiError{StatusCode: 401, Method: "POST", Path: tokenInfoPath},
			wantRejected:    true,
			wantInconclusiv: false,
		},
		{
			name:            "403 (the private app lacks the scope)",
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
			name:            "pre-send failure",
			err:             &preSendError{Method: "POST", Path: tokenInfoPath, err: errors.New("dial tcp: i/o timeout")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "mid-flight transport failure",
			err:             &transportError{Method: "POST", Path: tokenInfoPath, err: errors.New("connection reset by peer")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// AuthenticatedPortalID's own guards land here: a 200 that was not a valid
			// token-info response, or one carrying no usable hubId. HubSpot answered, but not
			// with anything this service can read as a verdict on the credential.
			name:            "an unreadable token-info response defaults to inconclusive",
			err:             errors.New("not a valid token-info response"),
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
