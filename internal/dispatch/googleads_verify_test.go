// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// verifyServers wires a token endpoint plus a search endpoint under a caller-supplied handler.
func verifyServers(t *testing.T, searchH http.HandlerFunc) []googleads.Option {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "googleAds:search") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		searchH(w, r)
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}
}

// TestGoogleAdsVerify_AcceptedIsVerified pins the happy path.
func TestGoogleAdsVerify_AcceptedIsVerified(t *testing.T) {
	opts := verifyServers(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"customer":{"id":"1234567890"}}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	got := d.VerifyCredential(context.Background(), "cncf", model.ProviderGoogleAds)
	if got.State != domain.VerificationVerified {
		t.Errorf("state = %q, want verified (reason: %s)", got.State, got.Reason)
	}
}

// TestGoogleAdsVerify_RejectionIsInvalid pins that a definite provider refusal maps to the
// ACTIONABLE state, and carries a reason.
func TestGoogleAdsVerify_RejectionIsInvalid(t *testing.T) {
	opts := verifyServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"denied"}}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	got := d.VerifyCredential(context.Background(), "cncf", model.ProviderGoogleAds)
	if got.State != domain.VerificationInvalid {
		t.Errorf("state = %q, want invalid for a 403", got.State)
	}
	if got.Reason == "" {
		t.Error("an invalid verdict carried no reason; the operator is not told what to fix")
	}
}

// TestGoogleAdsVerify_ProviderOutageIsUnverifiable is the dangerous-direction test: a 5xx must
// NEVER be reported as invalid, or an outage sends an operator to re-authenticate a working
// credential.
func TestGoogleAdsVerify_ProviderOutageIsUnverifiable(t *testing.T) {
	opts := verifyServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream"}}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	got := d.VerifyCredential(context.Background(), "cncf", model.ProviderGoogleAds)
	if got.State == domain.VerificationInvalid {
		t.Fatal("a provider outage was reported as an INVALID credential; this sends an operator to re-authenticate a working credential")
	}
	if got.State != domain.VerificationUnverifiable {
		t.Errorf("state = %q, want unverifiable", got.State)
	}
}

// TestGoogleAdsVerify_ConnectionStateIsUnverifiableNotInvalid pins that failures which never
// reach Google are NOT credential verdicts. Google has said nothing, so claiming the credential
// is invalid would be inventing evidence.
func TestGoogleAdsVerify_ConnectionStateIsUnverifiableNotInvalid(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderGoogleAds, AccountID: "1", EncryptedCredentials: []byte(goodGoogleAdsCreds), Status: model.StatusInactive}}, identityEncryptor{}},
		{"incomplete credentials", fakeConnReader{conn: activeGoogleAdsConn(`{"ClientID":"cid"}`)}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, errEncryptor{}},
		{"missing account id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderGoogleAds, EncryptedCredentials: []byte(goodGoogleAdsCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(tc.repo, tc.enc)
			got := d.VerifyCredential(context.Background(), "cncf", model.ProviderGoogleAds)
			if got.State == domain.VerificationInvalid {
				t.Fatalf("%s reported the CREDENTIAL as invalid, but Google was never contacted", tc.name)
			}
			if got.State != domain.VerificationUnverifiable {
				t.Errorf("state = %q, want unverifiable", got.State)
			}
			if got.Reason == "" {
				t.Error("no reason given; the operator cannot tell which system to fix")
			}
		})
	}
}

// TestGoogleAdsVerify_SatisfiesTheCapabilityInterface pins the compile-time contract. Without
// it, a signature drift would silently drop the dispatcher out of the derived verifier map and
// degrade the endpoint to "unverifiable" with no test failure anywhere.
func TestGoogleAdsVerify_SatisfiesTheCapabilityInterface(t *testing.T) {
	var _ domain.CredentialVerifier = (*GoogleAdsDispatcher)(nil)
}
