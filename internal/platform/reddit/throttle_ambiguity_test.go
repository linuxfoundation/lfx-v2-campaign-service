// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// redditThrottleClient wires a client whose API server always 429s, optionally with a
// Retry-After header.
func redditThrottleClient(t *testing.T, retryAfter string) *Client {
	t.Helper()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	// tinyBackoff, matching the other retry tests: the production base would make the
	// exhausted-retry case sleep 1s + 2s + 4s of real time on every suite run.
	return NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()),
		withRetryBaseDelay(tinyBackoff))
}

// A 429 PROVES Reddit received the request. If the context then expires while we wait to retry,
// the mutation may already have committed — so the outcome is AMBIGUOUS, never "not applied".
//
// This is corrupted STATE, not a wrong status: a possibly-applied status toggle reported as
// definitely-failed gets retried, and on the create path that means a second campaign spending
// real budget.
//
// The client's own doc comment already states this invariant for the IN-FLIGHT request; these
// are the BACKOFF-SLEEP paths two hundred lines below it, which returned the bare ctx error and
// so classified false. Driven end-to-end through a real 429 and a real expiring context,
// because the defect lived in retry-loop control flow rather than in the classifier — a unit
// test on createOutcomeAmbiguous alone would have passed throughout.
func TestThrottleBackoffCancellation_IsAmbiguousNotFailed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string // empty = no header, exercising the exponential-backoff arm
	}{
		{"server-declared Retry-After", "30"},
		{"no header, exponential backoff", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// DETERMINISTIC, not timed. The handler counts requests: the first serves the 429
			// and hands the client into the backoff sleep; the SECOND can only be reached once
			// that sleep completes. So cancelling from a goroutine that waits on the first
			// request — and blocking the client's only escape from the sleep — puts the cancel
			// unambiguously inside sleepCtx.
			//
			// Neither of my earlier attempts did this. A wall-clock deadline could expire while
			// httpClient.Do was still reading the 429, and cancelling inside the handler kills
			// the NEXT request instead; both land in a path that already wrapped its error
			// before this fix, so the test passed with the fix reverted. The long Retry-After
			// (and tinyBackoff on the other arm) is what makes the sleep the only place left.
			first := make(chan struct{})
			var once sync.Once
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limited"}`))
				once.Do(func() { close(first) })
			}))
			defer apiSrv.Close()
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tokenSrv.Close()

			go func() {
				<-first
				// The handler has written the whole 429 and returned. The client either is in
				// sleepCtx or is about to be, and the 30s Retry-After guarantees it cannot leave
				// before this cancel lands.
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()

			c := NewClient(testCreds, testAccount,
				WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()),
				withRetryBaseDelay(30*time.Second))

			err := c.UpdateCampaignStatus(ctx, "t5_abc123", "PAUSED")
			if err == nil {
				t.Fatal("expected the throttled request to fail")
			}
			// THE ARM CHECK, method-agnostic. Go's transport renders a Do-path cancellation as
			// `Patch "url": context canceled` for this call and `Post "url": ...` for others, so
			// matching one verb missed the other entirely — which is exactly what happened here.
			// Matching the URL is what identifies the arm regardless of method.
			if strings.Contains(err.Error(), apiSrv.URL) {
				t.Fatalf("cancellation landed in httpClient.Do, not the backoff sleep — this test would pass without the fix: %v", err)
			}
			if !createOutcomeAmbiguous(err) {
				t.Errorf("createOutcomeAmbiguous = false for a cancelled 429 backoff; the caller would retry a possibly-applied mutation. err = %v", err)
			}
		})
	}
}

// An EXHAUSTED 429 — the retry budget spent rather than the context cancelled — carries the same
// ambiguity: Reddit received the request and shed it, which says nothing about whether the
// mutation committed. Reddit's classifier had NO 429 branch, so this classified as definitely
// not applied. It needs no cancellation, so it cannot be satisfied by the in-flight Do path.
func TestThrottleExhausted_IsAmbiguousNotFailed(t *testing.T) {
	c := redditThrottleClient(t, "")

	err := c.UpdateCampaignStatus(context.Background(), "t5_abc123", "PAUSED")
	if err == nil {
		t.Fatal("expected the exhausted throttle to fail")
	}
	if !createOutcomeAmbiguous(err) {
		t.Errorf("createOutcomeAmbiguous = false for an exhausted 429: %v", err)
	}
}

// The over-cap abort is REACHABLE, not defensive: it fires whenever Reddit declares a
// rate-limit reset longer than maxRetryWait (60s). It follows a 429 like every other arm here,
// so it carries the same ambiguity — and it returned a plain fmt.Errorf, which matches nothing
// createOutcomeAmbiguous keys on.
func TestThrottleOverCapAbort_IsAmbiguousNotFailed(t *testing.T) {
	// 3600s: far beyond maxRetryWait, so the abort fires rather than a sleep.
	c := redditThrottleClient(t, "3600")

	err := c.UpdateCampaignStatus(context.Background(), "t5_abc123", "PAUSED")
	if err == nil {
		t.Fatal("expected the over-cap rate limit to abort")
	}
	if !createOutcomeAmbiguous(err) {
		t.Errorf("createOutcomeAmbiguous = false for an over-cap 429 abort; a possibly-applied mutation reads as definitely failed. err = %v", err)
	}
}
