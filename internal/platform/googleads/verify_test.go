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
	"sync/atomic"
	"testing"
)

// verifyClient builds a client whose API calls hit apiHandler and whose token calls succeed.
func verifyClient(t *testing.T, apiHandler http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(apiHandler)
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
}

// TestVerifyCredential_IsAccountScopedAndReadOnly is the most important test here. A probe
// that is not account-scoped would PASS for a credential pointed at the wrong ad account —
// exactly the misconfiguration verification exists to catch. And a probe that mutates could
// alter a paid resource.
func TestVerifyCredential_IsAccountScopedAndReadOnly(t *testing.T) {
	var gotPath, gotQuery string
	c := verifyClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotQuery = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"customer":{"id":"1234567890"}}]}`)
	})

	if err := c.VerifyCredential(context.Background()); err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	// The configured customer id MUST appear in the request path.
	if !strings.Contains(gotPath, "customers/1234567890/") {
		t.Errorf("probe path %q is not scoped to the configured customer id; a tenant-scoped probe would pass for a credential pointed at the WRONG account", gotPath)
	}
	// It must be the read-only search endpoint, never a mutate.
	if !strings.Contains(gotPath, "googleAds:search") {
		t.Errorf("probe path = %q, want the read-only googleAds:search endpoint", gotPath)
	}
	if strings.Contains(gotPath, ":mutate") {
		t.Fatalf("probe hit a MUTATING endpoint (%q); verification must never alter a paid resource", gotPath)
	}
	if !strings.Contains(gotQuery, "FROM customer") {
		t.Errorf("probe query = %q, want a read of the customer resource", gotQuery)
	}
}

// TestVerifyCredential_RejectionStatusesAreDefinitive pins which upstream statuses count as a
// DEFINITE credential rejection. Getting this wrong in either direction is harmful: too broad
// sends an operator to re-authenticate during an outage; too narrow hides a real bad token.
func TestVerifyCredential_RejectionStatusesAreDefinitive(t *testing.T) {
	rejecting := []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	}
	for _, code := range rejecting {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c := verifyClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			})
			err := c.VerifyCredential(context.Background())
			if err == nil {
				t.Fatalf("status %d produced no error", code)
			}
			if !CredentialRejected(err) {
				t.Errorf("status %d not classified as a credential rejection; the operator would never be told to re-authenticate", code)
			}
		})
	}
}

// TestVerifyCredential_AmbiguousStatusesAreNotRejections is the counterpart, and guards the
// dangerous direction: a provider outage must NEVER be reported as a bad credential.
func TestVerifyCredential_AmbiguousStatusesAreNotRejections(t *testing.T) {
	ambiguous := []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
	for _, code := range ambiguous {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c := verifyClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream"}}`)
			})
			err := c.VerifyCredential(context.Background())
			if err == nil {
				t.Fatalf("status %d produced no error", code)
			}
			if CredentialRejected(err) {
				t.Errorf("status %d classified as a CREDENTIAL REJECTION; a provider outage would send an operator to re-authenticate a working credential", code)
			}
		})
	}
}

// TestVerifyCredential_ThrottlingIsNotARejection pins 429 specifically: Google throttles under
// normal use, so treating it as a rejection would intermittently declare good credentials bad.
func TestVerifyCredential_ThrottlingIsNotARejection(t *testing.T) {
	var calls int32
	c := verifyClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "9999") // exceeds maxRetryWait -> aborts rather than sleeping
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})

	err := c.VerifyCredential(context.Background())
	if err == nil {
		t.Fatal("throttled probe produced no error")
	}
	if CredentialRejected(err) {
		t.Error("429 classified as a credential rejection; ordinary Google throttling would declare a good credential invalid")
	}
}

// TestCredentialRejected_AmbiguityGuardBeatsTheStatusCode is the test that pins the
// ambiguity check itself, rather than the status list.
//
// It exists because the status-list tests above CANNOT catch a removal of that check: 5xx and
// 429 are already absent from the rejection list, so deleting the guard leaves them
// classified correctly by luck. The guard only does load-bearing work when an AMBIGUOUS error
// WRAPS a status that IS in the rejection list — a transport failure carrying a 4xx. Then, and
// only then, does the ordering decide the verdict, and reporting it as `invalid` would send an
// operator to re-authenticate over what was really a network failure.
func TestCredentialRejected_AmbiguityGuardBeatsTheStatusCode(t *testing.T) {
	for _, code := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			// Sanity: bare, this status IS a definite rejection.
			bare := &apiError{StatusCode: code, Method: http.MethodPost, Path: "p"}
			if !CredentialRejected(bare) {
				t.Fatalf("precondition failed: bare %d should be a rejection", code)
			}
			// Wrapped in an ambiguous transport failure, it must NOT be.
			wrapped := &transportError{Method: http.MethodPost, Path: "p", Err: bare}
			if CredentialRejected(wrapped) {
				t.Errorf("a transport failure wrapping %d was classified as a credential rejection; the ambiguity check must take precedence over the status code, or a network failure sends an operator to re-authenticate", code)
			}
		})
	}
}

// TestCredentialRejected_NilAndNonAPIErrors pins that errors carrying no evidence about the
// credential are not rejections.
func TestCredentialRejected_NilAndNonAPIErrors(t *testing.T) {
	if CredentialRejected(nil) {
		t.Error("nil error classified as a rejection")
	}
	if CredentialRejected(errors.New("some local failure")) {
		t.Error("a non-API error classified as a rejection; it carries no evidence about the credential")
	}
	if CredentialRejected(context.Canceled) {
		t.Error("context cancellation classified as a credential rejection")
	}
}

// TestVerifyCredential_MalformedCustomerIDIsLocalNotRejection pins that a locally-invalid
// customer id is caught BEFORE any network call and is not blamed on Google.
func TestVerifyCredential_MalformedCustomerIDIsLocalNotRejection(t *testing.T) {
	var called int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), AccountConfig{CustomerID: "123-456-7890"}, // dashes: invalid
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	err := c.VerifyCredential(context.Background())
	if err == nil {
		t.Fatal("a malformed customer id produced no error")
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Error("a request was sent despite a locally-invalid customer id")
	}
	if CredentialRejected(err) {
		t.Error("a locally-malformed customer id was blamed on Google as a credential rejection")
	}
}
