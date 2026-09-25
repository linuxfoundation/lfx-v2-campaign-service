// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// tokenThenStatus mints a token on the token path and answers every API read with status.
//
// The two are separated because a probe that authenticates and THEN fails on the account read
// is the whole subject of these tests: a failure that arrives after the credential was honoured
// cannot be described as the credential being refused.
func tokenThenStatus(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "access_token") || r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertUnreachableVerdict pins the shape shared by both 404 cases: a confirmed failure whose
// message points at the ACCOUNT, not at the credential.
//
// Both halves matter, and each of the other two placements is wrong in its own way. Keeping 404
// in ProbeCredentialRejected produced OK: false with a message telling the operator to
// re-authorise a credential the platform had honoured. Dropping it from that predicate with no
// arm to catch it matches NEITHER predicate — an apiError is not inconclusive — so it becomes
// ErrServiceDefect, a typed 500 that pages us about a connection the operator needs to repoint.
// Verified by deleting each arm and watching this test fail with exactly those two messages.
func assertUnreachableVerdict(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Fatalf("ProbeConnection = %v, want a confirmed failure; a 404 on the configured account "+
			"is the platform answering the question asked, and the inconclusive default would "+
			"report OK: true for an account it just said it cannot find", err)
	}
	if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
		t.Error("the inconclusive sentinel reached a 404 on the configured account")
	}
	if strings.Contains(err.Error(), "credential") && !strings.Contains(err.Error(), "does not reach") {
		t.Errorf("message %q blames the stored credential, which the platform accepted; the remedy "+
			"is the account id on the connection row", err)
	}
	if !strings.Contains(err.Error(), "does not reach") {
		t.Errorf("message %q does not say the account is unreachable, which is what a 404 after a "+
			"successful authentication means", err)
	}
}

// TestProbe404IsUnreachableNotRejected covers the two probes that name the configured account IN
// the request path — the only two for which a 404 is an answer rather than evidence that an
// endpoint moved.
func TestProbe404IsUnreachableNotRejected(t *testing.T) {
	t.Run("reddit", func(t *testing.T) {
		srv := tokenThenStatus(t, http.StatusNotFound)
		d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
			reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL))

		assertUnreachableVerdict(t, d.ProbeConnection(context.Background(), "tlf", model.ProviderRedditAds))
	})

	t.Run("x", func(t *testing.T) {
		srv := tokenThenStatus(t, http.StatusNotFound)
		d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
			twitter.WithBaseURL(srv.URL))

		assertUnreachableVerdict(t, d.ProbeConnection(context.Background(), "tlf", model.ProviderTwitterAds))
	})
}

// TestGoogleAdsProbe_DashedAccountIDDoesNotBlameTheCredential covers the one provider config with
// no Pattern at the design layer.
//
// design/connection.go declares google-ads account_id with an Example and no Pattern, so the
// dashed form the Google Ads UI displays — 866-674-6580 — is storable. ListAccessibleCustomers
// answers in the undashed form and can never contain it, so the membership check missed and the
// probe reported "the credential authenticates but does not reach account 866-674-6580" about a
// credential that reaches that account perfectly well under the id Google actually uses. The
// verdict was confirmed, operator-facing, and pointed at the wrong thing.
func TestGoogleAdsProbe_DashedAccountIDDoesNotBlameTheCredential(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	conn := activeGoogleAdsConn(goodGoogleAdsCreds)
	conn.AccountID = "866-674-6580"
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		googleads.WithBaseURL(srv.URL), googleads.WithTokenURL(srv.URL))

	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Fatalf("ProbeConnection = %v, want a confirmed failure; an id no Google Ads request can "+
			"address cannot dispatch a campaign", err)
	}
	if strings.Contains(err.Error(), "does not reach") {
		t.Errorf("message %q says the credential cannot reach the account; the credential is fine "+
			"and the stored id is simply not in the shape Google uses", err)
	}
	if reached {
		t.Error("the probe enumerated upstream for an id it could have rejected from its own shape")
	}
}
