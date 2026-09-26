// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProbePredicates pins X's connection-probe classification (LFXV2-2665).
//
// X authenticates with an OAuth 1.0a four-tuple and runs no token exchange, so unlike Google,
// Reddit and Microsoft there is no refresh arm to classify: every verdict here comes from the
// account request itself. That probe addresses the configured account root directly, which is
// why a 404 gets its own predicate rather than joining the rejections — the same reasoning as
// Reddit's: "X refused your credential" and "X honoured your credential and has no such
// account" send the operator to different fields.
func TestProbePredicates(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantRejected    bool
		wantInconclusiv bool
		wantUnreachable bool
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
			name:            "404 on the configured account root is unreachable, not a rejected credential",
			err:             &apiError{StatusCode: 404},
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
			if got := ProbeAccountUnreachable(tc.err); got != tc.wantUnreachable {
				t.Errorf("ProbeAccountUnreachable = %v, want %v", got, tc.wantUnreachable)
			}
		})
	}
}

// TestVerifyAccountRejectsAnUnusableAccountIDBeforeAnyRequest pins the guard that keeps a
// malformed stored id off the wire.
//
// The id is interpolated into the account-scoped path, so "18ce54d4x5t/promoted_tweets" would
// make VerifyAccount GET a DIFFERENT subresource — and a 2xx from that would report the
// connection healthy on the strength of a request that answered a different question. The
// assertion that matters is the call count: a test that only checked the error would still pass
// if the request were made and then discarded.
func TestVerifyAccountRejectsAnUnusableAccountIDBeforeAnyRequest(t *testing.T) {
	for _, id := range []string{
		"18ce54d4x5t/promoted_tweets", // path injection: the finding's own example
		"acc?with=query",
		"acc#frag",
		"acc 1",
		"acc-1",
		strings.Repeat("a", maxAccountIDLen+1), // charset-valid, over the enumeration bound
	} {
		t.Run(id, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				_, _ = w.Write([]byte(`{"data":{"name":"Somebody Else"}}`))
			}))
			defer srv.Close()

			c := NewClient(
				Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
				AccountConfig{AccountID: id},
				WithBaseURL(srv.URL),
				WithWriteDelay(0),
			)
			err := c.VerifyAccount(context.Background())
			if !errors.Is(err, ErrInvalidAccountID) {
				t.Fatalf("VerifyAccount(%q) = %v, want ErrInvalidAccountID", id, err)
			}
			if n := atomic.LoadInt32(&calls); n != 0 {
				t.Errorf("made %d request(s) for an unusable account id; want none", n)
			}
			// Neither predicate may claim it: the dispatcher answers it as accountIDNotUsable,
			// and both a credential rejection and an unreachable-platform advisory would be wrong.
			if ProbeCredentialRejected(err) {
				t.Error("an unusable account id classified as a rejected credential")
			}
		})
	}
}
