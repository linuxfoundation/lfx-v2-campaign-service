// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		{"server-declared Retry-After", "2"},
		{"no header, exponential backoff", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfterS != "" {
					w.Header().Set("Retry-After", tc.retryAfterS)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
			}))
			defer srv.Close()

			// A deadline shorter than the backoff, so the sleep is what expires.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			c := NewClient(
				Credentials{AccessToken: "tok"},
				AccountConfig{AccountID: "act_777", CurrencyOffset: 100},
				WithBaseURL(srv.URL),
				WithClock(fixedMetaClock()),
			)

			// A status toggle rather than a create: it is the simplest MUTATING call that
			// reaches the transport with no input validation in front of it, so the 429 retry
			// loop is what the test actually exercises. The ambiguity contract is identical —
			// a toggle that may have applied must not be reported as definitely not applied.
			err := c.UpdateCampaignStatus(ctx, "23851234567890123", "PAUSED")
			if err == nil {
				t.Fatal("expected the throttled request to fail")
			}
			// THE ASSERTION THAT MATTERS. Not "did it error" — it always did — but whether the
			// caller is told the create may have committed.
			if !createOutcomeAmbiguous(err) {
				t.Errorf("createOutcomeAmbiguous = false for a cancelled 429 backoff; the caller would retry and double-create. err = %v", err)
			}
		})
	}
}
