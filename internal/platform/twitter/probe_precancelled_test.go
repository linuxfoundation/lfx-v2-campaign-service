// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeCancelledBeforeAnyRequestIsNotSent pins the metrics half of the probe contract for a
// caller that cancelled before the probe started (LFXV2-2665).
//
// The probe's upstream-call series is meant to describe X. Before doRequestAbs checked the
// context at its entry, an already-cancelled caller got the context error back out of
// http.Client.Do wrapped as a transportError — a shape ProbeNotSent does not recognise — so
// probeClass omitted domain.ErrConnectionProbeNotAttempted and the dispatcher recorded
// outcome="error" against X for a request X never received. The assertion that no request
// reached the server is what makes "not sent" a fact here rather than a label.
func TestProbeCancelledBeforeAnyRequestIsNotSent(t *testing.T) {
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
