// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// microsoftTwoCustomerServer answers as a credential that reaches TWO customers, with the
// configured ad account living under the one the connection is NOT configured for.
//
// 9999999 is the customer activeMicrosoftConn stores; 8888888 is the other one. Account
// 1234567 — the account the connection names — is reachable only under 8888888.
func microsoftTwoCustomerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
		case strings.Contains(r.URL.Path, "User"):
			_, _ = io.WriteString(w, `{"CustomerRoles":[{"CustomerId":9999999,"RoleId":41},{"CustomerId":8888888,"RoleId":41}]}`)
		default:
			var req struct {
				CustomerID json.Number `json:"CustomerId"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			if req.CustomerID.String() == "8888888" {
				_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1234567,"Name":"LF Events","Number":"X1234567","AccountLifeCycleStatus":"Active","PauseReason":0}]}`)
				return
			}
			// The configured customer reaches a different account entirely.
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":7654321,"Name":"Other","Number":"X7654321","AccountLifeCycleStatus":"Active","PauseReason":0}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMicrosoftProbe_EnumeratesUnderTheConfiguredCustomer pins that the probe tests the scope
// DISPATCH will use, not every scope the credential can reach.
//
// cachedMicrosoftClient builds the dispatch client with the stored customer_id, and
// doCustomerRequest sends it as the CustomerId header on every request — so a campaign runs
// under that customer and nothing else. The probe used to enumerate with a ZERO AccountConfig,
// which makes discoveryCustomerIDs walk every CustomerRole the credential holds. A connection
// whose customer_id was stale or simply wrong therefore passed its test whenever the account was
// reachable under some OTHER customer, and then failed at campaign creation under the customer
// actually stored. customer_id is operator-settable through the connection config API, so that
// is a reachable state.
//
// This narrowing is not the one Google Ads' probe refuses. There the filter was the account
// PICKER's and had nothing to do with dispatch, so absence from it proved nothing. Here the
// narrowing IS dispatch's, so absence is the true statement the operator needs.
func TestMicrosoftProbe_EnumeratesUnderTheConfiguredCustomer(t *testing.T) {
	srv := microsoftTwoCustomerServer(t)

	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

	err := d.ProbeConnection(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if err == nil {
		t.Fatal("ProbeConnection reported a healthy connection while account 1234567 is reachable " +
			"only under customer 8888888 and the connection is configured for 9999999; the probe " +
			"enumerated every customer the credential reaches instead of the one dispatch uses, so " +
			"this connection tests clean and fails at campaign creation")
	}
	if !errors.Is(err, domain.ErrConnectionProbeFailed) {
		t.Errorf("ProbeConnection = %v, want a confirmed failed test: the credential authenticated "+
			"and the configured account is not reachable as this connection is configured", err)
	}
}

// TestMicrosoftProbe_MalformedCustomerIDIsAVerdictNotInconclusive pins the third state of the
// same field, and it is the one that reported a broken connection as healthy.
//
// customer_id is operator-settable through the connection config API and
// validateMicrosoftConnection does not constrain it, so a non-numeric or zero value is
// storable. discoveryCustomerIDs refuses it — correctly — but used to refuse it with an
// unsentineled error, which neither probe predicate recognised, so ProbeInconclusive's
// unrecognised-error default answered true and the service reported an unreachable platform. Dispatch under
// that customer cannot work, so the test has to say so.
func TestMicrosoftProbe_MalformedCustomerIDIsAVerdictNotInconclusive(t *testing.T) {
	for _, customerID := range []string{"abc", "0", "-1", "1.5", "99999999999999999999999"} {
		t.Run(customerID, func(t *testing.T) {
			srv := microsoftTwoCustomerServer(t)

			conn := activeMicrosoftConn(goodMicrosoftCreds)
			conn.ProviderConfig = map[string]string{"customer_id": customerID, "account_id": "1234567"}
			d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
				microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

			err := d.ProbeConnection(context.Background(), "cncf", model.ProviderMicrosoftAds)
			if err == nil {
				t.Fatalf("ProbeConnection reported customer_id %q as a healthy connection; it is not a "+
					"Microsoft customer identity, so no request can be built from it and campaign "+
					"creation on this connection cannot succeed", customerID)
			}
			if errors.Is(err, domain.ErrConnectionProbeInconclusive) {
				t.Fatalf("ProbeConnection = %v for customer_id %q, want a confirmed verdict: "+
					"inconclusive names an outage to wait out instead of the field to correct", err, customerID)
			}
			if !errors.Is(err, domain.ErrConnectionProbeFailed) {
				t.Errorf("ProbeConnection = %v for customer_id %q, want ErrConnectionProbeFailed", err, customerID)
			}
		})
	}
}

// TestMicrosoftProbe_StillWalksEveryCustomerWithNoneConfigured pins the other half. With no
// customer_id stored the credential is the whole question, and one AccountsInfo/Query cannot
// answer it — only walking every CustomerRole from User/Query covers the set. Narrowing here
// would report an account the credential genuinely reaches as unreachable.
func TestMicrosoftProbe_StillWalksEveryCustomerWithNoneConfigured(t *testing.T) {
	srv := microsoftTwoCustomerServer(t)

	conn := activeMicrosoftConn(goodMicrosoftCreds)
	conn.ProviderConfig = map[string]string{}
	d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

	if err := d.ProbeConnection(context.Background(), "cncf", model.ProviderMicrosoftAds); err != nil {
		t.Fatalf("ProbeConnection = %v, want success: with no customer configured the probe must "+
			"walk every customer the credential reaches, and account 1234567 is reachable under one "+
			"of them", err)
	}
}
