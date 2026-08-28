// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A 429 PROVES Meta received the request. If the context then expires while we wait to retry,
// the mutation may already have committed — so the outcome is AMBIGUOUS, never "not applied".
//
// The failure this pins is not a wrong status code, it is CORRUPTED STATE: a possibly-committed
// campaign create reported as definitely-failed gets retried, and the retry creates a second
// campaign that spends real budget. The Google Ads client documents this hazard at
// googleads/client.go:757; Meta returned the bare ctx error and so classified it false.
//
// Driven end-to-end through a real 429 and a real expiring context rather than by calling
// createOutcomeAmbiguous directly: the defect was in the retry loop's control flow, not in the
// classifier, so a unit test on the classifier alone would have passed throughout.
func TestThrottleBackoffCancellation_IsAmbiguousNotFailed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retryAfterS string // empty = no header, exercising the exponential-backoff arm
	}{
		{"server-declared Retry-After", "30"},
		{"no header, exponential backoff", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// DETERMINISTIC, not timed. The first request serves the 429 and hands the client
			// into the backoff sleep; a long Retry-After (and a long backoff base on the other
			// arm) means it cannot leave that sleep on its own. Cancelling from a goroutine
			// released by the first request therefore lands inside sleepCtx.
			//
			// Neither of my earlier attempts did this. A wall-clock deadline could expire while
			// httpClient.Do was still reading the 429, and cancelling inside the handler kills
			// the NEXT request instead; both land in a path that already wrapped its error
			// before this fix, so the test passed with the fix reverted.
			first := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfterS != "" {
					w.Header().Set("Retry-After", tc.retryAfterS)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
				once.Do(func() { close(first) })
			}))
			defer srv.Close()

			go func() {
				<-first
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()

			c := NewClient(
				Credentials{AccessToken: "tok"},
				AccountConfig{AccountID: "act_777", CurrencyOffset: 100},
				WithBaseURL(srv.URL),
				WithClock(fixedMetaClock()),
				withRetryBaseDelay(30*time.Second),
			)

			// A status toggle rather than a create: the simplest MUTATING call that reaches the
			// transport with no input validation in front of it. The ambiguity contract is the
			// same, and a toggle that may have applied must not read as definitely not applied.
			err := c.UpdateCampaignStatus(ctx, "23851234567890123", "PAUSED")
			if err == nil {
				t.Fatal("expected the throttled request to fail")
			}
			// THE ARM CHECK, matched on the URL rather than the verb. Go renders a Do-path
			// cancellation as `Post "url": ...` here and `Patch "url": ...` on other calls, so
			// matching one verb misses the other — which is exactly what happened in the Reddit
			// copy of this test.
			if strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("cancellation landed in httpClient.Do, not the backoff sleep — this test would pass without the fix: %v", err)
			}
			if !createOutcomeAmbiguous(err) {
				t.Errorf("createOutcomeAmbiguous = false for a cancelled 429 backoff; the caller would retry and double-create. err = %v", err)
			}
		})
	}
}

// An EXHAUSTED 429 — the retry budget spent rather than the context cancelled — is ambiguous for
// the same reason: Meta received the request and shed it, which says nothing about whether the
// mutation committed. This arm needs no cancellation at all, so it cannot be satisfied by the
// in-flight Do path and pins the classifier's own 429 branch.
func TestThrottleExhausted_IsAmbiguousNotFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777", CurrencyOffset: 100},
		WithBaseURL(srv.URL),
		WithClock(fixedMetaClock()),
		withRetryBaseDelay(time.Millisecond),
	)

	err := c.UpdateCampaignStatus(context.Background(), "23851234567890123", "PAUSED")
	if err == nil {
		t.Fatal("expected the exhausted throttle to fail")
	}
	if !createOutcomeAmbiguous(err) {
		t.Errorf("createOutcomeAmbiguous = false for an exhausted 429: %v", err)
	}
}
