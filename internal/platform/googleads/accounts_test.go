// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestListAccessibleCustomers_Success tests the happy path of discovering accessible accounts.
func TestListAccessibleCustomers_Success(t *testing.T) {
	tokenFetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route to token endpoint
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			tokenFetched = true
			return
		}

		// Verify the request properties for listAccessibleCustomers
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if !urlHasSuffix(r.URL.Path, "/customers:listAccessibleCustomers") {
			t.Fatalf("expected listAccessibleCustomers endpoint, got %s", r.URL.Path)
		}

		// Verify headers
		if r.Header.Get("Authorization") == "" {
			t.Fatal("missing Authorization header")
		}
		if r.Header.Get("developer-token") == "" {
			t.Fatal("missing developer-token header")
		}

		// Return mock customer list
		resp := listAccessibleCustomersResponse{
			ResourceNames: []string{
				"customers/1234567890",
				"customers/0987654321",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890", Label: "Test"},
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	ctx := context.Background()
	accounts, err := client.ListAccessibleCustomers(ctx)

	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if !tokenFetched {
		t.Fatal("token was not fetched")
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].ResourceName != "customers/1234567890" {
		t.Fatalf("expected first account ID 'customers/1234567890', got '%s'", accounts[0].ResourceName)
	}
	if accounts[1].ResourceName != "customers/0987654321" {
		t.Fatalf("expected second account ID 'customers/0987654321', got '%s'", accounts[1].ResourceName)
	}
}

// TestListAccessibleCustomers_EmptyList tests the case where there are no accessible accounts.
func TestListAccessibleCustomers_EmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		resp := listAccessibleCustomersResponse{ResourceNames: []string{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890"},
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	ctx := context.Background()
	accounts, err := client.ListAccessibleCustomers(ctx)

	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(accounts))
	}
}

// TestListAccessibleCustomers_APIError tests handling of API errors from listAccessibleCustomers.
func TestListAccessibleCustomers_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    403,
				"message": "Developer token is invalid",
			},
		})
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "invalid", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890"},
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	ctx := context.Background()
	accounts, err := client.ListAccessibleCustomers(ctx)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if accounts != nil {
		t.Fatalf("expected nil accounts on error, got %v", accounts)
	}
	if apiErr, ok := err.(*apiError); !ok || apiErr.StatusCode != 403 {
		t.Fatalf("expected 403 apiError, got %v", err)
	}
}

// TestListAccessibleCustomers_MalformedResponse tests handling of invalid responses.
func TestListAccessibleCustomers_MalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890"},
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	ctx := context.Background()
	accounts, err := client.ListAccessibleCustomers(ctx)

	if err == nil {
		t.Fatal("expected error for malformed response, got nil")
	}
	if accounts != nil {
		t.Fatalf("expected nil accounts on error, got %v", accounts)
	}
	if _, ok := err.(*transportError); !ok {
		t.Fatalf("expected transportError for malformed response, got %T", err)
	}
}

// newAccountsTestClient builds a client pointed at srv for both the API and token
// endpoints, with a frozen clock so the token cache is deterministic.
func newAccountsTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890", Label: "Test"},
		WithBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)
}

// writeAccountsToken serves the OAuth token endpoint. Returns true when it handled r.
func writeAccountsToken(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/token" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": "mock_token",
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
	return true
}

// TestListAccessibleCustomers_SendsNoRequestBody asserts the discovery POST carries no
// request body. The endpoint is account-agnostic and takes no payload; sending one (or a
// stray Content-Type announcing one) would be a protocol error the Google Ads API may
// reject. doRequest omits Content-Type exactly when the body is nil, so this also pins
// that branch of doRequest for the nil-body caller.
func TestListAccessibleCustomers_SendsNoRequestBody(t *testing.T) {
	var (
		gotBody        []byte
		gotContentType string
		sawAPICall     bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		sawAPICall = true
		gotContentType = r.Header.Get("Content-Type")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/1"}})
	}))
	defer server.Close()

	if _, err := newAccountsTestClient(t, server).ListAccessibleCustomers(context.Background()); err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if !sawAPICall {
		t.Fatal("the API endpoint was never called")
	}
	if len(gotBody) != 0 {
		t.Errorf("expected an empty request body, got %d bytes: %q", len(gotBody), gotBody)
	}
	if gotContentType != "" {
		t.Errorf("expected no Content-Type on a bodiless request, got %q", gotContentType)
	}
}

// TestListAccessibleCustomers_APIErrorCarriesStatusAndCodes pins the non-2xx
// classification: the caller must receive an *apiError carrying the upstream status and
// the parsed Google Ads error codes, not an opaque error. Without this, a 403
// (bad developer token) is indistinguishable from a 500 at the call site.
func TestListAccessibleCustomers_APIErrorCarriesStatusAndCodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"denied","details":[{"errors":[{"errorCode":{"authorizationError":"DEVELOPER_TOKEN_NOT_APPROVED"}}]}]}}`))
	}))
	defer server.Close()

	_, err := newAccountsTestClient(t, server).ListAccessibleCustomers(context.Background())
	if err == nil {
		t.Fatal("expected an error for a 403 response, got nil")
	}

	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected the error to unwrap to *apiError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected StatusCode 403, got %d", apiErr.StatusCode)
	}
	if apiErr.Method != http.MethodPost {
		t.Errorf("expected Method POST, got %q", apiErr.Method)
	}
	if !strings.Contains(apiErr.Path, "listAccessibleCustomers") {
		t.Errorf("expected Path to name the endpoint, got %q", apiErr.Path)
	}
	// The error text must name the platform so a caller logging it can tell which
	// upstream failed without inspecting the concrete type.
	if !strings.Contains(apiErr.Error(), "google-ads") {
		t.Errorf("expected the message to identify the platform, got %q", apiErr.Error())
	}
}

// TestListAccessibleCustomers_TransportErrorUnwraps pins the post-send failure path: the
// caller gets a *transportError whose Unwrap exposes the underlying cause, so
// errors.Is/As against transport sentinels still work through the wrapper. A 2xx body that
// cannot be decoded is deliberately classified this way (the request DID reach Google).
func TestListAccessibleCustomers_TransportErrorUnwraps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	_, err := newAccountsTestClient(t, server).ListAccessibleCustomers(context.Background())
	if err == nil {
		t.Fatal("expected an error for an undecodable 2xx body, got nil")
	}

	var tErr *transportError
	if !errors.As(err, &tErr) {
		t.Fatalf("expected the error to unwrap to *transportError, got %T: %v", err, err)
	}
	if tErr.Unwrap() == nil {
		t.Fatal("transportError.Unwrap returned nil; the underlying cause must stay reachable")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("expected the JSON decode cause to remain reachable through Unwrap, got %v", err)
	}
}

// TestListAccessibleCustomers_PreSendDialErrorIsNotTransportError pins the pre-send
// distinction. isPreSendDialError separates "the request never left this process" from
// "the request reached Google and something failed after". Only the latter is a
// transportError; conflating them would let a caller treat an unsent request as
// possibly-applied. Discovery is a read, but the classification is shared with the
// mutating paths where the distinction decides whether an outcome is ambiguous.
func TestListAccessibleCustomers_PreSendDialErrorIsNotTransportError(t *testing.T) {
	// A server that is closed before use gives a connection-refused dial error on a
	// port with no listener.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := server.URL
	server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890", Label: "Test"},
		WithBaseURL(deadURL),
		// Token endpoint must succeed so the failure is the API dial, not token fetch.
		WithTokenURL(tokenOnlyServer(t).URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	_, err := client.ListAccessibleCustomers(context.Background())
	if err == nil {
		t.Fatal("expected an error dialing a closed port, got nil")
	}

	var tErr *transportError
	if errors.As(err, &tErr) {
		t.Fatalf("a pre-send dial failure must NOT be classified as *transportError, got %v", err)
	}
	if !strings.Contains(err.Error(), "google-ads") {
		t.Errorf("expected the message to identify the platform, got %q", err.Error())
	}
}

// tokenOnlyServer serves just the OAuth token endpoint, for tests whose API endpoint is
// deliberately unreachable.
func tokenOnlyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAccountsToken(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// urlHasSuffix is a helper to check URL path suffix regardless of API version.
func urlHasSuffix(path string, suffix string) bool {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i:] == suffix {
			return true
		}
	}
	return false
}
