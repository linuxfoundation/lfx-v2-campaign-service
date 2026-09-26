// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// refreshScopeObservationWindow is how long the token endpoint holds a request open waiting to
// be cancelled. Long enough that a cancellation travelling normally arrives well inside it,
// short enough that the detached case — which is SUPPOSED to sit here — does not slow the suite.
// The real detached deadline is redditRequestTimeout, which is far longer, so nothing about
// this window can make a detached refresh look cancelled.
const refreshScopeObservationWindow = 500 * time.Millisecond

// TestCallerScopedTokenRefresh_BindsTheRefreshToItsCaller pins both halves of the option, because
// each half is a different bug.
//
// The token refresh runs in a goroutine the leader starts and nobody joins. Detached — the
// default — it survives its caller on purpose: a shared client's other waiters are parked on that
// one flight, and the token it mints is reused by callers that have not arrived yet — which for
// this platform is the cached client resolveRedditClientWithCredsCache hands every dispatch. A
// probe's client has neither: that same resolver deliberately BYPASSES the cache for a probe, so
// the client it builds is used for one enumeration and dropped without ever being published.
// So when the orchestrator's probe deadline fires, a detached refresh keeps a goroutine, a
// socket and a file descriptor alive for the remainder of redditRequestTimeout to produce a
// token no one can read — once per probe, on every connection on the platform, and precisely when
// Reddit's token endpoint is the thing that is slow, which is when probes are being cancelled.
//
// The other half matters just as much: flipping the default would break the sharing the
// single-flight exists for, so the test asserts the detached client still IGNORES its caller's
// cancellation.
func TestCallerScopedTokenRefresh_BindsTheRefreshToItsCaller(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []Option
		wantCancelled bool
		why           string
	}{
		{
			name:          "caller-scoped, as a probe builds it",
			opts:          []Option{WithCallerScopedTokenRefresh()},
			wantCancelled: true,
			why: "the refresh outlived the probe that is its only consumer, holding a goroutine and " +
				"its connection open for the rest of redditRequestTimeout",
		},
		{
			name:          "detached, the shared-client default",
			opts:          nil,
			wantCancelled: false,
			why: "one caller's cancellation tore down a refresh the other waiters on this shared " +
				"client are parked on — the exact failure the single-flight's detach prevents",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := make(chan struct{})
			cancelled := make(chan bool, 1)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Drain the form body first: net/http only starts the background read
				// that notices a client hang-up — and so only cancels r.Context() —
				// once the request body has been consumed.
				_, _ = io.Copy(io.Discard, r.Body)
				close(reached)
				select {
				case <-r.Context().Done():
					// The server sees the client hang up, which only happens if the
					// context the request was built from was cancelled.
					cancelled <- true
				case <-time.After(refreshScopeObservationWindow):
					cancelled <- false
					tokenHandlerReturning("at-123")(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			c := NewClient(
				Credentials{ClientID: "cid", ClientSecret: "csecret", RefreshToken: "rtok"},
				AccountConfig{AccountID: "t2_abc", Label: "Test"},
				append([]Option{WithTokenURL(srv.URL), WithNowFunc(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })}, tc.opts...)...)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = c.refreshToken(ctx)
			}()

			// Cancel only once the refresh is genuinely in flight; cancelling earlier would
			// be refused by accessTokenValue's own already-done guard and prove nothing.
			<-reached
			cancel()

			// The CALLER returns promptly either way — that is the select on its own ctx, and
			// it is not what this test is about.
			select {
			case <-done:
			case <-time.After(refreshScopeObservationWindow + 2*time.Second):
				t.Fatal("refreshToken did not return after its context was cancelled")
			}

			select {
			case got := <-cancelled:
				if got != tc.wantCancelled {
					t.Errorf("token request cancelled = %v, want %v: %s", got, tc.wantCancelled, tc.why)
				}
			case <-time.After(refreshScopeObservationWindow + 2*time.Second):
				t.Fatal("the token endpoint never reported an outcome")
			}
		})
	}
}
