// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

			// Cancelled from a GOROUTINE released once the 429 has been fully written, so the
			// cancellation lands while the client is in sleepCtx rather than in httpClient.Do.
			//
			// Cancelling inside the handler does NOT work and is the trap this test exists to
			// avoid: the client is still mid-exchange there, so the cancel kills the NEXT Do
			// instead and the error reads `Post "...": context canceled` — a path that already
			// wrapped its error before this fix, so the test would pass with the sleep wrapping
			// reverted. I confirmed that by reverting it and watching this test stay green.
			//
			// The long Retry-After (and the default backoff on the other arm) keeps the sleep
			// wide enough that the cancel cannot race past it.
			served := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfterS != "" {
					w.Header().Set("Retry-After", tc.retryAfterS)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
				select {
				case served <- struct{}{}:
				default:
				}
			}))
			defer srv.Close()

			go func() {
				<-served
				// The handler has returned, so the client has the whole 429 and is now in the
				// backoff sleep. A short pause makes that ordering robust under CI scheduling.
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()

			c := NewClient(
				Credentials{AccessToken: "tok"},
				AccountConfig{AccountID: "act_777", CurrencyOffset: 100},
				WithBaseURL(srv.URL),
				WithClock(fixedMetaClock()),
			)

			// A status toggle rather than a create: the simplest MUTATING call that reaches the
			// transport with no input validation in front of it. The ambiguity contract is the
			// same, and a toggle that may have applied must not read as definitely not applied.
			err := c.UpdateCampaignStatus(ctx, "23851234567890123", "PAUSED")
			if err == nil {
				t.Fatal("expected the throttled request to fail")
			}
			// THE ARM CHECK. `Post "...": context canceled` means the cancel landed in Do, not
			// in the sleep — which would make the ambiguity assertion below prove nothing about
			// this fix.
			if strings.Contains(err.Error(), "Post \"") {
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
