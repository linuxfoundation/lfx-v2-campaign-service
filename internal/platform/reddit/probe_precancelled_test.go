// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestProbeCancelledBeforeAnyRequestIsNotSent pins the metrics provenance of a probe whose
// caller gave up before it started. VerifyAccount is the call internal/dispatch/reddit.go makes
// on the probe path, and it refreshes a token before it reads the account.
//
// A context already done at entry produces an error that answers ProbeInconclusive true — the
// check established neither half of ok's conjunction — and the question this test exists for is
// the OTHER predicate. Left unmarked, the bare ctx.Err() fell through ProbeNotSent's false
// default, probeReachedThePlatform found no local sentinel to name, and this deployment's own
// cancellation was recorded as campaign_upstream_call_duration_seconds{outcome="error"} against
// Reddit — an error sample for a call that never left the process.
//
// Only the ENTRY check is marked. refreshToken's waiter select returns ctx.Err() too, but by
// then a detached refresh may already be on the wire, so the same claim would be false there.
func TestProbeCancelledBeforeAnyRequestIsNotSent(t *testing.T) {
	var hits atomic.Int32
	count := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	tokenSrv := httptest.NewServer(count)
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(count)
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.VerifyAccount(ctx)
	if err == nil {
		t.Fatal("VerifyAccount succeeded on an already-cancelled context")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d HTTP calls were made; a cancelled caller must reach no endpoint", n)
	}
	if !ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = false; the probe made zero calls, so recording an upstream "+
			"error sample charges Reddit for this deployment's own cancellation", err)
	}
	if !ProbeInconclusive(err) {
		t.Errorf("ProbeInconclusive(%v) = false; a probe that never ran proves nothing about "+
			"the connection either way", err)
	}
	if ProbeCredentialRejected(err) {
		t.Errorf("ProbeCredentialRejected(%v) = true; nothing evaluated the credential", err)
	}
	// The marker WRAPS the context error rather than replacing it, so every existing caller
	// that branches on cancellation keeps working.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false; the marker must not hide the cause", err)
	}
}
