// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			"report an unreachable platform for an account it just said it cannot find", err)
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

// TestGoogleAdsProbe_ReachedButNotCampaignCapable covers the three ways a manager-mode
// hierarchy walk can answer about the configured account.
//
// The picker's enumeration is filtered to ENABLED, non-manager clients. Read as membership,
// both of the accounts below are ABSENT from it, and absence was reported as "the google ads
// credential authenticates but does not reach account 1234567890" — false in both cases, and
// pointing the operator at an account id that is correct. The remedies differ from each other
// too, which is why these are two verdicts rather than one.
func TestGoogleAdsProbe_ReachedButNotCampaignCapable(t *testing.T) {
	cases := []struct {
		name string
		// row is the configured account's customer_client row, as the hierarchy reports it.
		row string
		// wantFragment is the service-authored phrase the verdict must carry.
		wantFragment string
		// wantNotReach pins that the account is NOT described as unreached.
		wantNotReach bool
	}{
		{
			name:         "suspended account is reached, not unreachable",
			row:          `{"customerClient":{"id":"1234567890","descriptiveName":"LF","manager":false,"status":"SUSPENDED"}}`,
			wantFragment: "is not enabled",
			wantNotReach: true,
		},
		{
			name:         "manager account is reached but cannot hold campaigns",
			row:          `{"customerClient":{"id":"1234567890","descriptiveName":"LF MCC","manager":true,"status":"ENABLED"}}`,
			wantFragment: "manager account and cannot hold campaigns",
			wantNotReach: true,
		},
		{
			name:         "genuinely absent account is still unreachable",
			row:          `{"customerClient":{"id":"5555555555","descriptiveName":"Someone Else","manager":false,"status":"ENABLED"}}`,
			wantFragment: "does not reach",
			wantNotReach: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var query atomic.Value
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				query.Store(string(b))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+tc.row+`]}`)
			}))
			defer apiSrv.Close()

			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			err := d.ProbeConnection(context.Background(), "tlf", model.ProviderGoogleAds)
			if !errors.Is(err, domain.ErrConnectionProbeFailed) {
				t.Fatalf("ProbeConnection = %v, want a confirmed failure; none of these accounts "+
					"can run a campaign, which is the question the test asks", err)
			}
			if !strings.Contains(err.Error(), tc.wantFragment) {
				t.Errorf("message %q does not carry %q", err, tc.wantFragment)
			}
			if tc.wantNotReach && strings.Contains(err.Error(), "does not reach") {
				t.Errorf("message %q says the credential does not reach the account, but the "+
					"hierarchy returned that very account; the remedy is not a new account id", err)
			}
			// The probe must ask its OWN question. Borrowing the picker's status predicate is
			// precisely what turned a reached-but-disabled account into an absent one.
			if q, _ := query.Load().(string); strings.Contains(q, "status = 'ENABLED'") {
				t.Errorf("probe query %q carries the picker's status filter; a filtered walk "+
					"cannot tell a disabled account from an absent one", q)
			}
		})
	}
}

// TestGoogleAdsProbe_DashedAccountIDDoesNotBlameTheCredential covers a stored id the design layer
// no longer admits but the datastore can still hold.
//
// design/connection.go declared google-ads account_id with an Example and no Pattern until
// LFXV2-2665, so the dashed form the Google Ads UI displays — 866-674-6580 — was storable through
// the API itself; it is still storable by bootstrap, by migrations, and by every row written
// before that pattern landed, which is why this runtime guard stays. ListAccessibleCustomers
// answers in the undashed form and can never contain it, so the membership check missed and the
// probe reported "the credential authenticates but does not reach account 866-674-6580" about a
// credential that reaches that account perfectly well under the id Google actually uses. The
// verdict was confirmed, operator-facing, and pointed at the wrong thing.
func TestGoogleAdsProbe_DashedAccountIDDoesNotBlameTheCredential(t *testing.T) {
	// atomic.Bool for the same reason unreachableUpstream's hit flag is one: the handler runs on
	// its own goroutine and the assertion below reads this from the test goroutine. The passing
	// case hides it — the probe short-circuits before any request, so the write never happens —
	// which is exactly the trap. The moment this guard earns its keep, the write and the read
	// become concurrent and -race reports a race ON TOP OF the real assertion failure, burying
	// the regression the guard exists to name.
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		reached.Store(true)
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
	if reached.Load() {
		t.Error("the probe enumerated upstream for an id it could have rejected from its own shape")
	}
}
