// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// retryThen stands up a token server and an API server that answers the FIRST request with a
// retryable 429 and every later one with the supplied status and body. That is the exact shape
// #347 is about: the first attempt was received by Google and may have been applied, and the
// attempt that is finally reported on is a different one.
func retryThen(t *testing.T, laterStatus int, laterBody string) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	var mu sync.Mutex
	var attempts int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.WriteHeader(laterStatus)
		_, _ = io.WriteString(w, laterBody)
	}))
	t.Cleanup(apiSrv.Close)

	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

// alwaysStatus answers every API request with the same status and body — no 429, so no retry.
// This is the counter-case: an unretried definite refusal must stay definite.
func alwaysStatus(t *testing.T, status int, body string) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

const definite400 = `{"error":{"code":400,"message":"invalid","details":[{"errors":[{"errorCode":{"fieldError":"REQUIRED"},"message":"required"}]}]}}`

// TestRetriedMutateFailureIsUnconfirmed is the #347 regression. Each of these mutates is
// IDEMPOTENT and therefore retried on a 429, and before this fix the error returned after a retry
// carried nothing to say an earlier attempt had been received. A 400 is a DEFINITE refusal of the
// attempt that earned it, so every one of these was reported as "the platform was not modified" —
// false whenever the rate-limited first attempt had in fact been applied, and on the budget and
// bid levers false about money.
//
// Each subtest runs the same mutate twice: once where a 429 precedes the 400 (must be
// unconfirmed) and once where the 400 is the only answer (must stay definite). The second half is
// what proves the fix did not simply make every failure ambiguous, which would be its own defect —
// an operator told to "verify upstream" after a flat validation refusal learns nothing.
func TestRetriedMutateFailureIsUnconfirmed(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
	}{
		{
			name: "campaign budget amount update",
			call: func(c *Client) error {
				return c.UpdateCampaignBudget(context.Background(), "99", 5_000_000, budgetPeriodDaily)
			},
		},
		{
			name: "ad group cpc bid update",
			call: func(c *Client) error {
				return c.UpdateAdGroupCPCBid(context.Background(), "654", 1_000_000)
			},
		},
		{
			name: "campaign status update",
			call: func(c *Client) error {
				return c.UpdateCampaignStatus(context.Background(), "321", StatusPaused)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+": a 400 after a retried 429 is unconfirmed", func(t *testing.T) {
			err := tc.call(retryThen(t, http.StatusBadRequest, definite400))
			if err == nil {
				t.Fatal("want the 400 to surface as an error")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("IsOutcomeUnconfirmed = false, want true: the 429'd first attempt may have been applied\nerr: %v", err)
			}
			// The operator has to be able to read WHY it is unconfirmed, and the underlying
			// refusal must stay reachable for logging and for errors.As on *apiError.
			if !strings.Contains(err.Error(), "rate-limited") {
				t.Errorf("error text does not explain the retry: %v", err)
			}
			var ae *apiError
			if !errors.As(err, &ae) {
				t.Errorf("the final apiError must stay reachable through Unwrap, got %v", err)
			} else if ae.StatusCode != http.StatusBadRequest {
				t.Errorf("unwrapped StatusCode = %d, want 400", ae.StatusCode)
			}
		})

		t.Run(tc.name+": an unretried 400 stays definite", func(t *testing.T) {
			err := tc.call(alwaysStatus(t, http.StatusBadRequest, definite400))
			if err == nil {
				t.Fatal("want the 400 to surface as an error")
			}
			if IsOutcomeUnconfirmed(err) {
				t.Errorf("IsOutcomeUnconfirmed = true, want false: nothing was retried, so the 400 answers the only request that was sent\nerr: %v", err)
			}
		})
	}
}

// TestRetriedMutateSuccessIsNotMarked pins the other boundary: a mutate that SUCCEEDS after a
// retried 429 is confirmed, not ambiguous. The attempt being reported on is the one that
// succeeded, and marking it would send an operator to verify a write Google plainly acknowledged.
func TestRetriedMutateSuccessIsNotMarked(t *testing.T) {
	c := retryThen(t, http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/adGroups/654"}]}`)
	if err := c.UpdateAdGroupCPCBid(context.Background(), "654", 1_000_000); err != nil {
		t.Fatalf("a throttled-then-successful bid update must return nil: %v", err)
	}
}

// TestRetriedUnconfirmedDoesNotDoubleWrap pins retriedUnconfirmed's third clause. An error that
// ALREADY reports itself ambiguous — by shape (a 5xx) or through one of this package's explicit
// markers — is returned as it is. Without this the error an operator reads would be buried under a
// second layer saying the same thing, and the #384 marker on the budget-alignment path would be
// wrapped out of the shape CreateOutcome's carriesUnconfirmedWrite arm reads.
func TestRetriedUnconfirmedDoesNotDoubleWrap(t *testing.T) {
	already := &unconfirmedBudgetMutateError{err: errors.New("accepted but unacknowledged")}
	if got := retriedUnconfirmed("budget", 2, already); got != error(already) {
		t.Errorf("an already-unconfirmed error must pass through unwrapped, got %T", got)
	}

	ambiguousByShape := &apiError{StatusCode: http.StatusInternalServerError, Method: http.MethodPost, Path: "p", Body: "boom"}
	if got := retriedUnconfirmed("budget", 1, ambiguousByShape); got != error(ambiguousByShape) {
		t.Errorf("a 5xx is already ambiguous by shape and must pass through, got %T", got)
	}

	if got := retriedUnconfirmed("budget", 0, errors.New("plain")); got == nil {
		t.Error("a zero-retry error must be returned, not swallowed")
	} else if IsOutcomeUnconfirmed(got) {
		t.Error("a zero-retry error must not be marked unconfirmed")
	}

	if retriedUnconfirmed("budget", 3, nil) != nil {
		t.Error("a nil error must stay nil however many attempts were retried")
	}
}
