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
	"time"
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
			// The OTHER 4xx that is not an answer. A 408 means the endpoint, or an intermediary
			// in front of it, gave up waiting for the request: nothing evaluated the credential
			// and the same call can succeed on a retry. This package's token leg already read it
			// that way; the account leg did not, so an account-read 408 matched NEITHER predicate,
			// fell through probeClass's default arm and reached the operator as a typed 500
			// service defect — paging us for a timeout.
			name:            "api 408",
			err:             &apiError{StatusCode: 408},
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

// TestProbeNotSentForAlreadyDoneContext pins the metrics half of the probe contract for a
// caller that cancelled before the probe started (LFXV2-2665).
//
// The probe's upstream-call series is meant to describe X. Before doRequestAbs checked the
// context at its entry, an already-cancelled caller got the context error back out of
// http.Client.Do wrapped as a transportError — a shape ProbeNotSent does not recognise — so
// probeClass omitted domain.ErrConnectionProbeNotAttempted and the dispatcher recorded
// outcome="error" against X for a request X never received. The assertion that no request
// reached the server is what makes "not sent" a fact here rather than a label.
func TestProbeNotSentForAlreadyDoneContext(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"data":{"id":"acc1"}}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.VerifyAccount(ctx)
	if err == nil {
		t.Fatal("VerifyAccount on an already-cancelled context returned no error")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("made %d request(s) on an already-cancelled context; want none", n)
	}
	if !ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = false, want true", err)
	}
	// The marker wraps the context error rather than replacing it, so every existing caller
	// that asks the ordinary question keeps getting the ordinary answer.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false; the marker must wrap, not replace", err)
	}
	// Not sent is an ORTHOGONAL axis: the operator-facing verdict is still inconclusive, and
	// nothing here is a verdict on the credential or on the account.
	if !ProbeInconclusive(err) {
		t.Error("an unsent probe classified as conclusive")
	}
	if ProbeCredentialRejected(err) {
		t.Error("an unsent probe classified as a rejected credential")
	}
	if ProbeAccountUnreachable(err) {
		t.Error("an unsent probe classified as an unreachable account")
	}
	// A create that fails this way did NOT reach X, so it must not be retained as "may exist".
	if createOutcomeAmbiguous(err) {
		t.Error("a request that was never sent classified as an ambiguous create outcome")
	}
}

// TestProbeNotSentIsEntryTimeOnly pins the OTHER half of the marker's contract: a context error
// that surfaces from the round trip, where bytes may already have gone out, must NOT be claimed
// as proof the request was never sent. The server hangs until the caller's deadline elapses, so
// the failure arrives out of http.Client.Do rather than from doRequestAbs's entry check.
func TestProbeNotSentIsEntryTimeOnly(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"data":{"id":"acc1"}}`))
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.VerifyAccount(ctx)
	if err == nil {
		t.Fatal("VerifyAccount against a hanging server returned no error")
	}
	if ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = true for a mid-flight context expiry; the request HAD been sent", err)
	}
	if !ProbeInconclusive(err) {
		t.Error("a mid-flight context expiry classified as conclusive")
	}
}
