// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func testCreds() Credentials {
	return Credentials{
		ClientID:       "cid",
		ClientSecret:   "csecret",
		DeveloperToken: "devtok",
		RefreshToken:   "rtok",
	}
}

func testAccount() AccountConfig {
	return AccountConfig{CustomerID: "1234567890", Label: "Test"}
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// tokenHandler answers the OAuth2 token endpoint with a valid access token.
func tokenHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
}

func TestAccessToken_RefreshAndCache(t *testing.T) {
	var tokenCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("token request Content-Type = %q", r.Header.Get("Content-Type"))
		}
		tokenHandler(w, r)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))

	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if tok != "at-123" {
		t.Errorf("token = %q, want at-123", tok)
	}
	// Second call within expiry must reuse the cached token (no new HTTP call).
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("second token: %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 (cached)", got)
	}
}

func TestAccessToken_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.accessTokenValue(context.Background()); err == nil {
		t.Fatal("expected an error on a 401 token response, got nil")
	}
}

// TestAccessToken_ErrorDoesNotLeakSecrets verifies the token-refresh error path
// never echoes the OAuth response body — which carried the client_secret and
// refresh_token — into the returned error. This error can be persisted into a
// campaign's Steps, so a leak would be durable, not transient.
func TestAccessToken_ErrorDoesNotLeakSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// A hostile/diagnostic body reflecting credential material.
		_, _ = io.WriteString(w, `{"error":"invalid_grant","echo":"csecret rtok"}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error on a 401 token response, got nil")
	}
	for _, secret := range []string{"csecret", "rtok", "invalid_grant"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("token error leaked %q: %q", secret, err.Error())
		}
	}
}

func TestAccessToken_EmptyTokenIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"","expires_in":3600}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.accessTokenValue(context.Background()); err == nil {
		t.Fatal("expected an error for an empty access_token, got nil")
	}
}

// twoServer wires a token server and an API server so a full doRequest/gaqlSearch
// can run. The API handler is supplied by the test.
func twoServer(t *testing.T, apiHandler http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(apiHandler)
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
}

func TestDoRequest_SendsAuthAndDeveloperTokenHeaders(t *testing.T) {
	var gotAuth, gotDev, gotLogin string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDev = r.Header.Get("developer-token")
		gotLogin = r.Header.Get("login-customer-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.doRequest(context.Background(), http.MethodGet, "customers/1234567890/x", nil, false); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotAuth != "Bearer at-123" {
		t.Errorf("Authorization = %q, want Bearer at-123", gotAuth)
	}
	if gotDev != "devtok" {
		t.Errorf("developer-token = %q, want devtok", gotDev)
	}
	// No login-customer-id configured → header must be absent.
	if gotLogin != "" {
		t.Errorf("login-customer-id = %q, want empty", gotLogin)
	}
}

func TestDoRequest_SendsLoginCustomerIDWhenSet(t *testing.T) {
	var gotLogin string
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLogin = r.Header.Get("login-customer-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()

	acct := testAccount()
	acct.LoginCustomerID = "9999999999"
	c := NewClient(testCreds(), acct, WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	if _, err := c.doRequest(context.Background(), http.MethodGet, "customers/1234567890/x", nil, false); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotLogin != "9999999999" {
		t.Errorf("login-customer-id = %q, want 9999999999", gotLogin)
	}
}

func TestDoRequest_Non2xxIsAPIError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1234567890/googleAds:search", searchRequest{Query: "SELECT campaign.id FROM campaign"}, false)
	if err == nil {
		t.Fatal("expected an error on a 400 response, got nil")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("want *apiError, got %T: %v", err, err)
	}
	if ae.StatusCode != http.StatusBadRequest {
		t.Errorf("apiError.StatusCode = %d, want 400", ae.StatusCode)
	}
	// The upstream body must NOT be echoed into the error string.
	if strings.Contains(ae.Error(), "bad") {
		t.Errorf("apiError.Error() leaked the upstream body: %q", ae.Error())
	}
}

func TestDoRequest_MidflightFailureIsAmbiguous(t *testing.T) {
	// A 2xx with a body that can't be decoded downstream is exercised via
	// gaqlSearch below; here we force a transport failure by pointing at a closed
	// server so Do fails after DNS/dial — but to keep it deterministic we instead
	// close the connection mid-response.
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare a large body then write nothing and hijack-close to force a read
		// failure on a 2xx (ambiguous).
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1234567890/googleAds:search", searchRequest{Query: "x"}, false)
	if err == nil {
		t.Fatal("expected an error on a truncated 2xx, got nil")
	}
	var te *transportError
	if !errors.As(err, &te) {
		t.Errorf("a truncated 2xx read must be ambiguous (transportError), got %T: %v", err, err)
	}
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var followed bool
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			followed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1234567890/googleAds:search", searchRequest{Query: "x"}, false)
	if err == nil {
		t.Fatal("expected a 3xx to surface as an error, got nil")
	}
	if followed {
		t.Error("client followed the redirect — it must hand the 3xx back instead")
	}
}

// TestClient_OverridesInjectedCheckRedirectWithoutMutatingCaller verifies that an
// *http.Client supplied via WithHTTPClient has its CheckRedirect force-overridden
// to no-follow (a correctness requirement the ambiguity contract depends on), and
// that the override is applied to a copy so the caller's client is not mutated.
func TestClient_OverridesInjectedCheckRedirectWithoutMutatingCaller(t *testing.T) {
	var followed bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			followed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	defer apiSrv.Close()

	// A caller-supplied client that WOULD follow redirects (CheckRedirect nil).
	caller := &http.Client{}
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		WithHTTPClient(caller))

	if _, err := c.doRequest(context.Background(), http.MethodPost, "customers/1234567890/googleAds:search", searchRequest{Query: "x"}, false); err == nil {
		t.Fatal("expected a 3xx to surface as an error with the injected client, got nil")
	}
	if followed {
		t.Error("injected client followed the redirect — NewClient must override CheckRedirect")
	}
	if caller.CheckRedirect != nil {
		t.Error("caller's *http.Client CheckRedirect was mutated — the override must use a shallow copy")
	}
}

// TestWithAPIVersion_ReachesPath verifies the configured API version is the URL
// version segment, so a bad bump is caught by a test rather than the live API.
func TestWithAPIVersion_ReachesPath(t *testing.T) {
	var gotPath string
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		WithAPIVersion("v99"))
	if _, err := c.doRequest(context.Background(), http.MethodGet, "customers/1/x", nil, false); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if !strings.HasPrefix(gotPath, "/v99/") {
		t.Errorf("request path = %q, want /v99/ prefix", gotPath)
	}
}

// TestDoRequest_IdempotentRetriesOn429 verifies a rate-limited idempotent call
// (e.g. a GAQL :search read) is retried and eventually succeeds, honoring
// Retry-After.
func TestDoRequest_IdempotentRetriesOn429(t *testing.T) {
	var calls int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	if _, err := c.doRequest(context.Background(), http.MethodPost, "customers/1/googleAds:search", searchRequest{Query: "x"}, true); err != nil {
		t.Fatalf("idempotent 429 should retry and succeed, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server saw %d calls, want 3 (two 429s then success)", got)
	}
}

// TestDoRequest_NonIdempotentDoesNotRetryOn429 verifies a rate-limited
// NON-idempotent call (a :mutate create) is NOT retried — a 429 whose first
// attempt may have committed must not be blind-resent.
func TestDoRequest_NonIdempotentDoesNotRetryOn429(t *testing.T) {
	var calls int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1/campaigns:mutate", map[string]any{"x": 1}, false)
	if err == nil {
		t.Fatal("expected a 429 error for a non-idempotent call, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("non-idempotent call retried: server saw %d calls, want 1", got)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want a 429 apiError, got %T: %v", err, err)
	}
}

// TestDoRequest_Idempotent429ExhaustsRetries verifies an idempotent call that is
// rate-limited on every attempt gives up after retryMax and surfaces a 429.
func TestDoRequest_Idempotent429ExhaustsRetries(t *testing.T) {
	var calls int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1/googleAds:search", searchRequest{Query: "x"}, true)
	if err == nil {
		t.Fatal("expected a 429 error after exhausting retries, got nil")
	}
	// retryMax retries + the initial attempt = retryMax+1 total.
	if got := atomic.LoadInt32(&calls); got != int32(retryMax+1) {
		t.Errorf("server saw %d calls, want %d (initial + retryMax)", got, retryMax+1)
	}
}

// TestDoRequest_OverCapRetryAfterAborts verifies that a 429 whose Retry-After
// exceeds maxRetryWait ABORTS immediately (mirroring meta/reddit/twitter) rather
// than clamping and burning retries while the account is still throttled. The
// error reports the raw over-cap header.
func TestDoRequest_OverCapRetryAfterAborts(t *testing.T) {
	var calls int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "600") // 10 min, well over maxRetryWait (60s)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	_, err := c.doRequest(context.Background(), http.MethodPost, "customers/1/googleAds:search", searchRequest{Query: "x"}, true)
	if err == nil {
		t.Fatal("expected an over-cap Retry-After to abort, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("over-cap 429 should abort after one call, server saw %d", got)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want a 429 apiError, got %T: %v", err, err)
	}
	if !strings.Contains(ae.Body, "600") {
		t.Errorf("abort error should report the raw Retry-After (600), got: %q", ae.Body)
	}
}

func TestGAQLSearch_PaginatesToExhaustion(t *testing.T) {
	var page int32
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		// GAQL search MUST be POST (Google Ads REST rejects GET on :search). Assert
		// it here so a future edit that flips the method fails a test, not only the
		// live API.
		if r.Method != http.MethodPost {
			t.Errorf("gaqlSearch method = %s, want POST", r.Method)
		}
		var req searchRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		n := atomic.AddInt32(&page, 1)
		switch n {
		case 1:
			if req.PageToken != "" {
				t.Errorf("page 1 should have no pageToken, got %q", req.PageToken)
			}
			_, _ = io.WriteString(w, `{"results":[{"a":1},{"a":2}],"nextPageToken":"tok2"}`)
		case 2:
			if req.PageToken != "tok2" {
				t.Errorf("page 2 pageToken = %q, want tok2", req.PageToken)
			}
			_, _ = io.WriteString(w, `{"results":[{"a":3}],"nextPageToken":""}`)
		default:
			t.Errorf("unexpected extra page %d", n)
		}
	})
	rows, err := c.gaqlSearch(context.Background(), "SELECT campaign.id FROM campaign")
	if err != nil {
		t.Fatalf("gaqlSearch: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows across pages, want 3", len(rows))
	}
}

func TestGAQLSearch_RepeatedPageTokenAborts(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Always returns the same non-empty token → would loop forever.
		_, _ = io.WriteString(w, `{"results":[{"a":1}],"nextPageToken":"stuck"}`)
	})
	_, err := c.gaqlSearch(context.Background(), "SELECT campaign.id FROM campaign")
	if err == nil {
		t.Fatal("expected an error when the server repeats a page token, got nil")
	}
	if !strings.Contains(err.Error(), "repeated page token") {
		t.Errorf("error should explain the repeated token, got: %v", err)
	}
}

// TestGAQLSearch_AggregateRowCapAborts verifies the row cap trips: with a small
// maxSearchRows, a query that keeps paging past it aborts rather than retaining an
// unbounded result set.
func TestGAQLSearch_AggregateRowCapAborts(t *testing.T) {
	orig := maxSearchRows
	maxSearchRows = 3
	t.Cleanup(func() { maxSearchRows = orig })

	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 2 rows per page + an endless cursor → exceeds 3 rows on page 2.
		_, _ = io.WriteString(w, `{"results":[{"a":1},{"a":2}],"nextPageToken":"next"}`)
	})
	_, err := c.gaqlSearch(context.Background(), "SELECT campaign.id FROM campaign")
	if err == nil {
		t.Fatal("expected an error when the result set exceeds the row cap, got nil")
	}
	if !strings.Contains(err.Error(), "rows") {
		t.Errorf("error should mention the row cap, got: %v", err)
	}
}

// TestGAQLSearch_AggregateByteCapAborts verifies the byte cap trips on the FULL
// page payload (so it also bounds large nextPageToken strings, not just rows).
func TestGAQLSearch_AggregateByteCapAborts(t *testing.T) {
	origBytes, origRows := maxSearchBytes, maxSearchRows
	maxSearchBytes = 1 << 10  // 1 KiB
	maxSearchRows = 1_000_000 // keep rows out of the way; byte cap should trip first
	t.Cleanup(func() { maxSearchBytes, maxSearchRows = origBytes, origRows })

	// A large nextPageToken (few rows) — the byte cap must still catch it because it
	// counts the whole page payload.
	bigToken := strings.Repeat("x", 4096)
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"a":1}],"nextPageToken":"`+bigToken+`"}`)
	})
	_, err := c.gaqlSearch(context.Background(), "SELECT campaign.id FROM campaign")
	if err == nil {
		t.Fatal("expected an error when the accumulated payload exceeds the byte cap, got nil")
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error should mention the byte cap, got: %v", err)
	}
}

func TestIsPreSendDialError(t *testing.T) {
	// A plain error (not a dial/DNS failure) must NOT be classified pre-send.
	if isPreSendDialError(errors.New("some mid-flight error")) {
		t.Error("a generic error must not be pre-send")
	}
	if isPreSendDialError(fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF)) {
		t.Error("unexpected EOF (mid-flight) must not be pre-send")
	}
}

// TestDoRequest_RejectsMalformedCustomerID verifies a non-digits customer id
// (dashed, padded, or containing path-altering characters) is rejected before any
// request is built, rather than concatenated into the URL.
func TestDoRequest_RejectsMalformedCustomerID(t *testing.T) {
	cases := []string{"123-456-7890", " 123 ", "123/456", "123.456", "abc", ""}
	for _, cid := range cases {
		t.Run(cid, func(t *testing.T) {
			acct := testAccount()
			acct.CustomerID = cid
			// No servers needed — validation happens before any network call.
			c := NewClient(testCreds(), acct, WithClock(fixedClock()))
			_, err := c.doRequest(context.Background(), http.MethodGet, "customers/x/y", nil, false)
			if err == nil {
				t.Fatalf("expected a validation error for customer id %q, got nil", cid)
			}
			if !strings.Contains(err.Error(), "customer id") {
				t.Errorf("error should name the invalid customer id, got: %v", err)
			}
		})
	}

	// A malformed LoginCustomerID is also rejected.
	acct := testAccount()
	acct.LoginCustomerID = "999-888-777"
	c := NewClient(testCreds(), acct, WithClock(fixedClock()))
	if _, err := c.doRequest(context.Background(), http.MethodGet, "customers/x/y", nil, false); err == nil {
		t.Fatal("expected a validation error for a dashed login-customer-id, got nil")
	}
}

// TestAccessToken_ConcurrentSingleFlight verifies the token single-flight: with
// the token endpoint blocked, N concurrent callers trigger exactly ONE refresh, a
// cancelled waiter returns WHILE the shared refresh is STILL BLOCKED (proving it
// honors its own ctx and doesn't wait for the token), the shared refresh is NOT
// torn down by that cancellation, and the remaining callers still receive the
// token once it's released. Run under -race.
//
// The cancelled waiter's completion is asserted BEFORE `release` is closed — a
// waiter that (incorrectly) ignored cancellation and blocked on the token would
// still be running at that point and fail the assertion.
func TestAccessToken_ConcurrentSingleFlight(t *testing.T) {
	var tokenCalls int32
	handlerEntered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		select {
		case handlerEntered <- struct{}{}:
		default:
		}
		<-release // block until the test releases the refresh
		tokenHandler(w, nil)
	}))
	// Unblock the handler on ANY exit path (incl. a t.Fatal) BEFORE srv.Close():
	// defers run LIFO, so this runs first. Otherwise a failing assertion would leave
	// the handler stuck on <-release and srv.Close() would wait forever, hanging the
	// whole suite. releaseOnce makes this safe alongside the deliberate release below.
	defer srv.Close()
	defer releaseRefresh()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))

	// Leader: a normal caller that will become the single-flight leader and block
	// in the handler.
	leaderDone := make(chan result, 1)
	go func() {
		tok, err := c.accessTokenValue(context.Background())
		leaderDone <- result{tok, err}
	}()

	// Wait until the handler is actually blocked (the refresh is in flight) before
	// launching the cancelled waiter — so the waiter genuinely joins the in-flight
	// refresh rather than starting its own.
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("token handler never entered — leader didn't start the refresh")
	}

	// Cancelled waiter: joins the in-flight refresh, then is cancelled. It must
	// return BEFORE we release the refresh.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelledDone := make(chan error, 1)
	go func() {
		_, err := c.accessTokenValue(cancelCtx)
		cancelledDone <- err
	}()
	// Give the waiter a moment to register on the shared refresh, then cancel it.
	time.Sleep(20 * time.Millisecond)
	cancel()

	// The cancelled waiter must complete NOW — while the refresh is still blocked.
	select {
	case err := <-cancelledDone:
		if err == nil {
			t.Error("cancelled waiter returned nil error; want its context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return while the refresh was still blocked — it ignored cancellation")
	}

	// The refresh must still be alive (not torn down by the cancellation): more
	// followers can still join and succeed.
	const followers = 6
	var wg sync.WaitGroup
	toks := make([]string, followers)
	errs := make([]error, followers)
	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			toks[idx], errs[idx] = c.accessTokenValue(context.Background())
		}(i)
	}
	time.Sleep(20 * time.Millisecond)

	// Now release the shared refresh; the leader and all followers get the token.
	// (releaseRefresh is idempotent via sync.Once, so the deferred safety-release
	// on exit is a no-op after this.)
	releaseRefresh()
	wg.Wait()

	lr := <-leaderDone
	if lr.err != nil || lr.token != "at-123" {
		t.Errorf("leader: token=%q err=%v, want at-123/nil", lr.token, lr.err)
	}
	for i := 0; i < followers; i++ {
		if errs[i] != nil || toks[i] != "at-123" {
			t.Errorf("follower %d: token=%q err=%v, want at-123/nil", i, toks[i], errs[i])
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (single-flight)", got)
	}
}

type result struct {
	token string
	err   error
}

// TestIsPreSendDialError_PositiveCases verifies the pre-send classification's TRUE
// branch: a DNS failure and a connect-time dial failure (refused / no route /
// net unreachable) are pre-send; a dial error with a different syscall is not.
func TestIsPreSendDialError_PositiveCases(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dns error", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"dial host unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, true},
		{"dial net unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, true},
		{"wrapped dns error", fmt.Errorf("do: %w", &net.DNSError{Err: "nx", Name: "x"}), true},
		{"read op (not dial)", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{"dial with other syscall", &net.OpError{Op: "dial", Err: syscall.EPIPE}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPreSendDialError(tc.err); got != tc.want {
				t.Errorf("isPreSendDialError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
