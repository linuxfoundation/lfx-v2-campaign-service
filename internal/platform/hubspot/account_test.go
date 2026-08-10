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
	"strings"
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
			serverResponse: `{"hubId": "1234567"}`,
			expectPortalID: "1234567",
		},
		{
			name:           "hubId as numeric value",
			serverResponse: `{"hubId": 9876543}`,
			expectPortalID: "9876543",
		},
		{
			name:           "malformed JSON",
			serverResponse: `{invalid json}`,
			expectErr:      true,
			expectErrCause: false, // Cause is dropped, not wrapped
		},
		{
			name:           "missing hubId",
			serverResponse: `{}`,
			expectErr:      true,
		},
		{
			name:           "null hubId",
			serverResponse: `{"hubId": null}`,
			expectErr:      true,
		},
		{
			name:           "zero hubId",
			serverResponse: `{"hubId": 0}`,
			expectErr:      true,
		},
		{
			name:           "negative hubId",
			serverResponse: `{"hubId": "-42"}`,
			expectErr:      true,
		},
		{
			name:           "non-numeric hubId",
			serverResponse: `{"hubId": "abc"}`,
			expectErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != tokenInfoPath {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				// The token in the BODY is the whole reason this endpoint works for a
				// private app; the Authorization header alone is what the previous
				// account-info call sent, and that call is rejected outright. Pin it, or a
				// refactor that "tidies away" a body it reads as redundant silently
				// restores the bug.
				var sent struct {
					TokenKey string `json:"tokenKey"`
				}
				if derr := json.NewDecoder(r.Body).Decode(&sent); derr != nil {
					t.Fatalf("request body is not JSON: %v", derr)
				}
				if sent.TokenKey != "test-token" {
					t.Errorf("tokenKey = %q, want the private-app token", sent.TokenKey)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tt.serverResponse)
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

// TestAuthenticatedPortalID_MalformedResponseIsRedacted verifies that a malformed
// token-info response does not leak upstream data in logs.
func TestAuthenticatedPortalID_MalformedResponseIsRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Send response with marker bytes that should NOT appear in any error message
		_, _ = fmt.Fprint(w, `{"hubId": "SECRET_MARKER_12345"}`)
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
		t.Fatal("expected error for non-numeric hubId")
	}

	// Every assertion below is UNCONDITIONAL, and that is the point of this test.
	//
	// An earlier version guarded the marker check with `len(errMsg) < 100`, which inverted
	// it: encoding/json renders this exact input as `json: invalid number literal, trying
	// to unmarshal "\"SECRET_MARKER_12345\"" into Number`, so a regression that forwards
	// the cause produces a message about 115 characters long — past the guard. The check
	// ran only in the case where nothing had leaked and skipped itself in the one case it
	// existed to catch.
	//
	// Walk the whole chain, not just Error(). A leak reachable by errors.As is still a leak:
	// anything that renders a wrapped cause puts it in a log line.
	for e := err; e != nil; e = errors.Unwrap(e) {
		for _, marker := range []string{"SECRET", "MARKER", "12345"} {
			if strings.Contains(e.Error(), marker) {
				t.Errorf("error chain leaked upstream data (marker %q) at layer %T: %s", marker, e, e)
			}
		}
	}

	// The chain must also be flat: json.SyntaxError and json.UnmarshalTypeError reproduce
	// fragments of the input in their own messages, so reaching one means the cause was
	// forwarded even if this particular payload happened not to survive into it.
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		t.Errorf("error chain reaches a json decoder error, so the cause was not dropped: %v", err)
	}

	// Pin the message itself. Without this the test passes against an implementation that
	// returns a bare "decode failed" — redacted, but useless to the operator reading it.
	const want = "read hubspot account details: response is not valid JSON (32 bytes)"
	if err.Error() != want {
		t.Errorf("error message = %q, want %q", err.Error(), want)
	}
}
