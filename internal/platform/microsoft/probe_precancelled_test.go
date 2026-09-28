// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestProbeCancelledBeforeAnyRequestIsNotSent pins the metrics provenance of a probe whose
// caller gave up before it started. ListAdAccounts is the call internal/dispatch/microsoft.go
// makes on the probe path, and it reaches for a token before it reaches for the network.
//
// A context already done at entry produces an error that answers ProbeInconclusive true — the
// check established neither half of ok's conjunction — and the question this test exists for is
// the OTHER predicate. Left unmarked, the bare ctx.Err() fell through ProbeNotSent's false
// default, probeReachedThePlatform found no local sentinel to name, and this deployment's own
// cancellation was recorded as campaign_upstream_call_duration_seconds{outcome="error"} against
// Microsoft — an error sample for a call that never left the process.
//
// errRequestNotSent, this package's existing pre-send marker, is NOT the answer here: its own
// doc scopes it to the REST path alone, so reusing it would contradict a written contract and
// make the two markers' meanings drift. The token leg carries its own.
func TestProbeCancelledBeforeAnyRequestIsNotSent(t *testing.T) {
	var hits atomic.Int32
	count := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	tok := httptest.NewServer(count)
	t.Cleanup(tok.Close)
	api := httptest.NewServer(count)
	t.Cleanup(api.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tok.URL), WithBaseURL(api.URL), WithClock(fixedClock()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.ListAdAccounts(ctx); err != nil {
		assertNotSentCancellation(t, err, hits.Load())
	} else {
		t.Fatal("ListAdAccounts succeeded on an already-cancelled context")
	}
}

func assertNotSentCancellation(t *testing.T, err error, hits int32) {
	t.Helper()
	if hits != 0 {
		t.Fatalf("%d HTTP calls were made; a cancelled caller must reach no endpoint", hits)
	}
	if !ProbeNotSent(err) {
		t.Errorf("ProbeNotSent(%v) = false; the probe made zero calls, so recording an upstream "+
			"error sample charges Microsoft for this deployment's own cancellation", err)
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
