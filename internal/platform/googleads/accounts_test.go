// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			json.NewEncoder(w).Encode(map[string]interface{}{
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
		json.NewEncoder(w).Encode(resp)
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		resp := listAccessibleCustomersResponse{ResourceNames: []string{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "mock_token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
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

// urlHasSuffix is a helper to check URL path suffix regardless of API version.
func urlHasSuffix(path string, suffix string) bool {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i:] == suffix {
			return true
		}
	}
	return false
}
