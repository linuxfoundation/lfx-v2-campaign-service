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
	"sync"
	"testing"
	"time"
)

// TestListAccessibleCustomers_Success tests the happy path of discovering accessible accounts.
func TestListAccessibleCustomers_Success(t *testing.T) {
	// Everything the handler observes is captured under a mutex and asserted on the
	// TEST goroutine after the call returns. Two reasons: the handler runs on the
	// httptest server's goroutine, so unsynchronized capture is a data race the moment
	// a retry or a lingering keep-alive overlaps the read; and t.Fatal* is only legal
	// on the goroutine running the test — calling it from a handler skips the assertion
	// silently instead of failing.
	var (
		mu           sync.Mutex
		tokenFetched bool
		gotMethod    string
		gotPath      string
		gotAuth      string
		gotDevToken  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			mu.Lock()
			tokenFetched = true
			mu.Unlock()
			return
		}

		mu.Lock()
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotDevToken = r.Header.Get("developer-token")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{
			ResourceNames: []string{"customers/1234567890", "customers/0987654321"},
		})
	}))
	defer server.Close()

	client := newAccountsTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !tokenFetched {
		t.Fatal("token was not fetched")
	}
	// GET, not POST: CustomerService.ListAccessibleCustomers is bound to GET in the
	// Google Ads REST API and takes no body. Pinning the verb here is what stops the
	// :search/:mutate POST habit from silently reappearing and 404ing in production.
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if !urlHasSuffix(gotPath, "/customers:listAccessibleCustomers") {
		t.Errorf("expected listAccessibleCustomers endpoint, got %s", gotPath)
	}
	if gotAuth == "" {
		t.Error("missing Authorization header")
	}
	if gotDevToken == "" {
		t.Error("missing developer-token header")
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
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{}})
	}))
	defer server.Close()

	client := newAccountsTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
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
		if writeAccountsToken(w, r) {
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

	accounts, err := client.ListAccessibleCustomers(context.Background())
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
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	client := newAccountsTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
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

// newManagerTestClient is newAccountsTestClient with a manager (MCC) account instead of a
// chosen customer id — the configuration that triggers hierarchy expansion.
func newManagerTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{LoginCustomerID: "9999999999"},
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

// TestListAccessibleCustomers_SendsNoRequestBody asserts the discovery request carries no
// request body. The endpoint is account-agnostic and takes no payload; sending one (or a
// stray Content-Type announcing one) would be a protocol error the Google Ads API may
// reject. doRequest omits Content-Type exactly when the body is nil, so this also pins
// that branch of doRequest for the nil-body caller.
func TestListAccessibleCustomers_SendsNoRequestBody(t *testing.T) {
	// Captured under a mutex and asserted after the call — see the note in
	// TestListAccessibleCustomers_Success.
	var (
		mu             sync.Mutex
		gotBody        []byte
		gotBodyErr     error
		gotContentType string
		sawAPICall     bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		b, err := io.ReadAll(r.Body)
		mu.Lock()
		sawAPICall = true
		gotContentType = r.Header.Get("Content-Type")
		gotBody, gotBodyErr = b, err
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/1"}})
	}))
	defer server.Close()

	if _, err := newAccountsTestClient(t, server).ListAccessibleCustomers(context.Background()); err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawAPICall {
		t.Fatal("the API endpoint was never called")
	}
	if gotBodyErr != nil {
		t.Errorf("reading request body: %v", gotBodyErr)
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
	if apiErr.Method != http.MethodGet {
		t.Errorf("expected Method GET, got %q", apiErr.Method)
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

// TestListAccessibleCustomers_WorksWithoutCustomerID pins the fix for the chicken-and-egg
// that made account discovery unreachable. doRequest requires a digits-only
// c.account.CustomerID, but this endpoint is HOW a caller learns one: a connection that
// has credentials and no account chosen yet is precisely the state discovery exists to
// serve, and demanding an account id first meant the caller had to already know the
// answer. The client is built here with CustomerID deliberately empty.
func TestListAccessibleCustomers_WorksWithoutCustomerID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/1234567890"}})
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{Label: "Test"}, // no CustomerID — that is the point
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	accounts, err := client.ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("discovery must not require a customer id, got: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ResourceName != "customers/1234567890" {
		t.Errorf("accounts = %+v, want one customers/1234567890", accounts)
	}
}

// TestListAccessibleCustomers_RejectsMalformedLoginCustomerID pins that dropping the
// customer-id precondition did NOT drop the manager-id one. login-customer-id is still
// attached as a header on this call, so it still has to be well-formed.
func TestListAccessibleCustomers_RejectsMalformedLoginCustomerID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		t.Error("request reached the API despite an invalid login-customer-id")
	}))
	defer server.Close()

	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{LoginCustomerID: "123-456-7890"},
		WithBaseURL(server.URL),
		WithTokenURL(server.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)

	_, err := client.ListAccessibleCustomers(context.Background())
	if err == nil {
		t.Fatal("expected a validation error for a dashed login-customer-id, got nil")
	}
	if !strings.Contains(err.Error(), "login-customer-id") {
		t.Errorf("error = %v, want it to name login-customer-id", err)
	}
}

// TestListAccessibleCustomers_ExpandsManagerHierarchy pins the MCC case.
// customers:listAccessibleCustomers returns only what the authenticated user can act on
// DIRECTLY — it does not walk a manager hierarchy, whatever login-customer-id says. On an
// agency-managed connection that means the flat list is often just the manager itself and
// every child ad account (the ones a caller actually wants to pick) is missing. The
// customer_client expansion is what closes that gap, and it is also where labels come from.
func TestListAccessibleCustomers_ExpandsManagerHierarchy(t *testing.T) {
	var (
		mu             sync.Mutex
		searchPaths    []string
		gotLoginHeader string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		mu.Lock()
		gotLoginHeader = r.Header.Get("login-customer-id")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			mu.Lock()
			searchPaths = append(searchPaths, r.URL.Path)
			mu.Unlock()
			// The manager itself, a child, and a nested sub-manager. Only the child is
			// selectable: a manager account cannot hold campaigns.
			_, _ = io.WriteString(w, `{"results":[
				{"customerClient":{"id":"9999999999","descriptiveName":"Agency MCC","manager":true,"status":"ENABLED"}},
				{"customerClient":{"id":"2222222222","descriptiveName":"Child Ad Account","manager":false,"status":"ENABLED"}},
				{"customerClient":{"id":"3333333333","descriptiveName":"Sub MCC","manager":true,"status":"ENABLED"}}
			]}`)
			return
		}
		// The flat list sees only the manager — the whole reason expansion is needed.
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/9999999999"}})
	}))
	defer server.Close()

	client := newManagerTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}

	mu.Lock()
	paths := append([]string(nil), searchPaths...)
	loginHeader := gotLoginHeader
	mu.Unlock()

	if len(paths) != 1 {
		t.Fatalf("customer_client search ran %d times, want exactly 1: %v", len(paths), paths)
	}
	if !strings.Contains(paths[0], "customers/9999999999/googleAds:search") {
		t.Errorf("expansion ran against %s, want it scoped to the manager id", paths[0])
	}
	if loginHeader != "9999999999" {
		t.Errorf("login-customer-id header = %q, want 9999999999", loginHeader)
	}

	byName := map[string]string{}
	for _, a := range accounts {
		byName[a.ResourceName] = a.DescriptiveName
	}
	// The child must be present — its absence is the whole defect.
	if label, ok := byName["customers/2222222222"]; !ok {
		t.Errorf("child ad account missing from %+v; the flat list never returns it", accounts)
	} else if label != "Child Ad Account" {
		t.Errorf("child label = %q, want the descriptive_name the expansion carries", label)
	}
	// Manager accounts are filtered: they cannot hold campaigns, so offering one as a
	// choice would let a caller pick an account that fails at the first create.
	if _, ok := byName["customers/3333333333"]; ok {
		t.Errorf("sub-manager 3333333333 was offered as a selectable account: %+v", accounts)
	}
	// The CONFIGURED manager is filtered too, and it takes the other path to get here: it
	// arrives in the FLAT list (listAccessibleCustomers returns what the user can act on
	// directly, which for an MCC credential is the manager itself), where there is no
	// `manager` flag to recognise it by. Its own id is what identifies it. Leaving it in
	// would offer the caller an account that fails at the first campaign create.
	if _, ok := byName["customers/9999999999"]; ok {
		t.Errorf("the configured MCC 9999999999 was offered as a selectable account: %+v", accounts)
	}
	if len(accounts) != 1 {
		t.Errorf("accounts = %+v, want exactly the one child ad account", accounts)
	}
}

// TestListAccessibleCustomers_NoManagerSkipsExpansion pins the other half of the contract:
// without a manager id there is no hierarchy root to walk, so the flat list is the whole
// answer and no search request may be issued.
func TestListAccessibleCustomers_NoManagerSkipsExpansion(t *testing.T) {
	var (
		mu        sync.Mutex
		sawSearch bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			mu.Lock()
			sawSearch = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/1234567890"}})
	}))
	defer server.Close()

	if _, err := newAccountsTestClient(t, server).ListAccessibleCustomers(context.Background()); err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawSearch {
		t.Error("customer_client expansion ran with no login-customer-id configured")
	}
}

// TestListAccessibleCustomers_CustomerClientRowWithoutIDIsAnError pins the stated
// invariant that a row with no id fails the whole call. Silently dropping it would be
// the tempting alternative and is the wrong one: the operator would be shown a SHORT
// list with no indication that it is short, and would then conclude the missing account
// is not reachable by this credential — a false negative that looks authoritative.
// TestListAccessibleCustomers_CustomerClientRowWithUnusableIDIsAnError covers both
// unusable shapes. Absent is the obvious one; NON-NUMERIC is the one an emptiness check
// misses, and it is the dangerous one — the id is concatenated straight into
// "customers/"+id, so "1/other" forges a resource name pointing at a different account
// than the row describes, and a caller persists it as the connection's account id.
func TestListAccessibleCustomers_CustomerClientRowWithUnusableIDIsAnError(t *testing.T) {
	cases := []struct {
		name string
		row  string
	}{
		{"absent id", `{"customerClient":{"descriptiveName":"Nameless","manager":false,"status":"ENABLED"}}`},
		{"non-numeric id forging a path", `{"customerClient":{"id":"1/other","descriptiveName":"Forged","manager":false,"status":"ENABLED"}}`},
		{"id with dashes as shown in the UI", `{"customerClient":{"id":"123-456-7890","descriptiveName":"Dashed","manager":false,"status":"ENABLED"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if writeAccountsToken(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "googleAds:search") {
					// A well-formed row FIRST: the good row ahead of it is what makes a
					// silent drop plausible — the call would still "succeed".
					_, _ = io.WriteString(w, `{"results":[
						{"customerClient":{"id":"2222222222","descriptiveName":"Child","manager":false,"status":"ENABLED"}},
						`+tc.row+`
					]}`)
					return
				}
				_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: []string{"customers/9999999999"}})
			}))
			defer server.Close()

			client := newManagerTestClient(t, server)

			accounts, err := client.ListAccessibleCustomers(context.Background())
			if err == nil {
				t.Fatalf("an unusable customer_client id must fail the call, got accounts %+v", accounts)
			}
			if accounts != nil {
				t.Errorf("accounts must be nil on error, got %+v", accounts)
			}
			// It is a malformed RESPONSE, not a rejected request — the distinction is what
			// the dispatcher maps to a 503-with-retry rather than a client error.
			var te *transportError
			if !errors.As(err, &te) {
				t.Fatalf("error must unwrap to *transportError, got %T: %v", err, err)
			}
			if !strings.Contains(te.Err.Error(), "numeric customer id") {
				t.Errorf("diagnostic must name the defect, got %q", te.Err.Error())
			}
		})
	}
}

// TestListAccessibleCustomers_MalformedResourceNameIsAnError pins the same contract one
// layer up. AccessibleCustomer promises "customers/{digits}"; the flat list is the other
// source of those values and had no validation at all, so a malformed 2xx could return an
// empty, wrong-kind, or path-bearing string as a selectable account.
func TestListAccessibleCustomers_MalformedResourceNameIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		resName string
	}{
		{"empty", ""},
		{"bare id, no prefix", "9999999999"},
		{"wrong resource kind", "customerClients/9999999999"},
		{"extra path segment", "customers/9999999999/campaigns/1"},
		{"non-numeric id", "customers/abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if writeAccountsToken(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// A valid name first, for the same reason as above.
				_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{
					ResourceNames: []string{"customers/1111111111", tc.resName},
				})
			}))
			defer server.Close()

			client := newAccountsTestClient(t, server)

			accounts, err := client.ListAccessibleCustomers(context.Background())
			if err == nil {
				t.Fatalf("resource name %q must fail the call, got accounts %+v", tc.resName, accounts)
			}
			if accounts != nil {
				t.Errorf("accounts must be nil on error, got %+v", accounts)
			}
			var te *transportError
			if !errors.As(err, &te) {
				t.Fatalf("error must unwrap to *transportError, got %T: %v", err, err)
			}
			if !strings.Contains(te.Err.Error(), "customers/{digits}") {
				t.Errorf("diagnostic must name the expected shape, got %q", te.Err.Error())
			}
		})
	}
}

// TestListAccessibleCustomers_DedupPrefersLabelledCopy exercises the branch where the
// SAME account arrives twice: unlabelled from the flat list, labelled from the manager
// expansion. Every other test returns the child only from the expansion, so appending
// both copies — or keeping the unlabelled one — would pass them all.
func TestListAccessibleCustomers_DedupPrefersLabelledCopy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[
				{"customerClient":{"id":"2222222222","descriptiveName":"Child Ad Account","manager":false,"status":"ENABLED"}}
			]}`)
			return
		}
		// The credential can act on the child DIRECTLY as well as through the manager,
		// so it appears in both — but the flat list has no descriptive_name to give.
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{
			ResourceNames: []string{"customers/9999999999", "customers/2222222222"},
		})
	}))
	defer server.Close()

	client := newManagerTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want exactly one entry — the duplicate must be merged, not appended", accounts)
	}
	if accounts[0].ResourceName != "customers/2222222222" {
		t.Fatalf("got %+v, want the child ad account", accounts[0])
	}
	// The expansion is the ONLY source of descriptive_name, so a dedup that keeps the
	// first-seen (flat, unlabelled) copy silently costs the operator every label.
	if accounts[0].DescriptiveName != "Child Ad Account" {
		t.Errorf("DescriptiveName = %q, want the expansion's label to win over the unlabelled flat copy",
			accounts[0].DescriptiveName)
	}
}

// TestListAccessibleCustomers_ManagerModeExcludesAccountsOutsideTheHierarchy pins the
// rule that makes manager mode useful: in manager mode the SELECTABLE set is the
// manager's children, not the union of those with the flat list.
//
// listAccessibleCustomers is unscoped — the login-customer-id header does not filter
// it — but every other request this client makes DOES carry that header. So an account
// the user can reach directly while it sits under a different manager comes back in the
// flat list and then fails with PERMISSION_DENIED as soon as anything addresses it.
// Offering it is offering a choice that cannot work, and the failure surfaces at first
// dispatch, long after the connection was saved, where it reads as a credential problem
// rather than a wrong-account one.
//
// The flat list here deliberately contains three things: the configured manager, a
// child that IS under it, and an outsider that is not. Only the child may survive.
func TestListAccessibleCustomers_ManagerModeExcludesAccountsOutsideTheHierarchy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			// The hierarchy under 9999999999 contains only 2222222222.
			_, _ = io.WriteString(w, `{"results":[
				{"customerClient":{"id":"2222222222","descriptiveName":"In Hierarchy","manager":false,"status":"ENABLED"}}
			]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{
			ResourceNames: []string{
				"customers/9999999999", // the configured manager itself
				"customers/2222222222", // reachable directly AND under the manager
				"customers/7777777777", // reachable directly, under a DIFFERENT manager
			},
		})
	}))
	defer server.Close()

	client := newManagerTestClient(t, server)

	accounts, err := client.ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want exactly the one account inside the configured hierarchy", accounts)
	}
	if accounts[0].ResourceName != "customers/2222222222" {
		t.Fatalf("got %+v, want customers/2222222222", accounts[0])
	}
	if accounts[0].DescriptiveName != "In Hierarchy" {
		t.Errorf("DescriptiveName = %q, want the expansion's label", accounts[0].DescriptiveName)
	}
	for _, a := range accounts {
		if a.ResourceName == "customers/7777777777" {
			t.Errorf("an account outside the configured manager hierarchy was offered as selectable: %+v", a)
		}
		if a.ResourceName == "customers/9999999999" {
			t.Errorf("the configured manager account was offered as selectable: %+v", a)
		}
	}
}

// TestListAccessibleCustomers_ManagerModeDedupsRepeatedChildren covers the one dedup
// that survives in manager mode. customer_client reports a client once per path through
// the hierarchy, so a client of a sub-manager that is itself a client of the root
// appears twice. Appending both would put the same account in the picker twice.
func TestListAccessibleCustomers_ManagerModeDedupsRepeatedChildren(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[
				{"customerClient":{"id":"2222222222","descriptiveName":"Reachable Two Ways","manager":false,"status":"ENABLED"}},
				{"customerClient":{"id":"2222222222","descriptiveName":"Reachable Two Ways","manager":false,"status":"ENABLED"}}
			]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{
			ResourceNames: []string{"customers/9999999999"},
		})
	}))
	defer server.Close()

	accounts, err := newManagerTestClient(t, server).ListAccessibleCustomers(context.Background())
	if err != nil {
		t.Fatalf("ListAccessibleCustomers failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want the repeated child collapsed to one entry", accounts)
	}
}
