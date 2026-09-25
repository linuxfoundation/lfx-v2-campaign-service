// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbePredicates pins Reddit's connection-probe classification (LFXV2-2665).
//
// The 404 case is the one that differs from every sibling and is the reason this test exists
// as much as the token case: Reddit's probe names the configured account IN the request path,
// so a 404 is Reddit answering the question that was asked. It gets its own predicate rather
// than joining the rejections, because "Reddit refused your credential" and "Reddit honoured
// your credential and has no such account" send the operator to different fields. Flipping it
// to "neither predicate" would turn a connection the operator must repoint into a service
// defect that pages us.
//
// The 429 case pins the other half: a rate limit is the platform declining to answer, so a
// throttled token refresh must never read as a permanent refusal of the stored credential.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
		wantUnreachable bool
	}{
		{
			name:         "token endpoint refused the refresh",
			err:          fmt.Errorf("%w: reddit token refresh -> 400", ErrTokenRequestRejected),
			wantRejected: true,
			// Also true, because ProbeInconclusive defaults to true — which is exactly why the
			// dispatcher consults the rejection predicate first.
			wantInconclusiv: true,
		},
		{
			name:            "token endpoint unavailable",
			err:             fmt.Errorf("%w: reddit token refresh -> 502", errTokenEndpointUnavailable),
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "401",
			err:             &apiError{StatusCode: 401, Method: "GET", Path: "/ad_accounts/t2_gv9wtbfa"},
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
			// The departure from every sibling predicate. See ProbeAccountUnreachable's doc.
			name:            "404 on the configured account is unreachable, not a rejected credential",
			err:             &apiError{StatusCode: 404, Method: "GET", Path: "/ad_accounts/t2_gv9wtbfa"},
			wantRejected:    false,
			wantInconclusiv: false,
			wantUnreachable: true,
		},
		{
			name:            "429",
			err:             &apiError{StatusCode: 429},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "503",
			err:             &apiError{StatusCode: 503},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// A stored account id Reddit could never have issued is decidable without
			// contacting Reddit at all — which is exactly why NEITHER predicate claims it.
			// Reddit never evaluated the credential, so "rejected" would name the wrong
			// remedy; the dispatcher answers this one itself with accountIDNotUsable, and
			// this case exists to keep the predicate from taking it back.
			name:            "a malformed stored account id is not a credential rejection",
			err:             fmt.Errorf("%w: invalid reddit account id %q", ErrInvalidAccountID, "not-an-id"),
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			// Neither: Reddit refused the request this service built, on grounds that are not
			// about the credential or the account named in the path.
			name:            "400 is neither",
			err:             &apiError{StatusCode: 400},
			wantRejected:    false,
			wantInconclusiv: false,
		},
		{
			name:            "mid-flight transport failure",
			err:             &transportError{Method: "GET", Path: "/ad_accounts/x", Err: errors.New("connection reset by peer")},
			wantRejected:    false,
			wantInconclusiv: true,
		},
		{
			name:            "unrecognised error defaults to inconclusive",
			err:             errors.New("decoding ad account: unexpected end of JSON input"),
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
			if got := ProbeAccountUnreachable(tc.err); got != tc.wantUnreachable {
				t.Errorf("ProbeAccountUnreachable = %v, want %v", got, tc.wantUnreachable)
			}
		})
	}
}

// TestTokenRefresh429IsInconclusive pins the classification at its SOURCE rather than on a
// synthetic error, because the defect it guards was in fetchToken's status split, not in the
// predicates: a 429 is a 4xx, so splitting on >= 500 alone routed a throttled refresh into
// ErrTokenRequestRejected. ProbeCredentialRejected matches that first and probeClass evaluates
// rejection before inconclusive, so the connection test told an operator Reddit had permanently
// refused a credential Reddit had merely declined to evaluate. Reddit matters most of the three
// here: /api/v1/access_token throttles routinely.
func TestTokenRefresh429IsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(testCreds, testAccount, WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
	_, err := c.refreshToken(context.Background())
	if err == nil {
		t.Fatal("expected an error on a 429 token response, got nil")
	}
	if ProbeCredentialRejected(err) {
		t.Errorf("a throttled token refresh classified as a rejected credential: %v", err)
	}
	if !ProbeInconclusive(err) {
		t.Errorf("a throttled token refresh is not inconclusive: %v", err)
	}
}
