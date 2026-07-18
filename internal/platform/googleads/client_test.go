// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestIsPreSendDialError(t *testing.T) {
	// A plain error (not a dial/DNS failure) must NOT be classified pre-send.
	if isPreSendDialError(errors.New("some mid-flight error")) {
		t.Error("a generic error must not be pre-send")
	}
	if isPreSendDialError(fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF)) {
		t.Error("unexpected EOF (mid-flight) must not be pre-send")
	}
}
