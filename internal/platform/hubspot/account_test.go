// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthenticatedPortalID(t *testing.T) {
	tests := []struct {
		name           string
		serverResponse string
		expectErr      bool
		expectPortalID string
		expectErrCause bool
	}{
		{
			name:           "valid response",
			serverResponse: `{"portalId": "1234567"}`,
			expectPortalID: "1234567",
		},
		{
			name:           "portalId as numeric value",
			serverResponse: `{"portalId": 9876543}`,
			expectPortalID: "9876543",
		},
		{
			name:           "malformed JSON",
			serverResponse: `{invalid json}`,
			expectErr:      true,
			expectErrCause: false, // Cause is dropped, not wrapped
		},
		{
			name:           "missing portalId",
			serverResponse: `{}`,
			expectErr:      true,
		},
		{
			name:           "null portalId",
			serverResponse: `{"portalId": null}`,
			expectErr:      true,
		},
		{
			name:           "zero portalId",
			serverResponse: `{"portalId": 0}`,
			expectErr:      true,
		},
		{
			name:           "negative portalId",
			serverResponse: `{"portalId": "-42"}`,
			expectErr:      true,
		},
		{
			name:           "non-numeric portalId",
			serverResponse: `{"portalId": "abc"}`,
			expectErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/account-info/v3/details" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.serverResponse)
			}))
			defer server.Close()

			client := NewClient(
				Credentials{PrivateAppToken: "test-token"},
				AccountConfig{PortalID: "8112310"},
				WithBaseURL(server.URL),
				withRetryBaseDelay(time.Millisecond),
			)

			portalID, err := client.AuthenticatedPortalID(context.Background())

			if tt.expectErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				// Verify cause is not present (for malformed JSON case)
				if tt.expectErrCause {
					var syntaxErr *json.SyntaxError
					var typeErr *json.UnmarshalTypeError
					if !errors.As(err, &syntaxErr) && !errors.As(err, &typeErr) {
						t.Errorf("expected json error cause in chain, but not found")
					}
				} else {
					// For malformed JSON, verify cause is NOT in the chain
					var syntaxErr *json.SyntaxError
					var typeErr *json.UnmarshalTypeError
					if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
						t.Errorf("unexpected json error cause in chain (should have been dropped)")
					}
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if portalID != tt.expectPortalID {
					t.Errorf("expected %q, got %q", tt.expectPortalID, portalID)
				}
			}
		})
	}
}

// TestAuthenticatedPortalID_MalformedResponseIsRedacted verifies that malformed
// account-info responses do not leak upstream data in logs.
func TestAuthenticatedPortalID_MalformedResponseIsRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Send response with marker bytes that should NOT appear in any error message
		fmt.Fprint(w, `{"portalId": "SECRET_MARKER_12345"}`)
	}))
	defer server.Close()

	client := NewClient(
		Credentials{PrivateAppToken: "test-token"},
		AccountConfig{PortalID: "8112310"},
		WithBaseURL(server.URL),
		withRetryBaseDelay(time.Millisecond),
	)

	_, err := client.AuthenticatedPortalID(context.Background())
	if err == nil {
		t.Fatal("expected error for non-numeric portalId")
	}

	// Verify the marker string does not appear in the error message
	if errors.Is(err, err) {
		// Walk the chain to verify no cause contains the marker
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			t.Errorf("error chain contains json decoder error (should be redacted)")
		}
	}

	errMsg := err.Error()
	if len(errMsg) > 0 && len(errMsg) < 100 {
		// Error message should be generic, not containing upstream data
		if contains(errMsg, "SECRET") || contains(errMsg, "MARKER") || contains(errMsg, "12345") {
			t.Errorf("error message leaked upstream data: %s", errMsg)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
